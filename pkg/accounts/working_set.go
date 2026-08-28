package accounts

import (
	"sync"

	"github.com/gagliardetto/solana-go"
)

// WorkingSet is the canonical timeline's mutable suffix: confirmed-but-unrooted
// slot writes buffered in RAM over the durable rooted store. Reads hit the
// flat map (newest unrooted value per key, O(1)); the per-slot undo journal
// makes suffix eviction — the execute-on-receipt fork switch — O(evicted
// writes) instead of a full rescan. Siblings are never materialized as state:
// a switch unwinds this suffix and re-executes the certified block.
type WorkingSet struct {
	mu     sync.RWMutex
	bySlot map[uint64]*slotLayer
	order  []uint64               // held slots, ascending
	flat   map[[32]byte]flatEntry // newest unrooted value per key
}

type flatEntry struct {
	slot uint64 // owner slot of acct (load-bearing for undo application)
	acct *Account
}

type slotLayer struct {
	slot   uint64
	writes map[[32]byte]*Account
	// undo records, one per FIRST write of a key in this slot: what the flat
	// entry pointed at before this slot overwrote it. Applying a suffix's
	// undos newest-layer-first restores the flat map exactly.
	undo []undoPtr
}

// WorkingSetView is an immutable bank view over retained slot layers. The
// layer maps are append-complete before publication and remain alive through
// this reference even if the mutable WorkingSet later promotes or unwinds them.
type WorkingSetView struct {
	layers []*slotLayer
}

type undoPtr struct {
	key      [32]byte
	prevSlot uint64 // meaningful when existed
	existed  bool   // false: the key had no unrooted value before this slot
}

// NewWorkingSet creates an empty suffix; reads compose Lookup over the durable
// store externally (see pkg/replay).
func NewWorkingSet() *WorkingSet {
	return &WorkingSet{
		bySlot: make(map[uint64]*slotLayer),
		flat:   make(map[[32]byte]flatEntry),
	}
}

// Add appends slot's account writes at the tip, capturing one undo record per
// first-written key. Slots must arrive in strictly ascending order (one
// confirmed chain). A fork replacement must evict the old suffix first.
func (w *WorkingSet) Add(slot uint64, delta []*Account) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.order) != 0 && slot <= w.order[len(w.order)-1] {
		panic("working set slots must be added in strictly ascending order")
	}
	layer := &slotLayer{slot: slot, writes: make(map[[32]byte]*Account, len(delta))}
	w.bySlot[slot] = layer
	w.order = append(w.order, slot)
	for _, a := range delta {
		if a == nil {
			continue
		}
		key := [32]byte(a.Key)
		if _, again := layer.writes[key]; !again {
			// First write of this key in this slot: journal what flat held.
			if e, exists := w.flat[key]; exists {
				layer.undo = append(layer.undo, undoPtr{key: key, prevSlot: e.slot, existed: true})
			} else {
				layer.undo = append(layer.undo, undoPtr{key: key})
			}
		}
		layer.writes[key] = a
		w.flat[key] = flatEntry{slot: slot, acct: a}
	}
}

// HeldSlots reports the number of buffered unrooted slots (for RAM bounding).
func (w *WorkingSet) HeldSlots() int {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return len(w.order)
}

// Lookup returns the newest unrooted value for pubkey (nil, false if none held).
// No fall-through to durable; the newest held value is the correct pre-root value.
func (w *WorkingSet) Lookup(pubkey [32]byte) (*Account, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if e, ok := w.flat[pubkey]; ok {
		return e.acct, true
	}
	return nil, false
}

// LookupAt returns the newest held value written at or before slot. It keeps a
// captured bank from observing a later unrooted write published concurrently.
func (w *WorkingSet) LookupAt(slot uint64, pubkey [32]byte) (*Account, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.lookupAtLocked(slot, pubkey)
}

// ViewAt captures the held layers visible to slot without copying account
// maps. It is safe to retain across later Add, PromotePrefix, or EvictFrom.
func (w *WorkingSet) ViewAt(slot uint64) *WorkingSetView {
	w.mu.RLock()
	defer w.mu.RUnlock()
	layers := make([]*slotLayer, 0, len(w.order))
	for _, layerSlot := range w.order {
		if layerSlot > slot {
			break
		}
		layers = append(layers, w.bySlot[layerSlot])
	}
	return &WorkingSetView{layers: layers}
}

// Lookup returns the newest value retained by this exact bank view.
func (v *WorkingSetView) Lookup(pubkey [32]byte) (*Account, bool) {
	if v == nil {
		return nil, false
	}
	for i := len(v.layers) - 1; i >= 0; i-- {
		if acct, ok := v.layers[i].writes[pubkey]; ok {
			return acct, true
		}
	}
	return nil, false
}

func (w *WorkingSet) lookupAtLocked(slot uint64, pubkey [32]byte) (*Account, bool) {
	if e, ok := w.flat[pubkey]; ok && e.slot <= slot {
		return e.acct, true
	}
	for i := len(w.order) - 1; i >= 0; i-- {
		layerSlot := w.order[i]
		if layerSlot > slot {
			continue
		}
		if acct, ok := w.bySlot[layerSlot].writes[pubkey]; ok {
			return acct, true
		}
	}
	return nil, false
}

// LookupBatch fills out with the newest unrooted value for every matching
// pubkey. Misses are left nil. The returned accounts are retained immutable
// WorkingSet values; callers must copy-on-write before mutation.
//
// A block may contain tens of thousands of unique keys. Holding one read lock
// for the whole lookup avoids two atomic lock operations per key while still
// giving the caller a coherent view of the speculative suffix.
func (w *WorkingSet) LookupBatch(pubkeys []solana.PublicKey, out []*Account) {
	if len(pubkeys) != len(out) {
		panic("working set batch lookup length mismatch")
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	for i, pubkey := range pubkeys {
		if e, ok := w.flat[[32]byte(pubkey)]; ok {
			out[i] = e.acct
		}
	}
}

// LookupBatchAt is LookupAt under one read lock for a captured bank.
func (w *WorkingSet) LookupBatchAt(slot uint64, pubkeys []solana.PublicKey, out []*Account) {
	if len(pubkeys) != len(out) {
		panic("working set batch lookup length mismatch")
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	for i, pubkey := range pubkeys {
		out[i], _ = w.lookupAtLocked(slot, [32]byte(pubkey))
	}
}

// SlotDelta is one held slot's account writes, returned for durable promotion.
type SlotDelta struct {
	Slot  uint64
	Delta []*Account
}

// PromotionPrefix returns held slots <= through (ascending) with their writes,
// to durably commit before PromotePrefix(through). Values reference the stored
// accounts.
func (w *WorkingSet) PromotionPrefix(through uint64) []SlotDelta {
	w.mu.RLock()
	defer w.mu.RUnlock()

	var batch []SlotDelta
	for _, slot := range w.order { // ascending
		if slot > through {
			break
		}
		layer := w.bySlot[slot]
		delta := make([]*Account, 0, len(layer.writes))
		for _, a := range layer.writes {
			delta = append(delta, a)
		}
		batch = append(batch, SlotDelta{Slot: slot, Delta: delta})
	}
	return batch
}

// PromotePrefix drops the rooted prefix (held slots <= through); keys with no
// newer held writer fall through to durable. Caller MUST make them durable
// BEFORE this.
func (w *WorkingSet) PromotePrefix(through uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()

	kept := make([]uint64, 0, len(w.order))
	for _, slot := range w.order {
		if slot > through {
			kept = append(kept, slot)
			continue
		}
		for key := range w.bySlot[slot].writes {
			if e, ok := w.flat[key]; ok && e.slot <= through {
				delete(w.flat, key) // no surviving held writer -> durable
			}
		}
		delete(w.bySlot, slot)
	}
	w.order = kept
	// Undo records pointing into the promoted prefix now dangle; surviving
	// layers' undos are rewritten to "restore = fall through to durable",
	// which is exactly right: the promoted value IS the durable value.
	for _, slot := range w.order {
		layer := w.bySlot[slot]
		for i := range layer.undo {
			if layer.undo[i].existed && layer.undo[i].prevSlot <= through {
				layer.undo[i].existed = false
			}
		}
	}
}

// EvictFrom drops the abandoned suffix (held slots >= slot) — the fork-switch
// unwind. Undo journals apply newest-layer-first, restoring each affected key
// to its newest surviving value (or removing it so reads fall through to
// durable). Cost is O(evicted writes).
func (w *WorkingSet) EvictFrom(slot uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Identify the suffix (order is ascending).
	cut := len(w.order)
	for i, s := range w.order {
		if s >= slot {
			cut = i
			break
		}
	}
	suffix := w.order[cut:]
	if len(suffix) == 0 {
		return
	}

	// Apply undos newest layer first: for keys written in several evicted
	// layers, the OLDEST layer's undo applies last and wins — restoring the
	// pre-suffix state exactly.
	for i := len(suffix) - 1; i >= 0; i-- {
		layer := w.bySlot[suffix[i]]
		for _, u := range layer.undo {
			if !u.existed {
				delete(w.flat, u.key)
				continue
			}
			if prev, held := w.bySlot[u.prevSlot]; held && u.prevSlot < slot {
				if acct, ok := prev.writes[u.key]; ok {
					w.flat[u.key] = flatEntry{slot: u.prevSlot, acct: acct}
					continue
				}
			}
			// Previous writer already promoted (or missing): durable owns it.
			delete(w.flat, u.key)
		}
	}
	for _, s := range suffix {
		delete(w.bySlot, s)
	}
	w.order = w.order[:cut]
}

// CheckInvariants verifies flat ≡ fold(bySlot in order) — test hook (I6).
func (w *WorkingSet) CheckInvariants() error {
	w.mu.RLock()
	defer w.mu.RUnlock()

	want := make(map[[32]byte]flatEntry)
	for _, slot := range w.order {
		for key, acct := range w.bySlot[slot].writes {
			want[key] = flatEntry{slot: slot, acct: acct}
		}
	}
	if len(want) != len(w.flat) {
		return errInvariant("flat size", len(w.flat), len(want))
	}
	for key, e := range want {
		got, ok := w.flat[key]
		if !ok || got.slot != e.slot || got.acct != e.acct {
			return errInvariant("flat entry", got, e)
		}
	}
	return nil
}

type invariantError struct {
	what string
	got  any
	want any
}

func (e *invariantError) Error() string {
	return "working set invariant violated: " + e.what
}

func errInvariant(what string, got, want any) error {
	return &invariantError{what: what, got: got, want: want}
}
