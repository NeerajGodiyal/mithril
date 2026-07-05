package blockstream

import "testing"

func newRepairCatchupTestSource(maxGap uint64) *BlockSource {
	return NewBlockSource(&BlockSourceOpts{
		SourceType:               BlockSourceTurbine,
		TurbineBindAddr:          "127.0.0.1:0",
		RepairCatchupMaxGapSlots: maxGap,
		StartSlot:                1_000,
	})
}

// Construction arms the pending flag so the RPC scheduler can never race the
// gap before the shred-edge decision resolves.
func TestRepairCatchupPendingArmsAtConstruction(t *testing.T) {
	bs := newRepairCatchupTestSource(1024)
	if !bs.repairCatchupPending.Load() {
		t.Fatalf("turbine source with a threshold must arm the pending flag")
	}

	if disabled := newRepairCatchupTestSource(0); disabled.repairCatchupPending.Load() {
		t.Fatalf("threshold 0 must not arm repair catchup")
	}

	rpcOnly := NewBlockSource(&BlockSourceOpts{SourceType: BlockSourceRpc, RepairCatchupMaxGapSlots: 1024, StartSlot: 1_000})
	if rpcOnly.repairCatchupPending.Load() {
		t.Fatalf("non-turbine sources must not arm repair catchup")
	}
}

// The gap baseline is the RESUME FRONTIER (startSlot = snapshot/durable slot),
// not lastExecutedSlot — which is 0 on a fresh bootstrap because replay has
// not executed its first block. This is the regression guard for that bug: a
// snapshot-bootstrap node (lastExecutedSlot == 0) must still gate RPC from the
// snapshot slot, not from slot 1.
func TestRepairCatchupBaselineIsResumeFrontierNotLastExecuted(t *testing.T) {
	const snapshotSlot = 5_000
	bs := NewBlockSource(&BlockSourceOpts{
		SourceType:               BlockSourceTurbine,
		TurbineBindAddr:          "127.0.0.1:0",
		RepairCatchupMaxGapSlots: 1024,
		StartSlot:                snapshotSlot, // replay resumes here; nothing executed yet
	})
	if bs.lastExecutedSlot.Load() != 0 {
		t.Fatalf("premise: lastExecutedSlot is 0 before the first block executes")
	}
	if got := bs.repairCatchupResumeFrontier(); got != snapshotSlot {
		t.Fatalf("resume frontier = %d, want the snapshot slot %d (not lastExecutedSlot+1)", got, snapshotSlot)
	}
	// Pending gate must sit at the snapshot slot, so RPC is held off the gap...
	if bs.shouldUseRPCForSlot(snapshotSlot) || bs.shouldUseRPCForSlot(snapshotSlot+500) {
		t.Fatalf("pending: RPC must hold off from the snapshot slot, not from slot 1")
	}
	// ...but NOT the entire history below it (the lastExecutedSlot+1=1 bug
	// would have suppressed everything).
	if !bs.shouldUseRPCForSlot(snapshotSlot - 1) {
		t.Fatalf("slots below the resume frontier keep normal RPC behavior")
	}
}

// The gap belongs to turbine repair: RPC is suppressed at/above the gate while
// pending and while active, and resumes when both clear (fallback).
func TestRepairCatchupSuppressesRPCOverGap(t *testing.T) {
	const resume = 1_000 // = StartSlot in the test helper
	bs := newRepairCatchupTestSource(1024)

	// Pending: gate tracks the resume frontier (startSlot).
	if bs.shouldUseRPCForSlot(resume) {
		t.Fatalf("pending: RPC must hold off the resume frontier")
	}
	if bs.shouldUseRPCForSlot(resume + 500) {
		t.Fatalf("pending: RPC must hold off the whole prospective gap")
	}
	if !bs.shouldUseRPCForSlot(resume - 1) {
		t.Fatalf("slots below the resume frontier keep normal RPC behavior")
	}

	// Active: gate is the armed gap start.
	bs.repairCatchupPending.Store(false)
	bs.repairCatchupFrom.Store(2_001)
	bs.repairCatchupUntil.Store(2_800)
	if bs.shouldUseRPCForSlot(2_001) || bs.shouldUseRPCForSlot(2_800) {
		t.Fatalf("active: RPC must not fetch the armed gap")
	}
	if !bs.shouldUseRPCForSlot(2_000) {
		t.Fatalf("slots below the gap keep normal RPC behavior")
	}

	// Fallback: everything clears, RPC owns catchup again.
	bs.deactivateRepairCatchup(nil)
	if !bs.shouldUseRPCForSlot(2_001) {
		t.Fatalf("after fallback, RPC catchup must resume")
	}
}

// During catchup the handoff/decode gates bypass near-tip: gap blocks decode,
// buffer, and arm the handoff at the gap start even though replay is far
// behind the live edge.
func TestRepairCatchupBypassesNearTipGates(t *testing.T) {
	bs := newRepairCatchupTestSource(1024)
	bs.lastExecutedSlot.Store(2_000)
	bs.repairCatchupPending.Store(false)
	bs.repairCatchupFrom.Store(2_001)
	bs.repairCatchupUntil.Store(2_800)

	if bs.isNearTip.Load() {
		t.Fatalf("test premise: far from tip")
	}
	if !bs.shouldDecodeLightbringerSlot(2_050) {
		t.Fatalf("gap blocks must decode during repair catchup despite !nearTip")
	}
	if bs.shouldDecodeLightbringerSlot(1_500) {
		t.Fatalf("slots below the gap must not decode")
	}

	// Post-handoff: decode continues for slots >= handoff without nearTip.
	bs.lightbringerHandoffSlot.Store(2_001)
	if !bs.shouldDecodeLightbringerSlot(2_100) {
		t.Fatalf("post-handoff decode must bypass nearTip during repair catchup")
	}

	// prepare path: nearTip + replay-gap guards bypassed while active.
	bs.lightbringerHandoffSlot.Store(0)
	if _, _, prepared := bs.prepareLightbringerHandoff(2_001, 2_000); prepared {
		// Not prepared without buffered runway blocks — but it must get PAST
		// the nearTip/replay-gap guards and fail only on the empty runway.
		t.Fatalf("cannot prepare with an empty buffer")
	}

	// Deactivated: the same call is rejected by the nearTip guard again
	// (identical empty-runway input, different guard outcome is unobservable —
	// so assert via shouldDecode, which flips with the flag).
	bs.deactivateRepairCatchup(nil)
	if bs.shouldDecodeLightbringerSlot(2_050) {
		t.Fatalf("after deactivation, decode gating must revert to near-tip rules")
	}
}
