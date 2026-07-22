package alpenglow

import (
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
)

func TestParentReadyConnectsNotarizedBlockAcrossSkips(t *testing.T) {
	root := BlockID{Slot: 0, Hash: parentReadyHash(1)}
	tracker := NewParentReadyTracker(root)
	block := BlockID{Slot: 1, Hash: parentReadyHash(2)}

	events, err := tracker.AddNotarFallbackOrStronger(block)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("unexpected event before leader-window boundary: %+v", events)
	}
	if events := tracker.AddSkip(2); len(events) != 0 {
		t.Fatalf("unexpected event at slot 3: %+v", events)
	}
	events = tracker.AddSkip(3)
	if len(events) != 1 || events[0].Kind != ConsensusEventParentReady || events[0].Slot != 4 || events[0].Block != block {
		t.Fatalf("parent-ready event = %+v, want block %v for slot 4", events, block)
	}
	parent := tracker.BlockProductionParent(4)
	if parent.Kind != BlockProductionParentReady || parent.Parent != block {
		t.Fatalf("block production parent = %+v", parent)
	}
	if parent.ReadyAt.IsZero() {
		t.Fatal("block production parent did not retain its ParentReady timer origin")
	}
}

func TestParentReadyChoosesLowestBlockLikeAgave(t *testing.T) {
	root := BlockID{Slot: 0, Hash: parentReadyHash(9)}
	tracker := NewParentReadyTracker(root)
	block := BlockID{Slot: 1, Hash: parentReadyHash(2)}
	if _, err := tracker.AddNotarFallbackOrStronger(block); err != nil {
		t.Fatal(err)
	}
	tracker.AddSkip(1)
	tracker.AddSkip(2)
	tracker.AddSkip(3)

	parent := tracker.BlockProductionParent(4)
	if parent.Kind != BlockProductionParentReady || parent.Parent != root {
		t.Fatalf("parent = %+v, want lowest block %v", parent, root)
	}
}

func TestParentReadyRestoreRejectsNonEarlierParent(t *testing.T) {
	root := BlockID{Slot: 3, Hash: parentReadyHash(3)}
	tracker := NewParentReadyTracker(root)
	if tracker.Restore(4, BlockID{Slot: 4, Hash: parentReadyHash(4)}) {
		t.Fatal("self-parenting restore was accepted")
	}
	if tracker.Restore(4, BlockID{Slot: 5, Hash: parentReadyHash(5)}) {
		t.Fatal("future-parent restore was accepted")
	}
	if tracker.Root() != root.Slot {
		t.Fatalf("rejected restore changed root to %d", tracker.Root())
	}
}

func TestParentReadyRestoreRejectsUnboundedGap(t *testing.T) {
	root := BlockID{Slot: 1, Hash: parentReadyHash(1)}
	tracker := NewParentReadyTracker(root)
	if tracker.Restore(maxParentReadyRestoreGap+2, root) {
		t.Fatal("restore accepted an unbounded skipped-slot allocation")
	}
}

func TestParentReadyUnrelatedInvalidationDoesNotSuppressBootstrapRestore(t *testing.T) {
	root := BlockID{Slot: 3, Hash: parentReadyHash(3)}
	tracker := NewParentReadyTracker(root)
	unrelated := BlockID{Slot: 5, Hash: parentReadyHash(5)}
	tracker.InvalidateBlock(unrelated)
	parent := BlockID{Slot: 4, Hash: parentReadyHash(4)}
	if !tracker.Restore(6, parent) {
		t.Fatal("unrelated invalid tombstone suppressed a safe bootstrap restore")
	}

	tracker = NewParentReadyTracker(root)
	tracker.InvalidateBlock(parent)
	if tracker.Restore(6, parent) {
		t.Fatal("bootstrap restore reactivated its invalid parent")
	}
}

func TestParentReadyGenesisAllowedAtRoot(t *testing.T) {
	root := BlockID{Slot: 100, Hash: parentReadyHash(1)}
	tracker := NewParentReadyTracker(root)
	genesis := BlockID{Slot: 100, Hash: parentReadyHash(3)}
	if _, err := tracker.AddNotarFallbackOrStronger(genesis); err != nil {
		t.Fatal(err)
	}
	if tracker.HasNotarFallbackOrStronger(genesis) {
		t.Fatal("ordinary certificate at root should be ignored")
	}
	if _, err := tracker.AddGenesis(genesis); err != nil {
		t.Fatal(err)
	}
	if !tracker.HasNotarFallbackOrStronger(genesis) {
		t.Fatal("genesis certificate at root was not retained")
	}
}

func TestParentReadyInvalidatesLowestSiblingAndSelectsSurvivor(t *testing.T) {
	tracker := NewParentReadyTracker(BlockID{Slot: 0, Hash: parentReadyHash(9)})
	now := time.Unix(1_700_000_000, 0)
	tracker.now = func() time.Time { return now }
	low := BlockID{Slot: 3, Hash: parentReadyHash(1)}
	high := BlockID{Slot: 3, Hash: parentReadyHash(2)}
	if _, err := tracker.AddNotarFallbackOrStronger(low); err != nil {
		t.Fatal(err)
	}
	initialReadyAt := now
	now = now.Add(50 * time.Millisecond)
	if _, err := tracker.AddNotarFallbackOrStronger(high); err != nil {
		t.Fatal(err)
	}
	before := tracker.BlockProductionParent(4)
	if before.Kind != BlockProductionParentReady || before.Parent != low {
		t.Fatalf("pre-invalidation parent = %+v, want %v", before, low)
	}
	if !before.ReadyAt.Equal(initialReadyAt) {
		t.Fatalf("nonpreferred sibling moved timer to %v, want %v", before.ReadyAt, initialReadyAt)
	}
	now = now.Add(50 * time.Millisecond)
	events := tracker.InvalidateBlock(low)
	if len(events) != 1 || events[0].Kind != ConsensusEventParentReady || events[0].Slot != 4 || events[0].Block != high {
		t.Fatalf("corrected parent-ready events = %+v, want surviving sibling %v", events, high)
	}
	after := tracker.BlockProductionParent(4)
	if after.Kind != BlockProductionParentReady || after.Parent != high {
		t.Fatalf("post-invalidation parent = %+v, want %v", after, high)
	}
	if !after.ReadyAt.Equal(before.ReadyAt) {
		t.Fatalf("corrected parent moved timer to %v, want original %v", after.ReadyAt, before.ReadyAt)
	}
	if tracker.HasNotarFallbackOrStronger(low) {
		t.Fatal("invalid sibling remained active as notar-fallback-or-stronger")
	}
	if tracker.Restore(4, low) {
		t.Fatal("restore reactivated an invalid parent")
	}
}

func TestParentReadyInvalidatingNonpreferredSiblingKeepsTimer(t *testing.T) {
	tracker := NewParentReadyTracker(BlockID{Slot: 0, Hash: parentReadyHash(9)})
	now := time.Unix(1_700_000_000, 0)
	tracker.now = func() time.Time { return now }
	low := BlockID{Slot: 3, Hash: parentReadyHash(1)}
	high := BlockID{Slot: 3, Hash: parentReadyHash(2)}
	if _, err := tracker.AddNotarFallbackOrStronger(low); err != nil {
		t.Fatal(err)
	}
	readyAt := tracker.BlockProductionParent(4).ReadyAt
	now = now.Add(50 * time.Millisecond)
	if _, err := tracker.AddNotarFallbackOrStronger(high); err != nil {
		t.Fatal(err)
	}
	now = now.Add(50 * time.Millisecond)
	if events := tracker.InvalidateBlock(high); len(events) != 0 {
		t.Fatalf("nonpreferred invalidation emitted corrected ParentReady: %+v", events)
	}
	parent := tracker.BlockProductionParent(4)
	if parent.Kind != BlockProductionParentReady || parent.Parent != low {
		t.Fatalf("parent after nonpreferred invalidation = %+v, want %v", parent, low)
	}
	if !parent.ReadyAt.Equal(readyAt) {
		t.Fatalf("nonpreferred invalidation moved timer to %v, want %v", parent.ReadyAt, readyAt)
	}
}

func TestParentReadyInvalidOnlyParentBecomesNotReady(t *testing.T) {
	tracker := NewParentReadyTracker(BlockID{Slot: 0, Hash: parentReadyHash(9)})
	block := BlockID{Slot: 3, Hash: parentReadyHash(1)}
	if _, err := tracker.AddNotarFallbackOrStronger(block); err != nil {
		t.Fatal(err)
	}
	tracker.InvalidateBlock(block)
	if got := tracker.BlockProductionParent(4); got.Kind != BlockProductionParentNotReady {
		t.Fatalf("only invalid parent left production state %+v, want NotReady", got)
	}
}

func parentReadyHash(value byte) solana.Hash {
	var hash solana.Hash
	hash[0] = value
	return hash
}
