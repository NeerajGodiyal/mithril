package replay

import (
	"context"
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/state"
	"github.com/gagliardetto/solana-go"
)

// slotBlock identifies a specific block (fork) at a slot, for ingestion + equivocation.
type slotBlock struct {
	slot    uint64
	blockID [32]byte
}

// branchMeta is the per-branch replay context the BranchTree doesn't hold: the slot's
// bankhash and its as-of-slot resume context (deep-copied, no SysvarCache aliasing).
type branchMeta struct {
	bankhash [32]byte
	ctx      *state.ResumeContext
}

// forkCoordinator drives the multi-branch fork engine: an in-RAM BranchTree over the
// durable rooted store, per-branch bankhash/context, a (slot,blockID) index for fork
// ingestion + equivocation, and finality-gated two-phase promotion of the winner. It
// is the multi-branch generalization of unrootedTail (the linear single-branch case).
type forkCoordinator struct {
	tree      *accounts.BranchTree
	durable   blockAccountSource
	committer slotCommitter
	meta      map[uint64]*branchMeta // branchID -> bankhash + resume context
	index     map[slotBlock]uint64   // (slot,blockID) -> branchID
	haltCap   int
}

func newForkCoordinator(durable blockAccountSource, committer slotCommitter, haltCap int) *forkCoordinator {
	return &forkCoordinator{
		tree:      accounts.NewBranchTree(),
		durable:   durable,
		committer: committer,
		meta:      make(map[uint64]*branchMeta),
		index:     make(map[slotBlock]uint64),
		haltCap:   haltCap,
	}
}

// Ingest registers a block at slot/blockID extending parentBranchID (0 = over durable),
// returning its branch id. A repeat (slot,blockID) returns the existing id (idempotent);
// a different blockID at the same slot is a competing fork tracked as its own branch.
// Returns (0,false) if parentBranchID is non-zero but unknown.
func (fc *forkCoordinator) Ingest(parentBranchID, slot uint64, blockID [32]byte) (uint64, bool) {
	sb := slotBlock{slot: slot, blockID: blockID}
	if id, ok := fc.index[sb]; ok {
		return id, true
	}
	id, ok := fc.tree.AddBranch(parentBranchID, slot, blockID)
	if !ok {
		return 0, false
	}
	fc.index[sb] = id
	return id, true
}

// BranchIDAt returns the branch id for a (slot, blockID), so the replay loop can find
// the parent branch to extend or resolve a finalized block to its branch.
func (fc *forkCoordinator) BranchIDAt(slot uint64, blockID [32]byte) (uint64, bool) {
	id, ok := fc.index[slotBlock{slot: slot, blockID: blockID}]
	return id, ok
}

// GetAccount resolves pubkey on branchID: nearest-ancestor branch overlay, else durable.
// Reads are valid only for the branch owned by the current serial execution epoch —
// the caller must not evict/promote a branch that has in-flight readers.
func (fc *forkCoordinator) GetAccount(branchID, slot uint64, pubkey solana.PublicKey) (*accounts.Account, error) {
	if a, ok := fc.tree.Get(branchID, [32]byte(pubkey)); ok {
		return a, nil
	}
	return fc.durable.GetAccount(slot, pubkey)
}

// GetAccountsBatch resolves each key against branchID's overlay chain, falling through to
// a single durable batch for the misses, preserving order and placeholder semantics.
func (fc *forkCoordinator) GetAccountsBatch(ctx context.Context, branchID, slot uint64, pks []solana.PublicKey) ([]*accounts.Account, error) {
	return batchOverDurable(ctx, slot, pks, fc.durable, func(pk solana.PublicKey) (*accounts.Account, bool) {
		return fc.tree.Get(branchID, [32]byte(pk))
	})
}

// Commit installs a replayed slot's writes on its branch and records the branch's
// bankhash + resume context. ctx MUST be deep-copied (no SysvarCache pointers).
func (fc *forkCoordinator) Commit(branchID uint64, delta []*accounts.Account, bankhash []byte, ctx *state.ResumeContext) {
	fc.tree.Commit(branchID, delta)
	var bh [32]byte
	copy(bh[:], bankhash)
	fc.meta[branchID] = &branchMeta{bankhash: bh, ctx: ctx}
}

// Promote folds the finalized branch's chain into durable then drops it and all
// non-descendant branches from RAM (two-phase: durable-commit BEFORE tree drop, so a
// re-rooted survivor never reads a not-yet-updated base). Returns the highest slot made
// durable and the finalized branch's resume context. On a commit error it stops, leaves
// the tree intact, and returns the error (retry is idempotent via the redo log).
func (fc *forkCoordinator) Promote(finalizedBranchID uint64) (uint64, *state.ResumeContext, error) {
	chain := fc.tree.PromotionChain(finalizedBranchID)
	if len(chain) == 0 {
		return 0, nil, nil
	}
	// Every branch in the winning chain must have been committed (bankhash recorded).
	// Pre-validate so a missing one is caught before any durable write, rather than
	// persisting an empty bankhash + dropping the slot's state. A skipped slot is still
	// Committed (empty delta + its bankhash), so it passes.
	for _, ps := range chain {
		if fc.meta[ps.BranchID] == nil {
			return 0, nil, fmt.Errorf("cannot promote: branch %d (slot %d) not committed", ps.BranchID, ps.Slot)
		}
	}
	var promotedThrough, lastDurableBranch uint64
	for _, ps := range chain {
		if err := fc.committer.CommitRootedSlot(ps.Delta, ps.Slot, fc.meta[ps.BranchID].bankhash[:]); err != nil {
			// Partial failure: return the last durable slot's context (matches the
			// linear engine) so the caller's watermark and resume context stay paired.
			var ctx *state.ResumeContext
			if lastDurableBranch != 0 {
				ctx = fc.meta[lastDurableBranch].ctx
			}
			return promotedThrough, ctx, fmt.Errorf("promote slot %d: %w", ps.Slot, err)
		}
		promotedThrough = ps.Slot
		lastDurableBranch = ps.BranchID
	}

	ctx := fc.meta[finalizedBranchID].ctx
	fc.tree.Promote(finalizedBranchID)
	fc.pruneToLive()
	return promotedThrough, ctx, nil
}

// SetContext attaches/updates the resume context of a committed branch. ctx MUST be
// deep-copied (no SysvarCache pointers). No-op for an unknown/uncommitted branch.
func (fc *forkCoordinator) SetContext(branchID uint64, ctx *state.ResumeContext) {
	if m := fc.meta[branchID]; m != nil && ctx != nil {
		m.ctx = ctx
	}
}

// Evict drops a losing fork (branchID + descendants) from the tree and side maps.
func (fc *forkCoordinator) Evict(branchID uint64) {
	fc.tree.EvictSubtree(branchID)
	fc.pruneToLive()
}

// pruneToLive drops meta/index entries for branches no longer in the tree.
func (fc *forkCoordinator) pruneToLive() {
	live := fc.tree.LiveIDs()
	for id := range fc.meta {
		if !live[id] {
			delete(fc.meta, id)
		}
	}
	for sb, id := range fc.index {
		if !live[id] {
			delete(fc.index, sb)
		}
	}
}

// OverCap reports whether the live branch count exceeds the halt cap (fork fan-out or
// stalled finality growing RAM unbounded). Interim safety valve bounding branch COUNT;
// the depth×writes bound is P6 (bounded resource policy).
func (fc *forkCoordinator) OverCap() bool {
	return fc.haltCap > 0 && fc.tree.Len() > fc.haltCap
}
