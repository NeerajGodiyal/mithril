package blockstream

import (
	"math"
	"strings"
	"testing"
	"time"

	b "github.com/Overclock-Validator/mithril/pkg/block"
)

func TestLightbringerBlockConnectsLocked(t *testing.T) {
	bs := NewBlockSource(&BlockSourceOpts{
		SourceType: BlockSourceLightbringer,
		StartSlot:  100,
		EndSlot:    200,
	})

	bs.lastEmittedBlockSlot = 150

	if !bs.lightbringerBlockConnectsLocked(&b.Block{Slot: 151, FromLightbringer: true, SourceParentSlot: 150}) {
		t.Fatalf("expected Lightbringer block with matching parent slot to connect")
	}
	if bs.lightbringerBlockConnectsLocked(&b.Block{Slot: 151, FromLightbringer: true, SourceParentSlot: 149}) {
		t.Fatalf("expected Lightbringer block with mismatched parent slot to be rejected")
	}
	if bs.lightbringerBlockConnectsLocked(&b.Block{Slot: 151, FromLightbringer: true, SourceParentSlot: 0}) {
		t.Fatalf("expected Lightbringer block without parent metadata to be rejected once an anchor exists")
	}
	if !bs.lightbringerBlockConnectsLocked(&b.Block{Slot: 151, FromLightbringer: false}) {
		t.Fatalf("expected RPC block to pass through ancestry guard")
	}
}

func TestForceRPCForLightbringerParentMismatchClearsBufferedState(t *testing.T) {
	bs := NewBlockSource(&BlockSourceOpts{
		SourceType:           BlockSourceLightbringer,
		LightbringerEndpoint: "127.0.0.1:50051",
		StartSlot:            100,
		EndSlot:              200,
	})

	bs.lightbringerHandoffSlot.Store(101)
	bs.lightbringerActive.Store(true)

	bs.reorderBuffer[101] = &b.Block{Slot: 101, FromLightbringer: true, SourceParentSlot: 99}
	bs.reorderBuffer[102] = &b.Block{Slot: 102, FromLightbringer: true, SourceParentSlot: 101}
	bs.reorderBuffer[103] = &b.Block{Slot: 103, FromLightbringer: false}
	bs.slotState[101] = slotDone
	bs.slotState[102] = slotDone
	bs.slotState[103] = slotDone
	bs.lightbringerBuffer[104] = &b.Block{Slot: 104, FromLightbringer: true, SourceParentSlot: 102}
	bs.lightbringerBufferOrder = []uint64{104}

	bs.forceRPCForLightbringerParentMismatch(101, 99, 100)

	if got := bs.lightbringerForceRPCUntil.Load(); got != 101 {
		t.Fatalf("expected RPC to be forced until slot 101, got %d", got)
	}
	if got := bs.lightbringerCooldownUntil.Load(); got <= 101 {
		t.Fatalf("expected cooldown to extend past slot 101, got %d", got)
	}
	if got := bs.lightbringerHandoffSlot.Load(); got != 0 {
		t.Fatalf("expected handoff slot to be cleared, got %d", got)
	}
	if bs.lightbringerActive.Load() {
		t.Fatalf("expected Lightbringer to be marked inactive after parent mismatch")
	}
	if !bs.lightbringerNeedRPCResume.Load() {
		t.Fatalf("expected RPC resume flag to be raised after parent mismatch")
	}
	if _, exists := bs.reorderBuffer[101]; exists {
		t.Fatalf("expected mismatched Lightbringer slot 101 to be dropped from reorder buffer")
	}
	if _, exists := bs.reorderBuffer[102]; exists {
		t.Fatalf("expected prefetched Lightbringer slot 102 to be dropped from reorder buffer")
	}
	if _, exists := bs.reorderBuffer[103]; !exists {
		t.Fatalf("expected RPC slot 103 to remain buffered")
	}
	if _, exists := bs.slotState[101]; exists {
		t.Fatalf("expected slot state for 101 to be cleared")
	}
	if _, exists := bs.slotState[102]; exists {
		t.Fatalf("expected slot state for 102 to be cleared")
	}
	if _, exists := bs.slotState[103]; !exists {
		t.Fatalf("expected slot state for non-Lightbringer slot 103 to remain")
	}
	if len(bs.lightbringerBuffer) != 0 || len(bs.lightbringerBufferOrder) != 0 {
		t.Fatalf("expected prefetched Lightbringer buffer to be cleared")
	}
}

func TestPrepareLightbringerHandoffAllowsSkippedGapFromParentSlot(t *testing.T) {
	bs := NewBlockSource(&BlockSourceOpts{
		SourceType:           BlockSourceLightbringer,
		LightbringerEndpoint: "127.0.0.1:50051",
		StartSlot:            100,
		EndSlot:              200,
	})

	bs.isNearTip.Store(true)
	bs.lastEmittedBlockSlot = 150
	for slot := uint64(152); slot <= 158; slot++ {
		parentSlot := slot - 1
		if slot == 152 {
			parentSlot = 150
		}
		bs.lightbringerBuffer[slot] = &b.Block{Slot: slot, FromLightbringer: true, SourceParentSlot: parentSlot}
		bs.lightbringerBufferOrder = append(bs.lightbringerBufferOrder, slot)
	}

	blocks, handoffSlot, prepared := bs.prepareLightbringerHandoff(151, 150)
	if !prepared {
		t.Fatalf("expected handoff to prepare across a skipped gap")
	}
	if handoffSlot != 151 {
		t.Fatalf("expected handoff slot 151, got %d", handoffSlot)
	}
	if got := bs.lightbringerHandoffSlot.Load(); got != 151 {
		t.Fatalf("expected stored handoff slot 151, got %d", got)
	}
	if len(blocks) != 7 {
		t.Fatalf("expected buffered Lightbringer runway 152-158 to be retained, got %+v", blocks)
	}
	saw := make(map[uint64]bool, len(blocks))
	for _, blk := range blocks {
		saw[blk.Slot] = true
	}
	for slot := uint64(152); slot <= 158; slot++ {
		if !saw[slot] {
			t.Fatalf("expected buffered Lightbringer slot %d to be retained, got %+v", slot, blocks)
		}
	}
}

func TestPrepareLightbringerHandoffRequiresMinimumRunway(t *testing.T) {
	bs := NewBlockSource(&BlockSourceOpts{
		SourceType:           BlockSourceLightbringer,
		LightbringerEndpoint: "127.0.0.1:50051",
		StartSlot:            100,
		EndSlot:              200,
	})

	bs.isNearTip.Store(true)
	bs.lastEmittedBlockSlot = 150
	for slot := uint64(151); slot <= 152; slot++ {
		parentSlot := slot - 1
		if slot == 151 {
			parentSlot = 150
		}
		bs.lightbringerBuffer[slot] = &b.Block{Slot: slot, FromLightbringer: true, SourceParentSlot: parentSlot}
		bs.lightbringerBufferOrder = append(bs.lightbringerBufferOrder, slot)
	}

	if blocks, handoffSlot, prepared := bs.prepareLightbringerHandoff(151, 150); prepared || handoffSlot != 0 || len(blocks) != 0 {
		t.Fatalf("expected handoff to stay unarmed without the minimum Lightbringer runway, got prepared=%v handoff=%d blocks=%+v",
			prepared, handoffSlot, blocks)
	}
	if got := bs.lightbringerHandoffSlot.Load(); got != 0 {
		t.Fatalf("expected stored handoff slot to remain unset without enough runway, got %d", got)
	}
}

func TestPrepareLightbringerHandoffRequiresConnectedRunway(t *testing.T) {
	bs := NewBlockSource(&BlockSourceOpts{
		SourceType:           BlockSourceLightbringer,
		LightbringerEndpoint: "127.0.0.1:50051",
		StartSlot:            100,
		EndSlot:              200,
	})

	bs.isNearTip.Store(true)
	bs.lastEmittedBlockSlot = 150
	bs.lightbringerBuffer[151] = &b.Block{Slot: 151, FromLightbringer: true, SourceParentSlot: 150}
	bs.lightbringerBuffer[158] = &b.Block{Slot: 158, FromLightbringer: true, SourceParentSlot: 157}
	bs.lightbringerBufferOrder = []uint64{151, 158}

	if blocks, handoffSlot, prepared := bs.prepareLightbringerHandoff(151, 150); prepared || handoffSlot != 0 || len(blocks) != 0 {
		t.Fatalf("expected sparse Lightbringer buffer to stay unarmed without a connected runway, got prepared=%v handoff=%d blocks=%+v",
			prepared, handoffSlot, blocks)
	}
}

func TestPrepareLightbringerHandoffPurgesRPCOwnedStateAtBoundary(t *testing.T) {
	bs := NewBlockSource(&BlockSourceOpts{
		SourceType:           BlockSourceLightbringer,
		LightbringerEndpoint: "127.0.0.1:50051",
		StartSlot:            100,
		EndSlot:              200,
	})

	bs.isNearTip.Store(true)
	bs.lastEmittedBlockSlot = 150
	for slot := uint64(151); slot <= 158; slot++ {
		parentSlot := slot - 1
		if slot == 151 {
			parentSlot = 150
		}
		bs.lightbringerBuffer[slot] = &b.Block{Slot: slot, FromLightbringer: true, SourceParentSlot: parentSlot}
		bs.lightbringerBufferOrder = append(bs.lightbringerBufferOrder, slot)
	}

	bs.reorderBuffer[151] = &b.Block{Slot: 151, FromLightbringer: false}
	bs.reorderBuffer[152] = &b.Block{Slot: 152, FromLightbringer: false}
	bs.skippedSlots[153] = true
	bs.slotState[151] = slotInflight
	bs.slotState[152] = slotDone
	bs.retrySlots = []uint64{149, 151, 152}

	blocks, handoffSlot, prepared := bs.prepareLightbringerHandoff(151, 150)
	if !prepared {
		t.Fatalf("expected handoff to prepare")
	}
	if handoffSlot != 151 {
		t.Fatalf("expected handoff slot 151, got %d", handoffSlot)
	}
	if len(blocks) != 8 {
		t.Fatalf("expected buffered Lightbringer handoff runway 151-158, got %+v", blocks)
	}
	saw := make(map[uint64]bool, len(blocks))
	for _, blk := range blocks {
		if !blk.FromLightbringer {
			t.Fatalf("expected only Lightbringer blocks in handoff runway, got %+v", blocks)
		}
		saw[blk.Slot] = true
	}
	for slot := uint64(151); slot <= 158; slot++ {
		if !saw[slot] {
			t.Fatalf("expected buffered Lightbringer slot %d in handoff runway, got %+v", slot, blocks)
		}
	}
	if _, exists := bs.reorderBuffer[151]; exists {
		t.Fatalf("expected RPC buffered slot 151 to be purged at handoff")
	}
	if _, exists := bs.reorderBuffer[152]; exists {
		t.Fatalf("expected RPC buffered slot 152 to be purged at handoff")
	}
	if bs.skippedSlots[153] {
		t.Fatalf("expected RPC-owned skipped marker at slot 153 to be purged at handoff")
	}
	if _, exists := bs.slotState[151]; exists {
		t.Fatalf("expected slot state for 151 to be purged at handoff")
	}
	if _, exists := bs.slotState[152]; exists {
		t.Fatalf("expected slot state for 152 to be purged at handoff")
	}
	if len(bs.retrySlots) != 1 || bs.retrySlots[0] != 149 {
		t.Fatalf("expected retries at or beyond handoff to be purged, got %+v", bs.retrySlots)
	}
}

func TestSynthesizeLightbringerSkipsLockedMarksMissingSlots(t *testing.T) {
	bs := NewBlockSource(&BlockSourceOpts{
		SourceType:           BlockSourceLightbringer,
		LightbringerEndpoint: "127.0.0.1:50051",
		StartSlot:            100,
		EndSlot:              200,
	})

	bs.lightbringerActive.Store(true)
	bs.nextSlotToSend = 151
	bs.lastEmittedBlockSlot = 150
	bs.reorderBuffer[152] = &b.Block{Slot: 152, FromLightbringer: true, SourceParentSlot: 150}

	bs.reorderMu.Lock()
	synthesized := bs.synthesizeLightbringerSkipsLocked()
	bs.reorderMu.Unlock()

	if !synthesized {
		t.Fatalf("expected skipped slot inference to succeed")
	}
	if !bs.skippedSlots[151] {
		t.Fatalf("expected slot 151 to be marked skipped")
	}
}

func TestSynthesizeLightbringerSkipsLockedDropsDisconnectedWaitingBlockWhenDescendantConnects(t *testing.T) {
	bs := NewBlockSource(&BlockSourceOpts{
		SourceType:           BlockSourceLightbringer,
		LightbringerEndpoint: "127.0.0.1:50051",
		StartSlot:            100,
		EndSlot:              200,
	})

	bs.lightbringerActive.Store(true)
	bs.nextSlotToSend = 151
	bs.lastEmittedBlockSlot = 150
	bs.reorderBuffer[151] = &b.Block{Slot: 151, FromLightbringer: true, SourceParentSlot: 149}
	bs.reorderBuffer[152] = &b.Block{Slot: 152, FromLightbringer: true, SourceParentSlot: 150}

	bs.reorderMu.Lock()
	synthesized := bs.synthesizeLightbringerSkipsLocked()
	bs.reorderMu.Unlock()

	if !synthesized {
		t.Fatalf("expected disconnected waiting block to be bypassed via skip inference")
	}
	if !bs.skippedSlots[151] {
		t.Fatalf("expected slot 151 to be marked skipped")
	}
	if bs.reorderBuffer[151] != nil {
		t.Fatalf("expected disconnected Lightbringer block at slot 151 to be dropped")
	}
	if bs.reorderBuffer[152] == nil {
		t.Fatalf("expected connected descendant at slot 152 to remain buffered")
	}
}

func TestHandleDetectedLightbringerGapWaitsForStreamWhenLightbringerActive(t *testing.T) {
	bs := NewBlockSource(&BlockSourceOpts{
		SourceType:           BlockSourceLightbringer,
		LightbringerEndpoint: "127.0.0.1:50051",
		StartSlot:            100,
		EndSlot:              200,
	})

	bs.lightbringerActive.Store(true)
	bs.lightbringerHandoffSlot.Store(120)

	bs.handleDetectedLightbringerGap(125, 126, 125, 4)

	if got := bs.lightbringerForceRPCUntil.Load(); got != 0 {
		t.Fatalf("expected active Lightbringer gap to avoid forcing RPC, got %d", got)
	}
	if got := bs.lightbringerCooldownUntil.Load(); got != 0 {
		t.Fatalf("expected active Lightbringer gap to avoid setting cooldown, got %d", got)
	}
	if got := bs.lightbringerHandoffSlot.Load(); got != 120 {
		t.Fatalf("expected active Lightbringer gap to preserve handoff slot, got %d", got)
	}
	if !bs.lightbringerActive.Load() {
		t.Fatalf("expected Lightbringer to remain active")
	}
	if got := bs.lightbringerRepairSlot.Load(); got != 0 {
		t.Fatalf("expected active Lightbringer gap to avoid scheduling RPC repair, got %d", got)
	}
	if len(bs.retrySlots) != 0 {
		t.Fatalf("expected active Lightbringer gap to avoid queueing an RPC retry, got %+v", bs.retrySlots)
	}
}

func TestHandleDetectedLightbringerGapForcesRPCBeforeLightbringerIsActive(t *testing.T) {
	bs := NewBlockSource(&BlockSourceOpts{
		SourceType:           BlockSourceLightbringer,
		LightbringerEndpoint: "127.0.0.1:50051",
		StartSlot:            100,
		EndSlot:              200,
	})

	bs.lightbringerHandoffSlot.Store(120)

	bs.handleDetectedLightbringerGap(125, 126, 125, 4)

	if got := bs.lightbringerForceRPCUntil.Load(); got != 125 {
		t.Fatalf("expected pending Lightbringer gap to force RPC until slot 125, got %d", got)
	}
	if got := bs.lightbringerCooldownUntil.Load(); got <= 125 {
		t.Fatalf("expected pending Lightbringer gap to set recovery cooldown past 125, got %d", got)
	}
	if got := bs.lightbringerHandoffSlot.Load(); got != 0 {
		t.Fatalf("expected pending handoff to be cleared after forcing RPC, got %d", got)
	}
}

func TestShouldProbeAbsentConfirmationRequiresDepth(t *testing.T) {
	bs := NewBlockSource(&BlockSourceOpts{
		SourceType: BlockSourceRpc,
		StartSlot:  100,
		EndSlot:    200,
	})

	slot := uint64(150)
	bs.confirmedTip.Store(slot + rpcSkipConfirmDepth - 1)
	if bs.shouldProbeAbsentConfirmation(slot) {
		t.Fatalf("expected absent confirmation probe to stay disabled before the slot is far enough behind tip")
	}

	bs.confirmedTip.Store(slot + rpcSkipConfirmDepth)
	if !bs.shouldProbeAbsentConfirmation(slot) {
		t.Fatalf("expected absent confirmation probe once the slot is safely behind confirmed tip")
	}
}

func TestShouldFinalizeSkippedSlotRequiresConfirmedAbsenceProbe(t *testing.T) {
	bs := NewBlockSource(&BlockSourceOpts{
		SourceType: BlockSourceRpc,
		StartSlot:  100,
		EndSlot:    200,
	})

	slot := uint64(150)
	bs.confirmedTip.Store(slot + rpcSkipConfirmDepth)
	bs.waitingSlotErrors[slot] = &slotErrorInfo{
		slot:           slot,
		retryCount:     99,
		firstSeenAt:    time.Now().Add(-time.Hour),
		lastSeenAt:     time.Now(),
		lastErrorClass: "skipped",
	}

	if bs.shouldFinalizeSkippedSlot(slot, false) {
		t.Fatalf("expected skipped slot to remain provisional without a confirmed absence probe")
	}
	if !bs.shouldFinalizeSkippedSlot(slot, true) {
		t.Fatalf("expected skipped slot to finalize once absence is explicitly confirmed")
	}
}

func TestShouldFinalizeSkippedSlotAcceptsConfirmedSlotNotAvailable(t *testing.T) {
	bs := NewBlockSource(&BlockSourceOpts{
		SourceType: BlockSourceRpc,
		StartSlot:  100,
		EndSlot:    200,
	})

	slot := uint64(150)
	bs.confirmedTip.Store(slot + rpcSkipConfirmDepth)
	bs.waitingSlotErrors[slot] = &slotErrorInfo{
		slot:           slot,
		retryCount:     3,
		firstSeenAt:    time.Now().Add(-time.Minute),
		lastSeenAt:     time.Now(),
		lastErrorClass: "slot_not_available",
	}

	if !bs.shouldFinalizeSkippedSlot(slot, true) {
		t.Fatalf("expected confirmed slot-not-available observation to finalize as skipped")
	}
}

func TestRescueStaleWaitingSlotRequeuesHungSlot(t *testing.T) {
	bs := NewBlockSource(&BlockSourceOpts{
		SourceType: BlockSourceRpc,
		StartSlot:  100,
		EndSlot:    200,
	})

	slot := uint64(150)
	bs.slotState[slot] = slotInflight
	bs.inflightStart[slot] = time.Now().Add(-staleWaitingSlotRetry - time.Second)

	if !bs.rescueStaleWaitingSlot(slot, staleWaitingSlotRetry) {
		t.Fatalf("expected stale waiting slot to be rescued")
	}
	if _, exists := bs.slotState[slot]; exists {
		t.Fatalf("expected rescued slot state to be cleared")
	}
	if _, exists := bs.inflightStart[slot]; exists {
		t.Fatalf("expected rescued inflight timestamp to be cleared")
	}

	retries := bs.getRetrySlots()
	if len(retries) != 1 || retries[0] != slot {
		t.Fatalf("expected rescued slot %d to be requeued, got %v", slot, retries)
	}
}

func TestStopReasonDistinguishesFiniteCompletionFromUnexpectedLiveEnd(t *testing.T) {
	finite := NewBlockSource(&BlockSourceOpts{
		SourceType: BlockSourceRpc,
		StartSlot:  100,
		EndSlot:    200,
	})
	finite.setStopReason(blockSourceStopReasonCompleted, 200)
	if !finite.Completed() {
		t.Fatalf("expected finite block source to report completion")
	}
	if got := finite.StopReason(); !strings.Contains(got, "completed finite replay") {
		t.Fatalf("expected finite completion reason, got %q", got)
	}

	live := NewBlockSource(&BlockSourceOpts{
		SourceType: BlockSourceLightbringer,
		StartSlot:  100,
		EndSlot:    uint64(math.MaxUint64),
	})
	live.setStopReason(blockSourceStopReasonUnexpectedLiveEnd, 150)
	if live.Completed() {
		t.Fatalf("expected unexpected live stop to not report completion")
	}
	if got := live.StopReason(); !strings.Contains(got, "unexpectedly in live mode") {
		t.Fatalf("expected unexpected live stop reason, got %q", got)
	}
}
