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
