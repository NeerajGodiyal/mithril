package rewards

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/wide"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

// EpochBoundaryDiagnostics contains all diagnostic data for epoch boundary processing.
// This is populated during rewards calculation and written to a file.
type EpochBoundaryDiagnostics struct {
	// Epoch info
	PrevEpoch uint64
	NewEpoch  uint64
	Slot      uint64

	// Partition info
	LocalPartitions uint64
	RpcPartitions   uint64 // 0 if not available from RPC
	EligibleCount   uint64

	// Points and rewards
	TotalPoints         wide.Uint128
	TotalStakingRewards uint64
	LocalStakerTotal    uint64
	LocalVoterTotal     uint64
	RpcVoteTotal        uint64

	// Stake cache stats
	StakeCacheSize    int
	ActiveStake       uint64
	ActivatingStake   uint64
	DeactivatingStake uint64
	DeactivatedStake  uint64

	// Vote account comparison
	LocalVoteAccounts int
	RpcVoteAccounts   int

	// Detailed data for file output
	PointsPerStake map[solana.PublicKey]*CalculatedStakePoints
	StakingRewards map[solana.PublicKey]*CalculatedStakeRewards
	RpcRewards     []rpc.BlockReward
}

// WriteEpochBoundaryDiagnostics writes comprehensive diagnostics to a file in the run's log directory.
// Returns the path to the diagnostics file.
func WriteEpochBoundaryDiagnostics(diag *EpochBoundaryDiagnostics) string {
	logDir := mlog.GetLogDir()
	if logDir == "" {
		mlog.Log.Warnf("No log directory configured, skipping diagnostics file")
		return ""
	}

	filename := fmt.Sprintf("epoch_boundary_%d_to_%d_slot_%d.txt", diag.PrevEpoch, diag.NewEpoch, diag.Slot)
	diagPath := filepath.Join(logDir, filename)

	f, err := os.Create(diagPath)
	if err != nil {
		mlog.Log.Errorf("Failed to create diagnostics file: %v", err)
		return ""
	}
	defer f.Close()

	w := func(format string, args ...interface{}) {
		fmt.Fprintf(f, format+"\n", args...)
	}

	// Header
	w("=" + strings.Repeat("=", 79))
	w("EPOCH BOUNDARY DIAGNOSTICS")
	w("Generated: %s", time.Now().UTC().Format(time.RFC3339))
	w("=" + strings.Repeat("=", 79))
	w("")

	// Definitions section - explain all technical terms
	w("DEFINITIONS")
	w("-" + strings.Repeat("-", 79))
	w("  Epoch:           A period of ~2.5 days (~432,000 slots). Rewards are calculated")
	w("                   at the start of each epoch for work done in the previous epoch.")
	w("")
	w("  Boundary slot:   Last slot of the previous epoch (slot - 1). Stake account state")
	w("                   is read from this slot for rewards calculation.")
	w("")
	w("  Eligible:        Stake accounts that qualify for rewards. Must have:")
	w("                   - stake >= 1 lamport (after StakeMinimumDelegationForRewards feature)")
	w("                   - valid vote account in cache")
	w("                   - points > 0 (earned new vote credits this epoch)")
	w("                   - rewards > 0 after integer division")
	w("")
	w("  Partitions:      Rewards are distributed over multiple blocks (not all at once).")
	w("                   Each partition = up to 4096 stake accounts.")
	w("                   Formula: num_partitions = ceil(eligible_accounts / 4096)")
	w("")
	w("  Points:          stake_lamports * new_credits_earned_this_epoch")
	w("                   Determines share of the inflation pool each staker receives.")
	w("")
	w("  Staker rewards:  Lamports credited to stake accounts (after commission).")
	w("  Voter rewards:   Commission portion credited to vote accounts (validators).")
	w("")
	w("  Inflation pool:  Total new lamports minted for this epoch's rewards.")
	w("                   = capitalization * validator_rate * epoch_duration_in_years")
	w("")

	// Summary section
	w("SUMMARY")
	w("-" + strings.Repeat("-", 79))
	w("  Epoch transition:     %d -> %d", diag.PrevEpoch, diag.NewEpoch)
	w("  First slot of epoch:  %d", diag.Slot)
	w("  Boundary slot:        %d (last slot of epoch %d)", diag.Slot-1, diag.PrevEpoch)
	w("")

	// Partition comparison (key metric)
	w("PARTITIONS")
	w("-" + strings.Repeat("-", 79))
	partitionMatch := "MATCH"
	if diag.RpcPartitions != 0 && diag.LocalPartitions != diag.RpcPartitions {
		partitionMatch = "MISMATCH"
	}
	w("  Local:    %d", diag.LocalPartitions)
	if diag.RpcPartitions != 0 {
		w("  RPC:      %d", diag.RpcPartitions)
		w("  Status:   %s", partitionMatch)
	} else {
		w("  RPC:      (not available)")
	}
	w("  Eligible: %d stake accounts", diag.EligibleCount)
	w("  Formula:  ceil(eligible / 4096) = partitions")
	w("")

	// Rewards totals
	w("REWARDS TOTALS")
	w("-" + strings.Repeat("-", 79))
	w("  Total points:         %s", diag.TotalPoints.String())
	w("  Inflation pool:       %d lamports (%.4f SOL)", diag.TotalStakingRewards, float64(diag.TotalStakingRewards)/1e9)
	w("  Staker rewards:       %d lamports (%.4f SOL)", diag.LocalStakerTotal, float64(diag.LocalStakerTotal)/1e9)
	w("  Voter rewards:        %d lamports (%.4f SOL)", diag.LocalVoterTotal, float64(diag.LocalVoterTotal)/1e9)
	w("  Combined:             %d lamports", diag.LocalStakerTotal+diag.LocalVoterTotal)
	roundingLoss := int64(diag.TotalStakingRewards) - int64(diag.LocalStakerTotal+diag.LocalVoterTotal)
	w("  Rounding loss:        %d lamports", roundingLoss)
	w("")

	// Vote rewards comparison
	w("VOTE REWARDS COMPARISON")
	w("-" + strings.Repeat("-", 79))
	w("  Local vote accounts:  %d", diag.LocalVoteAccounts)
	w("  RPC vote accounts:    %d", diag.RpcVoteAccounts)
	w("  Local vote total:     %d lamports", diag.LocalVoterTotal)
	w("  RPC vote total:       %d lamports", diag.RpcVoteTotal)
	voteDiff := int64(diag.LocalVoterTotal) - int64(diag.RpcVoteTotal)
	w("  Diff (local - rpc):   %+d lamports", voteDiff)
	w("")

	// Stake cache breakdown
	w("STAKE CACHE BREAKDOWN")
	w("-" + strings.Repeat("-", 79))
	w("  Total delegations:    %d", diag.StakeCacheSize)
	w("  Active stake:         %.2f SOL", float64(diag.ActiveStake)/1e9)
	w("  Activating stake:     %.2f SOL (activation_epoch >= %d)", float64(diag.ActivatingStake)/1e9, diag.PrevEpoch)
	w("  Deactivating stake:   %.2f SOL (deactivation_epoch <= %d)", float64(diag.DeactivatingStake)/1e9, diag.PrevEpoch)
	w("  Deactivated stake:    %.2f SOL (deactivation_epoch < %d)", float64(diag.DeactivatedStake)/1e9, diag.PrevEpoch)
	w("")

	// Detailed vote account analysis (only if we have the data)
	if diag.StakingRewards != nil && len(diag.StakingRewards) > 0 {
		writeVoteAccountBreakdown(f, diag)
	}

	// Mismatch details (only if there are mismatches)
	if diag.RpcRewards != nil && len(diag.RpcRewards) > 0 {
		writeMismatchDetails(f, diag)
	}

	return diagPath
}

func writeVoteAccountBreakdown(f *os.File, diag *EpochBoundaryDiagnostics) {
	w := func(format string, args ...interface{}) {
		fmt.Fprintf(f, format+"\n", args...)
	}

	// Aggregate by vote account
	type voteStats struct {
		votePubkey    solana.PublicKey
		totalStake    uint64
		totalPoints   wide.Uint128
		voterRewards  uint64
		stakerRewards uint64
		numDelegations int
		commission    uint8
	}

	voteAgg := make(map[solana.PublicKey]*voteStats)
	stakeCache := global.StakeCache()

	for stakePk, sr := range diag.StakingRewards {
		if sr == nil {
			continue
		}
		stats, exists := voteAgg[sr.VotePubkey]
		if !exists {
			stats = &voteStats{votePubkey: sr.VotePubkey, commission: sr.Commission}
			voteAgg[sr.VotePubkey] = stats
		}
		stats.voterRewards += sr.VoterRewards
		stats.stakerRewards += sr.StakerRewards
		stats.numDelegations++

		if delegation := stakeCache[stakePk]; delegation != nil {
			stats.totalStake += delegation.StakeLamports
		}
	}

	// Add points from pointsPerStake
	for stakePk, pts := range diag.PointsPerStake {
		if pts == nil {
			continue
		}
		if delegation := stakeCache[stakePk]; delegation != nil {
			if stats, exists := voteAgg[delegation.VoterPubkey]; exists {
				stats.totalPoints = stats.totalPoints.Add(pts.Points)
			}
		}
	}

	// Sort by stake descending
	var sorted []*voteStats
	for _, stats := range voteAgg {
		sorted = append(sorted, stats)
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].totalStake > sorted[j].totalStake
	})

	w("=" + strings.Repeat("=", 79))
	w("TOP 50 VOTE ACCOUNTS BY STAKE")
	w("=" + strings.Repeat("=", 79))
	w("%-44s %5s %12s %12s %12s %6s", "vote_pubkey", "comm%", "stake(SOL)", "voter", "staker", "delegs")
	w(strings.Repeat("-", 95))

	for i, stats := range sorted {
		if i >= 50 {
			w("... and %d more vote accounts", len(sorted)-50)
			break
		}
		w("%-44s %4d%% %12.2f %12d %12d %6d",
			stats.votePubkey.String(),
			stats.commission,
			float64(stats.totalStake)/1e9,
			stats.voterRewards,
			stats.stakerRewards,
			stats.numDelegations)
	}
	w("")
}

func writeMismatchDetails(f *os.File, diag *EpochBoundaryDiagnostics) {
	w := func(format string, args ...interface{}) {
		fmt.Fprintf(f, format+"\n", args...)
	}

	// Build local vote rewards map
	localVoteRewards := AggregateVoteRewardsFromStakingRewards(diag.StakingRewards)

	// Build RPC vote rewards map
	rpcVoteRewards := make(map[solana.PublicKey]uint64)
	for _, r := range diag.RpcRewards {
		if string(r.RewardType) == RewardTypeVoting && r.Lamports > 0 {
			rpcVoteRewards[r.Pubkey] = uint64(r.Lamports)
		}
	}

	// Build commission and stake maps from staking rewards (aggregate by vote account)
	voteCommission := make(map[solana.PublicKey]uint8)
	voteStake := make(map[solana.PublicKey]uint64)
	stakeCache := global.StakeCache()
	for stakePk, sr := range diag.StakingRewards {
		if sr != nil {
			voteCommission[sr.VotePubkey] = sr.Commission
			if delegation := stakeCache[stakePk]; delegation != nil {
				voteStake[sr.VotePubkey] += delegation.StakeLamports
			}
		}
	}

	// Find mismatches
	type mismatch struct {
		pubkey     solana.PublicKey
		local      uint64
		rpc        uint64
		diff       int64
		commission uint8
		stake      uint64 // total stake delegated to this vote account
	}
	var mismatches []mismatch
	var localOnly []solana.PublicKey
	var rpcOnly []solana.PublicKey

	for pk, local := range localVoteRewards {
		rpc, exists := rpcVoteRewards[pk]
		if !exists {
			localOnly = append(localOnly, pk)
		} else if local != rpc {
			mismatches = append(mismatches, mismatch{pk, local, rpc, int64(local) - int64(rpc), voteCommission[pk], voteStake[pk]})
		}
	}
	for pk := range rpcVoteRewards {
		if _, exists := localVoteRewards[pk]; !exists {
			rpcOnly = append(rpcOnly, pk)
		}
	}

	// Find exact matches
	var exactMatches []solana.PublicKey
	for pk, local := range localVoteRewards {
		if rpc, exists := rpcVoteRewards[pk]; exists && local == rpc {
			exactMatches = append(exactMatches, pk)
		}
	}

	if len(mismatches) > 0 || len(localOnly) > 0 || len(rpcOnly) > 0 || len(exactMatches) > 0 {
		w("=" + strings.Repeat("=", 79))
		w("VOTE REWARDS MISMATCHES")
		w("=" + strings.Repeat("=", 79))
		w("")

		// Show exact matches first
		if len(exactMatches) > 0 {
			w("EXACT MATCHES (local == rpc): %d accounts", len(exactMatches))
			for _, pk := range exactMatches {
				comm := voteCommission[pk]
				w("  %s: %d lamports (commission=%d%%)", pk.String(), localVoteRewards[pk], comm)
			}
			w("")
		}

		if len(mismatches) > 0 {
			// Split by commission rate
			var comm0, comm100, commMid []mismatch
			for _, m := range mismatches {
				switch {
				case m.commission == 0:
					comm0 = append(comm0, m)
				case m.commission == 100:
					comm100 = append(comm100, m)
				default:
					commMid = append(commMid, m)
				}
			}

			// Sort by stake descending (for proportionality analysis)
			sortByStake := func(slice []mismatch) {
				sort.Slice(slice, func(i, j int) bool {
					return slice[i].stake > slice[j].stake
				})
			}

			// Helper to write stake-ordered view with diff/stake ratio
			writeStakeOrdered := func(label string, slice []mismatch, n int) {
				if len(slice) == 0 {
					return
				}
				// Sort by stake for this section
				sortByStake(slice)

				w("%s (%d total) - ORDERED BY STAKE:", label, len(slice))
				w("%-44s %5s %12s %15s %15s %12s", "vote_pubkey", "comm%", "stake(SOL)", "local", "diff", "diff/stake")
				w(strings.Repeat("-", 110))

				// Top N by stake
				w("  TOP %d BY STAKE (highest stake):", n)
				for i := 0; i < n && i < len(slice); i++ {
					m := slice[i]
					// Calculate diff per SOL of stake (lamports diff per SOL)
					var diffPerSol float64
					if m.stake > 0 {
						diffPerSol = float64(m.diff) / (float64(m.stake) / 1e9)
					}
					w("  %-44s %4d%% %12.2f %15d %+15d %12.6f",
						m.pubkey.String(), m.commission, float64(m.stake)/1e9, m.local, m.diff, diffPerSol)
				}

				// Bottom N by stake (only if we have enough to not overlap)
				if len(slice) > n {
					w("")
					w("  BOTTOM %d BY STAKE (lowest stake):", n)
					start := len(slice) - n
					if start < n {
						start = n
					}
					for i := start; i < len(slice); i++ {
						m := slice[i]
						var diffPerSol float64
						if m.stake > 0 {
							diffPerSol = float64(m.diff) / (float64(m.stake) / 1e9)
						}
						w("  %-44s %4d%% %12.2f %15d %+15d %12.6f",
							m.pubkey.String(), m.commission, float64(m.stake)/1e9, m.local, m.diff, diffPerSol)
					}
				}
				w("")
			}

			writeStakeOrdered("100% COMMISSION VALIDATORS", comm100, 10)
			writeStakeOrdered("0% COMMISSION VALIDATORS", comm0, 10)
			writeStakeOrdered("1-99% COMMISSION VALIDATORS", commMid, 10)

			w("TOTAL MISMATCHES: %d (0%%=%d, 100%%=%d, 1-99%%=%d)", len(mismatches), len(comm0), len(comm100), len(commMid))
			w("")
		}

		if len(localOnly) > 0 {
			w("LOCAL ONLY (in local but not in RPC): %d accounts", len(localOnly))
			for i, pk := range localOnly {
				if i >= 10 {
					w("  ... and %d more", len(localOnly)-10)
					break
				}
				w("  %s: %d lamports", pk.String(), localVoteRewards[pk])
			}
			w("")
		}

		if len(rpcOnly) > 0 {
			w("RPC ONLY (in RPC but not in local): %d accounts", len(rpcOnly))
			for i, pk := range rpcOnly {
				if i >= 10 {
					w("  ... and %d more", len(rpcOnly)-10)
					break
				}
				w("  %s: %d lamports", pk.String(), rpcVoteRewards[pk])
			}
			w("")
		}
	}
}

// LogEpochBoundarySummary logs a brief summary to the terminal.
// Detailed diagnostics go to the file.
func LogEpochBoundarySummary(diag *EpochBoundaryDiagnostics, diagPath string) {
	mlog.Log.Infof("")
	mlog.Log.Infof("━━━ EPOCH BOUNDARY: %d -> %d (slot %d) ━━━", diag.PrevEpoch, diag.NewEpoch, diag.Slot)

	// Key metrics on one line
	partitionStatus := "✓"
	if diag.RpcPartitions != 0 && diag.LocalPartitions != diag.RpcPartitions {
		partitionStatus = "✗"
	}
	mlog.Log.Infof("  partitions: %d (rpc=%d) %s | eligible: %d | points: %s",
		diag.LocalPartitions, diag.RpcPartitions, partitionStatus, diag.EligibleCount, diag.TotalPoints.String())

	// Rewards summary
	mlog.Log.Infof("  rewards: staker=%d voter=%d total=%d (pool=%d)",
		diag.LocalStakerTotal, diag.LocalVoterTotal, diag.LocalStakerTotal+diag.LocalVoterTotal, diag.TotalStakingRewards)

	// Vote comparison
	voteDiff := int64(diag.LocalVoterTotal) - int64(diag.RpcVoteTotal)
	if voteDiff == 0 {
		mlog.Log.Infof("  vote rewards: local=%d rpc=%d ✓", diag.LocalVoterTotal, diag.RpcVoteTotal)
	} else {
		mlog.Log.Warnf("  vote rewards: local=%d rpc=%d diff=%+d", diag.LocalVoterTotal, diag.RpcVoteTotal, voteDiff)
	}

	// Diagnostics file path
	if diagPath != "" {
		mlog.Log.Infof("  diagnostics: %s", diagPath)
	}
	mlog.Log.Infof("")
}
