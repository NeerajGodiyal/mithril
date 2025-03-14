package rewards

import (
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/Overclock-Validator/mithril/pkg/safemath"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/wide"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

const (
	RewardTypeFee     string = "Fee"
	RewardTypeRent    string = "Rent"
	RewardTypeVoting  string = "Voting"
	RewardTypeStaking string = "Staking"
)

type PartitionedRewardDistributionInfo struct {
	TotalStakingRewards         uint64
	FirstStakingRewardSlot      uint64
	LastStakingRewardSlot       uint64
	EahStartOffsetSlot          uint64
	EahStopOffsetSlot           uint64
	StartedAfterStartOffsetSlot bool
	EpochAcctsHash              []byte
}

func RetrievePartitionedStakingRewardsInfo(rpcc *rpcclient.RpcClient, epochSchedule *sealevel.SysvarEpochSchedule, epoch uint64, slot uint64) *PartitionedRewardDistributionInfo {
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
	if slot == firstSlotInEpoch {
		for _, rewardSlot := range rewardSlots {
			rewards, err := rpcc.GetRewardsForSlot(rewardSlot)
			if err != nil {
				panic(fmt.Sprintf("error retrieving reward data from rpc: %s", err))
			}

			for _, reward := range rewards {
				mlog.Log.Debugf("reward: %+v", reward)
				if string(reward.RewardType) == RewardTypeStaking {
					totalStakingRewards, err = safemath.CheckedAddU64(totalStakingRewards, uint64(reward.Lamports))
				}
			}
		}
	}

	eahCalcSlot := firstSlotInEpoch + (432000 / 4)
	eahInclusionSlot := firstSlotInEpoch + ((432000 / 4) * 3)

	return &PartitionedRewardDistributionInfo{TotalStakingRewards: totalStakingRewards, FirstStakingRewardSlot: firstSlotInEpoch,
		LastStakingRewardSlot: finalStakingRewardSlot, EahStartOffsetSlot: eahCalcSlot, EahStopOffsetSlot: eahInclusionSlot}
}

func DistributeVotingRewards(acctsDb *accountsdb.AccountsDb, rewards []rpc.BlockReward, slot uint64) ([]solana.PublicKey, uint64) {
	accts := make([]*accounts.Account, 0)
	rewardPks := make([]solana.PublicKey, 0)
	var totalVotingRewards uint64

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

			accts = append(accts, stakeAcct)

			totalVotingRewards, err = safemath.CheckedAddU64(totalVotingRewards, uint64(reward.Lamports))
			if err != nil {
				panic(fmt.Sprintf("overflow in accumulating voting rewards in slot %d", slot))
			}

			rewardPks = append(rewardPks, reward.Pubkey)
		}
	}

	if len(accts) != 0 {
		err := acctsDb.StoreAccounts(accts, slot)
		if err != nil {
			panic(fmt.Sprintf("error updating accounts for voting rewards in slot %d: %s", slot, err))
		}
	}

	return rewardPks, totalVotingRewards
}

func DistributeStakingRewards(acctsDb *accountsdb.AccountsDb, rewards []rpc.BlockReward, slot uint64) ([]solana.PublicKey, uint64) {
	var distributedLamports uint64
	accts := make([]*accounts.Account, 0)
	rewardPks := make([]solana.PublicKey, 0)

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

			safemath.SaturatingAddU64(stakeState.Stake.Stake.Delegation.StakeLamports, uint64(reward.Lamports))

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
			mlog.Log.Debugf("distributed partitioned rewards to %s, %d lamports", reward.Pubkey, reward.Lamports)
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

func CalculateRewardPointsPartitioned(acctsDb *accountsdb.AccountsDb, slotCtx *sealevel.SlotCtx, slot uint64, stakeHistory *sealevel.SysvarStakeHistory, newWarmupCooldownRateEpoch *uint64) wide.Uint128 {
	minimumStakeDelegation := minimumStakeDelegation(slotCtx)
	var totalPoints wide.Uint128

	for stakePk, valid := range slotCtx.StakeAccts {
		if !valid {
			continue
		}

		stakeAcct, err := acctsDb.GetAccount(slot, stakePk)
		if err != nil {
			panic(fmt.Sprintf("failed to get stake acct %s from accountsdb in calculating rewards points: %s", stakePk, err))
		}

		stakeState, err := sealevel.UnmarshalStakeState(stakeAcct.Data)
		if err != nil {
			panic(fmt.Sprintf("invalid stake acct state (%s) - should be impossible: %s", stakeAcct.Key, err))
		}

		if stakeState.Stake.Stake.Delegation.StakeLamports < minimumStakeDelegation {
			continue
		}

		voterPk := stakeState.Stake.Stake.Delegation.VoterPubkey
		voteAcct, err := acctsDb.GetAccount(slot, voterPk)
		if err != nil {
			panic(fmt.Sprintf("failed to get vote acct %s from accountsdb in calculating rewards points: %s", voterPk, err))
		}

		if voteAcct.Owner != sealevel.VoteProgramAddr {
			mlog.Log.Debugf("vote acct %s has the wrong owner (%s)", voteAcct.Key, voteAcct.Owner)
			continue
		}

		voteStateVersioned, err := sealevel.UnmarshalVersionedVoteState(voteAcct.Data)
		if err != nil {
			panic(fmt.Sprintf("invalid vote acct state (%s) - should be impossible: %s", voteAcct.Key, err))
		}

		acctPoints := calculatePoints(stakeHistory, stakeState, voteStateVersioned, newWarmupCooldownRateEpoch)
		totalPoints = totalPoints.Add(acctPoints)
	}

	return totalPoints
}

func calculatePoints(stakeHistory *sealevel.SysvarStakeHistory, stakeState *sealevel.StakeStateV2, voteState *sealevel.VoteStateVersions, newRateActivationEpoch *uint64) wide.Uint128 {
	return calculateStakePointsAndCredits(stakeHistory, stakeState, voteState, newRateActivationEpoch).Points
}

func calculateStakePointsAndCredits(stakeHistory *sealevel.SysvarStakeHistory, stakeState *sealevel.StakeStateV2, voteState *sealevel.VoteStateVersions, newRateActivationEpoch *uint64) *CalculatedStakePoints {
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
		return &CalculatedStakePoints{NewCreditsObserved: creditsInVote, ForceCreditsUpdateWithSkippedReward: true}
	}

	if creditsInVote == creditsInStake {
		return &CalculatedStakePoints{NewCreditsObserved: creditsInVote, ForceCreditsUpdateWithSkippedReward: false}
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

	return &CalculatedStakePoints{Points: points, NewCreditsObserved: newCreditsObserved}
}
