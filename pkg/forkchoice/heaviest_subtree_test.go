package forkchoice

import (
	"testing"

	"github.com/gagliardetto/solana-go"
)

// Scenarios ported from Agave core/src/consensus/heaviest_subtree_fork_choice.rs
// tests (the only official fork-choice test corpus). Canonical setup_forks tree:
//
//	     slot 0
//	       |
//	     slot 1
//	     /    \
//	slot 2    |
//	   |    slot 3
//	slot 4    |
//	        slot 5
//	          |
//	        slot 6
func hsKey(slot uint64) SlotHashKey { return SlotHashKey{Slot: slot} }

func hsKeyH(slot uint64, choice byte) SlotHashKey {
	k := SlotHashKey{Slot: slot}
	k.Hash[0] = choice
	return k
}

func setupForks() *HeaviestSubtreeForkChoice {
	choice := NewHeaviestSubtreeForkChoice(hsKey(0))
	add := func(child, parent uint64) {
		p := hsKey(parent)
		choice.AddNewLeafSlot(hsKey(child), &p)
	}
	add(1, 0)
	add(2, 1)
	add(4, 2)
	add(3, 1)
	add(5, 3)
	add(6, 5)
	return choice
}

func hsVoter(b byte) solana.PublicKey { return solana.PublicKey{0xF0, b} }

func flatStake(stake uint64) StakeFn {
	return func(solana.PublicKey, uint64) uint64 { return stake }
}

// Ported test_add_votes: v0->3, v1->2, v2->1 (stake 100 each). Subtrees of 1's
// children tie at 100 each; tie-break picks the lower slot (2), whose best leaf
// is 4 -> best overall = 4.
func TestHSFCAddVotes(t *testing.T) {
	choice := setupForks()
	best := choice.AddVotes([]VoteKey{
		{Pubkey: hsVoter(0), Key: hsKey(3)},
		{Pubkey: hsVoter(1), Key: hsKey(2)},
		{Pubkey: hsVoter(2), Key: hsKey(1)},
	}, flatStake(100))
	if best.Slot != 4 {
		t.Fatalf("best overall = %d, want 4 (official expectation)", best.Slot)
	}
	// weight aggregation: subtree(1) = 300, subtree(2) = 100, at(1) = 100
	if st, _ := choice.StakeVotedSubtree(hsKey(1)); st != 300 {
		t.Fatalf("subtree(1) = %d, want 300", st)
	}
	if st, _ := choice.StakeVotedSubtree(hsKey(2)); st != 100 {
		t.Fatalf("subtree(2) = %d, want 100", st)
	}
	if st, _ := choice.StakeVotedAt(hsKey(1)); st != 100 {
		t.Fatalf("at(1) = %d, want 100", st)
	}
}

// With zero votes, best descends by tie-break (lower slot) to leaf 4.
func TestHSFCBestOverallNoVotes(t *testing.T) {
	choice := setupForks()
	if best := choice.BestOverallSlot(); best.Slot != 4 {
		t.Fatalf("no-vote best = %d, want 4", best.Slot)
	}
	// deepest ignores stake: the 3->5->6 branch is taller
	if deepest := choice.DeepestOverallSlot(); deepest.Slot != 6 {
		t.Fatalf("deepest = %d, want 6", deepest.Slot)
	}
}

// A validator's later vote moves its stake: subtract from the old fork, add to
// the new one (latest-vote-per-validator semantics).
func TestHSFCVoteSwitchMovesStake(t *testing.T) {
	choice := setupForks()
	choice.AddVotes([]VoteKey{{Pubkey: hsVoter(0), Key: hsKey(4)}}, flatStake(100))
	if best := choice.BestOverallSlot(); best.Slot != 4 {
		t.Fatalf("after vote on 4: best = %d, want 4", best.Slot)
	}
	best := choice.AddVotes([]VoteKey{{Pubkey: hsVoter(0), Key: hsKey(6)}}, flatStake(100))
	if best.Slot != 6 {
		t.Fatalf("after switch to 6: best = %d, want 6", best.Slot)
	}
	if st, _ := choice.StakeVotedSubtree(hsKey(2)); st != 0 {
		t.Fatalf("old fork subtree(2) must drop to 0, got %d", st)
	}
	if st, _ := choice.StakeVotedAt(hsKey(6)); st != 100 {
		t.Fatalf("at(6) = %d, want 100", st)
	}
	// stale (older-slot) vote from the same validator must be ignored
	choice.AddVotes([]VoteKey{{Pubkey: hsVoter(0), Key: hsKey(4)}}, flatStake(100))
	if best := choice.BestOverallSlot(); best.Slot != 6 {
		t.Fatalf("stale vote must not move stake back: best = %d, want 6", best.Slot)
	}
}

// Ported from the aggregate_slot doc scenario: marking a heavy fork invalid
// excludes it from best-slot selection, but its stake still counts toward shared
// ancestors' subtree weight.
func TestHSFCMarkInvalidExcludesButKeepsWeight(t *testing.T) {
	choice := setupForks()
	choice.AddVotes([]VoteKey{
		{Pubkey: hsVoter(0), Key: hsKey(4)}, // 66-stake fork
		{Pubkey: hsVoter(1), Key: hsKey(4)},
		{Pubkey: hsVoter(2), Key: hsKey(3)}, // 34-stake fork
	}, flatStake(33))

	if best := choice.BestOverallSlot(); best.Slot != 4 {
		t.Fatalf("pre-mark best = %d, want 4", best.Slot)
	}
	choice.MarkForkInvalidCandidate(hsKey(4)) // 4 is an unconfirmed duplicate
	// Agave-documented behavior: stay on the HEAVIEST fork, halting at the last
	// valid ancestor of the duplicate (2) — do NOT jump to the lighter fork (3).
	if best := choice.BestOverallSlot(); best.Slot != 2 {
		t.Fatalf("post-mark best = %d, want 2 (heaviest fork's last valid ancestor)", best.Slot)
	}
	// weight of 4 still backs its ancestors (the doc's slot-2-vs-slot-3 argument)
	if st, _ := choice.StakeVotedSubtree(hsKey(2)); st != 66 {
		t.Fatalf("subtree(2) must keep the invalid fork's weight, got %d", st)
	}
	if cand, _ := choice.IsCandidate(hsKey(4)); cand {
		t.Fatal("4 must not be a candidate while an unconfirmed duplicate")
	}

	// duplicate-confirmation re-admits the fork
	choice.MarkForkValidCandidate(hsKey(4))
	if best := choice.BestOverallSlot(); best.Slot != 4 {
		t.Fatalf("post-confirm best = %d, want 4", best.Slot)
	}
	if dup, _ := choice.IsDuplicateConfirmed(hsKey(4)); !dup {
		t.Fatal("4 must be duplicate-confirmed after valid marking")
	}
}

// Invalid marking propagates to descendants (latest_invalid_ancestor), and the
// mark reaches the marked node itself (== own slot).
func TestHSFCInvalidAncestorPropagation(t *testing.T) {
	choice := setupForks()
	choice.MarkForkInvalidCandidate(hsKey(3))
	for _, s := range []uint64{3, 5, 6} {
		if inv, ok := choice.LatestInvalidAncestor(hsKey(s)); !ok || inv != 3 {
			t.Fatalf("slot %d latest_invalid_ancestor = (%d,%v), want (3,true)", s, inv, ok)
		}
	}
	if _, ok := choice.LatestInvalidAncestor(hsKey(2)); ok {
		t.Fatal("other fork must not inherit the invalid mark")
	}
	// leaves added under an invalid fork inherit the mark
	p := hsKey(6)
	choice.AddNewLeafSlot(hsKey(7), &p)
	if inv, ok := choice.LatestInvalidAncestor(hsKey(7)); !ok || inv != 3 {
		t.Fatalf("new leaf under invalid fork must inherit: (%d,%v)", inv, ok)
	}
}

// Ported test_add_votes_duplicate_tie: two versions of the same slot with equal
// stake — fork choice picks the smaller (slot, hash) key.
func TestHSFCDuplicateTieBreak(t *testing.T) {
	choice := setupForks()
	p := hsKey(4)
	dupA := hsKeyH(10, 0x0A) // duplicate versions of slot 10 under 4
	dupB := hsKeyH(10, 0x0B)
	choice.AddNewLeafSlot(dupA, &p)
	choice.AddNewLeafSlot(dupB, &p)

	best := choice.AddVotes([]VoteKey{
		{Pubkey: hsVoter(0), Key: dupA},
		{Pubkey: hsVoter(1), Key: dupB},
	}, flatStake(10))
	if best != dupA {
		t.Fatalf("tie between duplicate versions must pick the smaller hash: got %+v", best)
	}
}

// Same-slot revote: only a SMALLER hash replaces a validator's existing vote
// (duplicate-version correction); a larger hash is ignored. Ported from
// test_add_votes_duplicate_greater_hash_ignored / smaller_hash_prioritized.
func TestHSFCDuplicateSameSlotRevote(t *testing.T) {
	choice := setupForks()
	p := hsKey(4)
	dupA := hsKeyH(10, 0x0A)
	dupB := hsKeyH(10, 0x0B)
	choice.AddNewLeafSlot(dupA, &p)
	choice.AddNewLeafSlot(dupB, &p)

	choice.AddVotes([]VoteKey{{Pubkey: hsVoter(0), Key: dupB}}, flatStake(10))
	if st, _ := choice.StakeVotedAt(dupB); st != 10 {
		t.Fatalf("at(dupB) = %d, want 10", st)
	}
	// same slot, smaller hash: replaces
	choice.AddVotes([]VoteKey{{Pubkey: hsVoter(0), Key: dupA}}, flatStake(10))
	if st, _ := choice.StakeVotedAt(dupA); st != 10 {
		t.Fatalf("smaller-hash revote must land: at(dupA) = %d, want 10", st)
	}
	if st, _ := choice.StakeVotedAt(dupB); st != 0 {
		t.Fatalf("smaller-hash revote must subtract old: at(dupB) = %d, want 0", st)
	}
	// same slot, larger hash again: ignored
	choice.AddVotes([]VoteKey{{Pubkey: hsVoter(0), Key: dupB}}, flatStake(10))
	if st, _ := choice.StakeVotedAt(dupA); st != 10 {
		t.Fatalf("larger-hash revote must be ignored: at(dupA) = %d, want 10", st)
	}
}

// Ported test_set_root behavior: everything not descending from the new root is
// dropped; votes below the root are ignored.
func TestHSFCSetTreeRoot(t *testing.T) {
	choice := setupForks()
	choice.AddVotes([]VoteKey{{Pubkey: hsVoter(0), Key: hsKey(5)}}, flatStake(100))
	choice.SetTreeRoot(hsKey(3))
	if choice.ContainsBlock(hsKey(0)) || choice.ContainsBlock(hsKey(2)) || choice.ContainsBlock(hsKey(4)) {
		t.Fatal("non-descendants of the new root must be dropped")
	}
	if !choice.ContainsBlock(hsKey(3)) || !choice.ContainsBlock(hsKey(6)) {
		t.Fatal("the new root's subtree must survive")
	}
	if choice.TreeRoot() != hsKey(3) {
		t.Fatalf("tree root = %+v, want slot 3", choice.TreeRoot())
	}
	// a vote below the root is a no-op
	before, _ := choice.StakeVotedSubtree(hsKey(3))
	choice.AddVotes([]VoteKey{{Pubkey: hsVoter(9), Key: hsKey(1)}}, flatStake(100))
	after, _ := choice.StakeVotedSubtree(hsKey(3))
	if before != after {
		t.Fatal("votes below the tree root must be ignored")
	}
	if best := choice.BestOverallSlot(); best.Slot != 6 {
		t.Fatalf("best after re-root = %d, want 6", best.Slot)
	}
}

// Ported propagate_new_leaf: a new leaf on the best fork becomes the new best.
func TestHSFCPropagateNewLeaf(t *testing.T) {
	choice := setupForks()
	choice.AddVotes([]VoteKey{{Pubkey: hsVoter(0), Key: hsKey(4)}}, flatStake(100))
	p := hsKey(4)
	choice.AddNewLeafSlot(hsKey(8), &p)
	if best := choice.BestOverallSlot(); best.Slot != 8 {
		t.Fatalf("new leaf on best fork must become best: %d, want 8", best.Slot)
	}
	// a leaf on the lighter fork must NOT displace the best
	p6 := hsKey(6)
	choice.AddNewLeafSlot(hsKey(9), &p6)
	if best := choice.BestOverallSlot(); best.Slot != 8 {
		t.Fatalf("leaf on lighter fork must not become best: %d, want 8", best.Slot)
	}
	// deepest DOES follow the taller fork regardless of stake
	if deepest := choice.DeepestOverallSlot(); deepest.Slot != 9 {
		t.Fatalf("deepest = %d, want 9", deepest.Slot)
	}
}

// Duplicate-confirming a node confirms its ancestors (returned newly-confirmed
// set) and clears their invalid marks.
func TestHSFCValidCandidateConfirmsAncestors(t *testing.T) {
	choice := setupForks()
	newly := choice.MarkForkValidCandidate(hsKey(5))
	// 5, 3, 1 were unconfirmed (0 is confirmed as root)
	want := map[uint64]bool{5: true, 3: true, 1: true}
	if len(newly) != 3 {
		t.Fatalf("newly confirmed = %v, want slots 5,3,1", newly)
	}
	for _, k := range newly {
		if !want[k.Slot] {
			t.Fatalf("unexpected newly-confirmed slot %d", k.Slot)
		}
	}
	for s := range want {
		if dup, _ := choice.IsDuplicateConfirmed(hsKey(s)); !dup {
			t.Fatalf("slot %d must be duplicate-confirmed", s)
		}
	}
	if dup, _ := choice.IsDuplicateConfirmed(hsKey(6)); dup {
		t.Fatal("descendant 6 must NOT be confirmed by ancestor confirmation")
	}
}
