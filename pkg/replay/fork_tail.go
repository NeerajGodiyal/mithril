package replay

import (
	"context"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/state"
	"github.com/gagliardetto/solana-go"
)

// forkTail adapts forkCoordinator to the unrootedState interface the replay loop
// drives: it tracks the executed chain (tip branch + slot→branch map) and uses the
// slot bankhash as the block identity, so a finalized slot resolves to its branch.
// The live replay path is linear (one executed chain); competing forks will execute
// as side branches once branch-aware execution lands.
type forkTail struct {
	fc         *forkCoordinator
	tipBranch  uint64            // branch of the last replayed slot (0 = durable base)
	tipSlot    uint64            // slot of the tip branch
	slotBranch map[uint64]uint64 // executed chain: slot -> branch id
}

func newForkTail(durable blockAccountSource, committer slotCommitter, haltCap int) *forkTail {
	return &forkTail{
		fc:         newForkCoordinator(durable, committer, haltCap),
		slotBranch: make(map[uint64]uint64),
	}
}

// GetAccount resolves on the executed chain's tip branch (the slot being replayed
// extends it), else durable.
func (t *forkTail) GetAccount(slot uint64, pubkey solana.PublicKey) (*accounts.Account, error) {
	return t.fc.GetAccount(t.tipBranch, slot, pubkey)
}

func (t *forkTail) GetAccountsBatch(ctx context.Context, slot uint64, pks []solana.PublicKey) ([]*accounts.Account, error) {
	return t.fc.GetAccountsBatch(ctx, t.tipBranch, slot, pks)
}

// Add ingests the replayed slot as a child of the tip branch (bankhash = block
// identity) and buffers its writes; the new branch becomes the tip.
func (t *forkTail) Add(slot uint64, delta []*accounts.Account, bankhash []byte) {
	var blockID [32]byte
	copy(blockID[:], bankhash)
	id, ok := t.fc.Ingest(t.tipBranch, slot, blockID)
	if !ok {
		// Unreachable (tipBranch is always live); loud because silence = state loss.
		mlog.Log.Errorf("fork-aware: dropping slot %d writes: tip branch %d not live", slot, t.tipBranch)
		return
	}
	t.fc.Commit(id, delta, bankhash, nil)
	t.tipBranch = id
	t.tipSlot = slot
	t.slotBranch[slot] = id
}

// SetContext attaches the slot's deep-copied resume context to its branch.
func (t *forkTail) SetContext(slot uint64, ctx *state.ResumeContext) {
	if id, ok := t.slotBranch[slot]; ok {
		t.fc.SetContext(id, ctx)
	}
}

// promote folds the executed chain through the highest replayed slot <= through into
// durable (two-phase inside the coordinator) and prunes the promoted prefix.
func (t *forkTail) promote(through uint64) (uint64, *state.ResumeContext, error) {
	var bestSlot, bestID uint64
	found := false
	for s, id := range t.slotBranch {
		if s <= through && (!found || s > bestSlot) {
			bestSlot, bestID, found = s, id, true
		}
	}
	if !found {
		return 0, nil, nil
	}
	promotedThrough, ctx, err := t.fc.Promote(bestID)
	if promotedThrough > 0 {
		for s := range t.slotBranch {
			if s <= promotedThrough {
				delete(t.slotBranch, s)
			}
		}
		// If the tip itself was folded, the next slot extends the durable base.
		if t.tipSlot <= promotedThrough {
			t.tipBranch, t.tipSlot = 0, 0
		}
	}
	return promotedThrough, ctx, err
}

func (t *forkTail) OverCap() bool {
	return t.fc.OverCap()
}
