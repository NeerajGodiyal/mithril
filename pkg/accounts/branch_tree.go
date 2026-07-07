package accounts

import "sync"

// branch is one node of the fork tree: a copy-on-write overlay of a single slot's
// account writes over its parent (nil parent = the durable rooted base). Only
// accounts written on this branch have delta entries; the rest resolve up the chain.
type branch struct {
	id       uint64
	slot     uint64
	blockID  [32]byte
	parent   *branch
	children []*branch
	delta    map[[32]byte]*Account // this branch's own writes (zero-lamport = tombstone)
	frozen   bool                  // sticky: set once it has a child; never mutated again
}

// PromotedSlot is one folded slot from a promoted chain: its branch id, slot, and
// account writes. The caller commits these durably (per-slot bankhash/context are
// tracked by the caller, keyed by BranchID).
type PromotedSlot struct {
	BranchID uint64
	Slot     uint64
	Delta    []*Account
}

// BranchTree holds the in-RAM tree of confirmed-but-unrooted branches over a durable
// rooted store: reads resolve nearest-ancestor, the finalized branch is promoted
// (folded to durable) and every non-descendant branch is evicted. Composition with
// the durable layer is the caller's job (see StateView).
type BranchTree struct {
	mu     sync.RWMutex
	byID   map[uint64]*branch
	nextID uint64
}

// NewBranchTree creates an empty fork tree over the durable rooted base.
func NewBranchTree() *BranchTree {
	return &BranchTree{byID: make(map[uint64]*branch)}
}

// AddBranch creates a copy-on-write child of parentID (0 = over the durable base)
// for slot/blockID and freezes the parent. Returns (id, true), or (0, false) if a
// non-zero parentID is not found (refuses to create an orphan).
func (t *BranchTree) AddBranch(parentID, slot uint64, blockID [32]byte) (uint64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var parent *branch
	if parentID != 0 {
		if parent = t.byID[parentID]; parent == nil {
			return 0, false
		}
	}
	t.nextID++
	b := &branch{id: t.nextID, slot: slot, blockID: blockID, delta: make(map[[32]byte]*Account)}
	if parent != nil {
		b.parent = parent
		parent.children = append(parent.children, b)
		parent.frozen = true
	}
	t.byID[b.id] = b
	return b.id, true
}

// Commit installs a completed slot's writes on a leaf branch. No-op if the branch is
// unknown or frozen (ever had a child) — only never-parented leaves are mutable.
func (t *BranchTree) Commit(branchID uint64, delta []*Account) {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.byID[branchID]
	if b == nil || b.frozen {
		return
	}
	for _, a := range delta {
		if a != nil {
			b.delta[[32]byte(a.Key)] = a
		}
	}
}

// Get resolves pubkey on branchID by walking to the root; returns (acct, true) at the
// nearest ancestor that wrote it, else (nil, false) to fall through to durable. The
// returned *Account is shared (immutable): callers must copy-on-write before mutating.
func (t *BranchTree) Get(branchID uint64, pubkey [32]byte) (*Account, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for b := t.byID[branchID]; b != nil; b = b.parent {
		if a, ok := b.delta[pubkey]; ok {
			// Return a clone: stored branch state is an immutable snapshot. A caller
			// that mutates a read in place (e.g. crediting the slot leader's fees)
			// must not corrupt this branch or, via copy-on-write ancestry, its
			// descendants — that would silently break fork isolation.
			return a.Clone(), true
		}
	}
	return nil, false
}

// Len reports the number of live branches (for RAM bounding).
func (t *BranchTree) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.byID)
}

// LiveIDs returns the set of live branch ids, so callers can prune side maps keyed by
// branch id after Promote/EvictSubtree.
func (t *BranchTree) LiveIDs() map[uint64]bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	ids := make(map[uint64]bool, len(t.byID))
	for id := range t.byID {
		ids[id] = true
	}
	return ids
}

// EvictSubtree drops branchID and all its descendants (a losing fork) and returns the
// count removed.
func (t *BranchTree) EvictSubtree(branchID uint64) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.byID[branchID]
	if b == nil {
		return 0
	}
	if b.parent != nil {
		b.parent.children = removeChild(b.parent.children, b)
	}
	return t.dropRec(b)
}

// PromotionChain returns the ancestor chain up to branchID as folded slots ascending by
// slot, WITHOUT mutating the tree. Returned deltas reference stored *Account values
// (immutable); within a slot their order is unspecified. Two-phase with Promote: commit
// this chain durably first, THEN call Promote — mirrors UnrootedOverlay's contract.
func (t *BranchTree) PromotionChain(branchID uint64) []PromotedSlot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	target := t.byID[branchID]
	if target == nil {
		return nil
	}
	var chain []*branch
	for b := target; b != nil; b = b.parent {
		chain = append(chain, b)
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	out := make([]PromotedSlot, 0, len(chain))
	for _, b := range chain {
		delta := make([]*Account, 0, len(b.delta))
		for _, a := range b.delta {
			delta = append(delta, a)
		}
		out = append(out, PromotedSlot{BranchID: b.id, Slot: b.slot, Delta: delta})
	}
	return out
}

// Promote drops the folded chain up to branchID and every non-descendant branch (the
// chain plus all losing forks, including competing roots), and re-roots branchID's
// children over the new durable base. Call ONLY after the PromotionChain deltas are
// durable, else a re-rooted survivor read falls through to a not-yet-updated base.
func (t *BranchTree) Promote(branchID uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	target := t.byID[branchID]
	if target == nil {
		return
	}
	// Survivors = target's descendants; drop everything else, re-root direct children.
	survivors := make(map[uint64]bool)
	var collect func(b *branch)
	collect = func(b *branch) {
		for _, c := range b.children {
			survivors[c.id] = true
			collect(c)
		}
	}
	collect(target)
	for _, c := range target.children {
		c.parent = nil
	}
	for id := range t.byID {
		if !survivors[id] {
			delete(t.byID, id)
		}
	}
}

// dropRec removes b and its descendants from the id index. Caller holds t.mu.
func (t *BranchTree) dropRec(b *branch) int {
	n := 1
	for _, c := range b.children {
		n += t.dropRec(c)
	}
	delete(t.byID, b.id)
	return n
}

func removeChild(children []*branch, b *branch) []*branch {
	for i, c := range children {
		if c == b {
			return append(children[:i], children[i+1:]...)
		}
	}
	return children
}
