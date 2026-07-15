package global

import (
	"sync"

	"github.com/gagliardetto/solana-go"
)

var (
	alpenglowChainedRootsMu sync.RWMutex
	alpenglowChainedRoots   = make(map[uint64]solana.Hash)
)

// SetAlpenglowChainedMerkleRoot records the last data-shred merkle root for a slot.
// Child slots embed this value as the chained merkle root in their first FEC batch.
func SetAlpenglowChainedMerkleRoot(slot uint64, root solana.Hash) {
	if root == (solana.Hash{}) {
		return
	}
	alpenglowChainedRootsMu.Lock()
	defer alpenglowChainedRootsMu.Unlock()
	alpenglowChainedRoots[slot] = root
}

// AlpenglowChainedMerkleRoot returns the recorded last-shred merkle root for slot, if known.
func AlpenglowChainedMerkleRoot(slot uint64) (solana.Hash, bool) {
	alpenglowChainedRootsMu.RLock()
	defer alpenglowChainedRootsMu.RUnlock()
	root, ok := alpenglowChainedRoots[slot]
	if !ok || root == (solana.Hash{}) {
		return solana.Hash{}, false
	}
	return root, ok
}

// PruneAlpenglowChainedRootsBefore releases metadata older than the retained
// root. The root itself is kept because it can still parent the next block.
func PruneAlpenglowChainedRootsBefore(root uint64) {
	alpenglowChainedRootsMu.Lock()
	defer alpenglowChainedRootsMu.Unlock()
	for slot := range alpenglowChainedRoots {
		if slot < root {
			delete(alpenglowChainedRoots, slot)
		}
	}
}

// DeleteAlpenglowChainedRootsFrom removes parent-root metadata belonging to a
// discarded speculative suffix during an in-process fork switch.
func DeleteAlpenglowChainedRootsFrom(from uint64) {
	alpenglowChainedRootsMu.Lock()
	defer alpenglowChainedRootsMu.Unlock()
	for slot := range alpenglowChainedRoots {
		if slot >= from {
			delete(alpenglowChainedRoots, slot)
		}
	}
}
