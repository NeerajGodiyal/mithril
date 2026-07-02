package replay

import (
	"context"
	"fmt"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/state"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDurable is a blockAccountSource: known keys return their value, unknown
// keys return a placeholder (mirroring AccountsDb.GetAccountsBatch). batchCalls
// counts GetAccountsBatch invocations so tests can assert misses are batched.
type fakeDurable struct {
	known      map[solana.PublicKey]uint64
	batchCalls int
}

func (d *fakeDurable) GetAccount(slot uint64, pubkey solana.PublicKey) (*accounts.Account, error) {
	if l, ok := d.known[pubkey]; ok {
		return &accounts.Account{Key: pubkey, Lamports: l}, nil
	}
	return &accounts.Account{Key: pubkey}, nil // placeholder (zero lamports)
}

func (d *fakeDurable) GetAccountsBatch(ctx context.Context, slot uint64, pks []solana.PublicKey) ([]*accounts.Account, error) {
	d.batchCalls++
	out := make([]*accounts.Account, len(pks))
	for i, pk := range pks {
		out[i], _ = d.GetAccount(slot, pk)
	}
	return out, nil
}

func testKey(b byte) solana.PublicKey { return solana.PublicKey{b} }
func testAccount(b byte, lamports uint64) *accounts.Account {
	return &accounts.Account{Key: testKey(b), Lamports: lamports}
}
func testHash(b byte) [32]byte    { var h [32]byte; h[0] = b; return h }
func testHashBytes(b byte) []byte { h := make([]byte, 32); h[0] = b; return h }

// fakeCommitter records CommitSlotAtomic calls and applies deltas to a durable
// MemAccounts, optionally failing on a chosen slot.
type fakeCommitter struct {
	durable    accounts.MemAccounts
	committed  []uint64
	bankhashes [][]byte
	failOn     uint64
}

func (f *fakeCommitter) CommitRootedSlot(accts []*accounts.Account, slot uint64, bankhash []byte) error {
	if f.failOn != 0 && slot == f.failOn {
		return fmt.Errorf("commit boom at slot %d", slot)
	}
	for _, a := range accts {
		_ = f.durable.SetAccountWithoutLock(a.Key, a)
	}
	f.committed = append(f.committed, slot)
	f.bankhashes = append(f.bankhashes, append([]byte(nil), bankhash...))
	return nil
}

// Happy path: rooted prefix is committed durably in slot order and dropped from
// the overlay; the unrooted tail above `through` stays held; bankhashes pruned.
func TestPromoteRootedHappyPath(t *testing.T) {
	durable := accounts.NewMemAccounts()
	overlay := accounts.NewUnrootedOverlay()
	overlay.Add(5, []*accounts.Account{testAccount(1, 51), testAccount(2, 52)})
	overlay.Add(7, []*accounts.Account{testAccount(2, 72)})
	overlay.Add(9, []*accounts.Account{testAccount(3, 93)}) // unrooted tip, stays held

	bankhashes := map[uint64][32]byte{5: testHash(5), 7: testHash(7), 9: testHash(9)}
	fc := &fakeCommitter{durable: durable}

	promoted, err := promoteRooted(overlay, 7, bankhashes, fc)
	require.NoError(t, err)
	assert.Equal(t, uint64(7), promoted)
	assert.Equal(t, []uint64{5, 7}, fc.committed, "committed ascending")
	assert.Equal(t, 1, overlay.HeldSlots(), "only slot 9 remains")
	_, has5 := bankhashes[5]
	_, has9 := bankhashes[9]
	assert.False(t, has5, "promoted bankhash pruned")
	assert.True(t, has9, "unrooted bankhash retained")
}

// Partial failure: a mid-batch commit error must promote ONLY through the last
// durably-committed slot. The failed slot and all later held slots remain in the
// overlay so no fall-through read can see a gap.
func TestPromoteRootedPartialFailureStopsAtLastDurable(t *testing.T) {
	durable := accounts.NewMemAccounts()
	overlay := accounts.NewUnrootedOverlay()
	overlay.Add(5, []*accounts.Account{testAccount(1, 51)})
	overlay.Add(7, []*accounts.Account{testAccount(2, 72)})
	overlay.Add(9, []*accounts.Account{testAccount(3, 93)})

	bankhashes := map[uint64][32]byte{5: testHash(5), 7: testHash(7), 9: testHash(9)}
	fc := &fakeCommitter{durable: durable, failOn: 7}

	promoted, err := promoteRooted(overlay, 9, bankhashes, fc)
	require.Error(t, err, "commit failure surfaces")
	assert.Equal(t, uint64(5), promoted, "advance only to last durable slot")
	assert.Equal(t, []uint64{5}, fc.committed)
	assert.Equal(t, 2, overlay.HeldSlots(), "slots 7 and 9 stay held (not durable)")
	_, has5 := bankhashes[5]
	_, has7 := bankhashes[7]
	assert.False(t, has5, "promoted slot 5 pruned")
	assert.True(t, has7, "un-promoted slot 7 retained")
}

// An empty-delta (skip/empty) slot must still be committed so its bankhash is
// recorded in the durable store.
func TestPromoteRootedEmptyDeltaSlot(t *testing.T) {
	durable := accounts.NewMemAccounts()
	overlay := accounts.NewUnrootedOverlay()
	overlay.Add(5, nil) // empty block

	bankhashes := map[uint64][32]byte{5: testHash(5)}
	fc := &fakeCommitter{durable: durable}

	promoted, err := promoteRooted(overlay, 5, bankhashes, fc)
	require.NoError(t, err)
	assert.Equal(t, uint64(5), promoted)
	assert.Equal(t, []uint64{5}, fc.committed, "empty slot still committed for its bankhash")
	assert.Equal(t, 0, overlay.HeldSlots())
}

// Tail reads: overlay value wins; misses fall through to durable.
func TestUnrootedTailGetAccount(t *testing.T) {
	durable := &fakeDurable{known: map[solana.PublicKey]uint64{testKey(1): 100, testKey(2): 200}}
	tail := newUnrootedTail(durable, &fakeCommitter{}, 512)
	tail.Add(5, []*accounts.Account{testAccount(1, 51)}, testHashBytes(5)) // key 1 written unrooted

	a, err := tail.GetAccount(5, testKey(1))
	require.NoError(t, err)
	assert.Equal(t, uint64(51), a.Lamports, "overlay value wins over durable 100")

	b, err := tail.GetAccount(5, testKey(2))
	require.NoError(t, err)
	assert.Equal(t, uint64(200), b.Lamports, "miss falls through to durable")
}

// Tail batch read: order preserved, overlay hits win, misses come from ONE
// durable batch, placeholder for unknown keys.
func TestUnrootedTailGetAccountsBatch(t *testing.T) {
	durable := &fakeDurable{known: map[solana.PublicKey]uint64{testKey(2): 200}}
	tail := newUnrootedTail(durable, &fakeCommitter{}, 512)
	tail.Add(5, []*accounts.Account{testAccount(1, 51), testAccount(4, 54)}, testHashBytes(5))

	keys := []solana.PublicKey{testKey(1), testKey(2), testKey(3), testKey(4)}
	out, err := tail.GetAccountsBatch(context.Background(), 5, keys)
	require.NoError(t, err)
	require.Len(t, out, 4, "one entry per requested key, in order")
	assert.Equal(t, uint64(51), out[0].Lamports, "overlay hit")
	assert.Equal(t, uint64(200), out[1].Lamports, "durable hit")
	assert.Equal(t, uint64(0), out[2].Lamports, "unknown -> placeholder")
	assert.Equal(t, uint64(54), out[3].Lamports, "overlay hit")
	assert.Equal(t, 1, durable.batchCalls, "misses fetched in a single durable batch")
}

// OverCap trips only when held slots exceed the cap (backpressure on stalled rooting).
func TestUnrootedTailOverCap(t *testing.T) {
	tail := newUnrootedTail(&fakeDurable{}, &fakeCommitter{}, 2)
	tail.Add(1, nil, testHashBytes(1))
	tail.Add(2, nil, testHashBytes(2))
	assert.False(t, tail.OverCap(), "2 held == cap, not over")
	tail.Add(3, nil, testHashBytes(3))
	assert.True(t, tail.OverCap(), "3 held > cap 2")
}

// promote returns the resume context as of the highest promoted slot and prunes
// the context map for promoted slots, retaining still-held ones.
func TestUnrootedTailContextCaptureAndPromote(t *testing.T) {
	tail := newUnrootedTail(&fakeDurable{}, &fakeCommitter{durable: accounts.NewMemAccounts()}, 512)
	tail.Add(5, []*accounts.Account{testAccount(1, 51)}, testHashBytes(5))
	tail.SetContext(5, &state.ResumeContext{Slot: 5, Bankhash: "bh5"})
	tail.Add(7, []*accounts.Account{testAccount(2, 72)}, testHashBytes(7))
	tail.SetContext(7, &state.ResumeContext{Slot: 7, Bankhash: "bh7"})
	tail.Add(9, []*accounts.Account{testAccount(3, 93)}, testHashBytes(9))
	tail.SetContext(9, &state.ResumeContext{Slot: 9, Bankhash: "bh9"})

	promotedThrough, ctx, err := tail.promote(7)
	require.NoError(t, err)
	assert.Equal(t, uint64(7), promotedThrough)
	require.NotNil(t, ctx, "promote returns the as-of-R context")
	assert.Equal(t, uint64(7), ctx.Slot, "context is for the highest promoted slot (R)")

	_, has5 := tail.contexts[5]
	_, has7 := tail.contexts[7]
	_, has9 := tail.contexts[9]
	assert.False(t, has5, "promoted context pruned")
	assert.False(t, has7, "promoted context pruned")
	assert.True(t, has9, "still-held context retained")
}

// Nothing to promote (through below all held slots) is a no-op, not an error.
func TestPromoteRootedNoPrefix(t *testing.T) {
	durable := accounts.NewMemAccounts()
	overlay := accounts.NewUnrootedOverlay()
	overlay.Add(10, []*accounts.Account{testAccount(1, 10)})

	fc := &fakeCommitter{durable: durable}
	promoted, err := promoteRooted(overlay, 5, map[uint64][32]byte{}, fc)
	require.NoError(t, err)
	assert.Equal(t, uint64(0), promoted)
	assert.Empty(t, fc.committed)
	assert.Equal(t, 1, overlay.HeldSlots())
}
