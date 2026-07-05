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
