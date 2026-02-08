package replay

import (
	"bytes"
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/rewards"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/wide"
	bin "github.com/gagliardetto/binary"
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
	partitionedRewardsInfo := rewards.DeterminePartitionedStakingRewardsInfo(epochSchedule, &epochCtx.Inflation, epochCtx.Capitalization, epoch, epoch-1, slot, epochCtx.SlotsPerYear, f)
	totalRewards := partitionedRewardsInfo.TotalStakingRewards

	newWarmupCooldownRateEpoch := newWarmupCooldownRateEpoch(epochSchedule, f)
	voteCacheSnapshot := global.VoteCacheSnapshot()

	pointValue := rewards.PointValue{Rewards: totalRewards, Points: wide.Uint128{}}
	streamResult, streamErr := rewards.CalculateRewardsStreaming(
		acctsDb, slot, stakeHistory, newWarmupCooldownRateEpoch,
		voteCacheSnapshot, pointValue, epoch-1, slotCtx.Blockhash, slotCtx, f)
	if streamErr != nil {
		panic(fmt.Sprintf("streaming rewards calculation failed: %s", streamErr))
	}

	partitionedRewardsInfo.SpoolDir = streamResult.SpoolDir
	partitionedRewardsInfo.SpoolSlot = streamResult.SpoolSlot
	partitionedRewardsInfo.NumRewardPartitionsRemaining = streamResult.NumPartitions

	updatedAccts, parentUpdatedAccts, voteRewardsDistributed := rewards.DistributeVotingRewards(acctsDb, streamResult.ValidatorRewards, slot)

	newEpochRewards := sealevel.SysvarEpochRewards{DistributionStartingBlockHeight: block.BlockHeight + 1,
		NumPartitions: streamResult.NumPartitions, ParentBlockhash: block.LastBlockhash,
		TotalRewards: totalRewards, DistributedRewards: voteRewardsDistributed, TotalPoints: streamResult.TotalPoints, Active: true}

	epochRewardsAcct, err := acctsDb.GetAccount(slot, sealevel.SysvarEpochRewardsAddr)
	if err != nil {
		panic(fmt.Sprintf("unable to get EpochRewards from acctsdb: %s", err))
	}
	parentUpdatedAccts = append(parentUpdatedAccts, epochRewardsAcct.Clone())

	writer := new(bytes.Buffer)
	encoder := bin.NewBinEncoder(writer)
	newEpochRewards.MustMarshalWithEncoder(encoder)
	copy(epochRewardsAcct.Data, writer.Bytes())

	err = acctsDb.StoreAccounts([]*accounts.Account{epochRewardsAcct}, slot, nil)
	if err != nil {
		panic(fmt.Sprintf("unable to update EpochRewards sysvar to acctsdb: %s", err))
	}
	sealevel.SysvarCache.EpochRewards.Acct = epochRewardsAcct
	sealevel.SysvarCache.EpochRewards.Sysvar = &newEpochRewards

	updatedAccts = append(updatedAccts, epochRewardsAcct.Clone())
	epochCtx.Capitalization += voteRewardsDistributed

	return partitionedRewardsInfo, updatedAccts, parentUpdatedAccts
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

	distributedAccts, parentDistributedAccts, distributedLamports, burnedLamports := rewards.DistributeStakingRewardsFromSpool(acctsDb, partitionedEpochRewardsInfo.SpoolDir, partitionedEpochRewardsInfo.SpoolSlot, partitionIdx, currentSlot)
	parentDistributedAccts = append(parentDistributedAccts, epochRewardsAcct.Clone())

	// EpochRewards sysvar advances by distributed + burned (matching Agave/FD),
	// but capitalization only increases by distributed.
	epochRewards.Distribute(distributedLamports + burnedLamports)
	partitionedEpochRewardsInfo.NumRewardPartitionsRemaining--

	if partitionedEpochRewardsInfo.NumRewardPartitionsRemaining == 0 {
		epochRewards.Active = false
		rewards.CleanupPartitionedSpoolFiles(partitionedEpochRewardsInfo.SpoolDir, partitionedEpochRewardsInfo.SpoolSlot, epochRewards.NumPartitions)
	}

	writer := new(bytes.Buffer)
	encoder := bin.NewBinEncoder(writer)
	epochRewards.MustMarshalWithEncoder(encoder)
	copy(epochRewardsAcct.Data, writer.Bytes())

	err = acctsDb.StoreAccounts([]*accounts.Account{epochRewardsAcct}, currentSlot, nil)
	if err != nil {
		panic(fmt.Sprintf("unable to update EpochRewards sysvar to acctsdb: %s", err))
	}
	sealevel.SysvarCache.EpochRewards.Acct = epochRewardsAcct
	sealevel.SysvarCache.EpochRewards.Sysvar = &epochRewards

	distributedAccts = append(distributedAccts, epochRewardsAcct.Clone())
	epochCtx.Capitalization += distributedLamports

	if burnedLamports > 0 {
		mlog.Log.Warnf("partition %d: distributed=%d burned=%d", partitionIdx, distributedLamports, burnedLamports)
	}

	return distributedAccts, parentDistributedAccts
}
