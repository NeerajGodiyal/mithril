package rewards

import (
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/safemath"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go/rpc"
)

const (
	RewardTypeFee     string = "Fee"
	RewardTypeRent    string = "Rent"
	RewardTypeVoting  string = "Voting"
	RewardTypeStaking string = "Staking"
)

func HandleVotingRewardDistribution(acctsDb *accountsdb.AccountsDb, rewards []rpc.BlockReward, slot uint64) {
	accts := make([]*accounts.Account, 0)

	for _, reward := range rewards {
		if string(reward.RewardType) == "Voting" {
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
		}
	}

	if len(accts) != 0 {
		err := acctsDb.StoreAccounts(accts, slot)
		panic(fmt.Sprintf("error updating accounts for voting rewards in slot %d: %s", slot, err))
	}
}

func DistributePartitionedEpochRewards(acctsDb *accountsdb.AccountsDb, rewards []rpc.BlockReward, slot uint64) uint64 {
	var distributedLamports uint64
	accts := make([]*accounts.Account, 0)

	for _, reward := range rewards {
		if string(reward.RewardType) == "Staking" {
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

			distributedLamports += uint64(reward.Lamports)
		}
	}

	if len(accts) != 0 {
		err := acctsDb.StoreAccounts(accts, slot)
		panic(fmt.Sprintf("error updating accounts for partitioned epoch rewards in slot %d: %s", slot, err))
	}

	return distributedLamports
}
