package replay

import (
	"bytes"
	"fmt"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/boundary"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/rewards"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/version"
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

func beginPartitionedEpochRewardsDistribution(acctsDb *accountsdb.AccountsDb, slotCtx *sealevel.SlotCtx, stakeHistory *sealevel.SysvarStakeHistory, epochCtx *ReplayCtx, epochSchedule *sealevel.SysvarEpochSchedule, rpcc *rpcclient.RpcClient, rpcBackups []string, block *block.Block, f *features.Features, epoch uint64, slot uint64) (*rewards.PartitionedRewardDistributionInfo, []*accounts.Account, []*accounts.Account) {
	partitionedRewardsInfo := rewards.DeterminePartitionedStakingRewardsInfo(rpcc, rpcBackups, epochSchedule, &epochCtx.Inflation, epochCtx.Capitalization, epoch, epoch-1, slot, epochCtx.SlotsPerYear, f)
	totalRewards := partitionedRewardsInfo.TotalStakingRewards

	newWarmupCooldownRateEpoch := newWarmupCooldownRateEpoch(epochSchedule, f)
	var points wide.Uint128
	var pointsPerStakeAcct map[solana.PublicKey]*rewards.CalculatedStakePoints

	pointsPerStakeAcct, points = rewards.CalculateStakePoints(acctsDb, slotCtx, slot, stakeHistory, newWarmupCooldownRateEpoch)
	pointValue := rewards.PointValue{Rewards: totalRewards, Points: points}

	var validatorRewards map[solana.PublicKey]uint64
	partitionedRewardsInfo.StakingRewards, validatorRewards, partitionedRewardsInfo.RewardPartitions = rewards.CalculateStakeRewardsAndPartitions(pointsPerStakeAcct, slotCtx, stakeHistory, slot, epoch-1, pointValue, newWarmupCooldownRateEpoch, slotCtx.Features)
	updatedAccts, parentUpdatedAccts, voteRewardsDistributed := rewards.DistributeVotingRewards(acctsDb, validatorRewards, slot)
	partitionedRewardsInfo.NumRewardPartitionsRemaining = partitionedRewardsInfo.RewardPartitions.NumPartitions()

	newEpochRewards := sealevel.SysvarEpochRewards{DistributionStartingBlockHeight: block.BlockHeight + 1,
		NumPartitions: partitionedRewardsInfo.NumRewardPartitionsRemaining, ParentBlockhash: block.LastBlockhash,
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

	updatedAccts = append(updatedAccts, epochRewardsAcct.Clone())
	epochCtx.Capitalization += voteRewardsDistributed

	// Generate boundary report
	writeBoundaryReport(slotCtx, epochCtx, epoch, slot, points, pointsPerStakeAcct, partitionedRewardsInfo, validatorRewards, voteRewardsDistributed, &newEpochRewards)

	return partitionedRewardsInfo, updatedAccts, parentUpdatedAccts
}

// writeBoundaryReport generates and writes the LOCAL boundary report
func writeBoundaryReport(
	slotCtx *sealevel.SlotCtx,
	epochCtx *ReplayCtx,
	epoch uint64,
	slot uint64,
	points wide.Uint128,
	pointsPerStakeAcct map[solana.PublicKey]*rewards.CalculatedStakePoints,
	partitionedRewardsInfo *rewards.PartitionedRewardDistributionInfo,
	validatorRewards map[solana.PublicKey]uint64,
	voteRewardsDistributed uint64,
	epochRewardsSysvar *sealevel.SysvarEpochRewards,
) {
	// Always log summary to mithril.log
	mlog.Log.Infof("epoch %d: %d stake accounts, %d partitions, staking_rewards=%d, vote_rewards=%d, total_points=%s",
		epoch, len(partitionedRewardsInfo.StakingRewards), partitionedRewardsInfo.RewardPartitions.NumPartitions(),
		partitionedRewardsInfo.TotalStakingRewards, voteRewardsDistributed, points.String())

	// Check if boundary file logging is enabled
	if !boundary.IsEnabled() {
		return
	}

	// Count statistics from points calculation
	zero128 := wide.Uint128FromUint64(0)
	var numForceCreditsUpdate, numZeroPoints int
	for _, pts := range pointsPerStakeAcct {
		if pts.ForceCreditsUpdateWithSkippedReward {
			numForceCreditsUpdate++
		}
		if pts.Points.Eq(zero128) {
			numZeroPoints++
		}
	}

	// Count zero-reward stake accounts (points > 0 but reward calculated as 0)
	numZeroReward := 0
	for _, reward := range partitionedRewardsInfo.StakingRewards {
		if reward.StakerRewards == 0 && reward.VoterRewards == 0 {
			numZeroReward++
		}
	}

	// Calculate total staked from stake cache
	var totalStaked uint64
	for _, delegation := range global.StakeCache() {
		totalStaked += delegation.StakeLamports
	}

	// Get partition counts for debug level
	var partitionCounts []int
	if partitionedRewardsInfo.RewardPartitions != nil {
		numParts := partitionedRewardsInfo.RewardPartitions.NumPartitions()
		partitionCounts = make([]int, numParts)
		for i := uint64(0); i < numParts; i++ {
			partition := partitionedRewardsInfo.RewardPartitions.Partition(i)
			if partition != nil {
				partitionCounts[i] = int(partition.NumPubkeys())
			}
		}
	}

	report := &boundary.BoundaryReport{
		Header: boundary.HeaderSection{
			Source:          "LOCAL",
			Epoch:           epoch,
			PrevEpoch:       epoch - 1,
			BoundarySlot:    slotCtx.Slot,
			FirstRewardSlot: partitionedRewardsInfo.FirstStakingRewardSlot,
			Timestamp:       time.Now().UTC().Format(time.RFC3339),
			GitCommit:       version.GitCommit,
			RunID:           mlog.GetRunID(),
		},
		Inputs: boundary.InputsSection{
			Capitalization:   epochCtx.Capitalization,
			InflationRate:    epochCtx.Inflation.Total(float64(epoch)),
			ValidatorRewards: epochCtx.Inflation.Validator(float64(epoch)),
			TotalStaked:      totalStaked,
			NumStakeAccounts: len(global.StakeCache()),
			NumVoteAccounts:  len(validatorRewards),
			ParentBlockhash:  solana.HashFromBytes(slotCtx.Blockhash[:]).String(),
		},
		Schedule: boundary.ScheduleSection{
			Source: "local",
		},
		Rewards: boundary.RewardsSection{
			TotalPoints:           points.String(),
			TotalStakingRewards:   partitionedRewardsInfo.TotalStakingRewards,
			TotalVoteRewards:      voteRewardsDistributed,
			NumPartitions:         int(partitionedRewardsInfo.RewardPartitions.NumPartitions()),
			NumEligibleStakeAccts: len(partitionedRewardsInfo.StakingRewards),
			NumForceCreditsUpdate: numForceCreditsUpdate,
			NumZeroPoints:         numZeroPoints,
			NumZeroReward:         numZeroReward,
		},
		Sysvars: boundary.SysvarsSection{
			EpochRewardsActive:      epochRewardsSysvar.Active,
			DistributionStartHeight: epochRewardsSysvar.DistributionStartingBlockHeight,
			NumPartitions:           epochRewardsSysvar.NumPartitions,
			TotalRewards:            epochRewardsSysvar.TotalRewards,
			DistributedRewards:      epochRewardsSysvar.DistributedRewards,
			ParentBlockhash:         solana.HashFromBytes(epochRewardsSysvar.ParentBlockhash[:]).String(),
		},
		Partitions: &boundary.PartitionsSection{
			PartitionCounts: partitionCounts,
		},
	}

	if err := boundary.WriteReport(report, boundary.LevelCompare); err != nil {
		mlog.Log.Warnf("failed to write boundary report: %v", err)
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
	distributedAccts, parentDistributedAccts, distributedLamports := rewards.DistributeStakingRewardsForPartition(acctsDb, partitionedEpochRewardsInfo.RewardPartitions.Partition(partitionIdx), partitionedEpochRewardsInfo.StakingRewards, currentSlot)
	parentDistributedAccts = append(parentDistributedAccts, epochRewardsAcct.Clone())

	epochRewards.Distribute(distributedLamports)
	partitionedEpochRewardsInfo.NumRewardPartitionsRemaining--

	if partitionedEpochRewardsInfo.NumRewardPartitionsRemaining == 0 {
		epochRewards.Active = false
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
