package replay

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/state"
	"github.com/gagliardetto/solana-go"
)

// unrootedTailHaltCap bounds the in-RAM unrooted tail; replay halts if held slots
// exceed it rather than growing RAM unbounded (~16x normal rooting lag).
const unrootedTailHaltCap = 512

// batchCommitter durably folds a batch of rooted slots into the canonical
// store as one sequential segment (union-deduped, one fsync, atomic index
// flip). Satisfied by AccountsDb.CommitBatch.
type batchCommitter interface {
	CommitBatch(deltas []accounts.SlotDelta, throughSlot uint64, bankhashes map[uint64][32]byte, resumeCtx []byte) (accountsdb.BatchCommitResult, error)
}

// defaultFoldBatchSlots is the fold chunk size K when none is configured:
// rooted slots fold to disk K at a time (union-deduped), and a trailing
// partial chunk stays in RAM (bounded by K) until it fills or flush() runs.
const defaultFoldBatchSlots = 128

// FoldBatchSlots is the configured fold chunk size (storage.fold_batch_slots),
// set by node startup before ReplayBlocks. Zero uses the default. Larger K =
// better union dedupe and less NVMe wear, more RAM, longer crash re-execution.
var FoldBatchSlots = defaultFoldBatchSlots

// blockAccountSource is the slot-scoped read API the block loader needs; both
// AccountsDb and unrootedTail satisfy it, so the loader is mode-agnostic.
type blockAccountSource interface {
	GetAccount(slot uint64, pubkey solana.PublicKey) (*accounts.Account, error)
	GetAccountsBatch(ctx context.Context, slot uint64, pks []solana.PublicKey) ([]*accounts.Account, error)
}

// unrootedState is the in-RAM speculative-state engine the replay loop drives in
// rooted-durable mode: reads resolve speculative→durable, commits buffer in RAM,
// rooted slots promote to disk. Implemented by unrootedTail (linear) and forkTail
// (fork-aware, over forkCoordinator).
type unrootedState interface {
	blockAccountSource
	Add(slot uint64, delta []*accounts.Account, bankhash []byte)
	SetContext(slot uint64, ctx *state.ResumeContext)
	promote(through uint64) (uint64, *state.ResumeContext, error)
	// flush force-folds the trailing partial chunk <= through (graceful
	// shutdown), so restart re-execution is bounded by the fold batch size.
	flush(through uint64) (uint64, *state.ResumeContext, error)
	OverCap() bool
}

// unrootedTail layers an in-RAM UnrootedOverlay over the durable store: reads
// resolve overlay→durable, commits buffer until rooted slots promote out.
type unrootedTail struct {
	overlay    *accounts.WorkingSet
	durable    blockAccountSource // the canonical rooted store (for read fall-through)
	committer  batchCommitter     // durable promotion of rooted slot batches
	bankhashes map[uint64][32]byte
	batchSlots int // fold chunk size K
	// contexts holds the deep-copied end-of-slot resume context per held slot,
	// retained until promotion so the context as of the last rooted slot survives for resume.
	contexts map[uint64]*state.ResumeContext
	haltCap  int // halt replay if held slots exceed this (rooting stalled)
}

func newUnrootedTail(durable blockAccountSource, committer batchCommitter, haltCap int, batchSlots int) *unrootedTail {
	if batchSlots <= 0 {
		batchSlots = defaultFoldBatchSlots
	}
	return &unrootedTail{
		overlay:    accounts.NewWorkingSet(),
		durable:    durable,
		committer:  committer,
		bankhashes: make(map[uint64][32]byte),
		batchSlots: batchSlots,
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
	return batchOverDurable(ctx, slot, pks, t.durable, func(pk solana.PublicKey) (*accounts.Account, bool) {
		return t.overlay.Lookup([32]byte(pk))
	})
}

// batchOverDurable resolves each key via the speculative lookup, falling through to a
// single durable batch for the misses, preserving order and placeholder semantics.
func batchOverDurable(ctx context.Context, slot uint64, pks []solana.PublicKey, durable blockAccountSource, lookup func(solana.PublicKey) (*accounts.Account, bool)) ([]*accounts.Account, error) {
	if len(pks) == 0 {
		return nil, nil
	}
	out := make([]*accounts.Account, len(pks))
	var misses []solana.PublicKey
	var missIdx []int
	for i, pk := range pks {
		if a, ok := lookup(pk); ok {
			out[i] = a
		} else {
			misses = append(misses, pk)
			missIdx = append(missIdx, i)
		}
	}
	if len(misses) > 0 {
		loaded, err := durable.GetAccountsBatch(ctx, slot, misses)
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
	var slotBankhash [32]byte
	copy(slotBankhash[:], bankhash)
	t.bankhashes[slot] = slotBankhash
}

// SetContext attaches a held slot's end-of-slot resume context. ctx MUST be
// deep-copied (no pointers into the global SysvarCache); retained until promotion.
func (t *unrootedTail) SetContext(slot uint64, ctx *state.ResumeContext) {
	if ctx != nil {
		t.contexts[slot] = ctx
	}
}

// promote folds full K-slot chunks of the rooted prefix <= through to disk;
// a trailing partial chunk stays in RAM until it fills (or flush() forces it).
// Returns the highest slot now durable and its resume context (nil if none).
func (t *unrootedTail) promote(through uint64) (uint64, *state.ResumeContext, error) {
	return t.promoteChunked(through, false)
}

// flush force-folds everything <= through including the partial trailing
// chunk — the graceful-shutdown path, bounding restart re-execution.
func (t *unrootedTail) flush(through uint64) (uint64, *state.ResumeContext, error) {
	return t.promoteChunked(through, true)
}

func (t *unrootedTail) promoteChunked(through uint64, force bool) (uint64, *state.ResumeContext, error) {
	promotedThrough, err := promoteRootedBatched(t.overlay, through, t.bankhashes, t.contexts, t.committer, t.batchSlots, force)
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

// unwind drops all held slots >= fromSlot (the execute-on-receipt fork
// switch) and returns the retained resume context of the last surviving slot
// so the replay loop can rebuild execution state and re-run the certified
// version. Returns nil when no context for fromSlot-1 is retained (caller
// falls back to the rooted-checkpoint re-replay).
func (t *unrootedTail) unwind(fromSlot uint64) *state.ResumeContext {
	t.overlay.EvictFrom(fromSlot)
	for s := range t.bankhashes {
		if s >= fromSlot {
			delete(t.bankhashes, s)
		}
	}
	var ctx *state.ResumeContext
	for s, c := range t.contexts {
		if s >= fromSlot {
			delete(t.contexts, s)
			continue
		}
		if ctx == nil || s > ctx.Slot {
			ctx = c
		}
	}
	// ctx is the highest retained context with slot < fromSlot: the ACTUAL
	// executed parent. It need not be numerically fromSlot-1 — when slots between
	// it and fromSlot were skipped, they retain no context (only executed held
	// slots call SetContext), so the last executed slot IS the parent bank of the
	// certified block at fromSlot. Returning nil (parent already durably folded,
	// or nothing retained) routes the caller to the rooted-checkpoint fallback.
	return ctx
}

// OverCap reports whether the unrooted tail has grown past the halt cap, i.e.
// rooting has stalled and we must stop replay rather than grow RAM unbounded.
func (t *unrootedTail) OverCap() bool {
	return t.haltCap > 0 && t.overlay.HeldSlots() > t.haltCap
}

// promoteRootedBatched folds held slots <= through in chunks of batchSlots.
// Each chunk = one CommitBatch (one segment + one fsync + one index flip),
// then the chunk drops from RAM. A trailing partial chunk folds only when
// force is set; otherwise it stays in RAM, so restart re-execution from the
// blockstore is bounded by the chunk size. Crash-safe: an error stops at the
// last fully durable chunk boundary (a failed chunk leaves only an orphan
// segment that recovery GCs).
// safePromoteTarget is the dual-watermark fold target: certificate finality
// clamped by the trailing-verification watermark (only when the verifier is
// required) and never at or beyond a persisted-divergence floor. Promotion is
// driven by this target exceeding the durable watermark, so verified progress
// alone can advance it even when certificate finality is momentarily flat.
func safePromoteTarget(finality uint64, verifierRequired bool, verifiedWatermark, divergenceFloor uint64) uint64 {
	target := finality
	if verifierRequired && verifiedWatermark < target {
		target = verifiedWatermark
	}
	if divergenceFloor > 0 && target >= divergenceFloor {
		target = divergenceFloor - 1
	}
	return target
}

func promoteRootedBatched(
	overlay *accounts.WorkingSet,
	through uint64,
	bankhashes map[uint64][32]byte,
	contexts map[uint64]*state.ResumeContext,
	committer batchCommitter,
	batchSlots int,
	force bool,
) (promotedThrough uint64, err error) {
	prefix := overlay.PromotionPrefix(through)
	if len(prefix) == 0 {
		return 0, nil
	}

	for start := 0; start < len(prefix); start += batchSlots {
		end := start + batchSlots
		if end > len(prefix) {
			if !force {
				break // trailing partial chunk stays in RAM
			}
			end = len(prefix)
		}
		chunk := prefix[start:end]
		chunkThrough := chunk[len(chunk)-1].Slot

		chunkBankhashes := make(map[uint64][32]byte, len(chunk))
		for _, sd := range chunk {
			if bh, ok := bankhashes[sd.Slot]; ok {
				chunkBankhashes[sd.Slot] = bh
			}
		}

		// The chunk-top resume context rides in the manifest so the durable
		// watermark + context survive a hard crash without the state file. A
		// missing or unmarshalable context would produce a manifest that
		// recovery cannot resume from — it fatals when the store outruns the
		// state file. Fail the fold here instead of committing an unrecoverable
		// batch; the caller holds the watermark and the slots stay in RAM.
		ctx := contexts[chunkThrough]
		if ctx == nil {
			err = fmt.Errorf("promote chunk through slot %d: no resume context recorded for chunk-top slot", chunkThrough)
			break
		}
		ctxJSON, merr := json.Marshal(ctx)
		if merr != nil {
			err = fmt.Errorf("promote chunk through slot %d: marshal resume context: %w", chunkThrough, merr)
			break
		}

		if _, cerr := committer.CommitBatch(chunk, chunkThrough, chunkBankhashes, ctxJSON); cerr != nil {
			err = fmt.Errorf("promote chunk through slot %d: %w", chunkThrough, cerr)
			break
		}

		overlay.PromotePrefix(chunkThrough)
		for _, sd := range chunk {
			delete(bankhashes, sd.Slot)
		}
		promotedThrough = chunkThrough
	}
	return promotedThrough, err
}
