package replay

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/forkchoice"
	"github.com/Overclock-Validator/mithril/pkg/state"
	"github.com/gagliardetto/solana-go"
)

// Synthetic fork harness: drives the composed selection (heaviest-subtree) +
// state (fork coordinator) machinery with manufactured forks — the scenarios the
// RPC path can physically never produce.

func fdKey(slot uint64, h byte) forkchoice.SlotHashKey {
	k := forkchoice.SlotHashKey{Slot: slot}
	k.Hash[0] = h
	return k
}

// fdExec returns an executor whose single account write encodes which block
// version executed (key = slot byte, lamports = version byte), so durable state
// proves which fork won.
func fdExec(key forkchoice.SlotHashKey) ExecuteFn {
	return func(branchID uint64) ([]*accounts.Account, []byte, *state.ResumeContext, error) {
		acct := &accounts.Account{Key: solana.PublicKey{byte(key.Slot)}, Lamports: uint64(key.Hash[0])}
		bh := make([]byte, 32)
		bh[0] = byte(key.Slot)
		bh[1] = key.Hash[0]
		return []*accounts.Account{acct}, bh, &state.ResumeContext{Slot: key.Slot}, nil
	}
}

func fdVoter(b byte) solana.PublicKey { return solana.PublicKey{0xD0, b} }

func newTestDriver() (*forkDriver, *fakeCommitter) {
	committer := &fakeCommitter{durable: accounts.NewMemAccounts()}
	dur := &fakeDurable{known: map[solana.PublicKey]uint64{}}
	root := forkchoice.SlotHashKey{} // durable base at slot 0
	driver := newForkDriver(dur, committer, root, func(solana.PublicKey, uint64) uint64 { return 100 }, 512)
	return driver, committer
}

func TestForkDriverLinearHappyPath(t *testing.T) {
	driver, committer := newTestDriver()
	root := forkchoice.SlotHashKey{}
	k1, k2, k3 := fdKey(1, 1), fdKey(2, 1), fdKey(3, 1)
	if err := driver.OnBlock(k1, root, fdExec(k1)); err != nil {
		t.Fatal(err)
	}
	if err := driver.OnBlock(k2, k1, fdExec(k2)); err != nil {
		t.Fatal(err)
	}
	if err := driver.OnBlock(k3, k2, fdExec(k3)); err != nil {
		t.Fatal(err)
	}
	driver.OnVotes([]forkchoice.VoteKey{{Pubkey: fdVoter(1), Key: k3}})
	if driver.Tip() != k3 {
		t.Fatalf("tip = %+v, want slot 3", driver.Tip())
	}
	through, ctx, err := driver.OnFinalized(k2)
	if err != nil || through != 2 || ctx == nil || ctx.Slot != 2 {
		t.Fatalf("finalize: through=%d ctx=%+v err=%v", through, ctx, err)
	}
	if len(committer.committed) != 2 || committer.committed[0] != 1 || committer.committed[1] != 2 {
		t.Fatalf("durable slots = %v, want [1 2]", committer.committed)
	}
	// the unfinalized tip block survives and remains the tip
	if driver.Tip() != k3 {
		t.Fatalf("post-finalize tip = %+v, want slot 3", driver.Tip())
	}
}

// Skip-fork: chain A (1→2) vs chain B (1→3, skipping 2). Votes move the tip;
// finality promotes the winner and the loser's state never reaches durable.
func TestForkDriverSkipForkSwitchAndFinalize(t *testing.T) {
	driver, committer := newTestDriver()
	root := forkchoice.SlotHashKey{}
	k1, kA, kB := fdKey(1, 1), fdKey(2, 0xA), fdKey(3, 0xB) // B skips slot 2
	for _, blk := range []struct {
		key, parent forkchoice.SlotHashKey
	}{{k1, root}, {kA, k1}, {kB, k1}} {
		if err := driver.OnBlock(blk.key, blk.parent, fdExec(blk.key)); err != nil {
			t.Fatal(err)
		}
	}
	// one vote on A: tip = A
	driver.OnVotes([]forkchoice.VoteKey{{Pubkey: fdVoter(1), Key: kA}})
	if driver.Tip() != kA {
		t.Fatalf("tip = %+v, want A", driver.Tip())
	}
	// two votes on B: heaviest switches
	driver.OnVotes([]forkchoice.VoteKey{{Pubkey: fdVoter(2), Key: kB}, {Pubkey: fdVoter(3), Key: kB}})
	if driver.Tip() != kB {
		t.Fatalf("tip after votes = %+v, want B", driver.Tip())
	}
	// cluster finalizes B
	through, _, err := driver.OnFinalized(kB)
	if err != nil || through != 3 {
		t.Fatalf("finalize B: through=%d err=%v", through, err)
	}
	// durable must hold B's version at slot 3 and NOTHING from A's slot 2
	if a, _ := committer.durable.GetAccountWithoutLock(solana.PublicKey{3}); a == nil || a.Lamports != 0xB {
		t.Fatalf("slot-3 durable must be B's write: %v", a)
	}
	if a, _ := committer.durable.GetAccountWithoutLock(solana.PublicKey{2}); a != nil {
		t.Fatalf("loser A's slot-2 state must never reach durable: %v", a)
	}
	// state tree fully collapsed (no survivors above B)
	if driver.fc.tree.Len() != 0 {
		t.Fatalf("state tree should be empty, len=%d", driver.fc.tree.Len())
	}
}

// Equivocation: twin leader produces two versions of slot 2. Tie-break picks the
// smaller hash; duplicate-marking diverts selection; confirmation restores it;
// finalizing the confirmed version evicts its twin.
func TestForkDriverEquivocationLifecycle(t *testing.T) {
	driver, committer := newTestDriver()
	root := forkchoice.SlotHashKey{}
	k1, twinA, twinB := fdKey(1, 1), fdKey(2, 0xA), fdKey(2, 0xB)
	for _, blk := range []struct {
		key, parent forkchoice.SlotHashKey
	}{{k1, root}, {twinA, k1}, {twinB, k1}} {
		if err := driver.OnBlock(blk.key, blk.parent, fdExec(blk.key)); err != nil {
			t.Fatal(err)
		}
	}
	driver.OnVotes([]forkchoice.VoteKey{
		{Pubkey: fdVoter(1), Key: twinA},
		{Pubkey: fdVoter(2), Key: twinB},
	})
	if driver.Tip() != twinA { // equal stake: smaller hash wins
		t.Fatalf("tie tip = %+v, want twinA", driver.Tip())
	}
	// twinA detected as an unconfirmed duplicate: selection diverts
	driver.OnDuplicate(twinA)
	if driver.Tip() == twinA {
		t.Fatal("tip must divert off an unconfirmed duplicate")
	}
	// cluster duplicate-confirms twinA: selection returns
	driver.OnDuplicateConfirmed(twinA)
	if driver.Tip() != twinA {
		t.Fatalf("confirmed duplicate must be selectable again: tip=%+v", driver.Tip())
	}
	// finality on twinA: twinB (the equivocating loser) is evicted everywhere
	if _, _, err := driver.OnFinalized(twinA); err != nil {
		t.Fatal(err)
	}
	if a, _ := committer.durable.GetAccountWithoutLock(solana.PublicKey{2}); a == nil || a.Lamports != 0xA {
		t.Fatalf("durable slot-2 must be twinA's write: %v", a)
	}
	if _, ok := driver.fc.BranchIDAt(2, twinB.Hash); ok {
		t.Fatal("twinB must be evicted from the state index")
	}
}

// "Cluster confirms the block we didn't pick": we follow the heavier fork, but
// finality lands on the other one — the driver must promote the cluster's choice.
func TestForkDriverFinalityOverridesLocalChoice(t *testing.T) {
	driver, committer := newTestDriver()
	root := forkchoice.SlotHashKey{}
	k1, kA, kB := fdKey(1, 1), fdKey(2, 0xA), fdKey(3, 0xB)
	for _, blk := range []struct {
		key, parent forkchoice.SlotHashKey
	}{{k1, root}, {kA, k1}, {kB, k1}} {
		if err := driver.OnBlock(blk.key, blk.parent, fdExec(blk.key)); err != nil {
			t.Fatal(err)
		}
	}
	// our local view: A is heavier
	driver.OnVotes([]forkchoice.VoteKey{{Pubkey: fdVoter(1), Key: kA}, {Pubkey: fdVoter(2), Key: kA}})
	if driver.Tip() != kA {
		t.Fatalf("local tip = %+v, want A", driver.Tip())
	}
	// but the cluster finalizes B
	if _, _, err := driver.OnFinalized(kB); err != nil {
		t.Fatal(err)
	}
	if a, _ := committer.durable.GetAccountWithoutLock(solana.PublicKey{3}); a == nil || a.Lamports != 0xB {
		t.Fatalf("cluster's finalized fork must win durably: %v", a)
	}
	if a, _ := committer.durable.GetAccountWithoutLock(solana.PublicKey{2}); a != nil {
		t.Fatalf("our locally-preferred fork must not persist: %v", a)
	}
}

// Twins-style seeded generator: random skip-forks + twin (equivocating) leaders +
// random votes across many rounds. Invariants: the tip always exists, is always a
// candidate, execution failures never leak, and the finalized chain's state — and
// ONLY that chain's — reaches durable.
func TestForkDriverTwinsRandomized(t *testing.T) {
	for seed := int64(1); seed <= 5; seed++ {
		rng := rand.New(rand.NewSource(seed))
		driver, committer := newTestDriver()
		root := forkchoice.SlotHashKey{}

		type blockInfo struct{ key, parent forkchoice.SlotHashKey }
		blocks := map[forkchoice.SlotHashKey]blockInfo{}
		heads := []forkchoice.SlotHashKey{root} // possible parents
		voters := 8

		for slot := uint64(1); slot <= 40; slot++ {
			parent := heads[rng.Intn(len(heads))]
			versions := 1
			if rng.Intn(5) == 0 { // twin leader: equivocate
				versions = 2
			}
			for v := 0; v < versions; v++ {
				key := fdKey(slot, byte(0xA+v))
				if err := driver.OnBlock(key, parent, fdExec(key)); err != nil {
					t.Fatalf("seed %d: OnBlock(%d): %v", seed, slot, err)
				}
				blocks[key] = blockInfo{key: key, parent: parent}
				heads = append(heads, key)
			}
			// random votes from a few voters onto random known blocks
			var votes []forkchoice.VoteKey
			seen := map[byte]bool{}
			for i := 0; i < rng.Intn(3); i++ {
				vb := byte(rng.Intn(voters))
				if seen[vb] {
					continue
				}
				seen[vb] = true
				votes = append(votes, forkchoice.VoteKey{
					Pubkey: fdVoter(vb), Key: heads[rng.Intn(len(heads))]})
			}
			if len(votes) > 0 {
				driver.OnVotes(votes)
			}
			// invariant: tip exists and is a candidate
			tip := driver.Tip()
			if tip != root {
				if _, ok := blocks[tip]; !ok {
					t.Fatalf("seed %d: tip %+v is not a known block", seed, tip)
				}
				if cand, ok := driver.choice.IsCandidate(tip); !ok || !cand {
					t.Fatalf("seed %d: tip %+v is not a candidate", seed, tip)
				}
			}
		}

		// finalize the current tip; every durable slot must match the tip's chain
		tip := driver.Tip()
		if tip == root {
			continue // degenerate seed: no votes landed; nothing to finalize
		}
		if _, _, err := driver.OnFinalized(tip); err != nil {
			t.Fatalf("seed %d: finalize: %v", seed, err)
		}
		expected := map[uint64]byte{} // slot -> version byte on the winning chain
		for k := tip; k != root; k = blocks[k].parent {
			expected[k.Slot] = k.Hash[0]
		}
		for slot, version := range expected {
			a, _ := committer.durable.GetAccountWithoutLock(solana.PublicKey{byte(slot)})
			if a == nil || a.Lamports != uint64(version) {
				t.Fatalf("seed %d: durable slot %d = %v, want version %#x (finalized chain only)",
					seed, slot, a, version)
			}
		}
		for i, slotKey := range committer.committed {
			version, onChain := expected[slotKey]
			if !onChain {
				t.Fatalf("seed %d: durable slot %d is NOT on the finalized chain", seed, slotKey)
			}
			// per-commit bankhash carries (slot, version): the committed VERSION must
			// be the finalized chain's — catches a losing twin committed then overwritten
			if bh := committer.bankhashes[i]; bh[0] != byte(slotKey) || bh[1] != version {
				t.Fatalf("seed %d: slot %d committed version %#x, want %#x", seed, slotKey, bh[1], version)
			}
		}
	}
}

// F1: duplicate-confirmation gossip for a block we never ingested (or already
// pruned below the root) must be a safe no-op, not a panic.
func TestForkDriverDuplicateConfirmedUnknownKey(t *testing.T) {
	driver, _ := newTestDriver()
	driver.OnDuplicateConfirmed(fdKey(99, 9)) // must not panic
	root := forkchoice.SlotHashKey{}
	k1 := fdKey(1, 1)
	if err := driver.OnBlock(k1, root, fdExec(k1)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := driver.OnFinalized(k1); err != nil {
		t.Fatal(err)
	}
	driver.OnDuplicateConfirmed(fdKey(1, 1)) // now the root; must not panic
	driver.OnDuplicate(fdKey(99, 9))         // unknown; must not panic
}

// F2: out-of-order gossip — confirm a descendant (which transitively confirms its
// ancestors), then a duplicate-proof arrives for an ancestor. Must be a no-op.
func TestForkDriverDuplicateAfterConfirmedNoop(t *testing.T) {
	driver, _ := newTestDriver()
	root := forkchoice.SlotHashKey{}
	k1, k2 := fdKey(1, 1), fdKey(2, 1)
	if err := driver.OnBlock(k1, root, fdExec(k1)); err != nil {
		t.Fatal(err)
	}
	if err := driver.OnBlock(k2, k1, fdExec(k2)); err != nil {
		t.Fatal(err)
	}
	driver.OnDuplicateConfirmed(k2) // confirms k1 transitively
	driver.OnDuplicate(k1)          // stale duplicate-proof: must not panic, must not exclude
	if cand, _ := driver.choice.IsCandidate(k1); !cand {
		t.Fatal("confirmed ancestor must stay a candidate")
	}
}

// F3: finality implies duplicate-confirmation. A block marked duplicate then
// finalized WITHOUT an explicit confirm must not poison the re-rooted tree
// (new children must be candidates, and the tip must stay valid).
func TestForkDriverFinalizeImpliesDuplicateConfirmed(t *testing.T) {
	driver, _ := newTestDriver()
	root := forkchoice.SlotHashKey{}
	k1, twinA, twinB := fdKey(1, 1), fdKey(2, 0xA), fdKey(2, 0xB)
	for _, blk := range []struct {
		key, parent forkchoice.SlotHashKey
	}{{k1, root}, {twinA, k1}, {twinB, k1}} {
		if err := driver.OnBlock(blk.key, blk.parent, fdExec(blk.key)); err != nil {
			t.Fatal(err)
		}
	}
	driver.OnDuplicate(twinA)
	// cluster finalizes twinA directly (finalized ⟹ duplicate-confirmed)
	if _, _, err := driver.OnFinalized(twinA); err != nil {
		t.Fatal(err)
	}
	k3 := fdKey(3, 1)
	if err := driver.OnBlock(k3, twinA, fdExec(k3)); err != nil {
		t.Fatal(err)
	}
	if cand, ok := driver.choice.IsCandidate(k3); !ok || !cand {
		t.Fatal("children of the finalized (implicitly confirmed) root must be candidates")
	}
	driver.OnVotes([]forkchoice.VoteKey{{Pubkey: fdVoter(1), Key: k3}})
	if tip := driver.Tip(); tip != k3 {
		t.Fatalf("tip = %+v, want k3 (valid candidate)", tip)
	}
}

// F4: when the tip IS the tree root (everything finalized), TipBranch must report
// the durable base (branch 0) as a valid resolution, not a miss.
func TestForkDriverTipBranchAtRoot(t *testing.T) {
	driver, _ := newTestDriver()
	root := forkchoice.SlotHashKey{}
	k1 := fdKey(1, 1)
	if err := driver.OnBlock(k1, root, fdExec(k1)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := driver.OnFinalized(k1); err != nil {
		t.Fatal(err)
	}
	if branch, ok := driver.TipBranch(); !ok || branch != 0 {
		t.Fatalf("tip at root must resolve to the durable base: (%d,%v)", branch, ok)
	}
}

// F6: duplicate votes from one validator in a single batch must not panic the
// driver (dedupe keeps the vote the latest-vote filter would prefer).
func TestForkDriverDuplicateVotesInBatch(t *testing.T) {
	driver, _ := newTestDriver()
	root := forkchoice.SlotHashKey{}
	k1, k2 := fdKey(1, 1), fdKey(2, 1)
	if err := driver.OnBlock(k1, root, fdExec(k1)); err != nil {
		t.Fatal(err)
	}
	if err := driver.OnBlock(k2, k1, fdExec(k2)); err != nil {
		t.Fatal(err)
	}
	tip := driver.OnVotes([]forkchoice.VoteKey{
		{Pubkey: fdVoter(1), Key: k1},
		{Pubkey: fdVoter(1), Key: k2}, // same validator, later slot: must win
	})
	if tip != k2 {
		t.Fatalf("deduped batch should land the later vote: tip=%+v", tip)
	}
}

// A failing execution must evict the candidate and never make it selectable.
func TestForkDriverExecutionFailureEvicts(t *testing.T) {
	driver, _ := newTestDriver()
	root := forkchoice.SlotHashKey{}
	k1 := fdKey(1, 1)
	failExec := func(uint64) ([]*accounts.Account, []byte, *state.ResumeContext, error) {
		return nil, nil, nil, fmt.Errorf("boom")
	}
	if err := driver.OnBlock(k1, root, failExec); err == nil {
		t.Fatal("execution failure must propagate")
	}
	if driver.choice.ContainsBlock(k1) {
		t.Fatal("failed candidate must not enter fork choice")
	}
	if _, ok := driver.fc.BranchIDAt(1, k1.Hash); ok {
		t.Fatal("failed candidate must be evicted from state")
	}
}
