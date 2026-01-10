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
	"github.com/Overclock-Validator/mithril/pkg/epochstakes"
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

func updateStakeHistorySysvar(acctsDb *accountsdb.AccountsDb, block *block.Block, prevSlotCtx *sealevel.SlotCtx, targetEpoch uint64, epochSchedule *sealevel.SysvarEpochSchedule, f *features.Features) *sealevel.SysvarStakeHistory {
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

	newRateActivationEpoch := newWarmupCooldownRateEpoch(epochSchedule, f)

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

	// Return full stake map - don't filter by previous epoch's vote accounts.
	// New vote accounts can appear during an epoch (via CreateAccount + Initialize),
	// and their stake should be included in the next epoch's leader schedule.
	// The old filtering logic would drop any vote accounts that didn't exist
	// in prevSlotCtx.VoteAccts, causing missing stake in the schedule.
	return voteAcctStakes
}

// EpochTransitionContext holds the intermediate state needed between stake computation
// and rewards distribution. This allows leader schedule to be built in between,
// so schedule verification works even if rewards distribution crashes.
type EpochTransitionContext struct {
	StakeHistory               *sealevel.SysvarStakeHistory
	NewEpoch                   uint64
	FirstSlotInEpoch           uint64
	NewWarmupCooldownRateEpoch *uint64
}

// prepareEpochStakes computes the stake distribution for the new epoch.
// This must be called BEFORE leader schedule computation and rewards distribution.
// Returns the context needed by handleEpochRewards.
func prepareEpochStakes(acctsDb *accountsdb.AccountsDb, prevSlotCtx *sealevel.SlotCtx, epochSchedule *sealevel.SysvarEpochSchedule, f *features.Features, block *block.Block, epoch uint64) *EpochTransitionContext {
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

	newWarmupCooldownRateEpoch := newWarmupCooldownRateEpoch(epochSchedule, f)
	newEpoch := epoch + 1
	firstSlotInEpoch := epochSchedule.FirstSlotInEpoch(newEpoch)

	block.EpochStakesPerVoteAcct = refreshVoteAcctsCache(prevSlotCtx, acctsDb, &stakeHistory, newEpoch, newWarmupCooldownRateEpoch)
	block.TotalEpochStake = 0
	for _, stake := range block.EpochStakesPerVoteAcct {
		block.TotalEpochStake += stake
	}

	return &EpochTransitionContext{
		StakeHistory:               &stakeHistory,
		NewEpoch:                   newEpoch,
		FirstSlotInEpoch:           firstSlotInEpoch,
		NewWarmupCooldownRateEpoch: newWarmupCooldownRateEpoch,
	}
}

// handleEpochRewards distributes rewards and updates stake history.
// This should be called AFTER leader schedule computation to ensure schedule
// verification works even if rewards distribution crashes.
func handleEpochRewards(acctsDb *accountsdb.AccountsDb, rpcc *rpcclient.RpcClient, rpcBackups []string, partitionedEpochRewards bool, prevSlotCtx *sealevel.SlotCtx, replayCtx *ReplayCtx, epochSchedule *sealevel.SysvarEpochSchedule, f *features.Features, block *block.Block, epoch uint64, ctx *EpochTransitionContext) *rewards.PartitionedRewardDistributionInfo {
	var partitionedRewardsInfo *rewards.PartitionedRewardDistributionInfo

	if partitionedEpochRewards {
		partitionedRewardsInfo, block.EpochUpdatedAccts, block.ParentEpochUpdatedAccts = beginPartitionedEpochRewardsDistribution(acctsDb, prevSlotCtx, ctx.StakeHistory, replayCtx, epochSchedule, rpcc, rpcBackups, block, f, ctx.NewEpoch, ctx.FirstSlotInEpoch)
	} else {
		panic("only partitioned rewards supported")
	}

	updateStakeHistorySysvar(acctsDb, block, prevSlotCtx, epoch, epochSchedule, f)

	mlog.Log.Infof("epoch transition %d -> %d done.", epoch, ctx.NewEpoch)

	return partitionedRewardsInfo
}

// handleEpochTransition is the legacy function that bundles stake computation and rewards.
// Prefer using prepareEpochStakes + handleEpochRewards separately to allow leader schedule
// computation between them.
func handleEpochTransition(acctsDb *accountsdb.AccountsDb, rpcc *rpcclient.RpcClient, rpcBackups []string, partitionedEpochRewards bool, prevSlotCtx *sealevel.SlotCtx, replayCtx *ReplayCtx, epochSchedule *sealevel.SysvarEpochSchedule, f *features.Features, block *block.Block, epoch uint64) *rewards.PartitionedRewardDistributionInfo {
	ctx := prepareEpochStakes(acctsDb, prevSlotCtx, epochSchedule, f, block, epoch)
	return handleEpochRewards(acctsDb, rpcc, rpcBackups, partitionedEpochRewards, prevSlotCtx, replayCtx, epochSchedule, f, block, epoch, ctx)
}

// cacheEpochStakesForValidation populates global.EpochStakes with stake data
// computed during epoch transition. This enables leader schedule validation
// for epochs beyond the initial snapshot.
func cacheEpochStakesForValidation(epoch uint64, voteAcctStakes map[solana.PublicKey]uint64, totalStake uint64) {
	voteCache := global.VoteCache()
	cachedCount := 0
	skippedZeroStake := 0
	skippedMissingVoteCache := 0
	skippedZeroNodePk := 0
	missingVoteCacheStake := uint64(0) // Track stake of validators we couldn't cache
	missingNodePkStake := uint64(0)    // Track stake of validators with zero nodePk

	for votePk, stake := range voteAcctStakes {
		if stake == 0 {
			skippedZeroStake++
			continue
		}

		// Get NodePubkey from vote cache
		vs := voteCache[votePk]
		if vs == nil {
			skippedMissingVoteCache++
			missingVoteCacheStake += stake
			continue
		}

		nodePk := vs.NodePubkey()
		var zeroPk solana.PublicKey
		if nodePk == zeroPk {
			skippedZeroNodePk++
			missingNodePkStake += stake
			continue
		}

		// Create VoteAccount entry with NodePubkey for leader schedule computation
		voteAcct := &epochstakes.VoteAccount{
			NodePubkey: nodePk,
		}
		global.PutEpochStakesEntry(epoch, votePk, stake, voteAcct)
		cachedCount++
	}

	global.PutEpochTotalStake(epoch, totalStake)

	// Calculate total missing stake (both missing vote cache AND zero nodePk)
	totalMissingStake := missingVoteCacheStake + missingNodePkStake
	hasIssues := skippedMissingVoteCache > 0 || skippedZeroNodePk > 0

	// Log at Debug level if no issues, Info if there are skips
	if hasIssues {
		mlog.Log.Infof("cached epoch stakes: epoch=%d cached=%d/%d total_stake=%d",
			epoch, cachedCount, len(voteAcctStakes), totalStake)
		mlog.Log.Infof("  skipped: zero_stake=%d missing_vote_cache=%d(%d) zero_nodepk=%d(%d)",
			skippedZeroStake, skippedMissingVoteCache, missingVoteCacheStake, skippedZeroNodePk, missingNodePkStake)
	} else {
		mlog.Log.Debugf("cached epoch stakes: epoch=%d cached=%d total_stake=%d",
			epoch, cachedCount, totalStake)
	}

	// Warn if significant stake is missing (>1% of total)
	if totalMissingStake > 0 && totalStake > 0 {
		missingPct := float64(totalMissingStake) / float64(totalStake) * 100
		if missingPct > 1.0 {
			mlog.Log.Warnf("leader schedule: %.2f%% of stake (%d) missing for epoch %d (vote_cache=%d nodepk=%d)",
				missingPct, totalMissingStake, epoch, missingVoteCacheStake, missingNodePkStake)
		}
	}

	// Fatal warning if no validators could be cached
	if cachedCount == 0 && len(voteAcctStakes) > 0 {
		mlog.Log.Errorf("leader schedule: FATAL - no validators cached for epoch %d (vote_accts=%d)",
			epoch, len(voteAcctStakes))
	}
}
