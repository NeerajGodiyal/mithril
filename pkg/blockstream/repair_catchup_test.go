package blockstream

import (
	"testing"

	b "github.com/Overclock-Validator/mithril/pkg/block"
)

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

// The stuck-head rescue window is a narrow RPC exception INSIDE the repair
// gate: only the head slots the drive named are RPC-fetchable; everything
// beyond stays repair-owned, and deactivation closes the window with the
// rest of the drive state.
func TestRepairCatchupHeadRescueWindow(t *testing.T) {
	bs := newRepairCatchupTestSource(1024)
	bs.repairCatchupPending.Store(false)
	bs.repairCatchupFrom.Store(2_001)
	bs.repairCatchupUntil.Store(2_800)

	if bs.shouldUseRPCForSlot(2_050) {
		t.Fatalf("premise: gap slots are gated before the head window opens")
	}

	// The drive opens the window at the stuck head (Until before From — a
	// non-zero From is the open signal).
	bs.repairCatchupHeadUntil.Store(2_053)
	bs.repairCatchupHeadFrom.Store(2_050)

	for slot := uint64(2_050); slot <= 2_053; slot++ {
		if !bs.shouldUseRPCForSlot(slot) {
			t.Fatalf("head window slot %d must be RPC-fetchable", slot)
		}
	}
	if bs.shouldUseRPCForSlot(2_049) || bs.shouldUseRPCForSlot(2_054) {
		t.Fatalf("slots outside the head window stay repair-owned")
	}
	if !bs.shouldUseRPCForSlot(2_000) {
		t.Fatalf("slots below the gap keep normal RPC behavior")
	}

	// Fallback/teardown closes the head window along with the gate.
	bs.deactivateRepairCatchup(nil)
	if bs.repairCatchupHeadFrom.Load() != 0 || bs.repairCatchupHeadUntil.Load() != 0 {
		t.Fatalf("deactivation must close the head rescue window")
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

// Catchup stall rescue: when RPC catchup is head-of-line blocked, slots in
// the armed rescue window decode and deliver straight to the emitter — but
// ONLY inside the window, only pre-handoff, only outside near-tip.
func TestCatchupStallRescueGates(t *testing.T) {
	bs := NewBlockSource(&BlockSourceOpts{
		SourceType:      BlockSourceTurbine,
		TurbineBindAddr: "127.0.0.1:0",
		StartSlot:       1_000,
	})

	// Inactive: normal catchup gating (slot far from tip is dropped).
	if bs.shouldDecodeLightbringerSlot(1_005) {
		t.Fatalf("no rescue armed: catchup slots must not decode")
	}

	// Arm the window [1005, 1020].
	bs.rescueUntil.Store(1_020)
	bs.rescueFrom.Store(1_005)

	if !bs.catchupRescueCovers(1_005) || !bs.catchupRescueCovers(1_020) {
		t.Fatalf("window bounds must be covered")
	}
	if bs.catchupRescueCovers(1_004) || bs.catchupRescueCovers(1_021) {
		t.Fatalf("outside the window must not be covered")
	}
	if !bs.shouldDecodeLightbringerSlot(1_010) {
		t.Fatalf("rescue-window slot must decode during catchup")
	}
	if bs.shouldDecodeLightbringerSlot(1_030) {
		t.Fatalf("slots beyond the window keep normal catchup gating")
	}

	// Delivery: an assembled block inside the window goes straight to the
	// emitter's result queue (rpcIdx -1 marks it live-sourced).
	blk := &b.Block{Slot: 1_006, FromLightbringer: true, SourceParentSlot: 1_005}
	if !bs.ingestLiveShredBlock(blk) {
		t.Fatalf("ingest must accept the rescued block")
	}
	select {
	case res := <-bs.resultQueue:
		if res.slot != 1_006 || res.rpcIdx != -1 || res.block != blk {
			t.Fatalf("rescued block must be delivered as a live result, got %+v", res)
		}
	default:
		t.Fatalf("rescued block must be in the result queue, not the staging buffer")
	}

	// Outside the window: staged in the runway buffer as before, not delivered.
	other := &b.Block{Slot: 1_030, FromLightbringer: true, SourceParentSlot: 1_029}
	// (1_030 fails shouldDecode in catchup, so the receiver would drop it
	// before ingest; simulate the near-tip staging path being off by calling
	// ingest directly — it must buffer, not deliver.)
	if !bs.ingestLiveShredBlock(other) {
		t.Fatalf("ingest must not error on non-rescue blocks")
	}
	select {
	case res := <-bs.resultQueue:
		t.Fatalf("non-rescue block must not be delivered directly, got slot %d", res.slot)
	default:
	}
}
