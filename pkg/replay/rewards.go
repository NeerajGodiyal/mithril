package replay

import (
	"bytes"
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/features"

	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/rewards"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/wide"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
)

func newWarmupCooldownRateEpoch(epochSchedule *sealevel.SysvarEpochSchedule, f *features.Features) *uint64 {
	slot, existed := f.ActivationSlot(features.ReduceStakeWarmupCooldown)
	if !existed {
		return nil
	}
	epoch := epochSchedule.GetEpoch(slot)
	return &epoch
}

func beginPartitionedEpochRewardsDistribution(acctsDb *accountsdb.AccountsDb, slotCtx *sealevel.SlotCtx, stakeHistory *sealevel.SysvarStakeHistory, epochCtx *ReplayCtx, epochSchedule *sealevel.SysvarEpochSchedule, block *block.Block, f *features.Features, epoch uint64, slot uint64) (*rewards.PartitionedRewardDistributionInfo, []*accounts.Account, []*accounts.Account) {
	updatedAccts, parentUpdatedAccts, voteRewardsDistributed := rewards.DistributeVotingRewards(acctsDb, block.Rewards, slot)
	partitionedRewardsInfo := rewards.DeterminePartitionedStakingRewardsInfoLocal(epochSchedule, &epochCtx.Inflation, epochCtx.Capitalization, epoch, epoch-1, epochCtx.SlotsPerYear, f)
	totalRewards := partitionedRewardsInfo.TotalStakingRewards

	newWarmupCooldownRateEpoch := newWarmupCooldownRateEpoch(epochSchedule, f)
	var points wide.Uint128
	var pointsPerStakeAcct map[solana.PublicKey]*rewards.CalculatedStakePoints
	// Use locally computed NumRewardPartitions, NOT block.NumRewardPartitions (which comes from RPC and may be MaxUint64 if missing)
	pointsPerStakeAcct, points, partitionedRewardsInfo.RewardPartitions = rewards.CalculateTotalPointsAndPartitions(acctsDb, slotCtx, slot, partitionedRewardsInfo.NumRewardPartitions, stakeHistory, newWarmupCooldownRateEpoch)
	pointValue := rewards.PointValue{Rewards: totalRewards, Points: points}
	partitionedRewardsInfo.StakingRewards = rewards.CalculateStakeRewards(pointsPerStakeAcct, slotCtx, stakeHistory, slot, epoch-1, pointValue, newWarmupCooldownRateEpoch, slotCtx.Features)

	newEpochRewards := sealevel.SysvarEpochRewards{DistributionStartingBlockHeight: block.BlockHeight + 1,
		NumPartitions: partitionedRewardsInfo.NumRewardPartitions, ParentBlockhash: block.LastBlockhash,
		TotalRewards: totalRewards, DistributedRewards: voteRewardsDistributed, TotalPoints: points, Active: true}

	epochRewardsAcct, err := acctsDb.GetAccount(slot, sealevel.SysvarEpochRewardsAddr)
	if err != nil {
		panic(fmt.Sprintf("unable to get EpochRewards from acctsdb: %s", err))
	}
	parentUpdatedAccts = append(parentUpdatedAccts, epochRewardsAcct.Clone())

	writer := new(bytes.Buffer)
	encoder := bin.NewBinEncoder(writer)
	newEpochRewards.MustMarshalWithEncoder(encoder)
	copy(epochRewardsAcct.Data, writer.Bytes())

	err = acctsDb.StoreAccounts([]*accounts.Account{epochRewardsAcct}, slot)
	if err != nil {
		panic(fmt.Sprintf("unable to update EpochRewards sysvar to acctsdb: %s", err))
	}
	sealevel.SysvarCache.EpochRewards.Acct = epochRewardsAcct
	sealevel.SysvarCache.EpochRewards.Sysvar = &newEpochRewards

	mlog.Log.Infof("rewards distribution start: epoch=%d slot=%d block_height=%d",
		epoch, slot, block.BlockHeight)
	mlog.Log.Infof("  distribution_start_height=%d partitions=%d total_rewards=%d vote_rewards=%d",
		newEpochRewards.DistributionStartingBlockHeight, newEpochRewards.NumPartitions,
		newEpochRewards.TotalRewards, voteRewardsDistributed)

	updatedAccts = append(updatedAccts, epochRewardsAcct.Clone())
	epochCtx.Capitalization += voteRewardsDistributed

	return partitionedRewardsInfo, updatedAccts, parentUpdatedAccts
}

// recalculatePartitionedRewardsForResume rebuilds the partitionedRewardsInfo when resuming
// during the epoch rewards period. This is needed because the in-memory partitionedRewardsInfo
// is lost on crash/restart, but we can reconstruct it from:
//   - EpochRewards sysvar: NumPartitions, TotalRewards, TotalPoints, ParentBlockhash
//   - Stake/Vote caches: populated from AccountsDB during resume
//
// This works because stake accounts cannot change during the rewards period (stake program
// rejects all operations when EpochRewards.Active == true), so the same inputs produce
// the same partition assignments and reward calculations.
func recalculatePartitionedRewardsForResume(
	acctsDb *accountsdb.AccountsDb,
	stakeHistory *sealevel.SysvarStakeHistory,
	epochSchedule *sealevel.SysvarEpochSchedule,
	f *features.Features,
	epoch uint64,
	slot uint64,
) *rewards.PartitionedRewardDistributionInfo {
	// Get stored EpochRewards sysvar (already loaded into cache during resume)
	epochRewards := sealevel.SysvarCache.EpochRewards.Sysvar
	if epochRewards == nil {
		panic("recalculatePartitionedRewardsForResume: EpochRewards sysvar not in cache")
	}

	mlog.Log.Infof("rewards resume: reconstructing partitionedRewardsInfo from stored state")
	mlog.Log.Infof("  epoch=%d slot=%d num_partitions=%d total_rewards=%d distributed=%d",
		epoch, slot, epochRewards.NumPartitions, epochRewards.TotalRewards, epochRewards.DistributedRewards)

	// Create a minimal SlotCtx with the stored ParentBlockhash
	// This blockhash is used by CalculateRewardPartitionForPubkey for deterministic partition assignment
	mockSlotCtx := &sealevel.SlotCtx{
		Blockhash:  epochRewards.ParentBlockhash,
		Features:   f,
		AccountsDb: acctsDb,
	}

	// Rebuild partition assignments and calculate points
	newWarmupCooldownRateEpoch := newWarmupCooldownRateEpoch(epochSchedule, f)
	pointsPerStakeAcct, points, partitions := rewards.CalculateTotalPointsAndPartitions(
		acctsDb, mockSlotCtx, slot, epochRewards.NumPartitions, stakeHistory, newWarmupCooldownRateEpoch)

	// Validate points match stored value
	if !points.Eq(epochRewards.TotalPoints) {
		mlog.Log.Warnf("rewards resume: points mismatch - computed=%s stored=%s",
			points.String(), epochRewards.TotalPoints.String())
	}

	// Rebuild reward calculations using stored total rewards and computed points
	pointValue := rewards.PointValue{
		Rewards: epochRewards.TotalRewards,
		Points:  epochRewards.TotalPoints, // Use stored points for consistency
	}
	stakingRewards := rewards.CalculateStakeRewards(
		pointsPerStakeAcct, mockSlotCtx, stakeHistory, slot, epoch-1,
		pointValue, newWarmupCooldownRateEpoch, f)

	// Build the partitioned rewards info struct
	firstSlotInEpoch := epochSchedule.FirstSlotInEpoch(epoch)

	mlog.Log.Infof("rewards resume: reconstruction complete - partitions=%d stake_rewards=%d",
		len(partitions), len(stakingRewards))

	return &rewards.PartitionedRewardDistributionInfo{
		TotalStakingRewards:    epochRewards.TotalRewards,
		FirstStakingRewardSlot: firstSlotInEpoch + 1,
		LastStakingRewardSlot:  firstSlotInEpoch + epochRewards.NumPartitions,
		NumRewardPartitions:    epochRewards.NumPartitions,
		RewardPartitions:       partitions,
		StakingRewards:         stakingRewards,
	}
}

func distributePartitionedEpochRewardsForSlot(acctsDb *accountsdb.AccountsDb, epochCtx *ReplayCtx, partitionedEpochRewardsInfo *rewards.PartitionedRewardDistributionInfo, currentSlot uint64, currentBlockHeight uint64) ([]*accounts.Account, []*accounts.Account) {
	epochRewardsAcct, err := acctsDb.GetAccount(currentSlot, sealevel.SysvarEpochRewardsAddr)
	if err != nil {
		panic(fmt.Sprintf("unable to get EpochRewards from acctsdb: %s", err))
	}

	var epochRewards sealevel.SysvarEpochRewards
	decoder := bin.NewBinDecoder(epochRewardsAcct.Data)
	epochRewards.MustUnmarshalWithDecoder(decoder)

	partitionIdx := currentBlockHeight - epochRewards.DistributionStartingBlockHeight

	// Bounds check: if partitionIdx is out of range, set inactive and return early.
	// This prevents panic from out-of-bounds Partition() access and handles any unexpected height drift.
	if partitionIdx >= partitionedEpochRewardsInfo.NumRewardPartitions {
		mlog.Log.Warnf("rewards distribution: partitionIdx %d >= numPartitions %d, setting inactive",
			partitionIdx, partitionedEpochRewardsInfo.NumRewardPartitions)
		epochRewards.Active = false

		writer := new(bytes.Buffer)
		encoder := bin.NewBinEncoder(writer)
		epochRewards.MustMarshalWithEncoder(encoder)
		copy(epochRewardsAcct.Data, writer.Bytes())

		err = acctsDb.StoreAccounts([]*accounts.Account{epochRewardsAcct}, currentSlot)
		if err != nil {
			panic(fmt.Sprintf("unable to update EpochRewards sysvar to acctsdb: %s", err))
		}
		sealevel.SysvarCache.EpochRewards.Acct = epochRewardsAcct
		sealevel.SysvarCache.EpochRewards.Sysvar = &epochRewards

		return nil, nil
	}

	distributedAccts, parentDistributedAccts, distributedLamports := rewards.DistributeStakingRewardsForPartition(acctsDb, partitionedEpochRewardsInfo.RewardPartitions.Partition(partitionIdx), partitionedEpochRewardsInfo.StakingRewards, currentSlot)
	parentDistributedAccts = append(parentDistributedAccts, epochRewardsAcct.Clone())

	epochRewards.Distribute(distributedLamports)

	// Stop distribution when we've processed the last partition (partition-based, not slot-based)
	if partitionIdx >= partitionedEpochRewardsInfo.NumRewardPartitions-1 {
		epochRewards.Active = false
		mlog.Log.Infof("rewards distribution complete: slot=%d block_height=%d partition_idx=%d num_partitions=%d distributed=%d",
			currentSlot, currentBlockHeight, partitionIdx, partitionedEpochRewardsInfo.NumRewardPartitions, epochRewards.DistributedRewards)
	}

	writer := new(bytes.Buffer)
	encoder := bin.NewBinEncoder(writer)
	epochRewards.MustMarshalWithEncoder(encoder)
	copy(epochRewardsAcct.Data, writer.Bytes())

	err = acctsDb.StoreAccounts([]*accounts.Account{epochRewardsAcct}, currentSlot)
	if err != nil {
		panic(fmt.Sprintf("unable to update EpochRewards sysvar to acctsdb: %s", err))
	}
	sealevel.SysvarCache.EpochRewards.Acct = epochRewardsAcct
	sealevel.SysvarCache.EpochRewards.Sysvar = &epochRewards

	distributedAccts = append(distributedAccts, epochRewardsAcct.Clone())
	epochCtx.Capitalization += distributedLamports

	return distributedAccts, parentDistributedAccts
}
