package replay

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/epochstakes"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/leaderschedule"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"golang.org/x/exp/rand"
)

const (
	// NumConsecutiveLeaderSlots matches Solana's NUM_CONSECUTIVE_LEADER_SLOTS
	NumConsecutiveLeaderSlots = 4
	// MaxMismatchLogsPerEpoch caps mismatch logging to avoid disk churn
	MaxMismatchLogsPerEpoch = 100
	// SampleBoundarySlots is how many slots to check at epoch boundaries
	SampleBoundarySlots = 2000
	// SampleRandomSlots is how many random slots to sample in the middle
	SampleRandomSlots = 1000
)

var (
	mismatchLogOnce   sync.Once
	mismatchLogFile   *os.File
	mismatchLogWriter *bufio.Writer
	mismatchLogMu     sync.Mutex
)

// initMismatchLog creates/opens the mismatch log file (once per process).
// Uses the same log directory as Mithril's main logs.
func initMismatchLog(logsDir string) {
	mismatchLogOnce.Do(func() {
		if logsDir == "" {
			logsDir = "/mnt/mithril-logs"
		}
		// Create directory if it doesn't exist
		if err := os.MkdirAll(logsDir, 0755); err != nil {
			mlog.Log.Warnf("failed to create mismatch log directory: %v", err)
			return
		}
		logPath := filepath.Join(logsDir, "leader_schedule_mismatch.log")
		var err error
		mismatchLogFile, err = os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			mlog.Log.Warnf("failed to open leader schedule mismatch log: %v", err)
			return
		}
		mismatchLogWriter = bufio.NewWriter(mismatchLogFile)
		mlog.Log.Infof("leader schedule mismatch log: %s", logPath)
	})
}

// flushMismatchLog flushes buffered writes (call at end of epoch validation)
func flushMismatchLog() {
	mismatchLogMu.Lock()
	defer mismatchLogMu.Unlock()
	if mismatchLogWriter != nil {
		mismatchLogWriter.Flush()
	}
}

// logMismatch writes a mismatch entry (capped per epoch to avoid disk churn)
func logMismatch(epoch, slot uint64, localLeader, rpcLeader solana.PublicKey,
	voteAcct solana.PublicKey, stake uint64, mismatchCount *int) {
	if mismatchLogWriter == nil || *mismatchCount >= MaxMismatchLogsPerEpoch {
		return
	}
	*mismatchCount++
	mismatchLogMu.Lock()
	defer mismatchLogMu.Unlock()
	entry := fmt.Sprintf("[%s] epoch=%d slot=%d local=%s rpc=%s vote_acct=%s stake=%d\n",
		time.Now().Format(time.RFC3339), epoch, slot, localLeader, rpcLeader, voteAcct, stake)
	mismatchLogWriter.WriteString(entry)
}

// logInputSnapshot writes the top stakes to the mismatch log for debugging
func logInputSnapshot(epoch uint64, voteAcctStakes map[solana.PublicKey]uint64,
	voteAcctMap map[solana.PublicKey]*epochstakes.VoteAccount) {
	if mismatchLogWriter == nil {
		return
	}

	// Sort by stake descending to get top 10
	type stakeEntry struct {
		voteAcct solana.PublicKey
		stake    uint64
		nodePk   solana.PublicKey
	}
	entries := make([]stakeEntry, 0, len(voteAcctStakes))
	for pk, stake := range voteAcctStakes {
		var nodePk solana.PublicKey
		if va := voteAcctMap[pk]; va != nil {
			nodePk = va.NodePubkey
		}
		entries = append(entries, stakeEntry{voteAcct: pk, stake: stake, nodePk: nodePk})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].stake > entries[j].stake
	})

	mismatchLogMu.Lock()
	defer mismatchLogMu.Unlock()

	mismatchLogWriter.WriteString(fmt.Sprintf("\n[INPUTS] epoch=%d top_stakes:\n", epoch))
	for i := 0; i < min(10, len(entries)); i++ {
		e := entries[i]
		mismatchLogWriter.WriteString(fmt.Sprintf("  %d. vote=%s node=%s stake=%d\n",
			i+1, e.voteAcct, e.nodePk, e.stake))
	}
}

// ValidationStats holds statistics from schedule validation
type ValidationStats struct {
	SkippedZeroStake       int
	SkippedMissingNodePk   int
	SkippedMissingVoteAcct int
	TotalVoteAccts         int
	TotalStake             uint64
	MismatchCount          int
	Capped                 bool
	LocalFingerprint       string
	RPCFingerprint         string
}

// buildLocalLeaderSchedule builds a leader schedule from local state.
// Returns nil schedule if no valid stakes are available.
func buildLocalLeaderSchedule(
	epoch uint64,
	epochSchedule *sealevel.SysvarEpochSchedule,
	voteAcctStakes map[solana.PublicKey]uint64,
	voteAcctMap map[solana.PublicKey]*epochstakes.VoteAccount,
) (*leaderschedule.LeaderSchedule, ValidationStats) {
	stats := ValidationStats{TotalVoteAccts: len(voteAcctStakes)}

	// Filter and build epochVoteAccts map (only entries with stake > 0 and valid NodePubkey)
	epochVoteAccts := make(map[solana.PublicKey]*epochstakes.VoteAccount)
	filteredStakes := make(map[solana.PublicKey]uint64)

	for votePk, stake := range voteAcctStakes {
		if stake == 0 {
			stats.SkippedZeroStake++
			continue
		}

		va := voteAcctMap[votePk]
		if va == nil {
			stats.SkippedMissingVoteAcct++
			continue
		}

		// Check for zero NodePubkey (missing)
		var zeroPk solana.PublicKey
		if va.NodePubkey == zeroPk {
			stats.SkippedMissingNodePk++
			continue
		}

		epochVoteAccts[votePk] = va
		filteredStakes[votePk] = stake
		stats.TotalStake += stake
	}

	// Guard: empty stakes would panic in weightedrand
	if len(filteredStakes) == 0 {
		return nil, stats
	}

	// Get epoch length (handles warmup epochs correctly)
	slotsInEpoch := epochSchedule.SlotsInEpoch(epoch)

	// Build the schedule using leaderschedule.New
	ls := leaderschedule.New(
		epochVoteAccts,
		filteredStakes,
		epochSchedule,
		epoch,
		slotsInEpoch,
		NumConsecutiveLeaderSlots,
	)

	return ls, stats
}

// buildLocalLeaderScheduleFromVoteCache builds schedule using global.VoteCache() for NodePubkey lookups.
// Used at epoch boundaries when epochVoteAcctsMap may not be available.
// Returns nil schedule if no valid stakes are available.
func buildLocalLeaderScheduleFromVoteCache(
	epoch uint64,
	epochSchedule *sealevel.SysvarEpochSchedule,
	voteAcctStakes map[solana.PublicKey]uint64,
) (*leaderschedule.LeaderSchedule, ValidationStats) {
	stats := ValidationStats{TotalVoteAccts: len(voteAcctStakes)}

	voteCache := global.VoteCache()

	// Build epochVoteAccts map from vote cache
	epochVoteAccts := make(map[solana.PublicKey]*epochstakes.VoteAccount)
	filteredStakes := make(map[solana.PublicKey]uint64)

	for votePk, stake := range voteAcctStakes {
		if stake == 0 {
			stats.SkippedZeroStake++
			continue
		}

		vs := voteCache[votePk]
		if vs == nil {
			stats.SkippedMissingVoteAcct++
			continue
		}

		nodePk := vs.NodePubkey()
		var zeroPk solana.PublicKey
		if nodePk == zeroPk {
			stats.SkippedMissingNodePk++
			continue
		}

		// Create a VoteAccount with the NodePubkey
		va := &epochstakes.VoteAccount{
			NodePubkey: nodePk,
		}
		epochVoteAccts[votePk] = va
		filteredStakes[votePk] = stake
		stats.TotalStake += stake
	}

	// Guard: empty stakes would panic in weightedrand
	if len(filteredStakes) == 0 {
		return nil, stats
	}

	// Get epoch length (handles warmup epochs correctly)
	slotsInEpoch := epochSchedule.SlotsInEpoch(epoch)

	ls := leaderschedule.New(
		epochVoteAccts,
		filteredStakes,
		epochSchedule,
		epoch,
		slotsInEpoch,
		NumConsecutiveLeaderSlots,
	)

	return ls, stats
}

// scheduleFingerprint computes a short hash of schedule for quick comparison.
// Returns: "<base64(hash_first64)>/<base64(hash_last64)>"
func scheduleFingerprint(ls *leaderschedule.LeaderSchedule, firstSlot uint64, numSlots uint64) string {
	if ls == nil {
		return "nil/nil"
	}

	hashFirst := sha256.New()
	hashLast := sha256.New()

	// Hash first 64 slot leaders
	for i := uint64(0); i < min(64, numSlots); i++ {
		slot := firstSlot + i
		leader, ok := ls.LeaderForSlot(slot)
		if ok {
			hashFirst.Write(leader[:])
		}
	}

	// Hash last 64 slot leaders
	startLast := numSlots - min(64, numSlots)
	for i := startLast; i < numSlots; i++ {
		slot := firstSlot + i
		leader, ok := ls.LeaderForSlot(slot)
		if ok {
			hashLast.Write(leader[:])
		}
	}

	firstB64 := base64.StdEncoding.EncodeToString(hashFirst.Sum(nil)[:8])
	lastB64 := base64.StdEncoding.EncodeToString(hashLast.Sum(nil)[:8])
	return fmt.Sprintf("%s/%s", firstB64, lastB64)
}

// validateLeaderSchedule compares local vs RPC schedule and logs mismatches.
// Does NOT return error - mismatches are logged but don't stop replay.
func validateLeaderSchedule(
	blockEpoch uint64,
	epochSchedule *sealevel.SysvarEpochSchedule,
	rpcSchedule *leaderschedule.LeaderSchedule,
	logsDir string,
) {
	if rpcSchedule == nil {
		mlog.Log.Warnf("leader schedule validation: rpc schedule is nil, skipping")
		return
	}

	// Initialize mismatch log file (once per process)
	initMismatchLog(logsDir)

	// Stakes are stored under the epoch they're EFFECTIVE for, not the boundary epoch.
	// E.g., stakes frozen at end of epoch 499 are effective during epoch 500,
	// so they're stored as EpochStakes(500). LeaderScheduleEpoch returns 499 (the
	// boundary), but lookup should use blockEpoch (500).
	firstSlot := epochSchedule.FirstSlotInEpoch(blockEpoch)

	// Fetch stakes for blockEpoch (stored under the epoch they're effective for)
	voteAcctStakes := global.EpochStakes(blockEpoch)
	voteAcctMap := global.EpochStakesVoteAccts(blockEpoch)

	// Guard: skip if no stake data available for this epoch
	if voteAcctStakes == nil || len(voteAcctStakes) == 0 {
		mlog.Log.Warnf("leader schedule validation: no stake data for epoch=%d, skipping", blockEpoch)
		return
	}

	// Use blockEpoch for slot count (this is the epoch we're building schedule for)
	numSlots := epochSchedule.SlotsInEpoch(blockEpoch)

	// Log input snapshot for debugging
	logInputSnapshot(blockEpoch, voteAcctStakes, voteAcctMap)

	// Build local schedule for blockEpoch (same as RPC)
	localSchedule, stats := buildLocalLeaderSchedule(blockEpoch, epochSchedule, voteAcctStakes, voteAcctMap)

	// Guard: skip if local schedule couldn't be built (empty stakes after filtering)
	if localSchedule == nil {
		mlog.Log.Warnf("leader schedule validation: could not build local schedule (no valid stakes), skipping")
		mlog.Log.Infof("  epoch=%d vote_accts=%d skipped: zero_stake=%d missing_nodepk=%d missing_vote_acct=%d",
			blockEpoch, stats.TotalVoteAccts, stats.SkippedZeroStake, stats.SkippedMissingNodePk, stats.SkippedMissingVoteAcct)
		return
	}

	// Compute fingerprints (using blockEpoch's first slot and slot count)
	stats.LocalFingerprint = scheduleFingerprint(localSchedule, firstSlot, numSlots)
	stats.RPCFingerprint = scheduleFingerprint(rpcSchedule, firstSlot, numSlots)

	// Sample and compare slots
	mismatchCount := 0

	// Generate slots to sample: first 2k, last 2k, plus random 1k in middle
	slotsToSample := make([]uint64, 0, SampleBoundarySlots*2+SampleRandomSlots)

	// First boundary slots
	for i := uint64(0); i < min(SampleBoundarySlots, numSlots); i++ {
		slotsToSample = append(slotsToSample, firstSlot+i)
	}

	// Last boundary slots
	if numSlots > SampleBoundarySlots {
		startLast := numSlots - min(SampleBoundarySlots, numSlots)
		for i := startLast; i < numSlots; i++ {
			slotsToSample = append(slotsToSample, firstSlot+i)
		}
	}

	// Random slots in middle (deterministic based on blockEpoch for reproducibility)
	if numSlots > SampleBoundarySlots*2 {
		rng := rand.New(rand.NewSource(blockEpoch))
		middleStart := uint64(SampleBoundarySlots)
		middleEnd := numSlots - SampleBoundarySlots
		for i := 0; i < SampleRandomSlots && middleEnd > middleStart; i++ {
			offset := rng.Uint64() % (middleEnd - middleStart)
			slotsToSample = append(slotsToSample, firstSlot+middleStart+offset)
		}
	}

	// Compare sampled slots
	for _, slot := range slotsToSample {
		localLeader, localOk := localSchedule.LeaderForSlot(slot)
		rpcLeader, rpcOk := rpcSchedule.LeaderForSlot(slot)

		if !localOk || !rpcOk {
			continue // Skip slots not in schedules
		}

		if localLeader != rpcLeader {
			// Find the vote account that maps to the local leader for debugging
			var matchingVoteAcct solana.PublicKey
			var matchingStake uint64
			for votePk, va := range voteAcctMap {
				if va != nil && va.NodePubkey == localLeader {
					matchingVoteAcct = votePk
					matchingStake = voteAcctStakes[votePk]
					break
				}
			}
			logMismatch(blockEpoch, slot, localLeader, rpcLeader, matchingVoteAcct, matchingStake, &mismatchCount)
		}
	}

	stats.MismatchCount = mismatchCount
	stats.Capped = mismatchCount >= MaxMismatchLogsPerEpoch

	// Flush mismatch log
	flushMismatchLog()

	// Log per-epoch summary
	mlog.Log.Infof("leader schedule validation: epoch=%d first_slot=%d slots=%d",
		blockEpoch, firstSlot, numSlots)
	mlog.Log.Infof("  vote_accts=%d total_stake=%d skipped: zero_stake=%d missing_nodepk=%d missing_vote_acct=%d",
		stats.TotalVoteAccts, stats.TotalStake, stats.SkippedZeroStake, stats.SkippedMissingNodePk, stats.SkippedMissingVoteAcct)
	mlog.Log.Infof("  fingerprint: local=%s rpc=%s", stats.LocalFingerprint, stats.RPCFingerprint)
	mlog.Log.Infof("  sampled=%d mismatches=%d (capped=%v)", len(slotsToSample), stats.MismatchCount, stats.Capped)

	// Warn if there are mismatches or fingerprint differences
	if stats.LocalFingerprint != stats.RPCFingerprint {
		mlog.Log.Warnf("leader schedule validation: FINGERPRINT MISMATCH epoch=%d local=%s rpc=%s",
			blockEpoch, stats.LocalFingerprint, stats.RPCFingerprint)
	}
	if stats.MismatchCount > 0 {
		mlog.Log.Warnf("leader schedule validation: %d MISMATCHES found for epoch=%d - see %s/leader_schedule_mismatch.log",
			stats.MismatchCount, blockEpoch, logsDir)
	}
}

// validateLeaderScheduleFromVoteCache validates using global.VoteCache() for NodePubkey lookups.
// Used at epoch boundaries when epochVoteAcctsMap may not be available from snapshot.
func validateLeaderScheduleFromVoteCache(
	blockEpoch uint64,
	epochSchedule *sealevel.SysvarEpochSchedule,
	rpcSchedule *leaderschedule.LeaderSchedule,
	logsDir string,
) {
	if rpcSchedule == nil {
		mlog.Log.Warnf("leader schedule validation: rpc schedule is nil, skipping")
		return
	}

	// Initialize mismatch log file (once per process)
	initMismatchLog(logsDir)

	// Stakes are stored under the epoch they're EFFECTIVE for, not the boundary epoch.
	firstSlot := epochSchedule.FirstSlotInEpoch(blockEpoch)

	// Fetch stakes for blockEpoch (stored under the epoch they're effective for)
	voteAcctStakes := global.EpochStakes(blockEpoch)

	// Guard: skip if no stake data available for this epoch
	if voteAcctStakes == nil || len(voteAcctStakes) == 0 {
		mlog.Log.Warnf("leader schedule validation: no stake data for epoch=%d, skipping", blockEpoch)
		return
	}

	// Use blockEpoch for slot count (this is the epoch we're building schedule for)
	numSlots := epochSchedule.SlotsInEpoch(blockEpoch)

	// Build local schedule for blockEpoch (same as RPC)
	localSchedule, stats := buildLocalLeaderScheduleFromVoteCache(blockEpoch, epochSchedule, voteAcctStakes)

	// Guard: skip if local schedule couldn't be built (empty stakes after filtering)
	if localSchedule == nil {
		mlog.Log.Warnf("leader schedule validation: could not build local schedule (no valid stakes), skipping")
		mlog.Log.Infof("  epoch=%d vote_accts=%d skipped: zero_stake=%d missing_nodepk=%d missing_vote_state=%d",
			blockEpoch, stats.TotalVoteAccts, stats.SkippedZeroStake, stats.SkippedMissingNodePk, stats.SkippedMissingVoteAcct)
		return
	}

	// Compute fingerprints (using blockEpoch's first slot and slot count)
	stats.LocalFingerprint = scheduleFingerprint(localSchedule, firstSlot, numSlots)
	stats.RPCFingerprint = scheduleFingerprint(rpcSchedule, firstSlot, numSlots)

	// Sample and compare slots
	mismatchCount := 0

	// Generate slots to sample: first 2k, last 2k, plus random 1k in middle
	slotsToSample := make([]uint64, 0, SampleBoundarySlots*2+SampleRandomSlots)

	// First boundary slots
	for i := uint64(0); i < min(SampleBoundarySlots, numSlots); i++ {
		slotsToSample = append(slotsToSample, firstSlot+i)
	}

	// Last boundary slots
	if numSlots > SampleBoundarySlots {
		startLast := numSlots - min(SampleBoundarySlots, numSlots)
		for i := startLast; i < numSlots; i++ {
			slotsToSample = append(slotsToSample, firstSlot+i)
		}
	}

	// Random slots in middle (deterministic based on blockEpoch for reproducibility)
	if numSlots > SampleBoundarySlots*2 {
		rng := rand.New(rand.NewSource(blockEpoch))
		middleStart := uint64(SampleBoundarySlots)
		middleEnd := numSlots - SampleBoundarySlots
		for i := 0; i < SampleRandomSlots && middleEnd > middleStart; i++ {
			offset := rng.Uint64() % (middleEnd - middleStart)
			slotsToSample = append(slotsToSample, firstSlot+middleStart+offset)
		}
	}

	// Compare sampled slots
	voteCache := global.VoteCache()
	for _, slot := range slotsToSample {
		localLeader, localOk := localSchedule.LeaderForSlot(slot)
		rpcLeader, rpcOk := rpcSchedule.LeaderForSlot(slot)

		if !localOk || !rpcOk {
			continue
		}

		if localLeader != rpcLeader {
			// Find the vote account that maps to the local leader
			var matchingVoteAcct solana.PublicKey
			var matchingStake uint64
			for votePk, vs := range voteCache {
				if vs != nil && vs.NodePubkey() == localLeader {
					matchingVoteAcct = votePk
					matchingStake = voteAcctStakes[votePk]
					break
				}
			}
			logMismatch(blockEpoch, slot, localLeader, rpcLeader, matchingVoteAcct, matchingStake, &mismatchCount)
		}
	}

	stats.MismatchCount = mismatchCount
	stats.Capped = mismatchCount >= MaxMismatchLogsPerEpoch

	flushMismatchLog()

	// Log per-epoch summary
	mlog.Log.Infof("leader schedule validation (vote cache): epoch=%d first_slot=%d slots=%d",
		blockEpoch, firstSlot, numSlots)
	mlog.Log.Infof("  vote_accts=%d total_stake=%d skipped: zero_stake=%d missing_nodepk=%d missing_vote_state=%d",
		stats.TotalVoteAccts, stats.TotalStake, stats.SkippedZeroStake, stats.SkippedMissingNodePk, stats.SkippedMissingVoteAcct)
	mlog.Log.Infof("  fingerprint: local=%s rpc=%s", stats.LocalFingerprint, stats.RPCFingerprint)
	mlog.Log.Infof("  sampled=%d mismatches=%d (capped=%v)", len(slotsToSample), stats.MismatchCount, stats.Capped)

	// Warn if there are mismatches or fingerprint differences
	if stats.LocalFingerprint != stats.RPCFingerprint {
		mlog.Log.Warnf("leader schedule validation: FINGERPRINT MISMATCH epoch=%d local=%s rpc=%s",
			blockEpoch, stats.LocalFingerprint, stats.RPCFingerprint)
	}
	if stats.MismatchCount > 0 {
		mlog.Log.Warnf("leader schedule validation: %d MISMATCHES found for epoch=%d - see %s/leader_schedule_mismatch.log",
			stats.MismatchCount, blockEpoch, logsDir)
	}
}
