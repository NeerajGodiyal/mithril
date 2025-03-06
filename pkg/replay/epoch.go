package replay

import (
	"bytes"
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/rewards"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go/rpc"
)

func updateStakeHistorySysvar(acctsDb *accountsdb.AccountsDb, prevSlotCtx *sealevel.SlotCtx, epochSchedule *sealevel.SysvarEpochSchedule, targetEpoch uint64) *sealevel.SysvarStakeHistory {
	stakeHistoryAcct, err := prevSlotCtx.GetAccount(sealevel.SysvarStakeHistoryAddr)
	if err != nil {
		stakeHistoryAcct, err = acctsDb.GetAccount(prevSlotCtx.Slot, sealevel.SysvarStakeHistoryAddr)
		if err != nil {
			panic(fmt.Sprintf("unable to retrieve stakehistory sysvar: %s", err))
		}
	}

	decoder := bin.NewBinDecoder(stakeHistoryAcct.Data)
	var stakeHistory sealevel.SysvarStakeHistory
	stakeHistory.MustUnmarshalWithDecoder(decoder)

	newRateActivationEpoch := sealevel.NewWarmupCooldownRateEpochWithSlotCtx(prevSlotCtx, epochSchedule)
	var accumulatorStakeHistoryEntry sealevel.StakeHistoryEntry

	for stakePubkey := range prevSlotCtx.StakeAccts {
		stakeAcct, err := prevSlotCtx.GetAccount(stakePubkey)
		if err != nil {
			stakeAcct, err = acctsDb.GetAccount(prevSlotCtx.Slot, stakePubkey)
			if err != nil {
				panic(fmt.Sprintf("unable to retrieve staking account: %s", err))
			}
		}

		stakeState, err := sealevel.UnmarshalStakeState(stakeAcct.Data)
		if err != nil {
			continue
		}

		if stakeState.Status != sealevel.StakeStateV2StatusStake {
			continue
		}

		if stakeState.Stake.Stake.Delegation.StakeLamports == 0 {
			continue
		}

		delegation := stakeState.Stake.Stake.Delegation
		stakeHistoryEntry := delegation.StakeActivatingAndDeactivating(targetEpoch, stakeHistory, newRateActivationEpoch)

		accumulatorStakeHistoryEntry.Effective += stakeHistoryEntry.Effective
		accumulatorStakeHistoryEntry.Activating += stakeHistoryEntry.Activating
		accumulatorStakeHistoryEntry.Deactivating += stakeHistoryEntry.Deactivating
	}

	stakeHistory.Update(targetEpoch, accumulatorStakeHistoryEntry)

	buf := new(bytes.Buffer)
	encoder := bin.NewBinEncoder(buf)
	stakeHistory.MarshalWithEncoder(encoder)

	newStakeHistoryBytes := buf.Bytes()
	copy(stakeHistoryAcct.Data, newStakeHistoryBytes)

	err = acctsDb.StoreAccounts([]*accounts.Account{stakeHistoryAcct}, prevSlotCtx.Slot)
	if err != nil {
		panic(fmt.Sprintf("error storing new StakeHistory sysvar to accountsdb: %s", err))
	}

	return &stakeHistory
}

const numSlotsPerEpoch = 432000

func handleEpochTransition(acctsDb *accountsdb.AccountsDb, rpcc *rpcclient.RpcClient, partitionedEpochRewards bool, prevSlotCtx *sealevel.SlotCtx, epochSchedule *sealevel.SysvarEpochSchedule, blockResult *rpc.GetBlockResult, epoch uint64, slot uint64) (uint64, uint64) {
	stakeHistory := updateStakeHistorySysvar(acctsDb, prevSlotCtx, epochSchedule, epoch)

	var lastRewardsDistributionSlot uint64

	if partitionedEpochRewards {
		lastRewardsDistributionSlot = beginPartitionedEpochRewardsDistribution(acctsDb, prevSlotCtx, stakeHistory, epochSchedule, rpcc, blockResult, slot)
	} else {
		rewards.DistributeVotingRewards(acctsDb, blockResult.Rewards, slot)
		rewards.DistributeStakingRewards(acctsDb, blockResult.Rewards, slot)
	}

	// return EAH slot, which is the slot that is 1/4 through the epoch
	eahSlot := slot + (numSlotsPerEpoch / 4)
	return eahSlot, lastRewardsDistributionSlot
}
