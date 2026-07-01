package replay

import (
	"context"
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/state"
	"github.com/gagliardetto/solana-go"
)

// unrootedTailHaltCap bounds the in-RAM unrooted tail; replay halts if held slots
// exceed it rather than growing RAM unbounded (~16x normal rooting lag).
const unrootedTailHaltCap = 512

// slotCommitter durably folds one rooted slot into the canonical store,
// finalizing its crash-safe commit. Satisfied by AccountsDb.CommitRootedSlot.
type slotCommitter interface {
	CommitRootedSlot(accts []*accounts.Account, slot uint64, bankhash []byte) error
}

// blockAccountSource is the slot-scoped read API the block loader needs; both
// AccountsDb and unrootedTail satisfy it, so the loader is mode-agnostic.
type blockAccountSource interface {
	GetAccount(slot uint64, pubkey solana.PublicKey) (*accounts.Account, error)
	GetAccountsBatch(ctx context.Context, slot uint64, pks []solana.PublicKey) ([]*accounts.Account, error)
}

// unrootedTail layers an in-RAM UnrootedOverlay over the durable store: reads
// resolve overlay→durable, commits buffer until rooted slots promote out.
type unrootedTail struct {
	overlay    *accounts.UnrootedOverlay
	durable    blockAccountSource // the canonical rooted store (for read fall-through)
	committer  slotCommitter      // durable promotion of rooted slots
	bankhashes map[uint64][32]byte
	// contexts holds the deep-copied end-of-slot resume context per held slot,
	// retained until promotion so the context as of the last rooted slot survives for resume.
	contexts map[uint64]*state.ResumeContext
	haltCap  int // halt replay if held slots exceed this (rooting stalled)
}

func newUnrootedTail(durable blockAccountSource, committer slotCommitter, haltCap int) *unrootedTail {
	return &unrootedTail{
		overlay:    accounts.NewUnrootedOverlay(),
		durable:    durable,
		committer:  committer,
		bankhashes: make(map[uint64][32]byte),
		contexts:   make(map[uint64]*state.ResumeContext),
		haltCap:    haltCap,
	}
}

// GetAccount resolves the newest unrooted write for pubkey, else the durable
// (rooted) value read at slot.
func (t *unrootedTail) GetAccount(slot uint64, pubkey solana.PublicKey) (*accounts.Account, error) {
	if a, ok := t.overlay.Lookup([32]byte(pubkey)); ok {
		return a, nil
	}
	return t.durable.GetAccount(slot, pubkey)
}

// GetAccountsBatch returns one entry per requested key, in order, preferring the
// unrooted value and falling through to a single durable batch for the misses.
func (t *unrootedTail) GetAccountsBatch(ctx context.Context, slot uint64, pks []solana.PublicKey) ([]*accounts.Account, error) {
	if len(pks) == 0 {
		return nil, nil
	}
	out := make([]*accounts.Account, len(pks))
	var misses []solana.PublicKey
	var missIdx []int
	for i, pk := range pks {
		if a, ok := t.overlay.Lookup([32]byte(pk)); ok {
			out[i] = a
		} else {
			misses = append(misses, pk)
			missIdx = append(missIdx, i)
		}
	}
	if len(misses) > 0 {
		loaded, err := t.durable.GetAccountsBatch(ctx, slot, misses)
		if err != nil {
			return nil, err
		}
		// Durable returns one entry per requested key; guard so a contract
		// violation surfaces as an error, not an index panic.
		if len(loaded) != len(misses) {
			return nil, fmt.Errorf("durable GetAccountsBatch returned %d accounts for %d keys at slot %d", len(loaded), len(misses), slot)
		}
		for j, a := range loaded {
			out[missIdx[j]] = a
		}
	}
	return out, nil
}

// Add buffers a replayed slot's writes + bankhash in the overlay; it becomes
// durable only via promote(). Resume context is attached separately (SetContext).
func (t *unrootedTail) Add(slot uint64, delta []*accounts.Account, bankhash []byte) {
	t.overlay.Add(slot, delta)
	var bh [32]byte
	copy(bh[:], bankhash)
	t.bankhashes[slot] = bh
}

// SetContext attaches a held slot's end-of-slot resume context. ctx MUST be
// deep-copied (no pointers into the global SysvarCache); retained until promotion.
func (t *unrootedTail) SetContext(slot uint64, ctx *state.ResumeContext) {
	if ctx != nil {
		t.contexts[slot] = ctx
	}
}

// promote durably commits + drops every held slot <= through. Returns the highest
// slot now durable and its resume context as of the last rooted slot (nil if none). See promoteRooted.
func (t *unrootedTail) promote(through uint64) (uint64, *state.ResumeContext, error) {
	promotedThrough, err := promoteRooted(t.overlay, through, t.bankhashes, t.committer)
	if promotedThrough == 0 {
		return 0, nil, err
	}
	ctx := t.contexts[promotedThrough] // context as of the last rooted slot (read before pruning)
	for s := range t.contexts {
		if s <= promotedThrough {
			delete(t.contexts, s)
		}
	}
	return promotedThrough, ctx, err
}

// OverCap reports whether the unrooted tail has grown past the halt cap, i.e.
// rooting has stalled and we must stop replay rather than grow RAM unbounded.
func (t *unrootedTail) OverCap() bool {
	return t.haltCap > 0 && t.overlay.HeldSlots() > t.haltCap
}

// promoteRooted commits held slots <= through (ascending), then drops them, folding
// the rooted prefix onto disk. Crash-safe: stops at the first commit error, promoting
// only through the last durable slot. Returns the highest durable slot (0 if none).
func promoteRooted(
	overlay *accounts.UnrootedOverlay,
	through uint64,
	bankhashes map[uint64][32]byte,
	committer slotCommitter,
) (promotedThrough uint64, err error) {
	batch := overlay.PromotionPrefix(through)
	if len(batch) == 0 {
		return 0, nil
	}

	for _, sd := range batch {
		bh := bankhashes[sd.Slot]
		if cerr := committer.CommitRootedSlot(sd.Delta, sd.Slot, bh[:]); cerr != nil {
			err = fmt.Errorf("promote slot %d: %w", sd.Slot, cerr)
			break
		}
		promotedThrough = sd.Slot
	}

	if promotedThrough > 0 {
		overlay.PromotePrefix(promotedThrough)
		for _, sd := range batch {
			if sd.Slot > promotedThrough {
				break
			}
			delete(bankhashes, sd.Slot)
		}
	}
	return promotedThrough, err
}
