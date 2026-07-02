package accounts

import "testing"

// key32 adapts the pk helper to the [32]byte key BranchTree.Get expects.
func key32(b byte) [32]byte { return [32]byte(pk(b)) }

func TestBranchTreeGetAndCommit(t *testing.T) {
	tree := NewBranchTree()
	b, _ := tree.AddBranch(0, 1, [32]byte{})
	tree.Commit(b, []*Account{uoAcct(1, 100)})

	if a, ok := tree.Get(b, key32(1)); !ok || a.Lamports != 100 {
		t.Fatalf("get written acct: ok=%v acct=%v", ok, a)
	}
	if _, ok := tree.Get(b, key32(2)); ok {
		t.Fatal("unwritten key should miss (fall through to durable)")
	}
}

func TestBranchTreeCopyOnWriteAncestry(t *testing.T) {
	tree := NewBranchTree()
	parent, _ := tree.AddBranch(0, 1, [32]byte{})
	tree.Commit(parent, []*Account{uoAcct(1, 10)}) // parent writes key 1
	child, _ := tree.AddBranch(parent, 2, [32]byte{})
	tree.Commit(child, []*Account{uoAcct(2, 20)}) // child writes key 2

	// child sees parent's key 1 (nearest-ancestor) and its own key 2
	if a, ok := tree.Get(child, key32(1)); !ok || a.Lamports != 10 {
		t.Fatalf("child should inherit parent key1: ok=%v acct=%v", ok, a)
	}
	if a, ok := tree.Get(child, key32(2)); !ok || a.Lamports != 20 {
		t.Fatalf("child should see own key2: ok=%v acct=%v", ok, a)
	}
	// parent must NOT see the child's write
	if _, ok := tree.Get(parent, key32(2)); ok {
		t.Fatal("parent must not see child's write")
	}
}

func TestBranchTreeChildOverridesParent(t *testing.T) {
	tree := NewBranchTree()
	parent, _ := tree.AddBranch(0, 1, [32]byte{})
	tree.Commit(parent, []*Account{uoAcct(1, 10)})
	child, _ := tree.AddBranch(parent, 2, [32]byte{})
	tree.Commit(child, []*Account{uoAcct(1, 77)}) // same key, newer value

	if a, _ := tree.Get(child, key32(1)); a.Lamports != 77 {
		t.Fatalf("child override should win: %v", a)
	}
	if a, _ := tree.Get(parent, key32(1)); a.Lamports != 10 {
		t.Fatalf("parent value must be unchanged: %v", a)
	}
}

func TestBranchTreeTombstoneShadows(t *testing.T) {
	tree := NewBranchTree()
	parent, _ := tree.AddBranch(0, 1, [32]byte{})
	tree.Commit(parent, []*Account{uoAcct(1, 10)})
	child, _ := tree.AddBranch(parent, 2, [32]byte{})
	tree.Commit(child, []*Account{uoAcct(1, 0)}) // zero-lamport tombstone

	a, ok := tree.Get(child, key32(1))
	if !ok || a.Lamports != 0 {
		t.Fatalf("child should see the zero-lamport tombstone shadowing parent: ok=%v acct=%v", ok, a)
	}
}

func TestBranchTreeForkIsolation(t *testing.T) {
	tree := NewBranchTree()
	parent, _ := tree.AddBranch(0, 1, [32]byte{})
	tree.Commit(parent, []*Account{uoAcct(9, 1)})
	a, _ := tree.AddBranch(parent, 2, [32]byte{0xAA}) // competing children of the same parent
	b, _ := tree.AddBranch(parent, 2, [32]byte{0xBB})
	tree.Commit(a, []*Account{uoAcct(1, 111)})
	tree.Commit(b, []*Account{uoAcct(1, 222)})

	if av, _ := tree.Get(a, key32(1)); av.Lamports != 111 {
		t.Fatalf("branch A isolation: %v", av)
	}
	if bv, _ := tree.Get(b, key32(1)); bv.Lamports != 222 {
		t.Fatalf("branch B isolation: %v", bv)
	}
	// both inherit the shared parent account
	if pv, ok := tree.Get(a, key32(9)); !ok || pv.Lamports != 1 {
		t.Fatalf("branch A should inherit shared parent key9: ok=%v", ok)
	}
}

func TestBranchTreeFreezeOnChild(t *testing.T) {
	tree := NewBranchTree()
	parent, _ := tree.AddBranch(0, 1, [32]byte{})
	tree.Commit(parent, []*Account{uoAcct(1, 10)})
	tree.AddBranch(parent, 2, [32]byte{}) // parent now frozen

	tree.Commit(parent, []*Account{uoAcct(1, 99)}) // must be a no-op
	if a, _ := tree.Get(parent, key32(1)); a.Lamports != 10 {
		t.Fatalf("frozen parent must not accept writes: %v", a)
	}
}

func TestBranchTreeEvictSubtree(t *testing.T) {
	tree := NewBranchTree()
	p, _ := tree.AddBranch(0, 1, [32]byte{})
	a, _ := tree.AddBranch(p, 2, [32]byte{0xAA})
	b, _ := tree.AddBranch(p, 2, [32]byte{0xBB})
	c, _ := tree.AddBranch(a, 3, [32]byte{}) // descendant of A
	if tree.Len() != 4 {
		t.Fatalf("expected 4 branches, got %d", tree.Len())
	}

	removed := tree.EvictSubtree(a) // drop losing fork A and its child C
	if removed != 2 {
		t.Fatalf("expected 2 removed (A+C), got %d", removed)
	}
	if tree.Len() != 2 {
		t.Fatalf("expected 2 remaining (P,B), got %d", tree.Len())
	}
	if _, ok := tree.Get(c, key32(1)); ok {
		t.Fatal("evicted branch C should be gone")
	}
	// sibling B and parent P survive
	tree.Commit(b, []*Account{uoAcct(5, 5)})
	if _, ok := tree.Get(b, key32(5)); !ok {
		t.Fatal("sibling B should survive eviction")
	}
}

func TestBranchTreePromoteThrough(t *testing.T) {
	tree := NewBranchTree()
	// Interleave AddBranch/Commit like real execution: a slot is committed (while a
	// leaf) before the next slot's branch is created off it. Winning chain P(1)->a(2)->c(3).
	p, _ := tree.AddBranch(0, 1, [32]byte{})
	tree.Commit(p, []*Account{uoAcct(1, 10)})
	a, _ := tree.AddBranch(p, 2, [32]byte{0x0A})
	tree.Commit(a, []*Account{uoAcct(2, 20)})
	loserB, _ := tree.AddBranch(p, 2, [32]byte{0x0B}) // competes with a
	tree.Commit(loserB, []*Account{uoAcct(1, 999)})
	c, _ := tree.AddBranch(a, 3, [32]byte{0x0C})
	tree.Commit(c, []*Account{uoAcct(3, 30)})
	loserD, _ := tree.AddBranch(a, 3, [32]byte{0x0D}) // competes with c
	tree.Commit(loserD, []*Account{uoAcct(4, 999)})
	e, _ := tree.AddBranch(c, 4, [32]byte{}) // survivor above the winner
	tree.Commit(e, []*Account{uoAcct(5, 50)})

	out := tree.PromotionChain(c)
	tree.Promote(c)

	// deltas returned ascending by slot for durable commit
	if len(out) != 3 || out[0].Slot != 1 || out[1].Slot != 2 || out[2].Slot != 3 {
		t.Fatalf("expected slots [1,2,3], got %+v", out)
	}
	if out[0].Delta[0].Lamports != 10 || out[2].Delta[0].Lamports != 30 {
		t.Fatalf("promoted deltas wrong: %+v", out)
	}
	// only the survivor E remains; losers + folded chain gone
	if tree.Len() != 1 {
		t.Fatalf("expected 1 branch (E) after promote, got %d", tree.Len())
	}
	if _, ok := tree.Get(loserB, key32(1)); ok {
		t.Fatal("loser B should be dropped")
	}
	if _, ok := tree.Get(loserD, key32(4)); ok {
		t.Fatal("loser D should be dropped")
	}
	// E now sits over the durable base: its own write is visible, folded state is not
	if v, ok := tree.Get(e, key32(5)); !ok || v.Lamports != 50 {
		t.Fatalf("survivor E own write should remain: ok=%v", ok)
	}
	if _, ok := tree.Get(e, key32(3)); ok {
		t.Fatal("folded chain state must no longer be in the tree (now durable)")
	}
}

// RED-verify (design-conformance #1): promote must evict competing non-descendant
// roots (e.g. an equivocating block at the same slot), per Alpenglow/Agave "evict
// every non-descendant of the finalized block".
func TestBranchTreePromoteEvictsOtherRoots(t *testing.T) {
	tree := NewBranchTree()
	r1, _ := tree.AddBranch(0, 1, [32]byte{0xA1})
	tree.Commit(r1, []*Account{uoAcct(1, 10)})
	r2, _ := tree.AddBranch(0, 1, [32]byte{0xB2}) // competing equivocating root
	tree.Commit(r2, []*Account{uoAcct(1, 99)})

	tree.Promote(r1)
	if tree.Len() != 0 {
		t.Fatalf("competing root must be evicted on promote; Len=%d", tree.Len())
	}
}

// RED-verify (correctness F1): freeze must be sticky — a branch that ever had a
// child stays immutable even after that child is evicted.
func TestBranchTreeFreezeStickyAfterEvict(t *testing.T) {
	tree := NewBranchTree()
	p, _ := tree.AddBranch(0, 1, [32]byte{})
	tree.Commit(p, []*Account{uoAcct(1, 10)})
	a, _ := tree.AddBranch(p, 2, [32]byte{})
	tree.EvictSubtree(a)                      // p loses its only child
	tree.Commit(p, []*Account{uoAcct(1, 99)}) // must stay a no-op

	if v, _ := tree.Get(p, key32(1)); v.Lamports != 10 {
		t.Fatalf("frozen parent must stay immutable after child eviction; got %d", v.Lamports)
	}
}

// AddBranch must refuse to create an orphan when a non-zero parent is missing.
func TestBranchTreeAddBranchMissingParent(t *testing.T) {
	tree := NewBranchTree()
	if _, ok := tree.AddBranch(9999, 2, [32]byte{}); ok {
		t.Fatal("AddBranch with a missing non-zero parent must return ok=false")
	}
	if tree.Len() != 0 {
		t.Fatal("no orphan branch should be created")
	}
}

// The promote contract the durable applier relies on: the same key written on
// multiple chain slots is returned per-slot ascending, so applying slots in order
// lands on the highest-slot (newest) value.
func TestBranchTreePromoteCrossSlotOrdering(t *testing.T) {
	tree := NewBranchTree()
	p, _ := tree.AddBranch(0, 1, [32]byte{})
	tree.Commit(p, []*Account{uoAcct(1, 10)})
	a, _ := tree.AddBranch(p, 2, [32]byte{})
	tree.Commit(a, []*Account{uoAcct(1, 55)}) // same key, later slot

	out := tree.PromotionChain(a)
	tree.Promote(a)
	if len(out) != 2 || out[0].Slot != 1 || out[1].Slot != 2 {
		t.Fatalf("expected slots [1,2] ascending, got %+v", out)
	}
	if out[0].Delta[0].Lamports != 10 || out[1].Delta[0].Lamports != 55 {
		t.Fatalf("ascending order must let slot 2 (55) win last: %+v", out)
	}
}

// A tombstone (zero-lamport) on the winning chain must propagate into the promoted
// deltas so durable deletion is applied.
func TestBranchTreePromoteTombstone(t *testing.T) {
	tree := NewBranchTree()
	p, _ := tree.AddBranch(0, 1, [32]byte{})
	tree.Commit(p, []*Account{uoAcct(1, 0)}) // tombstone

	out := tree.PromotionChain(p)
	tree.Promote(p)
	if len(out) != 1 || len(out[0].Delta) != 1 || out[0].Delta[0].Lamports != 0 {
		t.Fatalf("tombstone must appear in promoted delta: %+v", out)
	}
}

func TestBranchTreeDeepChainPromote(t *testing.T) {
	tree := NewBranchTree()
	var id uint64
	for slot := uint64(1); slot <= 6; slot++ {
		nid, ok := tree.AddBranch(id, slot, [32]byte{byte(slot)})
		if !ok {
			t.Fatalf("add slot %d", slot)
		}
		tree.Commit(nid, []*Account{uoAcct(byte(slot), slot*10)})
		id = nid
	}
	out := tree.PromotionChain(id)
	tree.Promote(id) // promote the whole 6-deep chain
	if len(out) != 6 {
		t.Fatalf("expected 6 slot deltas, got %d", len(out))
	}
	for i, sd := range out {
		if sd.Slot != uint64(i+1) {
			t.Fatalf("delta %d slot = %d, want %d", i, sd.Slot, i+1)
		}
	}
	if tree.Len() != 0 {
		t.Fatalf("whole chain promoted, tree should be empty; Len=%d", tree.Len())
	}
}

// Concurrent readers hammering Get while the main goroutine mutates the tree — makes
// -race actually exercise the RWMutex (the other tests are single-goroutine).
func TestBranchTreeConcurrentReads(t *testing.T) {
	tree := NewBranchTree()
	root, _ := tree.AddBranch(0, 1, [32]byte{})
	tree.Commit(root, []*Account{uoAcct(1, 1)})

	stop := make(chan struct{})
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			for {
				select {
				case <-stop:
					done <- struct{}{}
					return
				default:
					tree.Get(root, key32(1))
					tree.Len()
				}
			}
		}()
	}

	parent := root
	for slot := uint64(2); slot <= 200; slot++ {
		id, ok := tree.AddBranch(parent, slot, [32]byte{})
		if !ok {
			continue
		}
		tree.Commit(id, []*Account{uoAcct(byte(slot), slot)})
		if slot%20 == 0 {
			tree.Promote(id) // fold + re-root; parent ids above become stale
			parent = id
		} else {
			parent = id
		}
	}
	close(stop)
	for range 8 {
		<-done
	}
}

func TestBranchTreeLen(t *testing.T) {
	tree := NewBranchTree()
	if tree.Len() != 0 {
		t.Fatal("empty tree")
	}
	p, _ := tree.AddBranch(0, 1, [32]byte{})
	tree.AddBranch(p, 2, [32]byte{})
	if tree.Len() != 2 {
		t.Fatalf("expected 2, got %d", tree.Len())
	}
}
