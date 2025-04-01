package replay

import (
	"bytes"
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/rewards"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/Overclock-Validator/mithril/pkg/safemath"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/util"
	"github.com/Overclock-Validator/wide"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
)

func newWarmupCooldownRateEpoch(epochSchedule *sealevel.SysvarEpochSchedule, f *features.Features) *uint64 {
	/*slot, existed := f.ActivationSlot(features.ReduceStakeWarmupCooldown)
	if !existed {
		return nil
	}

	epoch := epochSchedule.GetEpoch(slot)*/
	epoch := uint64(565)
	return &epoch
}

func calculatePartitionedEpochRewardsDuringRewardsWindow(partitionedRewardsInfo *rewards.PartitionedRewardDistributionInfo, acctsDb *accountsdb.AccountsDb, block *Block, epochSchedule *sealevel.SysvarEpochSchedule, slot uint64, epoch uint64, f *features.Features) {
	stakeHistoryAcct, err := acctsDb.GetAccount(slot, sealevel.SysvarStakeHistoryAddr)
	if err != nil {
		panic(fmt.Sprintf("unable to retrieve stakehistory sysvar: %s", err))
	}

	decoder := bin.NewBinDecoder(stakeHistoryAcct.Data)
	var stakeHistory sealevel.SysvarStakeHistory
	stakeHistory.MustUnmarshalWithDecoder(decoder)

	epochRewardsAcct, err := acctsDb.GetAccount(slot, sealevel.SysvarEpochRewardsAddr)
	if err != nil {
		panic(fmt.Sprintf("unable to retrieve epochrewards sysvar: %s", err))
	}

	decoder = bin.NewBinDecoder(epochRewardsAcct.Data)
	var epochRewards sealevel.SysvarEpochRewards
	epochRewards.MustUnmarshalWithDecoder(decoder)

	newWarmupCooldownRateEpoch := newWarmupCooldownRateEpoch(epochSchedule, f)
	var points wide.Uint128
	points, partitionedRewardsInfo.RewardPartitions = rewards.CalculateTotalPointsAndPartitionsDuringRewardsWindow(acctsDb, epochRewards.ParentBlockhash, block.StakeAccts, slot, partitionedRewardsInfo.NumRewardPartitions, &stakeHistory, newWarmupCooldownRateEpoch, f)

	totalRewards := partitionedRewardsInfo.TotalStakingRewards
	pointValue := rewards.PointValue{Rewards: totalRewards, Points: points}
	partitionedRewardsInfo.StakingRewards = rewards.CalculateStakeRewardsDuringRewardsWindow(acctsDb, block.StakeAccts, &stakeHistory, slot, epoch-1, pointValue, newWarmupCooldownRateEpoch, f)
}

func beginPartitionedEpochRewardsDistribution(acctsDb *accountsdb.AccountsDb, slotCtx *sealevel.SlotCtx, stakeHistory *sealevel.SysvarStakeHistory, epochCtx *EpochCtx, epochSchedule *sealevel.SysvarEpochSchedule, rpcc *rpcclient.RpcClient, block *Block, f *features.Features, epoch uint64, slot uint64) (*rewards.PartitionedRewardDistributionInfo, []solana.PublicKey) {
	rewardPks, voteRewardsDistributed := rewards.DistributeVotingRewards(acctsDb, block.Rewards, slot)
	partitionedRewardsInfo := rewards.DeterminePartitionedStakingRewardsInfo(rpcc, epochSchedule, &epochCtx.Inflation, epochCtx.Capitalization, epoch, epoch-1, slot, epochCtx.SlotsPerYear, f)

	var totalRewards uint64
	if f.IsActive(features.EnablePartitionedEpochReward) {
		var err error
		totalRewards, err = safemath.CheckedAddU64(voteRewardsDistributed, partitionedRewardsInfo.TotalStakingRewards)
		if err != nil {
			panic("overflow calculating total rewards")
		}
	} else {
		totalRewards = partitionedRewardsInfo.TotalStakingRewards
	}

	newWarmupCooldownRateEpoch := newWarmupCooldownRateEpoch(epochSchedule, f)
	var points wide.Uint128
	points, partitionedRewardsInfo.RewardPartitions = rewards.CalculateTotalPointsAndPartitions(acctsDb, slotCtx, slot, block.NumRewardPartitions, stakeHistory, newWarmupCooldownRateEpoch)
	pointValue := rewards.PointValue{Rewards: totalRewards, Points: points}

	partitionedRewardsInfo.StakingRewards = rewards.CalculateStakeRewards(acctsDb, slotCtx, stakeHistory, slot, epoch-1, pointValue, newWarmupCooldownRateEpoch, slotCtx.Features)

	newEpochRewards := sealevel.SysvarEpochRewards{DistributionStartingBlockHeight: block.BlockHeight + 1,
		NumPartitions: block.NumRewardPartitions, ParentBlockhash: block.LastBlockhash,
		TotalRewards: totalRewards, DistributedRewards: voteRewardsDistributed, TotalPoints: points, Active: true}

	mlog.Log.Debugf("epoch rewards initial: %s", newEpochRewards)

	epochRewardsAcct, err := acctsDb.GetAccount(slot, sealevel.SysvarEpochRewardsAddr)
	if err != nil {
		panic(fmt.Sprintf("unable to get EpochRewards from acctsdb: %s", err))
	}

	writer := new(bytes.Buffer)
	encoder := bin.NewBinEncoder(writer)
	newEpochRewards.MustMarshalWithEncoder(encoder)
	copy(epochRewardsAcct.Data, writer.Bytes())

	err = acctsDb.StoreAccounts([]*accounts.Account{epochRewardsAcct}, slot)
	if err != nil {
		panic(fmt.Sprintf("unable to update EpochRewards sysvar to acctsdb: %s", err))
	}

	rewardPks = append(rewardPks, sealevel.SysvarEpochRewardsAddr)
	epochCtx.Capitalization += voteRewardsDistributed

	return partitionedRewardsInfo, rewardPks
}

func distributePartitionedEpochRewardsForSlot(acctsDb *accountsdb.AccountsDb, epochCtx *EpochCtx, partitionedEpochRewardsInfo *rewards.PartitionedRewardDistributionInfo, currentSlot uint64, currentBlockHeight uint64, lastRewardsDistributionSlot uint64) []solana.PublicKey {
	epochRewardsAcct, err := acctsDb.GetAccount(currentSlot, sealevel.SysvarEpochRewardsAddr)
	if err != nil {
		panic(fmt.Sprintf("unable to get EpochRewards from acctsdb: %s", err))
	}

	var epochRewards sealevel.SysvarEpochRewards

	decoder := bin.NewBinDecoder(epochRewardsAcct.Data)
	epochRewards.MustUnmarshalWithDecoder(decoder)

	partitionIdx := currentBlockHeight - epochRewards.DistributionStartingBlockHeight
	distributedPks, distributedLamports := rewards.DistributeStakingRewardsForPartition(acctsDb, partitionedEpochRewardsInfo.RewardPartitions[partitionIdx], partitionedEpochRewardsInfo.StakingRewards, currentSlot)

	epochRewards.Distribute(distributedLamports)

	if currentSlot == lastRewardsDistributionSlot {
		epochRewards.Active = false
	}

	mlog.Log.Debugf("epoch rewards update: %s", epochRewards)

	writer := new(bytes.Buffer)
	encoder := bin.NewBinEncoder(writer)
	epochRewards.MustMarshalWithEncoder(encoder)
	copy(epochRewardsAcct.Data, writer.Bytes())

	err = acctsDb.StoreAccounts([]*accounts.Account{epochRewardsAcct}, currentSlot)
	if err != nil {
		panic(fmt.Sprintf("unable to update EpochRewards sysvar to acctsdb: %s", err))
	}

	mlog.Log.Debugf("distributePartitionedEpochRewards: slot %d, using partitionIdx %d (partitionedEpochRewardsInfo.FirstStakingRewardSlot = %d)", currentSlot, partitionIdx, partitionedEpochRewardsInfo.FirstStakingRewardSlot)
	rewardPks := make([]solana.PublicKey, 0, len(distributedPks)+1)
	rewardPks = append(rewardPks, distributedPks...)
	rewardPks = append(rewardPks, sealevel.SysvarEpochRewardsAddr)
	rewardPks = util.DedupePubkeys(rewardPks)

	epochCtx.Capitalization += distributedLamports

	return rewardPks
}
