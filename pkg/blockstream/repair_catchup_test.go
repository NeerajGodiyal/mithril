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

// Shreds-only mode (block.rpc_fallback=false, the shipped default): RPC never
// fetches blocks on a live-shred source — any slot, any distance, any state —
// and the live-gap recovery path must not arm the RPC force flags that would
// close the shred decode gates waiting for an RPC resume that cannot happen.
// Non-shred sources are unaffected: they ARE the RPC path. The repair monitor
// arms even with the gap threshold at 0, because repair is the only catchup
// there is.
func TestShredsOnlyModeGatesAllRPCBlockFetch(t *testing.T) {
	bs := NewBlockSource(&BlockSourceOpts{
		SourceType:               BlockSourceTurbine,
		TurbineBindAddr:          "127.0.0.1:0",
		RepairCatchupMaxGapSlots: 1024,
		StartSlot:                1_000,
		DisableRPCBlockFetch:     true,
	})
	bs.repairCatchupPending.Store(false)
	for _, slot := range []uint64{1, 999, 1_000, 5_000, 900_000} {
		if bs.shouldUseRPCForSlot(slot) {
			t.Fatalf("shreds-only: slot %d must never be RPC-fetchable", slot)
		}
	}

	bs.forceRPCForLightbringerGap(1_500, 0, 0, 0)
	if bs.lightbringerForceRPCUntil.Load() != 0 || bs.lightbringerCooldownUntil.Load() != 0 {
		t.Fatalf("shreds-only: live-gap recovery must not arm RPC force flags")
	}

	rpcSrc := NewBlockSource(&BlockSourceOpts{SourceType: BlockSourceRpc, StartSlot: 1_000, DisableRPCBlockFetch: true})
	if !rpcSrc.shouldUseRPCForSlot(10) {
		t.Fatalf("source=rpc must keep fetching blocks regardless of rpc_fallback")
	}

	// Lightbringer is a live shred stream WITHOUT repair machinery: it needs
	// RPC for old/evicted shreds, so shreds-only mode must not gate it.
	lb := NewBlockSource(&BlockSourceOpts{SourceType: BlockSourceLightbringer, LightbringerEndpoint: "localhost:1", StartSlot: 1_000, DisableRPCBlockFetch: true})
	if !lb.rpcBlockFetchAllowed() {
		t.Fatalf("source=lightbringer must keep RPC block fetch despite rpc_fallback=false")
	}
	if !lb.shouldUseRPCForSlot(2_000) {
		t.Fatalf("source=lightbringer catchup slots must stay RPC-fetchable")
	}

	noThreshold := NewBlockSource(&BlockSourceOpts{
		SourceType:           BlockSourceTurbine,
		TurbineBindAddr:      "127.0.0.1:0",
		StartSlot:            1_000,
		DisableRPCBlockFetch: true,
	})
	if !noThreshold.repairCatchupPending.Load() {
		t.Fatalf("shreds-only must arm the repair monitor even with threshold 0")
	}
}

// The FIRST handoff of a fresh boot: nothing has emitted or executed yet, so
// the runway anchor falls back to startSlot-1 (the resume block). Without
// the fallback the handoff can never arm — no block parent-links to slot 0 —
// and a shreds-only catchup deadlocks AT BOOT with the head fully assembled
// and staged, since the first emission can only come from this very handoff.
// Root cause behind four consecutive live catchup stalls.
func TestHandoffArmsOnFreshResumeAnchor(t *testing.T) {
	bs := newRepairCatchupTestSource(1024) // StartSlot 1_000
	bs.repairCatchupPending.Store(false)
	bs.repairCatchupFrom.Store(1_000)
	bs.repairCatchupUntil.Store(1_800)

	head := &b.Block{Slot: 1_000, SourceParentSlot: 999, FromLightbringer: true}
	bs.bufferLightbringerBlock(head)

	bs.maybePrepareLightbringerHandoff()

	if got := bs.lightbringerHandoffSlot.Load(); got != 1_000 {
		t.Fatalf("handoff must arm at the resume head on a fresh boot (anchor = startSlot-1); handoff_slot=%d", got)
	}
	select {
	case res := <-bs.resultQueue:
		if res.slot != 1_000 || res.block == nil {
			t.Fatalf("the staged head must drain to the emitter, got %+v", res)
		}
	default:
		t.Fatalf("an armed handoff must enqueue the runway head")
	}
}

// A certificate-skipped catchup head must be EMITTED as a skip, not merely
// crossed by the handoff runway: pre-handoff nothing feeds the results loop,
// so the drive injects a synthetic certified-skip result that the standard
// emission machinery consumes. This is the half of skipped-gap handling the
// runway test alone did not prove (observed live: an on-chain-skipped head
// slot with zero shreds stalled catchup for minutes).
func TestApplyCertifiedSkipToCatchupHead(t *testing.T) {
	bs := newRepairCatchupTestSource(1024)
	bs.reorderMu.Lock()
	bs.nextSlotToSend = 2_001
	bs.reorderMu.Unlock()

	if !bs.applyCertifiedSkipToCatchupHead(2_001) {
		t.Fatalf("skip must apply to the waiting head")
	}
	bs.reorderMu.Lock()
	certified := bs.alpenglowCertifiedSkips[2_001]
	bs.reorderMu.Unlock()
	if !certified {
		t.Fatalf("head must carry the CERTIFIED skip marker (post-handoff logic trusts it)")
	}
	select {
	case res := <-bs.resultQueue:
		if res.slot != 2_001 || !res.skipped || res.rpcIdx != -1 {
			t.Fatalf("expected a synthetic certified-skip result for the head, got %+v", res)
		}
	default:
		t.Fatalf("the results loop must be nudged — pre-handoff nothing else feeds it")
	}

	// Idempotent: a second application (skip already recorded) is a no-op.
	bs.reorderMu.Lock()
	bs.skippedSlots[2_001] = true
	bs.reorderMu.Unlock()
	if bs.applyCertifiedSkipToCatchupHead(2_001) {
		t.Fatalf("an already-recorded skip must not re-apply")
	}

	// Only the WAITING slot qualifies: skips beyond the head stay owned by
	// the ordinary decision machinery.
	if bs.applyCertifiedSkipToCatchupHead(2_010) {
		t.Fatalf("non-head slots must not be force-skipped by the drive")
	}
}

// No timer-based RPC exception exists inside the repair gate: while the
// drive is active, the ENTIRE gap — head included — is repair-owned. RPC
// takes catchup back only via the far-behind rule (drive-internal) or
// deactivation. This is the "repair owns the gap, no RPC creep" contract.
func TestRepairCatchupGateHasNoRPCException(t *testing.T) {
	bs := newRepairCatchupTestSource(1024)
	bs.repairCatchupPending.Store(false)
	bs.repairCatchupFrom.Store(2_001)
	bs.repairCatchupUntil.Store(2_800)

	for _, slot := range []uint64{2_001, 2_050, 2_800, 3_500} {
		if bs.shouldUseRPCForSlot(slot) {
			t.Fatalf("active drive: slot %d at/above the gate must stay repair-owned", slot)
		}
	}
	if !bs.shouldUseRPCForSlot(2_000) {
		t.Fatalf("slots below the gap keep normal RPC behavior")
	}

	bs.deactivateRepairCatchup(nil)
	if !bs.shouldUseRPCForSlot(2_050) {
		t.Fatalf("after deactivation, RPC catchup must resume")
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
