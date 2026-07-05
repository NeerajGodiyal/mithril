package turbine

import (
	"reflect"
	"testing"
)

// Helpers building slotState directly: repair selection is a pure function of
// held-shred/FEC-set state, so tests assert on the policy without
// constructing signed merkle shreds.

func newRepairSelectionSlot(slot uint64) *slotState {
	return &slotState{
		slot:    slot,
		shreds:  make(map[uint32]*Shred),
		fecSets: make(map[uint32]*fecState),
	}
}

// addCodedSet installs an FEC set with a KNOWN layout holding the given
// absolute data indices and codingHeld coding shreds.
func addCodedSet(s *slotState, start uint32, dataShreds, codingShreds uint16, dataHeld []uint32, codingHeld int) {
	fec := &fecState{
		slot:        s.slot,
		fecSetIndex: start,
		data:        make(map[uint32]*Shred),
		coding:      make(map[uint16]*Shred),
		layout:      fecLayout{dataShreds: dataShreds, codingShreds: codingShreds, shardSize: 1},
		haveLayout:  true,
	}
	for _, idx := range dataHeld {
		sh := &Shred{Slot: s.slot, Type: ShredTypeData, Index: idx, FECSetIndex: start}
		s.shreds[idx] = sh
		fec.data[idx-start] = sh
	}
	for pos := 0; pos < codingHeld; pos++ {
		fec.coding[uint16(pos)] = &Shred{Slot: s.slot, Type: ShredTypeCode, FECSetIndex: start, Position: uint16(pos)}
	}
	s.fecSets[start] = fec
}

// addUncodedData holds data shreds with NO layout knowledge (no coding heard
// for their sets) — the cold pre-join regime.
func addUncodedData(s *slotState, indices ...uint32) {
	for _, idx := range indices {
		s.shreds[idx] = &Shred{Slot: s.slot, Type: ShredTypeData, Index: idx}
	}
}

func seq(from, through uint32) []uint32 {
	out := make([]uint32, 0, through-from+1)
	for i := from; i <= through; i++ {
		out = append(out, i)
	}
	return out
}

// A set missing 12 data shreds while holding 8 coding shreds needs only 4
// repairs before Reed-Solomon recovers the rest — requesting all 12 would
// waste two thirds of the budget.
func TestRepairSelectionDeficitCapsCodedSet(t *testing.T) {
	s := newRepairSelectionSlot(9)
	addCodedSet(s, 0, 32, 32, seq(0, 19), 8)
	s.haveLast = true
	s.lastIndex = 31

	req, ok := s.repairRequest(256)
	if !ok {
		t.Fatal("expected a repair request")
	}
	want := []uint32{20, 21, 22, 23} // deficit = 32 - 20 data - 8 coding
	if !reflect.DeepEqual(req.MissingDataShreds, want) {
		t.Fatalf("missing = %v, want %v", req.MissingDataShreds, want)
	}
}

// Zero coding held means zero recovery leverage: every missing data shred
// must be fetched individually, ascending — the old linear behavior.
func TestRepairSelectionColdSlotRequestsAllMissing(t *testing.T) {
	s := newRepairSelectionSlot(9)
	addUncodedData(s, seq(0, 9)...)
	s.haveLast = true
	s.lastIndex = 31

	req, ok := s.repairRequest(256)
	if !ok {
		t.Fatal("expected a repair request")
	}
	if !reflect.DeepEqual(req.MissingDataShreds, seq(10, 31)) {
		t.Fatalf("missing = %v, want 10..31", req.MissingDataShreds)
	}
}

// Scarce budget should cross recovery thresholds, not spread beneath them:
// the set one shred short of recovering goes first.
func TestRepairSelectionCheapestUnlockFirst(t *testing.T) {
	s := newRepairSelectionSlot(9)
	addCodedSet(s, 0, 32, 32, seq(0, 9), 12)   // deficit 32-10-12 = 10
	addCodedSet(s, 32, 32, 32, seq(32, 56), 6) // deficit 32-25-6 = 1
	s.haveLast = true
	s.lastIndex = 63

	req, ok := s.repairRequest(256)
	if !ok {
		t.Fatal("expected a repair request")
	}
	want := append([]uint32{57}, seq(10, 19)...) // set B's single unlock, then set A's 10
	if !reflect.DeepEqual(req.MissingDataShreds, want) {
		t.Fatalf("missing = %v, want %v", req.MissingDataShreds, want)
	}
}

// A set that held enough to recover but errored anyway must fall back to
// fetching its missing data directly — deficit-capping to zero would starve
// the slot forever on poisoned coding state.
func TestRepairSelectionRecoveryFailureRequestsSpanDirectly(t *testing.T) {
	s := newRepairSelectionSlot(9)
	addCodedSet(s, 0, 32, 32, seq(0, 29), 4) // held 34 >= 32: recovery should have fired
	s.haveLast = true
	s.lastIndex = 31

	req, ok := s.repairRequest(256)
	if !ok {
		t.Fatal("expected a repair request")
	}
	if !reflect.DeepEqual(req.MissingDataShreds, []uint32{30, 31}) {
		t.Fatalf("missing = %v, want [30 31]", req.MissingDataShreds)
	}
}

// A known layout proves data extends through the span end, so the scan
// reaches past the highest RECEIVED index instead of waiting for a
// HighestWindowIndex round trip to discover the tail.
func TestRepairSelectionExtendsThroughCodedSpan(t *testing.T) {
	s := newRepairSelectionSlot(9)
	addCodedSet(s, 0, 32, 32, seq(0, 5), 2) // maxObserved 5, deficit 32-6-2 = 24

	req, ok := s.repairRequest(256)
	if !ok {
		t.Fatal("expected a repair request")
	}
	if !req.NeedHighestDataShred {
		t.Fatal("still no last shred: NeedHighestDataShred should hold")
	}
	if req.HighestDataShredIndex != 6 {
		t.Fatalf("highest probe hint = %d, want 6", req.HighestDataShredIndex)
	}
	if !reflect.DeepEqual(req.MissingDataShreds, seq(6, 29)) {
		t.Fatalf("missing = %v, want 6..29 (24 = deficit, from a span-extended scan)", req.MissingDataShreds)
	}
}

func TestRepairSelectionRespectsMaxMissing(t *testing.T) {
	s := newRepairSelectionSlot(9)
	addUncodedData(s, seq(0, 9)...)
	s.haveLast = true
	s.lastIndex = 31

	req, ok := s.repairRequest(3)
	if !ok {
		t.Fatal("expected a repair request")
	}
	if !reflect.DeepEqual(req.MissingDataShreds, []uint32{10, 11, 12}) {
		t.Fatalf("missing = %v, want [10 11 12]", req.MissingDataShreds)
	}
}

func TestSplitRepairBudget(t *testing.T) {
	cases := []struct {
		budget, edgeDemand, wantHead int
	}{
		{100, 3, 97},   // reserve bounded by tiny edge demand
		{100, 500, 80}, // full fifth reserved when the edge can use it
		{100, 0, 100},  // idle edge: the head is never capped
		{4, 100, 4},    // sub-5 budgets round the reserve to zero
	}
	for _, tc := range cases {
		if got := splitRepairBudget(tc.budget, tc.edgeDemand); got != tc.wantHead {
			t.Fatalf("splitRepairBudget(%d, %d) = %d, want %d", tc.budget, tc.edgeDemand, got, tc.wantHead)
		}
	}
}

func TestTierSendDemand(t *testing.T) {
	requests := []SlotRepairRequest{
		{Slot: 1, MissingDataShreds: []uint32{4, 7}, NeedHighestDataShred: true},
		{Slot: 2, MissingDataShreds: []uint32{0}},
	}
	if got := tierSendDemand(requests, true); got != 7 {
		t.Fatalf("head demand = %d, want 7 (first request's 3 sends doubled by fanout, plus 1)", got)
	}
	if got := tierSendDemand(requests, false); got != 4 {
		t.Fatalf("edge demand = %d, want 4", got)
	}
}
