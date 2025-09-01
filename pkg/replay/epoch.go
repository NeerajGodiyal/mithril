package replay

import (
	"bytes"
	"fmt"
	"sync"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/rewards"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/snapshot"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"github.com/panjf2000/ants/v2"
)

type ReplayCtx struct {
	CurrentFeatures   *features.Features
	Capitalization    uint64
	Inflation         rewards.Inflation
	SlotsPerYear      float64
	EpochAcctsHash    []byte
	HasEpochAcctsHash bool
}

func newReplayCtx(snapshotManifest *snapshot.SnapshotManifest) *ReplayCtx {
	epochCtx := new(ReplayCtx)
	epochCtx.Capitalization = snapshotManifest.Bank.Capitalization
	epochCtx.Inflation = snapshotManifest.Bank.Inflation
	epochCtx.SlotsPerYear = snapshotManifest.Bank.SlotsPerYear

	if snapshotManifest.EpochAccountHash != [32]byte{} {
		epochCtx.HasEpochAcctsHash = true
		epochCtx.EpochAcctsHash = snapshotManifest.EpochAccountHash[:]
	}

	return epochCtx
}

func updateStakeHistorySysvar(acctsDb *accountsdb.AccountsDb, prevSlotCtx *sealevel.SlotCtx, targetEpoch uint64) *sealevel.SysvarStakeHistory {
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

	newRateActivationEpoch := newWarmupCooldownRateEpoch(nil, nil)
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

func refreshVoteAcctsCache(prevSlotCtx *sealevel.SlotCtx, acctsDb *accountsdb.AccountsDb, stakeHistory *sealevel.SysvarStakeHistory, newEpoch uint64, newRateActivationEpoch *uint64) map[solana.PublicKey]uint64 {
	voteAcctStakes := make(map[solana.PublicKey]uint64)

	var mu sync.Mutex
	var wg sync.WaitGroup

	workerPool, _ := ants.NewPoolWithFunc(10000, func(i interface{}) {
		defer wg.Done()

		stakePk := i.(solana.PublicKey)
		stakeAcct, err := acctsDb.GetAccount(0, stakePk)
		if err != nil {
			panic(fmt.Sprintf("unable to get stake acct %s from accountsdb whilst refreshing vote accts cache", stakePk))
		}

		stakeState, err := sealevel.UnmarshalStakeState(stakeAcct.Data)
		if err != nil {
			return
		}

		votePk := stakeState.Stake.Stake.Delegation.VoterPubkey
		stakeLamports := stakeState.Stake.Stake.Delegation.Stake(newEpoch, *stakeHistory, newRateActivationEpoch)

		mu.Lock()
		voteAcctStakes[votePk] += stakeLamports
		mu.Unlock()
	})

	for stakePk, valid := range prevSlotCtx.StakeAccts {
		if !valid {
			continue
		}
		wg.Add(1)
		workerPool.Invoke(stakePk)
	}

	wg.Wait()
	workerPool.Release()
	ants.Release()

	newVoteAccts := make(map[solana.PublicKey]uint64, len(prevSlotCtx.VoteAccts))
	for voteAcctPk := range prevSlotCtx.VoteAccts {
		newVoteAccts[voteAcctPk] = voteAcctStakes[voteAcctPk]
	}

	return newVoteAccts
}

const numSlotsPerEpoch = 432000

func handleEpochTransition(acctsDb *accountsdb.AccountsDb, rpcc *rpcclient.RpcClient, partitionedEpochRewards bool, prevSlotCtx *sealevel.SlotCtx, replayCtx *ReplayCtx, epochSchedule *sealevel.SysvarEpochSchedule, f *features.Features, block *block.Block, epoch uint64) (*rewards.PartitionedRewardDistributionInfo, []solana.PublicKey, map[solana.PublicKey]uint64) {
	stakeHistory := updateStakeHistorySysvar(acctsDb, prevSlotCtx, epoch)

	var partitionedRewardsInfo *rewards.PartitionedRewardDistributionInfo
	newEpoch := epoch + 1
	firstSlotInEpoch := epochSchedule.FirstSlotInEpoch(newEpoch)
	newWarmupCooldownRateEpoch := newWarmupCooldownRateEpoch(epochSchedule, f)

	updatedVoteCache := refreshVoteAcctsCache(prevSlotCtx, acctsDb, stakeHistory, newEpoch, newWarmupCooldownRateEpoch)

	var updatedAcctsPks []solana.PublicKey
	if partitionedEpochRewards {
		partitionedRewardsInfo, updatedAcctsPks = beginPartitionedEpochRewardsDistribution(acctsDb, prevSlotCtx, stakeHistory, replayCtx, epochSchedule, rpcc, block, f, newEpoch, firstSlotInEpoch)
	} else {
		rewards.DistributeVotingRewards(acctsDb, block.Rewards, firstSlotInEpoch)
		_, credits, _ := rewards.CalculateRewardPointsCreditsAndPartitions(acctsDb, prevSlotCtx, firstSlotInEpoch, 0, stakeHistory, newWarmupCooldownRateEpoch)
		rewards.DistributeStakingRewards(acctsDb, block.Rewards, credits, firstSlotInEpoch)
	}

	updatedAcctsPks = append(updatedAcctsPks, sealevel.SysvarStakeHistoryAddr)

	return partitionedRewardsInfo, updatedAcctsPks, updatedVoteCache
}
