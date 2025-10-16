package replay

import (
	"bytes"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
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

func updateStakeHistorySysvar(acctsDb *accountsdb.AccountsDb, block *block.Block, prevSlotCtx *sealevel.SlotCtx, targetEpoch uint64) *sealevel.SysvarStakeHistory {
	stakeHistoryAcct, err := prevSlotCtx.GetAccount(sealevel.SysvarStakeHistoryAddr)
	if err != nil {
		stakeHistoryAcct, err = acctsDb.GetAccount(prevSlotCtx.Slot, sealevel.SysvarStakeHistoryAddr)
		if err != nil {
			panic(fmt.Sprintf("unable to retrieve stakehistory sysvar: %s", err))
		}
	}
	block.ParentEpochUpdatedAccts = append(block.ParentEpochUpdatedAccts, stakeHistoryAcct.Clone())

	decoder := bin.NewBinDecoder(stakeHistoryAcct.Data)
	var stakeHistory sealevel.SysvarStakeHistory
	stakeHistory.MustUnmarshalWithDecoder(decoder)

	newRateActivationEpoch := newWarmupCooldownRateEpoch(nil, nil)

	var wg sync.WaitGroup
	var effective atomic.Uint64
	var activating atomic.Uint64
	var deactivating atomic.Uint64

	workerPool, _ := ants.NewPoolWithFunc(runtime.GOMAXPROCS(0)*8, func(i interface{}) {
		defer wg.Done()

		delegation := i.(*sealevel.Delegation)
		if delegation.StakeLamports == 0 {
			return
		}

		stakeHistoryEntry := delegation.StakeActivatingAndDeactivating(targetEpoch, &stakeHistory, newRateActivationEpoch)

		effective.Add(stakeHistoryEntry.Effective)
		activating.Add(stakeHistoryEntry.Activating)
		deactivating.Add(stakeHistoryEntry.Deactivating)
	})

	for _, delegation := range global.StakeCache() {
		wg.Add(1)
		workerPool.Invoke(delegation)
	}

	wg.Wait()
	workerPool.Release()
	ants.Release()

	var accumulatorStakeHistoryEntry sealevel.StakeHistoryEntry
	accumulatorStakeHistoryEntry.Activating = activating.Load()
	accumulatorStakeHistoryEntry.Effective = effective.Load()
	accumulatorStakeHistoryEntry.Deactivating = deactivating.Load()
	stakeHistory.Update(targetEpoch, accumulatorStakeHistoryEntry)

	buf := new(bytes.Buffer)
	encoder := bin.NewBinEncoder(buf)
	stakeHistory.MustMarshalWithEncoder(encoder)
	newStakeHistoryBytes := buf.Bytes()
	copy(stakeHistoryAcct.Data, newStakeHistoryBytes)

	err = acctsDb.StoreAccounts([]*accounts.Account{stakeHistoryAcct}, prevSlotCtx.Slot)
	if err != nil {
		panic(fmt.Sprintf("error storing new StakeHistory sysvar to accountsdb: %s", err))
	}
	block.EpochUpdatedAccts = append(block.EpochUpdatedAccts, stakeHistoryAcct.Clone())

	return &stakeHistory
}

func refreshVoteAcctsCache(prevSlotCtx *sealevel.SlotCtx, acctsDb *accountsdb.AccountsDb, stakeHistory *sealevel.SysvarStakeHistory, newEpoch uint64, newRateActivationEpoch *uint64) map[solana.PublicKey]uint64 {
	voteAcctStakes := make(map[solana.PublicKey]uint64)

	var mu sync.Mutex
	var wg sync.WaitGroup

	workerPool, _ := ants.NewPoolWithFunc(runtime.GOMAXPROCS(0)*8, func(i interface{}) {
		defer wg.Done()

		delegation := i.(*sealevel.Delegation)
		votePk := delegation.VoterPubkey
		stakeLamports := delegation.Stake(newEpoch, stakeHistory, newRateActivationEpoch)

		mu.Lock()
		voteAcctStakes[votePk] += stakeLamports
		mu.Unlock()
	})

	for _, delegation := range global.StakeCache() {
		wg.Add(1)
		workerPool.Invoke(delegation)
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

func handleEpochTransition(acctsDb *accountsdb.AccountsDb, rpcc *rpcclient.RpcClient, partitionedEpochRewards bool, prevSlotCtx *sealevel.SlotCtx, replayCtx *ReplayCtx, epochSchedule *sealevel.SysvarEpochSchedule, f *features.Features, block *block.Block, epoch uint64) *rewards.PartitionedRewardDistributionInfo {
	var stakeHistory sealevel.SysvarStakeHistory
	stakeHistoryAcct, err := prevSlotCtx.GetAccount(sealevel.SysvarStakeHistoryAddr)
	if err != nil {
		stakeHistoryAcct, err = acctsDb.GetAccount(prevSlotCtx.Slot, sealevel.SysvarStakeHistoryAddr)
		if err != nil {
			panic("unable to get stake history sysvar")
		}
	}
	decoder := bin.NewBinDecoder(stakeHistoryAcct.Data)
	stakeHistory.MustUnmarshalWithDecoder(decoder)

	var partitionedRewardsInfo *rewards.PartitionedRewardDistributionInfo
	newEpoch := epoch + 1
	firstSlotInEpoch := epochSchedule.FirstSlotInEpoch(newEpoch)
	newWarmupCooldownRateEpoch := newWarmupCooldownRateEpoch(epochSchedule, f)

	block.VoteAccts = refreshVoteAcctsCache(prevSlotCtx, acctsDb, &stakeHistory, newEpoch, newWarmupCooldownRateEpoch)
	block.TotalEpochStake = 0
	for _, stake := range block.VoteAccts {
		block.TotalEpochStake += stake
	}

	if partitionedEpochRewards {
		partitionedRewardsInfo, block.EpochUpdatedAccts, block.ParentEpochUpdatedAccts = beginPartitionedEpochRewardsDistribution(acctsDb, prevSlotCtx, &stakeHistory, replayCtx, epochSchedule, rpcc, block, f, newEpoch, firstSlotInEpoch)
	} else {
		panic("only partitioned rewards supported")
	}

	updateStakeHistorySysvar(acctsDb, block, prevSlotCtx, epoch)
	mlog.Log.Infof("epoch transition %d -> %d done.", epoch, newEpoch)

	return partitionedRewardsInfo
}
