package accounts

import (
	"testing"
)

// Benchmarks for the fork-tree read path vs a flat resolved index, at the
// realistic depth (~31 unrooted slots) — the data behind the "resolved hot
// index" design decision.

func buildDeepChain(depth int, writesPerSlot int) (*BranchTree, uint64) {
	tree := NewBranchTree()
	parent := uint64(0)
	for d := 0; d < depth; d++ {
		id, _ := tree.AddBranch(parent, uint64(d+1), [32]byte{byte(d + 1)})
		var delta []*Account
		for w := 0; w < writesPerSlot; w++ {
			k := key32(byte(d*writesPerSlot + w + 1))
			delta = append(delta, &Account{Key: k, Lamports: uint64(d + 1)})
		}
		tree.Commit(id, delta)
		parent = id
	}
	return tree, parent
}

// Worst case: key not present anywhere in the chain — full depth walk.
func BenchmarkBranchTreeGetMissDepth31(b *testing.B) {
	tree, tip := buildDeepChain(31, 8)
	missKey := key32(0xFF)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tree.Get(tip, missKey)
	}
}

// Hit at the far end (written in the oldest slot) — near-full walk.
func BenchmarkBranchTreeGetDeepHitDepth31(b *testing.B) {
	tree, tip := buildDeepChain(31, 8)
	oldKey := key32(1) // written in slot 1, 31 levels up
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tree.Get(tip, oldKey)
	}
}

// Baseline: one flat map lookup (what a resolved hot index would cost).
func BenchmarkFlatMapLookup(b *testing.B) {
	m := make(map[[32]byte]*Account, 256)
	for i := 1; i <= 248; i++ {
		k := key32(byte(i))
		m[k] = &Account{Key: k}
	}
	missKey := key32(0xFF)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m[missKey]
	}
}
