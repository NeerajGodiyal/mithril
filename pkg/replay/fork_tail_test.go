package replay

import (
	"bytes"
	"context"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/state"
	"github.com/gagliardetto/solana-go"
)

// driveEngine runs the same replay-shaped sequence (Add+SetContext per slot, promote
// partway, reads, final promote) against any unrootedState engine and returns its
// observable outputs for parity comparison.
type engineOutputs struct {
	committedSlots []uint64
	midRead        *accounts.Account
	promote1       uint64
	promote2       uint64
	postRead       *accounts.Account
	overCap        bool
	durableK1      *accounts.Account
}

func driveEngine(t *testing.T, eng unrootedState, committer *fakeCommitter) engineOutputs {
	t.Helper()
	// slots 1..5: key1 written at 1 and 4 (cross-slot overwrite), key(N) per slot
	for slot := uint64(1); slot <= 5; slot++ {
		delta := []*accounts.Account{testAccount(byte(slot), slot*10)}
		if slot == 4 {
			delta = append(delta, testAccount(1, 444))
		}
		eng.Add(slot, delta, testHashBytes(byte(slot)))
		eng.SetContext(slot, &state.ResumeContext{Slot: slot})
	}
	var out engineOutputs
	// mid-state read: key1 must be the slot-4 override
	out.midRead, _ = eng.GetAccount(5, testKey(1))
	// promote through 3 (mid-chain), then through 5 (to the tip)
	out.promote1, _, _ = eng.promote(3)
	out.promote2, _, _ = eng.promote(5)
	out.overCap = eng.OverCap()
	out.committedSlots = append([]uint64(nil), committer.committed...)
	out.durableK1, _ = committer.durable.GetAccountWithoutLock(testKey(1))
	// batch read after full promotion falls through to the (fake) durable source
	if outs, err := eng.GetAccountsBatch(context.Background(), 6, []solana.PublicKey{testKey(1)}); err == nil && len(outs) == 1 {
		out.postRead = outs[0]
	}
	return out
}

// PARITY: forkTail (branch-tree engine in linear mode) must be observably identical
// to the proven unrootedTail for the same replay sequence.
func TestForkTailParityWithUnrootedTail(t *testing.T) {
	comA := &fakeCommitter{durable: accounts.NewMemAccounts()}
	linear := newUnrootedTail(&fakeDurable{known: map[solana.PublicKey]uint64{}}, comA, 512)
	outA := driveEngine(t, linear, comA)

	comB := &fakeCommitter{durable: accounts.NewMemAccounts()}
	forky := newForkTail(&fakeDurable{known: map[solana.PublicKey]uint64{}}, comB, 512)
	outB := driveEngine(t, forky, comB)

	if outA.promote1 != outB.promote1 || outA.promote2 != outB.promote2 {
		t.Fatalf("promote watermarks differ: linear=(%d,%d) fork=(%d,%d)",
			outA.promote1, outA.promote2, outB.promote1, outB.promote2)
	}
	if len(outA.committedSlots) != len(outB.committedSlots) {
		t.Fatalf("committed slots differ: linear=%v fork=%v", outA.committedSlots, outB.committedSlots)
	}
	for i := range outA.committedSlots {
		if outA.committedSlots[i] != outB.committedSlots[i] {
			t.Fatalf("commit order differs at %d: linear=%v fork=%v", i, outA.committedSlots, outB.committedSlots)
		}
	}
	if outA.midRead.Lamports != outB.midRead.Lamports {
		t.Fatalf("mid-state read differs: linear=%d fork=%d", outA.midRead.Lamports, outB.midRead.Lamports)
	}
	if outA.durableK1.Lamports != outB.durableK1.Lamports || outA.durableK1.Lamports != 444 {
		t.Fatalf("durable end-state differs or wrong: linear=%d fork=%d (want 444)",
			outA.durableK1.Lamports, outB.durableK1.Lamports)
	}
	if outA.overCap != outB.overCap {
		t.Fatalf("OverCap differs")
	}
}

// promote returns the finalized slot's context and prunes; a tip-catching promote
// resets the fork tail so the next slot extends the durable base.
func TestForkTailPromoteToTipThenContinue(t *testing.T) {
	committer := &fakeCommitter{durable: accounts.NewMemAccounts()}
	tail := newForkTail(&fakeDurable{known: map[solana.PublicKey]uint64{}}, committer, 512)

	tail.Add(1, []*accounts.Account{testAccount(1, 10)}, testHashBytes(1))
	tail.SetContext(1, &state.ResumeContext{Slot: 1})
	through, ctx, err := tail.promote(1) // rooted catches the tip
	if err != nil || through != 1 || ctx == nil || ctx.Slot != 1 {
		t.Fatalf("promote-to-tip: through=%d ctx=%+v err=%v", through, ctx, err)
	}

	// the next slot must extend the durable base, not a dead branch
	tail.Add(2, []*accounts.Account{testAccount(2, 20)}, testHashBytes(2))
	if a, _ := tail.GetAccount(2, testKey(2)); a == nil || a.Lamports != 20 {
		t.Fatalf("slot after tip-promote must be readable: %v", a)
	}
	if through, _, err := tail.promote(2); err != nil || through != 2 {
		t.Fatalf("follow-up promote: through=%d err=%v", through, err)
	}
	if a, _ := committer.durable.GetAccountWithoutLock(testKey(2)); a == nil || a.Lamports != 20 {
		t.Fatalf("slot-2 state must be durable: %v", a)
	}
}

// promote(through) where through is below every replayed slot must be a no-op.
func TestForkTailPromoteBelowChain(t *testing.T) {
	committer := &fakeCommitter{durable: accounts.NewMemAccounts()}
	tail := newForkTail(&fakeDurable{known: map[solana.PublicKey]uint64{}}, committer, 512)
	tail.Add(10, []*accounts.Account{testAccount(1, 1)}, testHashBytes(1))
	if through, ctx, err := tail.promote(5); through != 0 || ctx != nil || err != nil {
		t.Fatalf("promote below chain must no-op: %d %v %v", through, ctx, err)
	}
	if len(committer.committed) != 0 {
		t.Fatalf("nothing durable expected: %v", committer.committed)
	}
}

// Partial promote failure must leave the engine retryable: watermark + context stay
// paired, the tree keeps the unpromoted branches, and a retry (idempotent re-commit
// of the durable prefix via the redo path) completes to the tip.
func TestForkTailPromotePartialFailureThenRetry(t *testing.T) {
	committer := &fakeCommitter{durable: accounts.NewMemAccounts()}
	tail := newForkTail(&fakeDurable{known: map[solana.PublicKey]uint64{}}, committer, 512)
	for slot := uint64(1); slot <= 3; slot++ {
		tail.Add(slot, []*accounts.Account{testAccount(byte(slot), slot*10)}, testHashBytes(byte(slot)))
		tail.SetContext(slot, &state.ResumeContext{Slot: slot})
	}

	committer.failOn = 2
	through, ctx, err := tail.promote(3)
	if err == nil {
		t.Fatal("expected partial failure")
	}
	if through != 1 {
		t.Fatalf("watermark should be last durable slot 1, got %d", through)
	}
	// context must be paired with the watermark (slot 1), matching the linear engine
	if ctx == nil || ctx.Slot != 1 {
		t.Fatalf("partial-failure context must be slot 1's, got %+v", ctx)
	}

	committer.failOn = 0
	through, ctx, err = tail.promote(3) // retry: re-commits slot 1-2 idempotently, then 3
	if err != nil || through != 3 || ctx == nil || ctx.Slot != 3 {
		t.Fatalf("retry should complete to tip: through=%d ctx=%+v err=%v", through, ctx, err)
	}
	for b := byte(1); b <= 3; b++ {
		if a, _ := committer.durable.GetAccountWithoutLock(testKey(b)); a == nil || a.Lamports != uint64(b)*10 {
			t.Fatalf("slot %d state must be durable after retry: %v", b, a)
		}
	}
}

// bankhash bytes recorded per slot must round-trip through the fork engine unchanged.
func TestForkTailBankhashRoundTrip(t *testing.T) {
	committer := &fakeCommitter{durable: accounts.NewMemAccounts()}
	tail := newForkTail(&fakeDurable{known: map[solana.PublicKey]uint64{}}, committer, 512)
	tail.Add(1, []*accounts.Account{testAccount(1, 1)}, testHashBytes(0xEE))
	if _, _, err := tail.promote(1); err != nil {
		t.Fatal(err)
	}
	if len(committer.bankhashes) != 1 || !bytes.Equal(committer.bankhashes[0], testHashBytes(0xEE)) {
		t.Fatalf("bankhash must round-trip: %v", committer.bankhashes)
	}
}
