package accounts

import (
	"maps"
	"math/rand"
	"testing"
)

// modelTree is a naive reference: every branch holds a FULL copy of its composed
// visible state (durable base + ancestor writes + own writes). Trivially correct by
// construction; the real BranchTree composed with a durable map must match it.
type modelTree struct {
	durable  map[[32]byte]uint64            // the model's rooted base
	state    map[uint64]map[[32]byte]uint64 // branchID -> full composed view
	parent   map[uint64]uint64
	children map[uint64][]uint64
	frozen   map[uint64]bool
}

func newModelTree() *modelTree {
	return &modelTree{
		durable:  make(map[[32]byte]uint64),
		state:    make(map[uint64]map[[32]byte]uint64),
		parent:   make(map[uint64]uint64),
		children: make(map[uint64][]uint64),
		frozen:   make(map[uint64]bool),
	}
}

func (m *modelTree) addBranch(id, parentID uint64) bool {
	if parentID != 0 && m.state[parentID] == nil {
		return false
	}
	full := make(map[[32]byte]uint64)
	if parentID == 0 {
		maps.Copy(full, m.durable) // root branches see the durable base
	} else {
		maps.Copy(full, m.state[parentID])
	}
	m.state[id] = full
	m.parent[id] = parentID
	m.children[parentID] = append(m.children[parentID], id)
	if parentID != 0 {
		m.frozen[parentID] = true
	}
	return true
}

func (m *modelTree) commit(id uint64, writes map[[32]byte]uint64) {
	if m.state[id] == nil || m.frozen[id] {
		return
	}
	maps.Copy(m.state[id], writes)
}

func (m *modelTree) evict(id uint64) {
	if m.state[id] == nil {
		return
	}
	for _, c := range m.children[id] {
		m.evict(c)
	}
	delete(m.state, id)
	delete(m.frozen, id)
	delete(m.children, id)
}

func (m *modelTree) promote(id uint64) {
	if m.state[id] == nil {
		return
	}
	// The target's composed view becomes the new durable base (it already includes
	// the old base + the folded chain's writes). Survivors = target's descendants.
	m.durable = make(map[[32]byte]uint64)
	maps.Copy(m.durable, m.state[id])
	survivors := make(map[uint64]bool)
	var mark func(b uint64)
	mark = func(b uint64) {
		for _, c := range m.children[b] {
			if m.state[c] != nil {
				survivors[c] = true
				mark(c)
			}
		}
	}
	mark(id)
	for b := range m.state {
		if !survivors[b] {
			delete(m.state, b)
			delete(m.frozen, b)
			delete(m.children, b)
		}
	}
}

// Model-based randomized test: ~1500 seeded random ops driven against the real
// BranchTree (composed with a durable map, exactly as the replay loop composes it)
// and the naive full-copy model; after every op, every live branch's COMPOSED view of
// every key must match. Catches interleaving bugs no hand-picked scenario covers.
func TestBranchTreeModelBased(t *testing.T) {
	rng := rand.New(rand.NewSource(42)) // deterministic
	tree := NewBranchTree()
	model := newModelTree()
	realDurable := make(map[[32]byte]uint64) // the real side's rooted base

	var live []uint64
	nextSlot := uint64(1)
	keys := make([][32]byte, 12)
	for i := range keys {
		keys[i] = key32(byte(i + 1))
	}

	syncLive := func() {
		ids := tree.LiveIDs()
		live = live[:0]
		for id := range ids {
			live = append(live, id)
		}
	}

	// composed read: branch overlay chain, else durable — what the replay loop sees
	composedGet := func(id uint64, k [32]byte) (uint64, bool) {
		if a, ok := tree.Get(id, k); ok {
			return a.Lamports, true
		}
		v, ok := realDurable[k]
		return v, ok
	}

	checkParity := func(op string, step int) {
		for _, id := range live {
			for _, k := range keys {
				got, ok := composedGet(id, k)
				want, wantOk := model.state[id][k]
				if ok != wantOk {
					t.Fatalf("step %d %s: branch %d key %d presence mismatch: real=%v model=%v", step, op, id, k[0], ok, wantOk)
				}
				if ok && got != want {
					t.Fatalf("step %d %s: branch %d key %d value mismatch: real=%d model=%d", step, op, id, k[0], got, want)
				}
			}
		}
	}

	pickLive := func() uint64 {
		if len(live) == 0 {
			return 0
		}
		return live[rng.Intn(len(live))]
	}

	for step := range 1500 {
		switch op := rng.Intn(10); {
		case op < 4 || len(live) == 0: // add a branch (root or child of a random live one)
			parent := uint64(0)
			if len(live) > 0 && rng.Intn(3) > 0 {
				parent = pickLive()
			}
			id, ok := tree.AddBranch(parent, nextSlot, [32]byte{byte(nextSlot)})
			if mok := model.addBranch(id, parent); ok != mok {
				t.Fatalf("step %d: addBranch ok mismatch real=%v model=%v", step, ok, mok)
			}
			nextSlot++
			syncLive()
			checkParity("add", step)
		case op < 7: // commit random writes to a random live branch
			id := pickLive()
			writes := make(map[[32]byte]uint64)
			var delta []*Account
			for range rng.Intn(4) + 1 {
				k := keys[rng.Intn(len(keys))]
				v := uint64(rng.Intn(1000)) // 0 = tombstone, also exercised
				writes[k] = v
				delta = append(delta, &Account{Key: k, Lamports: v})
			}
			tree.Commit(id, delta)
			model.commit(id, writes)
			checkParity("commit", step)
		case op < 8: // evict a random live subtree
			id := pickLive()
			tree.EvictSubtree(id)
			model.evict(id)
			syncLive()
			checkParity("evict", step)
		default: // two-phase promote through a random live branch
			id := pickLive()
			chain := tree.PromotionChain(id)
			for _, ps := range chain { // fold ascending into the real durable base
				for _, a := range ps.Delta {
					realDurable[[32]byte(a.Key)] = a.Lamports
				}
			}
			tree.Promote(id)
			model.promote(id)
			syncLive()
			checkParity("promote", step)
		}
	}
	if tree.Len() != len(model.state) {
		t.Fatalf("final live-count mismatch: real=%d model=%d", tree.Len(), len(model.state))
	}
}
