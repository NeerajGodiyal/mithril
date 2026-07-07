package replay

import (
	"context"
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/state"
	"github.com/gagliardetto/solana-go"
)

// branchReader pins account reads to one branch of the fork tree: a block executing
// on that branch resolves state through its own ancestry (branch delta → parent
// deltas → durable), never through the linear tip. It serves both the per-read
// shadow seam (sealevel.AccountReader) and the block-load seam (blockAccountSource).
type branchReader struct {
	fc     *forkCoordinator
	branch uint64
}

func (r *branchReader) GetAccount(slot uint64, pubkey solana.PublicKey) (*accounts.Account, error) {
	return r.fc.GetAccount(r.branch, slot, pubkey)
}

func (r *branchReader) GetAccountsBatch(ctx context.Context, slot uint64, pks []solana.PublicKey) ([]*accounts.Account, error) {
	return r.fc.GetAccountsBatch(ctx, r.branch, slot, pks)
}

// captureTail satisfies unrootedState for executing a block against a pinned branch
// view while CAPTURING the results instead of committing them — the fork driver owns
// the commit (fc.Commit on the branch), keeping execution and state commitment
// separate. Reads resolve through the parent branch; the captured delta/bankhash/
// context are what the coordinator stores for the new branch.
type captureTail struct {
	branchReader
	capturedSlot     uint64
	capturedDelta    []*accounts.Account
	capturedBankhash []byte
	capturedCtx      *state.ResumeContext
}

func (t *captureTail) Add(slot uint64, delta []*accounts.Account, bankhash []byte) {
	t.capturedSlot = slot
	t.capturedDelta = delta
	t.capturedBankhash = bankhash
}

func (t *captureTail) SetContext(slot uint64, ctx *state.ResumeContext) {
	if slot == t.capturedSlot {
		t.capturedCtx = ctx
	}
}

// promote never runs on a capture: branch state becomes durable only when the fork
// driver promotes the finalized branch through the coordinator.
func (t *captureTail) promote(through uint64) (uint64, *state.ResumeContext, error) {
	return 0, nil, fmt.Errorf("captureTail cannot promote; commit the branch via the fork coordinator")
}

func (t *captureTail) OverCap() bool { return false }
