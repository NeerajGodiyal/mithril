package replay

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"maps"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	a "github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/epochstakes"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/rewards"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/state"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
)

type ReplayCtx struct {
	CurrentFeatures     *features.Features
	Capitalization      uint64
	CapitalizationAudit uint64
	Inflation           rewards.Inflation
	SlotsPerYear        float64
	EpochAcctsHash      []byte
	HasEpochAcctsHash   bool
}

func logVoteStateV4FeatureStatus(epoch uint64, boundarySlot uint64, f *features.Features) {
	type featureStatus struct {
		gate      features.FeatureGate
		agaveNote string
	}

	statuses := []featureStatus{
		{gate: features.VoteStateV4},
		{gate: features.DelayCommissionUpdates},
		{gate: features.CommissionRateInBasisPoints},
		{gate: features.BlsPubkeyManagementInVoteAccount},
		{gate: features.CustomCommissionCollector, agaveNote: "Agave runtime currently hard-disables this feature"},
		{gate: features.BlockRevenueSharing, agaveNote: "Agave runtime currently hard-disables this feature"},
	}

	mlog.Log.FileOnlyf("vote-state-v4 feature status: epoch=%d boundary_slot=%d", epoch, boundarySlot)
	for _, status := range statuses {
		active := f.IsActive(status.gate)
		activationSlot, ok := f.ActivationSlot(status.gate)
		if ok {
			if status.agaveNote != "" {
				mlog.Log.FileOnlyf("  %s: active=%t activation_slot=%d note=%s", status.gate.Name, active, activationSlot, status.agaveNote)
			} else {
				mlog.Log.FileOnlyf("  %s: active=%t activation_slot=%d", status.gate.Name, active, activationSlot)
			}
		} else if status.agaveNote != "" {
			mlog.Log.FileOnlyf("  %s: active=%t activation_slot=inactive note=%s", status.gate.Name, active, status.agaveNote)
		} else {
			mlog.Log.FileOnlyf("  %s: active=%t activation_slot=inactive", status.gate.Name, active)
		}
	}

	mlog.Log.FileOnlyf("  reward-path summary: vote_state_v4=%t commission_rate_in_basis_points=%t delay_commission_updates=%t",
		f.IsActive(features.VoteStateV4),
		f.IsActive(features.CommissionRateInBasisPoints),
		f.IsActive(features.DelayCommissionUpdates))
}

// newReplayCtx creates a new ReplayCtx, preferring values from resumeState if available.
// This ensures resume uses fresh values instead of potentially stale manifest data.
func newReplayCtx(mithrilState *state.MithrilState, resumeState *ResumeState) (*ReplayCtx, error) {
	epochCtx := new(ReplayCtx)

	// Priority 1: Resume state (has most recent values)
	if resumeState != nil && resumeState.Capitalization > 0 {
		epochCtx.Capitalization = resumeState.Capitalization
		epochCtx.CapitalizationAudit = resumeState.Capitalization
		epochCtx.SlotsPerYear = resumeState.SlotsPerYear
		epochCtx.Inflation = rewards.Inflation{
			Initial:        resumeState.InflationInitial,
			Terminal:       resumeState.InflationTerminal,
			Taper:          resumeState.InflationTaper,
			FoundationVal:  resumeState.InflationFoundation,
			FoundationTerm: resumeState.InflationFoundationTerm,
		}
	} else if mithrilState != nil && mithrilState.ManifestCapitalization > 0 {
		// Priority 2: State file manifest_* fields (fresh start)
		epochCtx.Capitalization = mithrilState.ManifestCapitalization
		epochCtx.CapitalizationAudit = mithrilState.ManifestCapitalization
		epochCtx.SlotsPerYear = mithrilState.ManifestSlotsPerYear
		epochCtx.Inflation = rewards.Inflation{
			Initial:        mithrilState.ManifestInflationInitial,
			Terminal:       mithrilState.ManifestInflationTerminal,
			Taper:          mithrilState.ManifestInflationTaper,
			FoundationVal:  mithrilState.ManifestInflationFoundation,
			FoundationTerm: mithrilState.ManifestInflationFoundationTerm,
		}
	} else {
		return nil, fmt.Errorf("state file missing manifest_capitalization - delete AccountsDB and rebuild from snapshot")
	}

	// Epoch account hash from state file (required)
	if mithrilState != nil && mithrilState.ManifestEpochAcctsHash != "" {
		epochAcctsHash, err := base64.StdEncoding.DecodeString(mithrilState.ManifestEpochAcctsHash)
		if err != nil {
			return nil, fmt.Errorf("corrupted state file: failed to decode manifest_epoch_accts_hash: %w", err)
		}
		if len(epochAcctsHash) == 32 {
			epochCtx.HasEpochAcctsHash = true
			epochCtx.EpochAcctsHash = epochAcctsHash
		}
	}
	// Note: epoch account hash may be empty for snapshots before SIMD-0160

	return epochCtx, nil
}

// BoundaryStakeScanResult holds all accumulated data from a single streaming pass
// over stake accounts at the epoch boundary, serving both stake history update
// and epoch stakes/vote cache refresh.
type BoundaryStakeScanResult struct {
	// For stake history update
	StakeHistoryEffective    uint64
	StakeHistoryActivating   uint64
	StakeHistoryDeactivating uint64
	// For epoch stakes + vote cache
	VoteAcctStakes      map[solana.PublicKey]uint64
	EffectiveStakes     map[solana.PublicKey]uint64
	TotalEffectiveStake uint64
}

type epochRewardVoteCacheStats struct {
	TotalReferenced        int
	LoadedLive             int
	CarriedFromCurrent     int
	CarriedFromPrevious    int
	CarriedForwardFullData int
	Unavailable            int
}

// scanStakesForEpochBoundary performs a single streaming pass over all stake accounts,
// accumulating data needed by both updateStakeHistorySysvar and updateEpochStakesAndRefreshVoteCache.
func scanStakesForEpochBoundary(acctsDb *accountsdb.AccountsDb, slot uint64, targetEpoch uint64, leaderScheduleEpoch uint64, stakeHistory *sealevel.SysvarStakeHistory, epochSchedule *sealevel.SysvarEpochSchedule, f *features.Features) *BoundaryStakeScanResult {
	newRateActivationEpoch := newWarmupCooldownRateEpoch(epochSchedule, f)

	// Stake history accumulators (atomic — high contention from worker pool)
	var shEffective atomic.Uint64
	var shActivating atomic.Uint64
	var shDeactivating atomic.Uint64

	// Epoch stakes accumulators (mutex-guarded maps)
	voteAcctStakes := make(map[solana.PublicKey]uint64)
	var voteAcctStakesMu sync.Mutex
	effectiveStakes := make(map[solana.PublicKey]uint64)
	var effectiveStakesMu sync.Mutex
	var totalEffectiveStake atomic.Uint64

	_, err := global.StreamStakeAccounts(acctsDb, slot,
		func(pk solana.PublicKey, delegation *sealevel.Delegation, creditsObs uint64) {
			// --- Stake history accumulation ---
			if delegation.StakeLamports > 0 {
				entry := delegation.StakeActivatingAndDeactivating(targetEpoch, stakeHistory, newRateActivationEpoch)
				shEffective.Add(entry.Effective)
				shActivating.Add(entry.Activating)
				shDeactivating.Add(entry.Deactivating)
			}

			// --- Epoch stakes accumulation ---
			voteAcctStakesMu.Lock()
			voteAcctStakes[delegation.VoterPubkey] += delegation.StakeLamports
			voteAcctStakesMu.Unlock()

			effectiveStake := delegation.Stake(leaderScheduleEpoch, stakeHistory, newRateActivationEpoch)
			if effectiveStake > 0 {
				effectiveStakesMu.Lock()
				effectiveStakes[delegation.VoterPubkey] += effectiveStake
				effectiveStakesMu.Unlock()
				totalEffectiveStake.Add(effectiveStake)
			}
		})
	if err != nil {
		panic(fmt.Sprintf("error scanning stake accounts at epoch boundary: %s", err))
	}

	return &BoundaryStakeScanResult{
		StakeHistoryEffective:    shEffective.Load(),
		StakeHistoryActivating:   shActivating.Load(),
		StakeHistoryDeactivating: shDeactivating.Load(),
		VoteAcctStakes:           voteAcctStakes,
		EffectiveStakes:          effectiveStakes,
		TotalEffectiveStake:      totalEffectiveStake.Load(),
	}
}

func loadEpochRewardVoteAccount(acctsDb *accountsdb.AccountsDb, slot uint64, votePk solana.PublicKey) (*epochstakes.VoteAccount, error) {
	acct, err := acctsDb.GetAccount(slot, votePk)
	if err != nil {
		return nil, err
	}
	if acct == nil {
		return nil, fmt.Errorf("nil account")
	}
	if acct.Lamports == 0 || acct.Owner != a.VoteProgramAddr || len(acct.Data) == 0 {
		return nil, fmt.Errorf("invalid vote account state")
	}

	voteAcct, err := epochstakes.NewVoteAccountFromAccount(acct)
	if err != nil {
		return nil, err
	}
	if voteAcct.NodePubkey == (solana.PublicKey{}) {
		return nil, fmt.Errorf("vote account has zero node pubkey")
	}
	return voteAcct, nil
}

func populateEpochRewardVoteAccounts(leaderScheduleEpoch uint64, currentEpoch uint64, previousEpoch uint64, acctsDb *accountsdb.AccountsDb, slot uint64, voteAcctStakes map[solana.PublicKey]uint64) epochRewardVoteCacheStats {
	stats := epochRewardVoteCacheStats{TotalReferenced: len(voteAcctStakes)}
	currentVoteAccts := global.EpochStakesVoteAccts(currentEpoch)
	previousVoteAccts := global.EpochStakesVoteAccts(previousEpoch)

	for votePk := range voteAcctStakes {
		if voteAcct, err := loadEpochRewardVoteAccount(acctsDb, slot, votePk); err == nil {
			global.PutEpochVoteAccount(leaderScheduleEpoch, votePk, voteAcct)
			stats.LoadedLive++
			continue
		}

		carryForward := func(source *epochstakes.VoteAccount, current bool) bool {
			if source == nil {
				return false
			}
			cloned := source.Clone()
			global.PutEpochVoteAccount(leaderScheduleEpoch, votePk, cloned)
			if current {
				stats.CarriedFromCurrent++
			} else {
				stats.CarriedFromPrevious++
			}
			if len(cloned.Data) > 0 {
				stats.CarriedForwardFullData++
			}
			return true
		}

		if carryForward(currentVoteAccts[votePk], true) {
			continue
		}

		if carryForward(previousVoteAccts[votePk], false) {
			continue
		}

		stats.Unavailable++
	}

	return stats
}

// updateStakeHistorySysvar applies pre-computed stake history data to the sysvar.
func updateStakeHistorySysvar(acctsDb *accountsdb.AccountsDb, block *block.Block, prevSlotCtx *sealevel.SlotCtx, targetEpoch uint64, scanResult *BoundaryStakeScanResult) *sealevel.SysvarStakeHistory {
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

	var accumulatorStakeHistoryEntry sealevel.StakeHistoryEntry
	accumulatorStakeHistoryEntry.Effective = scanResult.StakeHistoryEffective
	accumulatorStakeHistoryEntry.Activating = scanResult.StakeHistoryActivating
	accumulatorStakeHistoryEntry.Deactivating = scanResult.StakeHistoryDeactivating
	stakeHistory.Update(targetEpoch, accumulatorStakeHistoryEntry)

	buf := new(bytes.Buffer)
	encoder := bin.NewBinEncoder(buf)
	stakeHistory.MustMarshalWithEncoder(encoder)
	newStakeHistoryBytes := buf.Bytes()
	copy(stakeHistoryAcct.Data, newStakeHistoryBytes)

	err = acctsDb.StoreAccounts([]*accounts.Account{stakeHistoryAcct}, prevSlotCtx.Slot, nil)
	if err != nil {
		panic(fmt.Sprintf("error storing new StakeHistory sysvar to accountsdb: %s", err))
	}
	block.EpochUpdatedAccts = append(block.EpochUpdatedAccts, stakeHistoryAcct.Clone())

	return &stakeHistory
}

func handleEpochTransition(acctsDb *accountsdb.AccountsDb, partitionedEpochRewards bool, prevSlotCtx *sealevel.SlotCtx, replayCtx *ReplayCtx, epochSchedule *sealevel.SysvarEpochSchedule, f *features.Features, block *block.Block, epoch uint64) *rewards.PartitionedRewardDistributionInfo {
	// Flush any pending stake pubkeys to the index file before scanning.
	// The async StoreAccounts callback from the previous block may not have
	// run yet, so flush here to ensure the index is complete for the scan.
	acctsDbDir := filepath.Join(acctsDb.AcctsDir, "..")
	if _, err := global.FlushPendingStakePubkeys(acctsDbDir); err != nil {
		mlog.Log.Errorf("failed to flush stake pubkeys before epoch scan: %v", err)
	}

	// Load stake history (used by both scan and rewards)
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
	leaderScheduleEpoch := epochSchedule.LeaderScheduleEpoch(block.Slot)
	logVoteStateV4FeatureStatus(newEpoch, firstSlotInEpoch, f)

	// Single streaming pass for both stake history and epoch stakes
	t0 := time.Now()
	scanResult := scanStakesForEpochBoundary(acctsDb, prevSlotCtx.Slot, epoch, leaderScheduleEpoch, &stakeHistory, epochSchedule, f)
	t1 := time.Now()

	updateEpochStakesAndRefreshVoteCache(leaderScheduleEpoch, epoch, block, acctsDb, prevSlotCtx.Slot, scanResult)
	t2 := time.Now()

	if global.ManageLeaderSchedule() {
		if len(global.EpochStakesVoteAccts(newEpoch)) > 0 {
			_, err = PrepareLeaderScheduleLocal(newEpoch, epochSchedule, "")
		} else {
			_, err = PrepareLeaderScheduleLocalFromVoteCache(newEpoch, epochSchedule, "")
		}

		if err != nil {
			panic(err)
		}

		var hasLeader bool
		block.Leader, hasLeader = global.LeaderForSlot(block.Slot)
		if !hasLeader {
			panic(fmt.Sprintf("couldn't find leader for slot %d at epoch boundary", block.Slot))
		}
	}
	t3 := time.Now()

	logCapitalizationAudit(firstSlotInEpoch, replayCtx.Capitalization, replayCtx.CapitalizationAudit)

	if partitionedEpochRewards {
		partitionedRewardsInfo, block.EpochUpdatedAccts, block.ParentEpochUpdatedAccts = beginPartitionedEpochRewardsDistribution(acctsDb, prevSlotCtx, &stakeHistory, replayCtx, epochSchedule, block, f, newEpoch, firstSlotInEpoch)
	} else {
		panic("only partitioned rewards supported")
	}
	t4 := time.Now()

	updateStakeHistorySysvar(acctsDb, block, prevSlotCtx, epoch, scanResult)
	t5 := time.Now()

	// Compact stake index at epoch boundary — removes duplicates from appends
	if err := global.CompactStakePubkeyIndex(acctsDbDir); err != nil {
		mlog.Log.Errorf("failed to compact stake pubkey index: %v", err)
	}

	mlog.Log.Infof("Timing: scan=%.1fs epochStakes=%.1fs leaderSched=%.1fs rewards=%.1fs stakeHistory=%.1fs total=%.1fs",
		t1.Sub(t0).Seconds(), t2.Sub(t1).Seconds(), t3.Sub(t2).Seconds(),
		t4.Sub(t3).Seconds(), t5.Sub(t4).Seconds(), t5.Sub(t0).Seconds())
	mlog.Log.Infof("=======================")

	return partitionedRewardsInfo
}

// updateEpochStakesAndRefreshVoteCache applies pre-computed vote/effective stake data
// from the boundary scan to refresh the vote cache and store epoch stakes.
func updateEpochStakesAndRefreshVoteCache(leaderScheduleEpoch uint64, previousEpoch uint64, b *block.Block, acctsDb *accountsdb.AccountsDb, slot uint64, scanResult *BoundaryStakeScanResult) {
	// Check if we need to compute epoch stakes (skip on resume)
	hasEpochStakes := global.HasEpochStakes(leaderScheduleEpoch)

	// ALWAYS refresh vote cache from AccountsDB, even if HasEpochStakes is true
	// This ensures the vote cache has fresh NodePubkey for leader schedule
	if err := RebuildVoteCacheFromAccountsDB(acctsDb, slot, scanResult.VoteAcctStakes, 0); err != nil {
		mlog.Log.Errorf("failed to rebuild vote cache at epoch boundary: %v", err)
	}

	// Skip epoch stakes storage if already cached (resume)
	if hasEpochStakes {
		rebuildAuthorizedVotersFromEpochVoteAccounts(b.Epoch)
		mlog.Log.Infof("already had EpochStakes for epoch %d", leaderScheduleEpoch)
		return
	}

	rewardVoteStats := populateEpochRewardVoteAccounts(leaderScheduleEpoch, b.Epoch, previousEpoch, acctsDb, slot, scanResult.VoteAcctStakes)
	mlog.Log.FileOnlyf("epoch reward vote cache: epoch=%d referenced=%d live_loaded=%d carried_from_current=%d carried_from_previous=%d carried_forward_full_data=%d unavailable=%d",
		leaderScheduleEpoch,
		rewardVoteStats.TotalReferenced,
		rewardVoteStats.LoadedLive,
		rewardVoteStats.CarriedFromCurrent,
		rewardVoteStats.CarriedFromPrevious,
		rewardVoteStats.CarriedForwardFullData,
		rewardVoteStats.Unavailable,
	)

	// Store epoch stakes computed during scanning
	for votePk, stake := range scanResult.EffectiveStakes {
		global.PutEpochStake(leaderScheduleEpoch, votePk, stake)
	}
	global.PutEpochTotalStake(leaderScheduleEpoch, scanResult.TotalEffectiveStake)

	// Rebuild authorized voters cache from epoch-cached vote accounts for the new epoch.
	// This matches the reward path's account source and avoids depending on the
	// lossy live vote cache rebuild.
	rebuildAuthorizedVotersFromEpochVoteAccounts(b.Epoch)

	maps.Copy(b.EpochStakesPerVoteAcct, global.EpochStakes(leaderScheduleEpoch))
	b.TotalEpochStake = scanResult.TotalEffectiveStake
}

// rebuildAuthorizedVotersFromEpochVoteAccounts rebuilds the epoch authorized voters
// cache from the per-epoch cached vote accounts used by rewards.
func rebuildAuthorizedVotersFromEpochVoteAccounts(epoch uint64) {
	voteAcctMap := global.EpochStakesVoteAccts(epoch)
	newCache := epochstakes.NewEpochAuthorizedVotersCache()

	for voteAcct, voteAccount := range voteAcctMap {
		if voteAccount == nil {
			continue
		}
		voteState, err := voteAccount.VoteState()
		if err != nil || voteState == nil {
			continue
		}
		switch voteState.Type {
		case sealevel.VoteStateVersionV0_23_5:
			// V0_23_5 has a single authorized voter
			newCache.PutEntry(voteAcct, voteState.V0_23_5.AuthorizedVoter)
		case sealevel.VoteStateVersionV1_14_11:
			voter, _, err := voteState.V1_14_11.AuthorizedVoters.GetOrCalculateAuthorizedVoterForEpoch(epoch)
			if err == nil {
				newCache.PutEntry(voteAcct, voter)
			}
		case sealevel.VoteStateVersionCurrent:
			voter, _, err := voteState.Current.AuthorizedVoters.GetOrCalculateAuthorizedVoterForEpoch(epoch)
			if err == nil {
				newCache.PutEntry(voteAcct, voter)
			}
		case sealevel.VoteStateVersionV4:
			voter, _, err := voteState.V4.AuthorizedVoters.GetOrCalculateAuthorizedVoterForEpoch(epoch)
			if err == nil {
				newCache.PutEntry(voteAcct, voter)
			}
		}
	}

	global.SetEpochAuthorizedVoters(newCache)
	mlog.Log.Infof("forkchoice: rebuilt authorized voters cache for epoch %d (%d entries)", epoch, newCache.Len())
}
