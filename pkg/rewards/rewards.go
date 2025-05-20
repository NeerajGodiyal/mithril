package rewards

import (
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/Overclock-Validator/mithril/pkg/safemath"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/wide"
	"github.com/dgryski/go-sip13"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/panjf2000/ants/v2"
)

const (
	RewardTypeFee     string = "Fee"
	RewardTypeRent    string = "Rent"
	RewardTypeVoting  string = "Voting"
	RewardTypeStaking string = "Staking"
)

type PartitionedRewardDistributionInfo struct {
	TotalStakingRewards    uint64
	FirstStakingRewardSlot uint64
	LastStakingRewardSlot  uint64
	EahStartOffsetSlot     uint64
	EahStopOffsetSlot      uint64
	NumRewardPartitions    uint64
	Credits                map[solana.PublicKey]CalculatedStakePoints
	RewardPartitions       map[uint64][]solana.PublicKey
	StakingRewards         map[solana.PublicKey]*CalculatedStakeRewards
}

func SlotInYearForInflation(epochSchedule *sealevel.SysvarEpochSchedule, slotsPerYear float64, epoch uint64, f *features.Features) float64 {
	numSlots := GetInflationNumSlots(epochSchedule, epoch, f)
	return float64(numSlots) / slotsPerYear
}

func GetInflationNumSlots(epochSchedule *sealevel.SysvarEpochSchedule, epoch uint64, f *features.Features) uint64 {
	inflationActivationSlot := GetInflationStartSlot(f)
	inflationStartSlot := epochSchedule.FirstSlotInEpoch(safemath.SaturatingSubU64(epochSchedule.GetEpoch(inflationActivationSlot), 1))
	return epochSchedule.FirstSlotInEpoch(epoch) - inflationStartSlot
}

func GetInflationStartSlot(f *features.Features) uint64 {
	fullInflationFeatures := f.FullInflationFeaturesEnabled()
	var activationSlots []uint64

	for _, inflationFeature := range fullInflationFeatures {
		activationSlot, _ := f.ActivationSlot(inflationFeature)
		activationSlots = append(activationSlots, activationSlot)
	}

	sort.Slice(activationSlots, func(i, j int) bool {
		return activationSlots[i] < activationSlots[j]
	})

	if len(activationSlots) == 0 {
		picoActivationSlot, isActivated := f.ActivationSlot(features.PicoInflation)
		if !isActivated {
			return 0
		} else {
			return picoActivationSlot
		}
	} else {
		return activationSlots[0]
	}
}

func CalculatePreviousEpochInflationRewards(epochSchedule *sealevel.SysvarEpochSchedule, inflation *Inflation, prevEpochCapitalization, epoch, prevEpoch uint64, slotsPerYear float64, f *features.Features) uint64 {
	slotInYear := SlotInYearForInflation(epochSchedule, slotsPerYear, epoch, f)
	validatorRate := inflation.Validator(slotInYear)
	prevEpochDurationInYears := float64(epochSchedule.SlotsInEpoch(prevEpoch)) / slotsPerYear

	validatorRewards := validatorRate * float64(prevEpochCapitalization) * prevEpochDurationInYears
	return uint64(validatorRewards)
}

func DeterminePartitionedStakingRewardsInfo(rpcc *rpcclient.RpcClient, epochSchedule *sealevel.SysvarEpochSchedule, inflation *Inflation, prevEpochCapitalization uint64, epoch uint64, prevEpoch uint64, slot uint64, slotsPerYear float64, f *features.Features) *PartitionedRewardDistributionInfo {
	var totalStakingRewards uint64

	firstSlotInEpoch := epochSchedule.FirstSlotInEpoch(epoch)
	numRewardPartitions, err := rpcc.GetNumRewardPartitions(firstSlotInEpoch)
	if err != nil {
		panic(err)
	}

	rewardSlots, err := rpcc.GetStakingRewardSlots(firstSlotInEpoch, numRewardPartitions)
	if err != nil {
		panic(err)
	}

	finalStakingRewardSlot := rewardSlots[len(rewardSlots)-1]

	// we only need number for total staking reward at the beginning of a new epoch
	// (for setting up the EpochRewards sysvar)
	if f.IsActive(features.EnablePartitionedEpochReward) {
		//mlog.Log.Debugf("RetrievePartitionedStakingRewardsInfo - EnablePartitionedEpochReward case")

		if slot == firstSlotInEpoch {
			for _, rewardSlot := range rewardSlots {
				rewards, err := rpcc.GetRewardsForSlot(rewardSlot)
				if err != nil {
					panic(fmt.Sprintf("error retrieving reward data from rpc: %s", err))
				}

				for _, reward := range rewards {
					//mlog.Log.Debugf("reward: %+v", reward)
					if string(reward.RewardType) == RewardTypeStaking {
						totalStakingRewards += uint64(reward.Lamports)
					}
				}
			}
		}
	} else if f.IsActive(features.EnablePartitionedEpochRewardsSuperfeature) {
		//mlog.Log.Debugf("RetrievePartitionedStakingRewardsInfo - EnablePartitionedEpochRewardsSuperfeature case")
		totalStakingRewards = CalculatePreviousEpochInflationRewards(epochSchedule, inflation, prevEpochCapitalization, epoch, prevEpoch, slotsPerYear, f)
	} else {
		panic("shouldn't be here without partitioned rewards enabled")
	}

	eahCalcSlot := firstSlotInEpoch + (432000 / 4)
	eahInclusionSlot := firstSlotInEpoch + ((432000 / 4) * 3)

	return &PartitionedRewardDistributionInfo{TotalStakingRewards: totalStakingRewards, FirstStakingRewardSlot: firstSlotInEpoch + 1,
		LastStakingRewardSlot: finalStakingRewardSlot, EahStartOffsetSlot: eahCalcSlot, EahStopOffsetSlot: eahInclusionSlot, NumRewardPartitions: numRewardPartitions}
}

func DistributeVotingRewards(acctsDb *accountsdb.AccountsDb, rewards []rpc.BlockReward, slot uint64) ([]solana.PublicKey, uint64) {
	var totalVotingRewards uint64
	var numVoteRewardEntries uint64

	for _, reward := range rewards {
		if string(reward.RewardType) == RewardTypeVoting {
			numVoteRewardEntries++
		}
	}

	var idx uint64
	accts := make([]*accounts.Account, numVoteRewardEntries)
	rewardPks := make([]solana.PublicKey, numVoteRewardEntries)

	for _, reward := range rewards {
		if string(reward.RewardType) == RewardTypeVoting {
			stakeAcct, err := acctsDb.GetAccount(slot, reward.Pubkey)
			if err != nil {
				panic(fmt.Sprintf("unable to get acct %s from acctsdb for voting rewards distribution in slot %d", reward.Pubkey, slot))
			}

			stakeAcct.Lamports, err = safemath.CheckedAddU64(stakeAcct.Lamports, uint64(reward.Lamports))
			if err != nil {
				panic(fmt.Sprintf("overflow in voting rewards distribution in slot %d to acct %s: %s", slot, reward.Pubkey, err))
			}

			if stakeAcct.Lamports != reward.PostBalance {
				panic(fmt.Sprintf("post-balance for acct %s in distributing voting rewards in slot %d did not match expected %d (actual %d)", reward.Pubkey, slot, reward.PostBalance, stakeAcct.Lamports))
			}

			accts[idx] = stakeAcct

			totalVotingRewards, err = safemath.CheckedAddU64(totalVotingRewards, uint64(reward.Lamports))
			if err != nil {
				panic(fmt.Sprintf("overflow in accumulating voting rewards in slot %d", slot))
			}

			rewardPks[idx] = reward.Pubkey
			idx++
		}
	}

	if idx != 0 {
		err := acctsDb.StoreAccounts(accts[:idx], slot)
		if err != nil {
			panic(fmt.Sprintf("error updating accounts for voting rewards in slot %d: %s", slot, err))
		}
	}

	return rewardPks[:idx], totalVotingRewards
}

func DistributeStakingRewardsForPartition(acctsDb *accountsdb.AccountsDb, partition []solana.PublicKey, stakingRewards map[solana.PublicKey]*CalculatedStakeRewards, slot uint64) ([]solana.PublicKey, uint64) {
	var distributedLamports uint64
	accts := make([]*accounts.Account, len(partition))
	rewardPks := make([]solana.PublicKey, len(partition))

	var idx uint64
	for _, stakePk := range partition {
		reward, ok := stakingRewards[stakePk]
		if !ok {
			//mlog.Log.Debugf("no staking rewards present in map for %s", stakePk)
			continue
		}

		stakeAcct, err := acctsDb.GetAccount(slot, stakePk)
		if err != nil {
			panic(fmt.Sprintf("unable to get acct %s from acctsdb for partitioned epoch rewards distribution in slot %d", stakePk, slot))
		}

		// update the delegation in the stake account state
		stakeState, err := sealevel.UnmarshalStakeState(stakeAcct.Data)
		if err != nil {
			panic(fmt.Sprintf("unable to deserialize stake account in distributing partitioned rewards: %s", err))
		}

		stakeState.Stake.Stake.CreditsObserved = reward.NewCreditsObserved
		stakeState.Stake.Stake.Delegation.StakeLamports = safemath.SaturatingAddU64(stakeState.Stake.Stake.Delegation.StakeLamports, uint64(reward.StakerRewards))

		newStakeStateBytes, err := sealevel.MarshalStakeStake(stakeState)
		if err != nil {
			panic(fmt.Sprintf("unable to serialize new stake account state in distributing partitioned rewards: %s", err))
		}
		copy(stakeAcct.Data, newStakeStateBytes)

		// update lamports in stake account
		stakeAcct.Lamports, err = safemath.CheckedAddU64(stakeAcct.Lamports, uint64(reward.StakerRewards))
		if err != nil {
			panic(fmt.Sprintf("overflow in partitioned epoch rewards distribution in slot %d to acct %s: %s", slot, stakePk, err))
		}

		accts[idx] = stakeAcct
		rewardPks[idx] = stakePk

		distributedLamports += uint64(reward.StakerRewards)
		//mlog.Log.Debugf("distributed partitioned rewards to %s, %d lamports", stakePk, reward.StakerRewards)

		idx++
	}

	if idx != 0 {
		err := acctsDb.StoreAccounts(accts[:idx], slot)
		if err != nil {
			panic(fmt.Sprintf("error updating accounts for partitioned epoch rewards in slot %d: %s", slot, err))
		}
	}

	return rewardPks[:idx], distributedLamports
}

func DistributeStakingRewards(acctsDb *accountsdb.AccountsDb, rewards []rpc.BlockReward, credits map[solana.PublicKey]CalculatedStakePoints, slot uint64) ([]solana.PublicKey, uint64) {
	var distributedLamports uint64
	accts := make([]*accounts.Account, 0, len(rewards))
	rewardPks := make([]solana.PublicKey, 0, len(rewards))

	for _, reward := range rewards {
		if string(reward.RewardType) == RewardTypeStaking {
			stakeAcct, err := acctsDb.GetAccount(slot, reward.Pubkey)
			if err != nil {
				panic(fmt.Sprintf("unable to get acct %s from acctsdb for partitioned epoch rewards distribution in slot %d", reward.Pubkey, slot))
			}

			// update the delegation in the stake account state
			stakeState, err := sealevel.UnmarshalStakeState(stakeAcct.Data)
			if err != nil {
				panic(fmt.Sprintf("unable to deserialize stake account in distributing partitioned rewards: %s", err))
			}

			stakeState.Stake.Stake.CreditsObserved = credits[reward.Pubkey].NewCreditsObserved
			stakeState.Stake.Stake.Delegation.StakeLamports = safemath.SaturatingAddU64(stakeState.Stake.Stake.Delegation.StakeLamports, uint64(reward.Lamports))

			newStakeStateBytes, err := sealevel.MarshalStakeStake(stakeState)
			if err != nil {
				panic(fmt.Sprintf("unable to serialize new stake account state in distributing partitioned rewards: %s", err))
			}
			copy(stakeAcct.Data, newStakeStateBytes)

			// update lamports in stake account
			stakeAcct.Lamports, err = safemath.CheckedAddU64(stakeAcct.Lamports, uint64(reward.Lamports))
			if err != nil {
				panic(fmt.Sprintf("overflow in partitioned epoch rewards distribution in slot %d to acct %s: %s", slot, reward.Pubkey, err))
			}

			if stakeAcct.Lamports != reward.PostBalance {
				panic(fmt.Sprintf("post-balance for acct %s in distributing epoch rewards in slot %d did not match expected %d (actual %d)", reward.Pubkey, slot, reward.PostBalance, stakeAcct.Lamports))
			}

			accts = append(accts, stakeAcct)
			rewardPks = append(rewardPks, reward.Pubkey)

			distributedLamports += uint64(reward.Lamports)
			//mlog.Log.Debugf("distributed partitioned rewards to %s, %d lamports", reward.Pubkey, reward.Lamports)
		}
	}

	if len(accts) != 0 {
		err := acctsDb.StoreAccounts(accts, slot)
		if err != nil {
			panic(fmt.Sprintf("error updating accounts for partitioned epoch rewards in slot %d: %s", slot, err))
		}
	}

	return rewardPks, distributedLamports
}

type CalculatedStakePoints struct {
	Points                              wide.Uint128
	NewCreditsObserved                  uint64
	ForceCreditsUpdateWithSkippedReward bool
}

func minimumStakeDelegation(slotCtx *sealevel.SlotCtx) uint64 {
	if !slotCtx.Features.IsActive(features.StakeMinimumDelegationForRewards) {
		return 0
	}

	if slotCtx.Features.IsActive(features.StakeRaiseMinimumDelegationTo1Sol) {
		return 1000000000
	}

	return 1
}

func minimumStakeDelegationFeatures(f *features.Features) uint64 {
	if !f.IsActive(features.StakeMinimumDelegationForRewards) {
		return 0
	}

	if f.IsActive(features.StakeRaiseMinimumDelegationTo1Sol) {
		return 1000000000
	}

	return 1
}

func CalculateRewardPartitionForPubkey(pubkey solana.PublicKey, blockhash [32]byte, numPartitions uint64) uint64 {
	var data [64]byte
	copy(data[:32], blockhash[:])
	copy(data[32:], pubkey[:])
	hash := sip13.Sum64(0, 0, data[:])

	ulongMaxPlus1 := wide.Uint128FromUint64(math.MaxUint64).Add(wide.Uint128FromUint64(1))
	partitionIdx := wide.Uint128FromUint64(numPartitions).Mul(wide.Uint128FromUint64(hash)).Div(ulongMaxPlus1)
	partitionIdx64 := partitionIdx.Uint64()

	//mlog.Log.Debugf("using blockhash %s in epoch rewards hasher, and num_partitions %d: hash = %d, partitionIdx = %d", solana.HashFromBytes(blockhash[:]), numPartitions, hash, partitionIdx64)

	return partitionIdx64
}

type PointValue struct {
	Rewards uint64
	Points  wide.Uint128
}

type CalculatedStakeRewards struct {
	StakerRewards      uint64
	VoterRewards       uint64
	NewCreditsObserved uint64
}

func CalculateStakeRewards(acctsDb *accountsdb.AccountsDb, slotCtx *sealevel.SlotCtx, stakeHistory *sealevel.SysvarStakeHistory, slot uint64, rewardedEpoch uint64, pointValue PointValue, newRateActivationEpoch *uint64, f *features.Features) map[solana.PublicKey]*CalculatedStakeRewards {
	stakeInfoResults := make(map[solana.PublicKey]*CalculatedStakeRewards, 1500000)
	minimumStakeDelegation := minimumStakeDelegation(slotCtx)

	var mu sync.Mutex
	var wg sync.WaitGroup

	workerPool, _ := ants.NewPoolWithFunc(10000, func(i interface{}) {
		defer wg.Done()

		stakePk := i.(solana.PublicKey)
		stakeAcct, err := acctsDb.GetAccount(slot, stakePk)
		if err != nil {
			//mlog.Log.Debugf("failed to get stake acct %s from accountsdb in calculating rewards points: %s", stakePk, err)
			return
		}

		if stakeAcct.Lamports == 0 {
			return
		}

		stakeState, err := sealevel.UnmarshalStakeState(stakeAcct.Data)
		if err != nil {
			//mlog.Log.Debugf("invalid stake acct state (%s) - should be impossible: %s", stakeAcct.Key, err)
			return
		}

		if stakeState.Stake.Stake.Delegation.StakeLamports < minimumStakeDelegation {
			return
		}

		voterPk := stakeState.Stake.Stake.Delegation.VoterPubkey
		voteAcct, err := acctsDb.GetAccount(slot, voterPk)
		if err != nil {
			//mlog.Log.Debugf("failed to get vote acct %s from accountsdb in calculating rewards points: %s", voterPk, err)
			return
		}

		if voteAcct.Owner != sealevel.VoteProgramAddr {
			//mlog.Log.Debugf("vote acct %s has the wrong owner (%s)", voteAcct.Key, voteAcct.Owner)
			return
		}

		voteStateVersioned, err := sealevel.UnmarshalVersionedVoteState(voteAcct.Data)
		if err != nil {
			//mlog.Log.Debugf("invalid vote acct state (%s) - should be impossible: %s", voteAcct.Key, err)
			return
		}

		calculatedStakeRewards := CalculateStakeRewardsForAcct(stakePk, stakeHistory, stakeState, voteStateVersioned, rewardedEpoch, pointValue, newRateActivationEpoch)
		if calculatedStakeRewards != nil {
			mu.Lock()
			stakeInfoResults[stakePk] = calculatedStakeRewards
			mu.Unlock()
		}
	})

	for stakePk, valid := range slotCtx.StakeAccts {
		if !valid {
			continue
		}
		wg.Add(1)
		workerPool.Invoke(stakePk)
	}
	wg.Wait()
	workerPool.Release()
	ants.Release()

	return stakeInfoResults
}

func CalculateStakeRewardsDuringRewardsWindow(acctsDb *accountsdb.AccountsDb, stakeAccts map[solana.PublicKey]bool, stakeHistory *sealevel.SysvarStakeHistory, slot uint64, rewardedEpoch uint64, pointValue PointValue, newRateActivationEpoch *uint64, f *features.Features) map[solana.PublicKey]*CalculatedStakeRewards {
	stakeInfoResults := make(map[solana.PublicKey]*CalculatedStakeRewards)
	minimumStakeDelegation := minimumStakeDelegationFeatures(f)

	for stakePk := range stakeAccts {
		stakeAcct, err := acctsDb.GetAccount(slot, stakePk)
		if err != nil {
			//mlog.Log.Debugf("failed to get stake acct %s from accountsdb in calculating rewards points: %s", stakePk, err)
			continue
		}

		if stakeAcct.Lamports == 0 {
			continue
		}

		stakeState, err := sealevel.UnmarshalStakeState(stakeAcct.Data)
		if err != nil {
			//mlog.Log.Debugf("invalid stake acct state (%s) - should be impossible: %s", stakeAcct.Key, err)
			continue
		}

		if stakeState.Stake.Stake.Delegation.StakeLamports < minimumStakeDelegation {
			continue
		}

		voterPk := stakeState.Stake.Stake.Delegation.VoterPubkey
		voteAcct, err := acctsDb.GetAccount(slot, voterPk)
		if err != nil {
			//mlog.Log.Debugf("failed to get vote acct %s from accountsdb in calculating rewards points: %s", voterPk, err)
			continue
		}

		if voteAcct.Owner != sealevel.VoteProgramAddr {
			//mlog.Log.Debugf("vote acct %s has the wrong owner (%s)", voteAcct.Key, voteAcct.Owner)
			continue
		}

		voteStateVersioned, err := sealevel.UnmarshalVersionedVoteState(voteAcct.Data)
		if err != nil {
			//mlog.Log.Debugf("invalid vote acct state (%s) - should be impossible: %s", voteAcct.Key, err)
			continue
		}

		calculatedStakeRewards := CalculateStakeRewardsForAcct(stakePk, stakeHistory, stakeState, voteStateVersioned, rewardedEpoch, pointValue, newRateActivationEpoch)
		if calculatedStakeRewards != nil {
			stakeInfoResults[stakePk] = calculatedStakeRewards
		}
	}

	return stakeInfoResults
}

func CalculateStakeRewardsForAcct(stakePubkey solana.PublicKey, stakeHistory *sealevel.SysvarStakeHistory, stakeState *sealevel.StakeStateV2, voteState *sealevel.VoteStateVersions, rewardedEpoch uint64, pointValue PointValue, newRateActivationEpoch *uint64) *CalculatedStakeRewards {
	stakePointsResult := calculateStakePointsAndCredits(stakeHistory, stakeState, voteState, newRateActivationEpoch)

	if pointValue.Rewards == 0 || stakeState.Stake.Stake.Delegation.ActivationEpoch == rewardedEpoch {
		stakePointsResult.ForceCreditsUpdateWithSkippedReward = true
	}

	if stakePointsResult.ForceCreditsUpdateWithSkippedReward {
		result := &CalculatedStakeRewards{NewCreditsObserved: stakePointsResult.NewCreditsObserved}
		return result
	}

	zero128 := wide.Uint128FromUint64(0)
	if stakePointsResult.Points.Eq(zero128) || pointValue.Points.Eq(zero128) {
		//mlog.Log.Debugf("CalculateStakeRewardsForAcct: returning nil for %s. stakePointsResult.Points = %d, pointValue.Points = %d", stakePubkey, stakePointsResult.Points.Uint64(), pointValue.Points.Uint64())
		return nil
	}

	rewards128 := stakePointsResult.Points.Mul(wide.Uint128FromUint64(pointValue.Rewards)).Div(pointValue.Points)
	if !rewards128.IsUint64() {
		//mlog.Log.Debugf("CalculateStakeRewardsForAcct: returning nil for %s. rewards128 not a uint64. %s", stakePubkey, rewards128)
		return nil
	}

	rewards := rewards128.Uint64()
	if rewards == 0 {
		//mlog.Log.Debugf("CalculateStakeRewardsForAcct: returning nil for %s. rewards == 0", stakePubkey)
		return nil
	}

	splitResult := voteCommissionSplit(voteState, rewards)
	if splitResult.IsSplit && (splitResult.VoterPortion == 0 || splitResult.StakerPortion == 0) {
		//mlog.Log.Debugf("CalculateStakeRewardsForAcct: returning nil for %s. IsSplit = %t, splitResult.VoterPortion = %d, splitResult.StakerPortion = %d", stakePubkey, splitResult.VoterPortion, splitResult.StakerPortion)
		return nil
	}

	result := &CalculatedStakeRewards{StakerRewards: splitResult.StakerPortion,
		VoterRewards: splitResult.VoterPortion, NewCreditsObserved: stakePointsResult.NewCreditsObserved}

	//mlog.Log.Debugf("returning CalculatedStakeRewards for %s. %+v", stakePubkey, result)

	return result
}

type CommissionSplit struct {
	VoterPortion  uint64
	StakerPortion uint64
	IsSplit       bool
}

func voteCommissionSplit(voteState *sealevel.VoteStateVersions, rewards uint64) CommissionSplit {
	var commission byte

	switch voteState.Type {
	case sealevel.VoteStateVersionCurrent:
		{
			commission = voteState.Current.Commission
		}

	case sealevel.VoteStateVersionV0_23_5:
		{
			commission = voteState.V0_23_5.Commission
		}

	case sealevel.VoteStateVersionV1_14_11:
		{
			commission = voteState.V1_14_11.Commission
		}
	}

	commissionSplit := uint64(min(commission, 100))

	result := CommissionSplit{}
	result.IsSplit = commissionSplit != 0 && commissionSplit != 100

	if commissionSplit == 0 {
		result.VoterPortion = 0
		result.StakerPortion = rewards
		return result
	}

	if commissionSplit == 100 {
		result.VoterPortion = rewards
		result.StakerPortion = 0
		return result
	}

	result.VoterPortion = wide.Uint128FromUint64(rewards).Mul(wide.Uint128FromUint64(commissionSplit)).Div(wide.Uint128FromUint64(100)).Uint64()
	result.StakerPortion = wide.Uint128FromUint64(rewards).Mul(wide.Uint128FromUint64(100 - commissionSplit)).Div(wide.Uint128FromUint64(100)).Uint64()
	return result
}

func CalculateRewardPointsCreditsAndPartitions(acctsDb *accountsdb.AccountsDb, slotCtx *sealevel.SlotCtx, slot uint64, numPartitions uint64, stakeHistory *sealevel.SysvarStakeHistory, newWarmupCooldownRateEpoch *uint64) (wide.Uint128, map[solana.PublicKey]CalculatedStakePoints, map[uint64][]solana.PublicKey) {
	minimumStakeDelegation := minimumStakeDelegation(slotCtx)
	var totalPoints wide.Uint128
	credits := make(map[solana.PublicKey]CalculatedStakePoints)
	partitions := make(map[uint64][]solana.PublicKey)

	for stakePk, valid := range slotCtx.StakeAccts {
		if !valid {
			continue
		}

		stakeAcct, err := acctsDb.GetAccount(slot, stakePk)
		if err != nil {
			//mlog.Log.Debugf("failed to get stake acct %s from accountsdb in calculating rewards points: %s", stakePk, err)
			continue
		}

		if stakeAcct.Lamports == 0 {
			continue
		}

		stakeState, err := sealevel.UnmarshalStakeState(stakeAcct.Data)
		if err != nil {
			//mlog.Log.Debugf("invalid stake acct state (%s) - should be impossible: %s", stakeAcct.Key, err)
			continue
		}

		if stakeState.Stake.Stake.Delegation.StakeLamports < minimumStakeDelegation {
			continue
		}

		voterPk := stakeState.Stake.Stake.Delegation.VoterPubkey
		voteAcct, err := acctsDb.GetAccount(slot, voterPk)
		if err != nil {
			//mlog.Log.Debugf("failed to get vote acct %s from accountsdb in calculating rewards points: %s", voterPk, err)
			continue
		}

		if voteAcct.Owner != sealevel.VoteProgramAddr {
			//mlog.Log.Debugf("vote acct %s has the wrong owner (%s)", voteAcct.Key, voteAcct.Owner)
			continue
		}

		voteStateVersioned, err := sealevel.UnmarshalVersionedVoteState(voteAcct.Data)
		if err != nil {
			//mlog.Log.Debugf("invalid vote acct state (%s) - should be impossible: %s", voteAcct.Key, err)
			continue
		}

		pointsAndCredits := calculateStakePointsAndCredits(stakeHistory, stakeState, voteStateVersioned, newWarmupCooldownRateEpoch)
		totalPoints = totalPoints.Add(pointsAndCredits.Points)
		credits[stakePk] = pointsAndCredits

		if numPartitions != 0 {
			partitionIdx := CalculateRewardPartitionForPubkey(stakePk, slotCtx.Blockhash, numPartitions)
			_, exists := partitions[partitionIdx]
			if !exists {
				partitions[partitionIdx] = make([]solana.PublicKey, 0)
			}
			partitions[partitionIdx] = append(partitions[partitionIdx], stakePk)
			//mlog.Log.Debugf("partitionIdx for stake account %s: %d", stakePk, partitionIdx)
		}
	}

	return totalPoints, credits, partitions
}

func CalculateTotalPointsAndPartitions(acctsDb *accountsdb.AccountsDb, slotCtx *sealevel.SlotCtx, slot uint64, numPartitions uint64, stakeHistory *sealevel.SysvarStakeHistory, newWarmupCooldownRateEpoch *uint64) (wide.Uint128, map[uint64][]solana.PublicKey) {
	minimumStakeDelegation := minimumStakeDelegation(slotCtx)
	var totalPoints wide.Uint128
	partitions := make(map[uint64][]solana.PublicKey, 500)

	var wg sync.WaitGroup
	var partitionsMutex sync.Mutex
	var totalPointsMutex sync.Mutex

	workerPool, _ := ants.NewPoolWithFunc(10000, func(i interface{}) {
		defer wg.Done()
		stakePk := i.(solana.PublicKey)

		stakeAcct, err := acctsDb.GetAccount(slot, stakePk)
		if err != nil {
			//mlog.Log.Debugf("failed to get stake acct %s from accountsdb in calculating rewards points: %s", stakePk, err)
			return
		}

		if stakeAcct.Lamports == 0 {
			return
		}

		stakeState, err := sealevel.UnmarshalStakeState(stakeAcct.Data)
		if err != nil {
			//mlog.Log.Debugf("invalid stake acct state (%s) - should be impossible: %s", stakeAcct.Key, err)
			return
		}

		if stakeState.Stake.Stake.Delegation.StakeLamports < minimumStakeDelegation {
			return
		}

		voterPk := stakeState.Stake.Stake.Delegation.VoterPubkey
		voteAcct, err := acctsDb.GetAccount(slot, voterPk)
		if err != nil {
			//mlog.Log.Debugf("failed to get vote acct %s from accountsdb in calculating rewards points: %s", voterPk, err)
			return
		}

		if voteAcct.Owner != sealevel.VoteProgramAddr {
			//mlog.Log.Debugf("vote acct %s has the wrong owner (%s)", voteAcct.Key, voteAcct.Owner)
			return
		}

		voteStateVersioned, err := sealevel.UnmarshalVersionedVoteState(voteAcct.Data)
		if err != nil {
			//mlog.Log.Debugf("invalid vote acct state (%s) - should be impossible: %s", voteAcct.Key, err)
			return
		}

		pointsAndCredits := calculateStakePointsAndCredits(stakeHistory, stakeState, voteStateVersioned, newWarmupCooldownRateEpoch)

		totalPointsMutex.Lock()
		totalPoints = totalPoints.Add(pointsAndCredits.Points)
		totalPointsMutex.Unlock()

		if numPartitions != 0 {
			partitionIdx := CalculateRewardPartitionForPubkey(stakePk, slotCtx.Blockhash, numPartitions)
			partitionsMutex.Lock()
			_, exists := partitions[partitionIdx]
			if !exists {
				partitions[partitionIdx] = make([]solana.PublicKey, 0)
			}
			partitions[partitionIdx] = append(partitions[partitionIdx], stakePk)
			partitionsMutex.Unlock()
			//mlog.Log.Debugf("partitionIdx for stake account %s: %d", stakePk, partitionIdx)
		}
	})

	for stakePk, valid := range slotCtx.StakeAccts {
		if !valid {
			continue
		}
		wg.Add(1)
		workerPool.Invoke(stakePk)
	}

	wg.Wait()
	workerPool.Release()
	ants.Release()

	return totalPoints, partitions
}

func CalculateTotalPointsAndPartitionsDuringRewardsWindow(acctsDb *accountsdb.AccountsDb, blockhash [32]byte, stakeAccts map[solana.PublicKey]bool, slot uint64, numPartitions uint64, stakeHistory *sealevel.SysvarStakeHistory, newWarmupCooldownRateEpoch *uint64, f *features.Features) (wide.Uint128, map[uint64][]solana.PublicKey) {
	minimumStakeDelegation := minimumStakeDelegationFeatures(f)
	var totalPoints wide.Uint128
	partitions := make(map[uint64][]solana.PublicKey)

	for stakePk, valid := range stakeAccts {
		if !valid {
			continue
		}

		stakeAcct, err := acctsDb.GetAccount(slot, stakePk)
		if err != nil {
			//mlog.Log.Debugf("failed to get stake acct %s from accountsdb in calculating rewards points: %s", stakePk, err)
			continue
		}

		if stakeAcct.Lamports == 0 {
			continue
		}

		stakeState, err := sealevel.UnmarshalStakeState(stakeAcct.Data)
		if err != nil {
			//mlog.Log.Debugf("invalid stake acct state (%s) - should be impossible: %s", stakeAcct.Key, err)
			continue
		}

		if stakeState.Stake.Stake.Delegation.StakeLamports < minimumStakeDelegation {
			continue
		}

		voterPk := stakeState.Stake.Stake.Delegation.VoterPubkey
		voteAcct, err := acctsDb.GetAccount(slot, voterPk)
		if err != nil {
			//mlog.Log.Debugf("failed to get vote acct %s from accountsdb in calculating rewards points: %s", voterPk, err)
			continue
		}

		if voteAcct.Owner != sealevel.VoteProgramAddr {
			//mlog.Log.Debugf("vote acct %s has the wrong owner (%s)", voteAcct.Key, voteAcct.Owner)
			continue
		}

		voteStateVersioned, err := sealevel.UnmarshalVersionedVoteState(voteAcct.Data)
		if err != nil {
			//mlog.Log.Debugf("invalid vote acct state (%s) - should be impossible: %s", voteAcct.Key, err)
			continue
		}

		pointsAndCredits := calculateStakePointsAndCredits(stakeHistory, stakeState, voteStateVersioned, newWarmupCooldownRateEpoch)
		totalPoints = totalPoints.Add(pointsAndCredits.Points)

		if numPartitions != 0 {
			partitionIdx := CalculateRewardPartitionForPubkey(stakePk, blockhash, numPartitions)
			_, exists := partitions[partitionIdx]
			if !exists {
				partitions[partitionIdx] = make([]solana.PublicKey, 0)
			}
			partitions[partitionIdx] = append(partitions[partitionIdx], stakePk)
			//mlog.Log.Debugf("partitionIdx for stake account %s: %d", stakePk, partitionIdx)
		}
	}

	return totalPoints, partitions
}

func calculateStakePointsAndCredits(stakeHistory *sealevel.SysvarStakeHistory, stakeState *sealevel.StakeStateV2, voteState *sealevel.VoteStateVersions, newRateActivationEpoch *uint64) CalculatedStakePoints {
	creditsInStake := stakeState.Stake.Stake.CreditsObserved

	var epochCredits []sealevel.EpochCredits

	switch voteState.Type {
	case sealevel.VoteStateVersionCurrent:
		{
			epochCredits = voteState.Current.EpochCredits
		}

	case sealevel.VoteStateVersionV0_23_5:
		{
			epochCredits = voteState.V0_23_5.EpochCredits
		}

	case sealevel.VoteStateVersionV1_14_11:
		{
			epochCredits = voteState.V1_14_11.EpochCredits
		}

	default:
		{
			panic("invalid vote state - should be impossible")
		}
	}

	var creditsInVote uint64
	if len(epochCredits) != 0 {
		creditsInVote = epochCredits[len(epochCredits)-1].Credits
	}

	if creditsInVote < creditsInStake {
		return CalculatedStakePoints{NewCreditsObserved: creditsInVote, ForceCreditsUpdateWithSkippedReward: true}
	}

	if creditsInVote == creditsInStake {
		return CalculatedStakePoints{NewCreditsObserved: creditsInVote, ForceCreditsUpdateWithSkippedReward: false}
	}

	var points wide.Uint128
	newCreditsObserved := creditsInStake

	for _, ec := range epochCredits {
		finalEpochCredits := ec.Credits
		initialEpochCredits := ec.PrevCredits
		var earnedCredits wide.Uint128

		if creditsInStake < initialEpochCredits {
			earnedCredits = wide.Uint128FromUint64(finalEpochCredits).Sub(wide.Uint128FromUint64(initialEpochCredits))
		} else if creditsInStake < finalEpochCredits {
			earnedCredits = wide.Uint128FromUint64(finalEpochCredits).Sub(wide.Uint128FromUint64(newCreditsObserved))
		}

		newCreditsObserved = max(newCreditsObserved, finalEpochCredits)
		stakeAmount := stakeState.Stake.Stake.Delegation.StakeActivatingAndDeactivating(ec.Epoch, *stakeHistory, newRateActivationEpoch).Effective
		points = points.Add(wide.Uint128FromUint64(stakeAmount).Mul(earnedCredits))
	}

	return CalculatedStakePoints{Points: points, NewCreditsObserved: newCreditsObserved}
}
