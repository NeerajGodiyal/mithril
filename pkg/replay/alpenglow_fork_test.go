package replay

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/alpenglow"
	"github.com/Overclock-Validator/mithril/pkg/forkchoice"
	"github.com/gagliardetto/solana-go"
)

// The full Alpenglow fork-choice loop, end to end: two competing blocks for a slot
// both execute in memory (multi-branch MVCC), a real Alpenglow certificate decides
// the winner, the fork engine promotes the certified branch to durable state and
// evicts the loser — WITHOUT the loser's state ever reaching disk. This is the
// TowerBFT-equivalent "choose the correct branch" proof, driven by certs.
func TestAlpenglowForkChoiceSelectsCertifiedBranch(t *testing.T) {
	driver, committer := newTestDriver()
	tracker := alpenglow.NewChainTracker()
	root := forkchoice.SlotHashKey{}

	// slot 1: single block off the durable root.
	k1 := fdKey(1, 0xA1)
	if err := driver.OnBlock(k1, root, fdExec(k1)); err != nil {
		t.Fatalf("slot1: %v", err)
	}
	// slot 2: a FORK — two competing blocks, both executed speculatively in memory.
	kWin := fdKey(2, 0xB)  // the branch the certificate will name
	kLose := fdKey(2, 0xA) // the losing branch
	if err := driver.OnBlock(kWin, k1, fdExec(kWin)); err != nil {
		t.Fatalf("slot2 winner: %v", err)
	}
	if err := driver.OnBlock(kLose, k1, fdExec(kLose)); err != nil {
		t.Fatalf("slot2 loser: %v", err)
	}

	// Real cert stream: notarize slot 1, then notarize + fast-finalize the WINNER
	// block for slot 2. The certs — not any weight computation — decide the branch.
	h1 := solana.Hash{0xA1}
	hWin := solana.Hash{0xB}
	feed := func(c alpenglow.Certificate) {
		c.SignatureVerified, c.StakeVerified = true, true
		if _, err := tracker.ObserveCertificate(c); err != nil {
			t.Fatalf("observe cert %s slot %d: %v", c.Type, c.Slot, err)
		}
	}
	feed(alpenglow.Certificate{Type: alpenglow.CertificateNotarize, Slot: 1, BlockHash: h1})
	feed(alpenglow.Certificate{Type: alpenglow.CertificateNotarize, Slot: 2, BlockHash: hWin})
	feed(alpenglow.Certificate{Type: alpenglow.CertificateFinalizeFast, Slot: 2, BlockHash: hWin})

	// Drive the fork engine from the certificate decisions.
	dec1, ok := tracker.NextDecision(0)
	if !ok || dec1.Kind != alpenglow.ChainDecisionKindBlock || dec1.Slot != 1 {
		t.Fatalf("slot1 decision = %+v (ok=%v), want block@1", dec1, ok)
	}
	if err := applyAlpenglowForkDecision(driver, dec1, []forkchoice.SlotHashKey{k1}); err != nil {
		t.Fatalf("apply slot1: %v", err)
	}
	dec2, ok := tracker.NextDecision(1)
	if !ok || dec2.Kind != alpenglow.ChainDecisionKindBlock || dec2.Slot != 2 {
		t.Fatalf("slot2 decision = %+v (ok=%v), want block@2 (cert must name a branch, not conflict)", dec2, ok)
	}
	if err := applyAlpenglowForkDecision(driver, dec2, []forkchoice.SlotHashKey{kWin, kLose}); err != nil {
		t.Fatalf("apply slot2: %v", err)
	}

	// Finalize the certified chain through slot 2.
	through, _, err := driver.OnFinalized(kWin)
	if err != nil || through != 2 {
		t.Fatalf("finalize: through=%d err=%v, want 2", through, err)
	}

	// PROOF: durable slot-2 state is the WINNER's write (0xB), and the loser's
	// write (0xA) never reached disk. fdExec writes lamports=version to pubkey=slot,
	// so the value at pubkey{2} tells us which branch was promoted.
	acct, _ := committer.durable.GetAccountWithoutLock(solana.PublicKey{2})
	if acct == nil {
		t.Fatal("slot-2 state missing from durable — certified branch was not promoted")
	}
	if acct.Lamports != 0xB {
		t.Fatalf("durable slot-2 = 0x%X, want 0xB — fork engine promoted the WRONG branch (loser=0xA)", acct.Lamports)
	}
	// slot 1 must also be durable (certified linear prefix).
	if a, _ := committer.durable.GetAccountWithoutLock(solana.PublicKey{1}); a == nil || a.Lamports != 0xA1 {
		t.Fatalf("durable slot-1 = %v, want the certified block 0xA1", a)
	}
}

// Safety: if two DIFFERENT blocks are both certified for one slot (a protocol
// safety violation), the fork engine must NOT silently promote one — it halts.
func TestAlpenglowForkChoiceHaltsOnConflictingCerts(t *testing.T) {
	driver, _ := newTestDriver()
	tracker := alpenglow.NewChainTracker()

	feed := func(c alpenglow.Certificate) {
		c.SignatureVerified, c.StakeVerified = true, true
		_, _ = tracker.ObserveCertificate(c)
	}
	// Two notarize certs, same slot, DIFFERENT blocks → conflict.
	feed(alpenglow.Certificate{Type: alpenglow.CertificateNotarize, Slot: 5, BlockHash: solana.Hash{0xA}})
	feed(alpenglow.Certificate{Type: alpenglow.CertificateNotarize, Slot: 5, BlockHash: solana.Hash{0xB}})

	dec, ok := tracker.NextDecision(4)
	if !ok || dec.Kind != alpenglow.ChainDecisionKindConflict {
		t.Fatalf("two certified blocks for slot 5 must be a conflict, got %+v (ok=%v)", dec, ok)
	}
	competing := []forkchoice.SlotHashKey{fdKey(5, 0xA), fdKey(5, 0xB)}
	if err := applyAlpenglowForkDecision(driver, dec, competing); err == nil {
		t.Fatal("conflicting certs must halt (return error), not silently promote a branch")
	}
}
