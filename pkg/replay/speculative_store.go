package replay

import (
	"fmt"
	"sync"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
)

const maxSpeculativeLayers = 64

// SpeculativeLayer holds post-slot account state for keys modified during speculative execution.
type SpeculativeLayer struct {
	Slot       uint64
	ParentSlot uint64
	Deltas     map[solana.PublicKey]*accounts.Account
}

// SpeculativeStore resolves account state by walking parent layers back to the finalized slot.
type SpeculativeStore struct {
	mu             sync.RWMutex
	finalizedSlot  uint64
	layers         map[uint64]*SpeculativeLayer
}

func newSpeculativeStore() *SpeculativeStore {
	return &SpeculativeStore{
		layers: make(map[uint64]*SpeculativeLayer),
	}
}

func (st *SpeculativeStore) FinalizedSlot() uint64 {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.finalizedSlot
}

func (st *SpeculativeStore) SetFinalizedSlot(slot uint64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.finalizedSlot = slot
}

func (st *SpeculativeStore) UseStoreForParent(parentSlot uint64) bool {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return parentSlot > st.finalizedSlot
}

func (st *SpeculativeStore) LayerCount() int {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return len(st.layers)
}

// Resolve returns the account state at the end of endSlot by walking local layers to finalizedSlot.
func (st *SpeculativeStore) Resolve(endSlot uint64, pk solana.PublicKey, db *accountsdb.AccountsDb) (*accounts.Account, error) {
	st.mu.RLock()
	finalized := st.finalizedSlot
	st.mu.RUnlock()

	if endSlot <= finalized {
		if db == nil {
			return nil, fmt.Errorf("speculative store: nil accounts db")
		}
		return db.GetAccount(endSlot, pk)
	}

	st.mu.RLock()
	defer st.mu.RUnlock()

	slot := endSlot
	for slot > finalized {
		layer, ok := st.layers[slot]
		if !ok {
			return nil, fmt.Errorf("speculative store: missing layer for slot %d while resolving %s", slot, pk)
		}
		if acct, ok := layer.Deltas[pk]; ok {
			return acct.Clone(), nil
		}
		slot = layer.ParentSlot
	}
	if db == nil {
		return nil, fmt.Errorf("speculative store: nil accounts db")
	}
	return db.GetAccount(finalized, pk)
}

// RecordLayer stores post-slot account state for deferred replay. snapshotAccts is the
// end-of-slot bank-hash account set (includes sysvars updated outside ModifiedAccts).
func (st *SpeculativeStore) RecordLayer(slot, parentSlot uint64, slotCtx *sealevel.SlotCtx, snapshotAccts []*accounts.Account) error {
	if slotCtx == nil {
		return fmt.Errorf("speculative store: nil slot context for slot %d", slot)
	}
	if slotCtx.Slot != slot {
		return fmt.Errorf("speculative store: slot context slot %d != layer slot %d", slotCtx.Slot, slot)
	}

	layer := &SpeculativeLayer{
		Slot:       slot,
		ParentSlot: parentSlot,
		Deltas:     make(map[solana.PublicKey]*accounts.Account),
	}

	for _, acct := range snapshotAccts {
		if acct == nil {
			continue
		}
		layer.Deltas[acct.Key] = acct.Clone()
	}

	slotCtx.AcctMapsMu.Lock()
	modified := make([]solana.PublicKey, 0, len(slotCtx.ModifiedAccts))
	for pk := range slotCtx.ModifiedAccts {
		if _, ok := layer.Deltas[pk]; ok {
			continue
		}
		modified = append(modified, pk)
	}
	slotCtx.AcctMapsMu.Unlock()

	for _, pk := range modified {
		acct, err := slotCtx.GetAccount(pk)
		if err != nil {
			return fmt.Errorf("speculative store: record layer slot %d: %w", slot, err)
		}
		layer.Deltas[pk] = acct.Clone()
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.layers) >= maxSpeculativeLayers && st.layers[slot] == nil {
		return fmt.Errorf("speculative store: layer limit %d exceeded", maxSpeculativeLayers)
	}
	st.layers[slot] = layer
	return nil
}

func (st *SpeculativeStore) PruneLayersAbove(anchorSlot uint64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for slot := range st.layers {
		if slot > anchorSlot {
			delete(st.layers, slot)
		}
	}
}

func (st *SpeculativeStore) PruneLayersThrough(committedSlot uint64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for slot := range st.layers {
		if slot <= committedSlot {
			delete(st.layers, slot)
		}
	}
}

func (st *SpeculativeStore) Clear() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.layers = make(map[uint64]*SpeculativeLayer)
}
