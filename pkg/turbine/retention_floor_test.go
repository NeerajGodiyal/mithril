package turbine

import "testing"

// Repair catchup pins a retention floor so gap slots far behind the live edge
// stay accepted, requestable, and retained until replay consumes them.
func TestRetentionFloorKeepsCatchupWindowAlive(t *testing.T) {
	a := NewSlotAssembler()
	a.maxObservedSlot = 10_000 // live edge

	oldSlot := uint64(10_000 - maxRetainedIncompleteSlotLag - 100) // deep in the gap

	if !a.slotTooOldLocked(oldSlot) {
		t.Fatalf("without a floor, slot %d should be too old at edge %d", oldSlot, a.maxObservedSlot)
	}

	a.SetRetentionFloor(oldSlot)
	if a.slotTooOldLocked(oldSlot) {
		t.Fatalf("with floor %d, slot %d must be accepted", oldSlot, oldSlot)
	}
	if a.slotTooOldLocked(oldSlot + 50) {
		t.Fatalf("slots above the floor must be accepted")
	}
	if !a.slotTooOldLocked(oldSlot - 1) {
		t.Fatalf("slots below the floor keep the normal lag rule")
	}

	// Clearing the floor restores lag-based retention.
	a.SetRetentionFloor(0)
	if !a.slotTooOldLocked(oldSlot) {
		t.Fatalf("clearing the floor must restore the lag cutoff")
	}
}

func TestRetentionFloorProtectsStateFromPruning(t *testing.T) {
	a := NewSlotAssembler()
	floor := uint64(1_000)
	a.SetRetentionFloor(floor)

	// Partial state at the floor, plus completed marker + block-id hint just above.
	a.slots[floor] = &slotState{slot: floor, shreds: map[uint32]*Shred{0: {}}, fecSets: map[uint32]*fecState{}}
	a.completedSlots[floor+1] = struct{}{}

	// Live edge races far ahead; prune must keep everything >= floor.
	a.maxObservedSlot = floor + maxRetainedIncompleteSlotLag + 5_000
	a.pruneOldSlotsLocked()

	if a.slots[floor] == nil {
		t.Fatalf("partial state at the floor must survive pruning")
	}
	if _, ok := a.completedSlots[floor+1]; !ok {
		t.Fatalf("completed marker above the floor must survive pruning")
	}

	// Priority repair slots inside the window survive too.
	a.priorityRepairSlots[floor+2] = struct{}{}
	a.priorityRepairOrder = append(a.priorityRepairOrder, floor+2)
	a.prunePriorityRepairSlotsLocked()
	if _, ok := a.priorityRepairSlots[floor+2]; !ok {
		t.Fatalf("priority repair slot inside the catchup window must survive pruning")
	}

	// Without the floor the same state is pruned.
	a.SetRetentionFloor(0)
	a.pruneOldSlotsLocked()
	if a.slots[floor] != nil {
		t.Fatalf("clearing the floor must let normal pruning reclaim the window")
	}
	a.prunePriorityRepairSlotsLocked()
	if _, ok := a.priorityRepairSlots[floor+2]; ok {
		t.Fatalf("clearing the floor must let priority pruning reclaim the window")
	}
}

func TestRetentionFloorProtectsRepairHeadAtIncompleteSlotCap(t *testing.T) {
	a := NewSlotAssembler()
	floor := uint64(1_000)
	a.SetRetentionFloor(floor)

	// Fill beyond the absolute cap with future live states. The old eviction
	// policy selected the numerically oldest state here, deleting the repair
	// head every time another response arrived.
	for slot := floor; slot < floor+maxRetainedIncompleteSlotCap+64; slot++ {
		a.slots[slot] = &slotState{
			slot:    slot,
			shreds:  map[uint32]*Shred{0: {}},
			fecSets: make(map[uint32]*fecState),
		}
	}
	priorityFuture := floor + maxRetainedIncompleteSlotCap + 63
	a.priorityRepairSlots[floor] = struct{}{}
	a.priorityRepairSlots[priorityFuture] = struct{}{}
	a.priorityRepairOrder = append(a.priorityRepairOrder, floor, priorityFuture)
	a.maxObservedSlot = floor + maxRetainedIncompleteSlotCap + 63

	a.pruneOldSlotsLocked()

	if got := len(a.slots); got != maxRetainedIncompleteSlotCap {
		t.Fatalf("retained %d incomplete slots, want cap %d", got, maxRetainedIncompleteSlotCap)
	}
	if a.slots[floor] == nil {
		t.Fatalf("repair head at retention floor was evicted at the cap")
	}
	if a.slots[priorityFuture] == nil {
		t.Fatalf("priority-pinned future slot was evicted at the cap")
	}
	if a.slots[priorityFuture-1] != nil {
		t.Fatalf("highest non-priority future slot survived cap eviction")
	}
	if got := a.evictedSlots; got != 64 {
		t.Fatalf("evicted %d slots, want 64", got)
	}
}
