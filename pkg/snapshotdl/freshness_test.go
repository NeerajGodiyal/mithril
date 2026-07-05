package snapshotdl

import (
	"testing"

	"github.com/Overclock-Validator/solana-snapshot-finder-go/pkg/rpc"
)

// Freshness-first full-source selection: candidates are restricted to base
// slots whose ACHIEVABLE incremental end (anywhere in the network) is within
// the threshold of the live tip; download speed only ranks within that set.
// This is the guard against the observed failure: a fast node whose
// snapshotting wedged outranking slower nodes that were hours fresher.

func fullNode(url string, fullSlot int64) rpc.NodeResult {
	return rpc.NodeResult{RPC: url, FullSlot: fullSlot, FullURL: "http://" + url + "/full"}
}

func fullWithInc(url string, fullSlot, incBase, incEnd int64) rpc.NodeResult {
	n := fullNode(url, fullSlot)
	n.HasInc = true
	n.IncBase = incBase
	n.IncSlot = incEnd
	return n
}

func fullSlotsOf(results []rpc.NodeResult) map[int64]bool {
	out := map[int64]bool{}
	for _, r := range results {
		out[r.FullSlot] = true
	}
	return out
}

func TestFreshBaseRestriction(t *testing.T) {
	const tip = 100_000
	results := []rpc.NodeResult{
		// Fast-but-wedged source: full at 10k, its best incremental ends 80k
		// behind tip. Must be EXCLUDED despite any speed advantage.
		fullWithInc("stale-fast", 10_000, 10_000, 20_000),
		// Fresh base: full at 90k with an incremental ending 500 behind tip.
		fullWithInc("fresh-a", 90_000, 90_000, 99_500),
		// Another node on the same fresh base (no incremental of its own).
		fullNode("fresh-b", 90_000),
		// Full with no matching incremental anywhere: excluded as before.
		fullNode("orphan", 50_000),
	}

	filtered, stats := filterByIncrementalBaseMatch(results, tip, 2000)

	slots := fullSlotsOf(filtered)
	if slots[10_000] {
		t.Fatalf("stale base 10000 must be excluded (best end 80k behind tip)")
	}
	if slots[50_000] {
		t.Fatalf("orphan base without incrementals must be excluded")
	}
	if !slots[90_000] || len(filtered) != 2 {
		t.Fatalf("both fresh-base sources must survive (speed ranks them later), got %+v", filtered)
	}
	if stats.freshBases != 1 {
		t.Fatalf("expected 1 fresh base, got %d", stats.freshBases)
	}
}

// Whole cluster stale: fall back to the FRESHEST ACHIEVABLE base (least-stale
// end state), never the fastest-but-stalest, and record how far behind it is.
func TestAllStaleFallsBackToFreshestAchievable(t *testing.T) {
	const tip = 100_000
	results := []rpc.NodeResult{
		fullWithInc("staler", 10_000, 10_000, 20_000),    // 80k behind
		fullWithInc("lessStale", 40_000, 40_000, 60_000), // 40k behind — best achievable
	}

	filtered, stats := filterByIncrementalBaseMatch(results, tip, 2000)

	slots := fullSlotsOf(filtered)
	if !slots[40_000] || slots[10_000] {
		t.Fatalf("must select only the freshest-achievable base, got %+v", filtered)
	}
	if stats.freshestEndBehind != 40_000 {
		t.Fatalf("freshestEndBehind = %d, want 40000", stats.freshestEndBehind)
	}
}

// No base has any matching incremental: unfiltered, as before (downstream
// policies own that edge).
func TestNoMatchingBasesLeavesResultsUntouched(t *testing.T) {
	results := []rpc.NodeResult{
		fullNode("a", 10_000),
		fullNode("b", 40_000),
	}
	filtered, _ := filterByIncrementalBaseMatch(results, 100_000, 2000)
	if len(filtered) != len(results) {
		t.Fatalf("no-matching-base case must not filter, got %d/%d", len(filtered), len(results))
	}
}

// Threshold 0 / unknown tip disables the freshness restriction but keeps the
// base-match filter (legacy behavior).
func TestFreshnessDisabledKeepsBaseMatchOnly(t *testing.T) {
	results := []rpc.NodeResult{
		fullWithInc("stale", 10_000, 10_000, 20_000),
		fullWithInc("fresh", 90_000, 90_000, 99_500),
		fullNode("orphan", 50_000),
	}
	filtered, _ := filterByIncrementalBaseMatch(results, 0, 2000)
	slots := fullSlotsOf(filtered)
	if !slots[10_000] || !slots[90_000] || slots[50_000] {
		t.Fatalf("with no tip, both matched bases stay and orphan drops: %+v", slots)
	}
	filtered, _ = filterByIncrementalBaseMatch(results, 100_000, 0)
	slots = fullSlotsOf(filtered)
	if !slots[10_000] || !slots[90_000] {
		t.Fatalf("threshold 0 disables freshness restriction: %+v", slots)
	}
}
