package accounts

import "sync"

// UnrootedOverlay buffers confirmed-but-unrooted slot writes in RAM over a durable
// rooted store; reads prefer the newest unrooted write, else fall through to durable.
type UnrootedOverlay struct {
	mu     sync.RWMutex
	layers map[uint64]map[[32]byte]*Account // slot -> that slot's writes
	order  []uint64                         // held slots, ascending
	flat   map[[32]byte]flatEntry           // newest unrooted value per key
}

type flatEntry struct {
	slot uint64 // owner slot of acct (load-bearing for recompute)
	acct *Account
}

// NewUnrootedOverlay creates an empty unrooted tail holding only in-RAM layers;
// reads compose Lookup over the durable store externally (see pkg/replay).
func NewUnrootedOverlay() *UnrootedOverlay {
	return &UnrootedOverlay{
		layers: make(map[uint64]map[[32]byte]*Account),
		flat:   make(map[[32]byte]flatEntry),
	}
}

// Add appends slot's account writes at the tip. Slots must arrive in ascending
// order (one confirmed fork), so an added slot is always >= every held slot.
func (u *UnrootedOverlay) Add(slot uint64, delta []*Account) {
	u.mu.Lock()
	defer u.mu.Unlock()

	layer, ok := u.layers[slot]
	if !ok {
		layer = make(map[[32]byte]*Account, len(delta))
		u.layers[slot] = layer
		u.order = append(u.order, slot)
	}
	for _, a := range delta {
		if a == nil {
			continue
		}
		key := [32]byte(a.Key)
		layer[key] = a
		// Newest wins. Guard on owner slot so an out-of-order add can never
		// install an older value over a newer one.
		if e, exists := u.flat[key]; !exists || slot >= e.slot {
			u.flat[key] = flatEntry{slot: slot, acct: a}
		}
	}
}

// HeldSlots reports the number of buffered unrooted slots (for RAM bounding).
func (u *UnrootedOverlay) HeldSlots() int {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return len(u.order)
}

// Lookup returns the newest unrooted value for pubkey (nil, false if none held).
// No fall-through to durable; the newest held value is the correct pre-root value.
func (u *UnrootedOverlay) Lookup(pubkey [32]byte) (*Account, bool) {
	u.mu.RLock()
	defer u.mu.RUnlock()
	if e, ok := u.flat[pubkey]; ok {
		return e.acct, true
	}
	return nil, false
}

// SlotDelta is one held slot's account writes, returned for durable promotion.
type SlotDelta struct {
	Slot  uint64
	Delta []*Account
}

// PromotionPrefix returns held slots <= through (ascending) with their writes, to
// durably commit before PromotePrefix(through). Values reference the stored accounts.
func (u *UnrootedOverlay) PromotionPrefix(through uint64) []SlotDelta {
	u.mu.RLock()
	defer u.mu.RUnlock()

	var batch []SlotDelta
	for _, slot := range u.order { // order is ascending
		if slot > through {
			break
		}
		layer := u.layers[slot]
		delta := make([]*Account, 0, len(layer))
		for _, a := range layer {
			delta = append(delta, a)
		}
		batch = append(batch, SlotDelta{Slot: slot, Delta: delta})
	}
	return batch
}

// PromotePrefix drops the rooted prefix (held slots <= through); keys with no newer
// held writer fall through to durable. Caller MUST make them durable BEFORE this.
func (u *UnrootedOverlay) PromotePrefix(through uint64) {
	u.mu.Lock()
	defer u.mu.Unlock()

	kept := make([]uint64, 0, len(u.order))
	for _, slot := range u.order {
		if slot > through {
			kept = append(kept, slot)
			continue
		}
		for key := range u.layers[slot] {
			if e, ok := u.flat[key]; ok && e.slot <= through {
				delete(u.flat, key) // no surviving held writer -> fall through to durable
			}
		}
		delete(u.layers, slot)
	}
	u.order = kept
}

// EvictFrom drops the abandoned suffix (held slots >= slot) on a reorg, reverting
// affected keys to their newest surviving value. Multi-branch (#14) only.
func (u *UnrootedOverlay) EvictFrom(slot uint64) {
	u.mu.Lock()
	defer u.mu.Unlock()

	kept := make([]uint64, 0, len(u.order))
	var removedKeys [][32]byte
	for _, s := range u.order {
		if s < slot {
			kept = append(kept, s)
			continue
		}
		for key := range u.layers[s] {
			removedKeys = append(removedKeys, key)
		}
		delete(u.layers, s)
	}
	u.order = kept

	// Revert keys whose newest writer was evicted (ownerSlot >= slot). The guard
	// dedups keys in several removed layers: first recompute drops ownerSlot, rest skip.
	for _, key := range removedKeys {
		if e, ok := u.flat[key]; ok && e.slot >= slot {
			u.recomputeLocked(key)
		}
	}
}

// recomputeLocked rescans kept layers newest-first for the surviving newest value
// for key, or removes it so reads fall through to durable. Caller holds u.mu.
func (u *UnrootedOverlay) recomputeLocked(key [32]byte) {
	for i := len(u.order) - 1; i >= 0; i-- {
		if acc, ok := u.layers[u.order[i]][key]; ok {
			u.flat[key] = flatEntry{slot: u.order[i], acct: acc}
			return
		}
	}
	delete(u.flat, key)
}
