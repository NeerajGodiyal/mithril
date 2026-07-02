package replay

import (
	"context"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/state"
	"github.com/gagliardetto/solana-go"
)

func newTestCoordinator() (*forkCoordinator, *fakeCommitter) {
	durableSrc := &fakeDurable{known: map[solana.PublicKey]uint64{}}
	committer := &fakeCommitter{durable: accounts.NewMemAccounts()}
	return newForkCoordinator(durableSrc, committer, 512), committer
}

func TestForkCoordinatorIngestAndRead(t *testing.T) {
	coordinator, _ := newTestCoordinator()
	b, ok := coordinator.Ingest(0, 1, testHash(0xB1))
	if !ok {
		t.Fatal("ingest over durable base should succeed")
	}
	coordinator.Commit(b, []*accounts.Account{testAccount(1, 100)}, testHashBytes(1), &state.ResumeContext{Slot: 1})

	if a, err := coordinator.GetAccount(b, 1, testKey(1)); err != nil || a.Lamports != 100 {
		t.Fatalf("branch read: err=%v acct=%v", err, a)
	}
	// unwritten key falls through to durable (fake returns a zero-lamport placeholder)
	if a, err := coordinator.GetAccount(b, 1, testKey(2)); err != nil || a.Lamports != 0 {
		t.Fatalf("durable fall-through: err=%v acct=%v", err, a)
	}
}

func TestForkCoordinatorIngestIdempotentAndMissingParent(t *testing.T) {
	coordinator, _ := newTestCoordinator()
	b1, _ := coordinator.Ingest(0, 1, testHash(0xAA))
	b2, ok := coordinator.Ingest(0, 1, testHash(0xAA)) // same (slot,blockID)
	if !ok || b1 != b2 {
		t.Fatalf("repeat ingest must be idempotent: b1=%d b2=%d ok=%v", b1, b2, ok)
	}
	if _, ok := coordinator.Ingest(9999, 2, testHash(0xCC)); ok {
		t.Fatal("ingest under a missing parent must fail")
	}
}

func TestForkCoordinatorEquivocationTwoBlocksSameSlot(t *testing.T) {
	coordinator, _ := newTestCoordinator()
	p, _ := coordinator.Ingest(0, 1, testHash(0x01))
	coordinator.Commit(p, []*accounts.Account{testAccount(9, 1)}, testHashBytes(1), nil)
	a, _ := coordinator.Ingest(p, 2, testHash(0x0A)) // two competing blocks at slot 2
	b, okB := coordinator.Ingest(p, 2, testHash(0x0B))
	if !okB || a == b {
		t.Fatalf("competing blocks must be distinct branches: a=%d b=%d", a, b)
	}
	coordinator.Commit(a, []*accounts.Account{testAccount(1, 111)}, testHashBytes(2), nil)
	coordinator.Commit(b, []*accounts.Account{testAccount(1, 222)}, testHashBytes(3), nil)
	if av, _ := coordinator.GetAccount(a, 2, testKey(1)); av.Lamports != 111 {
		t.Fatalf("fork A isolation: %v", av)
	}
	if bv, _ := coordinator.GetAccount(b, 2, testKey(1)); bv.Lamports != 222 {
		t.Fatalf("fork B isolation: %v", bv)
	}
}

func TestForkCoordinatorGetAccountsBatch(t *testing.T) {
	coordinator, durableSrc := newTestCoordinatorWithKnown(map[solana.PublicKey]uint64{testKey(2): 20})
	_ = durableSrc
	b, _ := coordinator.Ingest(0, 1, testHash(1))
	coordinator.Commit(b, []*accounts.Account{testAccount(1, 10)}, testHashBytes(1), nil) // key1 in overlay, key2 in durable
	out, err := coordinator.GetAccountsBatch(context.Background(), b, 1, []solana.PublicKey{testKey(1), testKey(2)})
	if err != nil || len(out) != 2 {
		t.Fatalf("batch: err=%v out=%v", err, out)
	}
	if out[0].Lamports != 10 || out[1].Lamports != 20 {
		t.Fatalf("batch resolution wrong: %v %v", out[0], out[1])
	}
}

func TestForkCoordinatorPromoteWinner(t *testing.T) {
	coordinator, committer := newTestCoordinator()
	p, _ := coordinator.Ingest(0, 1, testHash(1))
	coordinator.Commit(p, []*accounts.Account{testAccount(1, 10)}, testHashBytes(1), &state.ResumeContext{Slot: 1})
	a, _ := coordinator.Ingest(p, 2, testHash(2))
	coordinator.Commit(a, []*accounts.Account{testAccount(2, 20)}, testHashBytes(2), &state.ResumeContext{Slot: 2})
	loser, _ := coordinator.Ingest(p, 2, testHash(0x99)) // competing fork at slot 2
	coordinator.Commit(loser, []*accounts.Account{testAccount(2, 999)}, testHashBytes(9), nil)
	c, _ := coordinator.Ingest(a, 3, testHash(3))
	coordinator.Commit(c, []*accounts.Account{testAccount(3, 30)}, testHashBytes(3), &state.ResumeContext{Slot: 3})

	through, ctx, err := coordinator.Promote(c)
	if err != nil || through != 3 {
		t.Fatalf("promote: through=%d err=%v", through, err)
	}
	if ctx == nil || ctx.Slot != 3 {
		t.Fatalf("promote should return the finalized branch's context (slot 3): %+v", ctx)
	}
	// durable committed ascending, winner's state applied, loser never committed
	if len(committer.committed) != 3 || committer.committed[0] != 1 || committer.committed[2] != 3 {
		t.Fatalf("committed slots wrong: %v", committer.committed)
	}
	if a, _ := committer.durable.GetAccountWithoutLock(testKey(3)); a == nil || a.Lamports != 30 {
		t.Fatalf("winner slot-3 state must be durable: %v", a)
	}
	// tree + side maps collapsed to survivors (none here → empty)
	if coordinator.tree.Len() != 0 || len(coordinator.meta) != 0 || len(coordinator.index) != 0 {
		t.Fatalf("post-promote not pruned: len=%d meta=%d index=%d", coordinator.tree.Len(), len(coordinator.meta), len(coordinator.index))
	}
}

func TestForkCoordinatorPromotePartialFailureStopsAndKeepsTree(t *testing.T) {
	coordinator, committer := newTestCoordinator()
	committer.failOn = 2 // slot 2 commit fails
	p, _ := coordinator.Ingest(0, 1, testHash(1))
	coordinator.Commit(p, []*accounts.Account{testAccount(1, 10)}, testHashBytes(1), nil)
	a, _ := coordinator.Ingest(p, 2, testHash(2))
	coordinator.Commit(a, []*accounts.Account{testAccount(2, 20)}, testHashBytes(2), nil)

	through, _, err := coordinator.Promote(a)
	if err == nil {
		t.Fatal("expected commit error")
	}
	if through != 1 {
		t.Fatalf("promotedThrough should be the last durable slot (1), got %d", through)
	}
	// tree left intact for idempotent retry
	if coordinator.tree.Len() != 2 {
		t.Fatalf("tree must be untouched on partial failure; Len=%d", coordinator.tree.Len())
	}
	if len(committer.committed) != 1 || committer.committed[0] != 1 {
		t.Fatalf("only slot 1 should be committed: %v", committer.committed)
	}
}

func TestForkCoordinatorEvict(t *testing.T) {
	coordinator, _ := newTestCoordinator()
	p, _ := coordinator.Ingest(0, 1, testHash(1))
	a, _ := coordinator.Ingest(p, 2, testHash(2))
	coordinator.Commit(a, []*accounts.Account{testAccount(1, 1)}, testHashBytes(2), nil)
	coordinator.Evict(a)
	if coordinator.tree.Len() != 1 {
		t.Fatalf("evict should drop A, leaving P; Len=%d", coordinator.tree.Len())
	}
	if _, ok := coordinator.meta[a]; ok {
		t.Fatal("evicted branch meta must be pruned")
	}
}

func TestForkCoordinatorOverCap(t *testing.T) {
	durableSrc := &fakeDurable{known: map[solana.PublicKey]uint64{}}
	committer := &fakeCommitter{durable: accounts.NewMemAccounts()}
	coordinator := newForkCoordinator(durableSrc, committer, 2) // cap = 2
	parent := uint64(0)
	for slot := uint64(1); slot <= 3; slot++ {
		id, _ := coordinator.Ingest(parent, slot, testHash(byte(slot)))
		parent = id
	}
	if !coordinator.OverCap() {
		t.Fatalf("3 branches over cap 2 should be OverCap; Len=%d", coordinator.tree.Len())
	}
}

// Promote must refuse a winning chain that contains an ingested-but-never-committed
// branch (else it would persist an empty bankhash and drop that slot's state).
func TestForkCoordinatorPromoteUncommittedBranchRefused(t *testing.T) {
	coordinator, committer := newTestCoordinator()
	p, _ := coordinator.Ingest(0, 1, testHash(1))
	coordinator.Commit(p, []*accounts.Account{testAccount(1, 10)}, testHashBytes(1), nil)
	b, _ := coordinator.Ingest(p, 2, testHash(2)) // ingested, never committed
	c, _ := coordinator.Ingest(b, 3, testHash(3))
	coordinator.Commit(c, []*accounts.Account{testAccount(3, 30)}, testHashBytes(3), nil)

	if _, _, err := coordinator.Promote(c); err == nil {
		t.Fatal("promote must refuse a chain with an uncommitted branch")
	}
	if len(committer.committed) != 0 {
		t.Fatalf("nothing may be committed durably on refusal; got %v", committer.committed)
	}
	if coordinator.tree.Len() != 3 {
		t.Fatalf("tree must be intact after refusal; Len=%d", coordinator.tree.Len())
	}
}

func TestForkCoordinatorBranchIDAt(t *testing.T) {
	coordinator, _ := newTestCoordinator()
	id, _ := coordinator.Ingest(0, 1, testHash(7))
	if got, ok := coordinator.BranchIDAt(1, testHash(7)); !ok || got != id {
		t.Fatalf("BranchIDAt should find the ingested branch: got=%d ok=%v", got, ok)
	}
	if _, ok := coordinator.BranchIDAt(1, testHash(8)); ok {
		t.Fatal("unknown (slot,blockID) must miss")
	}
	coordinator.Commit(id, []*accounts.Account{testAccount(1, 1)}, testHashBytes(1), nil)
	coordinator.Promote(id)
	if _, ok := coordinator.BranchIDAt(1, testHash(7)); ok {
		t.Fatal("after promote the branch id must be pruned from the index")
	}
}

// Concurrent readers on a fixed branch id while the main goroutine ingests/commits/
// promotes — makes -race exercise the coordinator's read path against tree mutation.
func TestForkCoordinatorConcurrentReads(t *testing.T) {
	coordinator, _ := newTestCoordinator()
	root, _ := coordinator.Ingest(0, 1, testHash(1))
	coordinator.Commit(root, []*accounts.Account{testAccount(1, 1)}, testHashBytes(1), nil)

	stop := make(chan struct{})
	done := make(chan struct{})
	for range 8 {
		go func() {
			for {
				select {
				case <-stop:
					done <- struct{}{}
					return
				default:
					// GetAccount hits the locked tree then durable; exercises the read
					// path against concurrent tree mutation. (GetAccountsBatch is omitted
					// only because the test fake's call counter isn't concurrency-safe.)
					coordinator.GetAccount(root, 1, testKey(1)) // root may be promoted away → misses to durable (safe)
				}
			}
		}()
	}

	parent := root
	for slot := uint64(2); slot <= 200; slot++ {
		id, ok := coordinator.Ingest(parent, slot, testHash(byte(slot)))
		if !ok {
			continue
		}
		coordinator.Commit(id, []*accounts.Account{testAccount(byte(slot), slot)}, testHashBytes(byte(slot)), nil)
		parent = id
		if slot%25 == 0 {
			coordinator.Promote(id)
		}
	}
	close(stop)
	for range 8 {
		<-done
	}
}

func newTestCoordinatorWithKnown(known map[solana.PublicKey]uint64) (*forkCoordinator, *fakeDurable) {
	durableSrc := &fakeDurable{known: known}
	committer := &fakeCommitter{durable: accounts.NewMemAccounts()}
	return newForkCoordinator(durableSrc, committer, 512), durableSrc
}
