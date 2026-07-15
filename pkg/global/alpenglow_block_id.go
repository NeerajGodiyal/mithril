package global

import (
	"sync"

	"github.com/gagliardetto/solana-go"
)

var (
	alpenglowBlockIDsMu sync.RWMutex
	alpenglowBlockIDs   = make(map[uint64]solana.Hash)
)

// SetAlpenglowBlockID records the canonical Alpenglow double-merkle block ID for a slot.
func SetAlpenglowBlockID(slot uint64, blockID solana.Hash) {
	if blockID == (solana.Hash{}) {
		return
	}
	alpenglowBlockIDsMu.Lock()
	defer alpenglowBlockIDsMu.Unlock()
	alpenglowBlockIDs[slot] = blockID
}

// AlpenglowBlockIDCount returns the number of recorded Alpenglow block IDs.
func AlpenglowBlockIDCount() int {
	alpenglowBlockIDsMu.RLock()
	defer alpenglowBlockIDsMu.RUnlock()
	return len(alpenglowBlockIDs)
}

// AlpenglowBlockID returns the recorded Alpenglow block ID for slot, if known.
func AlpenglowBlockID(slot uint64) (solana.Hash, bool) {
	alpenglowBlockIDsMu.RLock()
	defer alpenglowBlockIDsMu.RUnlock()
	id, ok := alpenglowBlockIDs[slot]
	if !ok || id == (solana.Hash{}) {
		return solana.Hash{}, false
	}
	return id, ok
}

// PruneAlpenglowBlockIDsBefore releases metadata older than the retained root.
// The root itself is kept because it can still be the parent of the next block.
func PruneAlpenglowBlockIDsBefore(root uint64) {
	alpenglowBlockIDsMu.Lock()
	defer alpenglowBlockIDsMu.Unlock()
	for slot := range alpenglowBlockIDs {
		if slot < root {
			delete(alpenglowBlockIDs, slot)
		}
	}
}

// DeleteAlpenglowBlockIDsFrom removes a discarded speculative suffix during
// an in-process fork switch. Replay repopulates the selected branch as it runs.
func DeleteAlpenglowBlockIDsFrom(from uint64) {
	alpenglowBlockIDsMu.Lock()
	defer alpenglowBlockIDsMu.Unlock()
	for slot := range alpenglowBlockIDs {
		if slot >= from {
			delete(alpenglowBlockIDs, slot)
		}
	}
}

// ResetAlpenglowChainMetadata clears run-local block IDs and chained roots.
// Replay repopulates both from successfully executed blocks.
func ResetAlpenglowChainMetadata() {
	alpenglowBlockIDsMu.Lock()
	clear(alpenglowBlockIDs)
	alpenglowBlockIDsMu.Unlock()

	alpenglowChainedRootsMu.Lock()
	clear(alpenglowChainedRoots)
	alpenglowChainedRootsMu.Unlock()
}
