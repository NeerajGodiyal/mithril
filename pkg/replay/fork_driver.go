package replay

import (
	"bytes"
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/forkchoice"
	"github.com/Overclock-Validator/mithril/pkg/state"
)

// ExecuteFn replays one candidate block on its branch: reads resolve through the
// branch (nearest-ancestor then durable) and the block's writes + bankhash are
// returned for the branch commit. The turbine-era loop passes real ProcessBlock;
// tests pass synthetic executors.
type ExecuteFn func(branchID uint64) (delta []*accounts.Account, bankhash []byte, ctx *state.ResumeContext, err error)

// forkDriver composes fork SELECTION (HeaviestSubtreeForkChoice) with fork STATE
// (forkCoordinator): candidate blocks are executed into isolated branches, votes
// drive the heaviest-fork tip, duplicates are excluded until confirmed, and
// finality promotes the winner (folding it durable, evicting all losers).
type forkDriver struct {
	fc      *forkCoordinator
	choice  *forkchoice.HeaviestSubtreeForkChoice
	stakeAt forkchoice.StakeFn
	tip     forkchoice.SlotHashKey // current heaviest tip (the chain to extend)
}

func newForkDriver(durable blockAccountSource, committer slotCommitter, root forkchoice.SlotHashKey, stakeAt forkchoice.StakeFn, haltCap int) *forkDriver {
	return &forkDriver{
		fc:      newForkCoordinator(durable, committer, haltCap),
		choice:  forkchoice.NewHeaviestSubtreeForkChoice(root),
		stakeAt: stakeAt,
		tip:     root,
	}
}

// OnBlock ingests and executes one candidate block version. parent is the root
// key for the first block above durable. Competing versions of the same slot land
// on isolated branches. Idempotent per (slot, blockID).
func (d *forkDriver) OnBlock(key, parent forkchoice.SlotHashKey, execute ExecuteFn) error {
	if d.choice.ContainsBlock(key) {
		return nil
	}
	parentBranch := uint64(0) // 0 = extend the durable base (parent == tree root)
	if parent != d.choice.TreeRoot() {
		pb, ok := d.fc.BranchIDAt(parent.Slot, parent.Hash)
		if !ok {
			return fmt.Errorf("fork driver: parent (%d) not ingested", parent.Slot)
		}
		parentBranch = pb
	}
	id, ok := d.fc.Ingest(parentBranch, key.Slot, key.Hash)
	if !ok {
		return fmt.Errorf("fork driver: ingest (%d) under branch %d failed", key.Slot, parentBranch)
	}
	delta, bankhash, ctx, err := execute(id)
	if err != nil {
		// Execution failure = dead candidate: never a vote target, never promotable.
		d.fc.Evict(id)
		return fmt.Errorf("fork driver: execute (%d): %w", key.Slot, err)
	}
	d.fc.Commit(id, delta, bankhash, ctx)
	d.choice.AddNewLeafSlot(key, &parent)
	if best := d.choice.BestOverallSlot(); best != d.tip {
		d.tip = best
	}
	return nil
}

// OnVotes applies observed votes and returns the (possibly switched) heaviest tip.
// Duplicate votes per validator in one batch are deduped to the vote the
// latest-vote rule prefers (higher slot; same slot only for a smaller hash).
func (d *forkDriver) OnVotes(votes []forkchoice.VoteKey) forkchoice.SlotHashKey {
	best := make(map[[32]byte]forkchoice.VoteKey, len(votes))
	for _, v := range votes {
		pk := [32]byte(v.Pubkey)
		if prev, ok := best[pk]; ok {
			if v.Key.Slot < prev.Key.Slot ||
				(v.Key.Slot == prev.Key.Slot && bytes.Compare(v.Key.Hash[:], prev.Key.Hash[:]) >= 0) {
				continue
			}
		}
		best[pk] = v
	}
	deduped := make([]forkchoice.VoteKey, 0, len(best))
	for _, v := range best {
		deduped = append(deduped, v)
	}
	d.tip = d.choice.AddVotes(deduped, d.stakeAt)
	return d.tip
}

// OnDuplicate marks a block version an unconfirmed duplicate: excluded from
// selection (weight still backs ancestors) until confirmed via OnDuplicateConfirmed.
// No-op for unknown/pruned keys and for already-confirmed blocks (stale gossip).
func (d *forkDriver) OnDuplicate(key forkchoice.SlotHashKey) {
	if !d.choice.ContainsBlock(key) {
		return
	}
	if confirmed, _ := d.choice.IsDuplicateConfirmed(key); confirmed {
		return // finality/confirmation already decided this version; stale proof
	}
	d.choice.MarkForkInvalidCandidate(key)
	if best := d.choice.BestOverallSlot(); best != d.tip {
		d.tip = best
	}
}

// OnDuplicateConfirmed re-admits a version once the cluster confirms it.
// No-op for unknown/pruned keys (stale or never-ingested gossip).
func (d *forkDriver) OnDuplicateConfirmed(key forkchoice.SlotHashKey) {
	if !d.choice.ContainsBlock(key) {
		return
	}
	d.choice.MarkForkValidCandidate(key)
	if best := d.choice.BestOverallSlot(); best != d.tip {
		d.tip = best
	}
}

// OnFinalized promotes the finalized block's chain durably (two-phase; losers
// evicted from both the state tree and fork choice) and re-roots selection at it.
// Returns the highest slot made durable and its resume context.
func (d *forkDriver) OnFinalized(key forkchoice.SlotHashKey) (uint64, *state.ResumeContext, error) {
	branchID, ok := d.fc.BranchIDAt(key.Slot, key.Hash)
	if !ok {
		return 0, nil, fmt.Errorf("fork driver: finalized block (%d) not ingested", key.Slot)
	}
	// Finality implies duplicate-confirmation: clear any stale invalid marks so the
	// re-rooted tree (and leaves later added under it) stay valid candidates.
	d.choice.MarkForkValidCandidate(key)
	through, ctx, err := d.fc.Promote(branchID)
	if err != nil {
		return through, ctx, err
	}
	d.choice.SetTreeRoot(key)
	if best := d.choice.BestOverallSlot(); best != d.tip {
		d.tip = best
	}
	return through, ctx, nil
}

// Tip is the current heaviest valid tip — the chain the node follows.
func (d *forkDriver) Tip() forkchoice.SlotHashKey { return d.tip }

// TipBranch resolves the tip's state branch for reads/extension. A tip that IS
// the tree root (everything finalized) resolves to the durable base (branch 0).
func (d *forkDriver) TipBranch() (uint64, bool) {
	if d.tip == d.choice.TreeRoot() {
		return 0, true
	}
	return d.fc.BranchIDAt(d.tip.Slot, d.tip.Hash)
}

// OverCap reports fork-state RAM pressure (rooting stalled or fork spam).
func (d *forkDriver) OverCap() bool { return d.fc.OverCap() }
