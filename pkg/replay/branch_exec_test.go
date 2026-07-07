package replay

import (
	"context"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/state"
	"github.com/gagliardetto/solana-go"
)

// A branch-pinned reader must resolve each branch's own account state — never the
// sibling's — and a capture tail must record execution results without committing
// anything to the coordinator.
func TestBranchReaderIsolationAndCaptureTail(t *testing.T) {
	key := testKey(7)
	durable := &fakeDurable{known: map[solana.PublicKey]uint64{key: 100}}
	fc := newForkCoordinator(durable, &fakeCommitter{}, 16)

	// Two sibling branches over the durable base with divergent values for the key.
	brA, ok := fc.Ingest(0, 10, testHash(0xA))
	if !ok {
		t.Fatal("ingest A")
	}
	brB, ok := fc.Ingest(0, 10, testHash(0xB))
	if !ok {
		t.Fatal("ingest B")
	}
	fc.Commit(brA, []*accounts.Account{testAccount(7, 111)}, testHashBytes(0xA), &state.ResumeContext{})
	fc.Commit(brB, []*accounts.Account{testAccount(7, 222)}, testHashBytes(0xB), &state.ResumeContext{})

	readerA := &branchReader{fc: fc, branch: brA}
	readerB := &branchReader{fc: fc, branch: brB}

	acctA, err := readerA.GetAccount(10, key)
	if err != nil || acctA.Lamports != 111 {
		t.Fatalf("branch A read: %v lamports=%d (want 111)", err, acctA.Lamports)
	}
	acctB, err := readerB.GetAccount(10, key)
	if err != nil || acctB.Lamports != 222 {
		t.Fatalf("branch B read: %v lamports=%d (want 222)", err, acctB.Lamports)
	}
	batch, err := readerA.GetAccountsBatch(context.Background(), 10, []solana.PublicKey{key})
	if err != nil || len(batch) != 1 || batch[0].Lamports != 111 {
		t.Fatalf("branch A batch read: %v (want 111)", err)
	}

	// captureTail: captures without committing.
	cap := &captureTail{branchReader: branchReader{fc: fc, branch: brA}}
	delta := []*accounts.Account{testAccount(7, 333)}
	cap.Add(11, delta, testHashBytes(0xC))
	cap.SetContext(11, &state.ResumeContext{Slot: 11})
	if cap.capturedSlot != 11 || cap.capturedBankhash == nil || cap.capturedCtx == nil {
		t.Fatal("capture tail did not record execution results")
	}
	// Reads through the capture tail resolve the pinned branch (A), unaffected by capture.
	acct, err := cap.GetAccount(11, key)
	if err != nil || acct.Lamports != 111 {
		t.Fatalf("capture tail read: %v lamports=%d (want 111 — capture must not commit)", err, acct.Lamports)
	}
	// And the coordinator itself never saw slot 11 on either branch.
	if _, ok := fc.BranchIDAt(11, [32]byte(testHash(0xC))); ok {
		t.Fatal("capture leaked into the coordinator")
	}
	if _, _, err := cap.promote(11); err == nil {
		t.Fatal("capture tail must refuse to promote")
	}
}
