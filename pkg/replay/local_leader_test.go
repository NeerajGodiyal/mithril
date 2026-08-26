package replay

import (
	"errors"
	"sync"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/rootedevents"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/state"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestRegisterTakeLocalLeaderCommit(t *testing.T) {
	t.Cleanup(ResetLocalLeaderCommits)
	slotCtx := &sealevel.SlotCtx{Slot: 7, FinalBankhash: []byte{1, 2, 3}}
	RegisterLocalLeaderCommit(slotCtx)

	got, ok := TakeLocalLeaderCommit(7)
	require.True(t, ok)
	require.Same(t, slotCtx, got.SlotCtx)

	_, ok = TakeLocalLeaderCommit(7)
	require.False(t, ok)
}

func TestResetChainTipDropsLocalLeaderCommits(t *testing.T) {
	t.Cleanup(ResetChainTip)
	RegisterLocalLeaderCommit(&sealevel.SlotCtx{Slot: 9})
	ResetChainTip()
	_, ok := TakeLocalLeaderCommit(9)
	require.False(t, ok)
}

func TestAdoptLocalLeaderBlockUsesProducerSlotCtx(t *testing.T) {
	t.Cleanup(ResetLocalLeaderCommits)

	key := solana.PublicKey{0x11}
	acct := &accounts.Account{Key: key, Lamports: 42, Data: []byte{9}}
	slotCtx := &sealevel.SlotCtx{
		Slot:          20,
		Accounts:      accounts.NewMemAccounts(),
		ModifiedAccts: map[solana.PublicKey]bool{key: true},
		FinalBankhash: append([]byte{8}, make([]byte, 31)...),
	}
	require.NoError(t, slotCtx.SetAccount(key, acct))
	bankSysvars, err := sealevel.NewBankSysvars(20, &accounts.Account{
		Key:      sealevel.SysvarClockAddr,
		Lamports: 1,
		Data:     (&sealevel.SysvarClock{Slot: 20}).MustMarshal(),
	})
	require.NoError(t, err)
	require.NoError(t, slotCtx.PublishBankSysvars(bankSysvars))
	RegisterLocalLeaderCommit(slotCtx)

	block := &b.Block{Slot: 20, ParentSlot: 19, FromLocalProduction: true}
	persisted := &persistedTracker{}
	got, err := adoptLocalLeaderBlock(block, nil, NewTransactionStatusCache(), persisted)
	require.NoError(t, err)
	require.Same(t, slotCtx, got)

	slot, hash := persisted.Get()
	require.Equal(t, uint64(20), slot)
	require.Equal(t, slotCtx.FinalBankhash, hash)

	_, ok := TakeLocalLeaderCommit(20)
	require.False(t, ok, "adopt must consume the registered SlotCtx")
}

func TestAdoptLocalLeaderBlockFailsClosedWithoutCommit(t *testing.T) {
	t.Cleanup(ResetLocalLeaderCommits)
	_, err := adoptLocalLeaderBlock(&b.Block{Slot: 3, FromLocalProduction: true}, nil, NewTransactionStatusCache(), nil)
	require.Error(t, err)
	require.ErrorContains(t, err, "missing producer SlotCtx")
}

func TestAdoptLocalLeaderBlockFailsClosedWithoutBankSysvars(t *testing.T) {
	t.Cleanup(ResetLocalLeaderCommits)
	RegisterLocalLeaderCommit(&sealevel.SlotCtx{Slot: 4})
	_, err := adoptLocalLeaderBlock(&b.Block{Slot: 4, FromLocalProduction: true}, nil, NewTransactionStatusCache(), nil)
	require.Error(t, err)
	require.ErrorContains(t, err, "missing bank sysvars")
}

func TestAdoptLocalLeaderBlockUsesOwnedExactFinalizedDelta(t *testing.T) {
	t.Cleanup(ResetLocalLeaderCommits)

	const slot = uint64(20)
	slotCtx := localLeaderTestSlotCtx(t, slot)
	rentOnly := &accounts.Account{Key: solana.PublicKey{0x44}, Lamports: 41, Data: []byte{1, 2, 3}}
	RegisterLocalLeaderCommitData(slotCtx, []*accounts.Account{rentOnly}, true, nil, true)
	rentOnly.Lamports = 99
	rentOnly.Data[0] = 9

	tail := rootedCaptureTestTail(t)
	block := &b.Block{Slot: slot, ParentSlot: slot - 1, FromLocalProduction: true}
	_, err := adoptLocalLeaderBlock(block, tail, NewTransactionStatusCache(), &persistedTracker{})
	require.NoError(t, err)

	deltas := tail.overlay.PromotionPrefix(slot)
	require.Len(t, deltas, 1)
	require.Len(t, deltas[0].Delta, 1)
	require.Equal(t, uint64(41), deltas[0].Delta[0].Lamports)
	require.Equal(t, []byte{1, 2, 3}, deltas[0].Delta[0].Data)
	observations, ok := tail.transactions[slot]
	require.True(t, ok, "zero-transaction slots must still record capture presence")
	require.Empty(t, observations)
}

func TestAdoptLocalLeaderBlockRejectsObservationMismatchBeforeTailMutation(t *testing.T) {
	t.Cleanup(ResetLocalLeaderCommits)

	const slot = uint64(21)
	slotCtx := localLeaderTestSlotCtx(t, slot)
	RegisterLocalLeaderCommitData(slotCtx, nil, true, nil, true)
	tail := rootedCaptureTestTail(t)
	block := &b.Block{
		Slot: slot, ParentSlot: slot - 1, FromLocalProduction: true,
		Transactions: []*solana.Transaction{{}},
	}
	_, err := adoptLocalLeaderBlock(block, tail, NewTransactionStatusCache(), &persistedTracker{})
	require.ErrorContains(t, err, "0 rooted transaction observations for 1 transactions")
	require.Zero(t, tail.overlay.HeldSlots())
	require.NotContains(t, tail.transactions, slot)
}

func TestAdoptLocalLeaderBlockRequiresExactDeltaForRootedTail(t *testing.T) {
	t.Cleanup(ResetLocalLeaderCommits)
	global.ClearPendingStakePubkeys()
	t.Cleanup(global.ClearPendingStakePubkeys)

	const slot = uint64(22)
	voteKey := solana.PublicKey{0x91}
	stakeKey := solana.PublicKey{0x92}
	global.DeleteVoteCacheItem(voteKey)
	t.Cleanup(func() { global.DeleteVoteCacheItem(voteKey) })
	slotCtx := localLeaderTestSlotCtx(t, slot)
	slotCtx.RecordVoteCacheUpdate(voteKey, &sealevel.VoteStateVersions{})
	slotCtx.RecordPendingStakePubkey(stakeKey)
	RegisterLocalLeaderCommit(slotCtx)
	tail := rootedCaptureTestTail(t)
	_, err := adoptLocalLeaderBlock(
		&b.Block{Slot: slot, ParentSlot: slot - 1, FromLocalProduction: true},
		tail,
		NewTransactionStatusCache(),
		&persistedTracker{},
	)
	require.ErrorContains(t, err, "exact finalized account delta was not captured")
	require.Zero(t, tail.overlay.HeldSlots())
	require.Nil(t, global.VoteCacheItem(voteKey))
	require.Empty(t, global.PendingStakeEntriesSnapshot())
}

type rejectingRootedEventTail struct {
	*unrootedTail
	err   error
	calls int
}

func (t *rejectingRootedEventTail) RecordRootedEventSlot(uint64, uint64, []rootedevents.TransactionObservation) error {
	t.calls++
	return t.err
}

func TestAdoptLocalLeaderBlockStatusFailurePublishesNothing(t *testing.T) {
	t.Cleanup(ResetLocalLeaderCommits)

	cache := NewTransactionStatusCache()
	require.NoError(t, cache.CommitBlock(statusCacheTestBlock(10)))
	const slot = uint64(12)
	slotCtx := localLeaderTestSlotCtx(t, slot)
	RegisterLocalLeaderCommitData(slotCtx, nil, true, nil, true)
	baseTail := rootedCaptureTestTail(t)
	tail := &rejectingRootedEventTail{unrootedTail: baseTail, err: errors.New("must not be called")}
	persisted := &persistedTracker{}

	_, err := adoptLocalLeaderBlock(
		&b.Block{Slot: slot, ParentSlot: 11, FromLocalProduction: true},
		tail,
		cache,
		persisted,
	)
	require.Error(t, err)
	require.Zero(t, tail.calls)
	require.Zero(t, baseTail.overlay.HeldSlots())
	require.NotContains(t, baseTail.transactions, slot)
	persistedSlot, persistedHash := persisted.Get()
	require.Zero(t, persistedSlot)
	require.Empty(t, persistedHash)
	tip, ok := cache.TipSlot()
	require.True(t, ok)
	require.Equal(t, uint64(10), tip)
}

func TestAdoptLocalLeaderBlockEventFailureRestoresStatusAndPublishesNothing(t *testing.T) {
	t.Cleanup(ResetLocalLeaderCommits)

	cache := NewTransactionStatusCache()
	require.NoError(t, cache.CommitBlock(statusCacheTestBlock(10)))
	const slot = uint64(11)
	slotCtx := localLeaderTestSlotCtx(t, slot)
	RegisterLocalLeaderCommitData(slotCtx, nil, true, nil, true)
	baseTail := rootedCaptureTestTail(t)
	recordErr := errors.New("rooted event limit")
	tail := &rejectingRootedEventTail{unrootedTail: baseTail, err: recordErr}
	persisted := &persistedTracker{}

	_, err := adoptLocalLeaderBlock(
		&b.Block{Slot: slot, ParentSlot: 10, FromLocalProduction: true},
		tail,
		cache,
		persisted,
	)
	require.ErrorIs(t, err, recordErr)
	require.Equal(t, 1, tail.calls)
	require.Zero(t, baseTail.overlay.HeldSlots())
	require.NotContains(t, baseTail.transactions, slot)
	persistedSlot, persistedHash := persisted.Get()
	require.Zero(t, persistedSlot)
	require.Empty(t, persistedHash)
	tip, ok := cache.TipSlot()
	require.True(t, ok)
	require.Equal(t, uint64(10), tip)
}

func rootedCaptureTestTail(t *testing.T) *unrootedTail {
	t.Helper()
	tail := newUnrootedTail(nil, nil, 512, 2, "")
	require.NoError(t, tail.SetRootedEventHooks(RootedEventHooks{
		Install: func([]accounts.SlotDelta, map[uint64]rootedevents.SlotMeta) (*state.RootedEventBatchRef, error) {
			return nil, nil
		},
	}))
	return tail
}

func localLeaderTestSlotCtx(t *testing.T, slot uint64) *sealevel.SlotCtx {
	t.Helper()
	slotCtx := &sealevel.SlotCtx{
		Slot:          slot,
		Accounts:      accounts.NewMemAccounts(),
		AcctMapsMu:    &sync.Mutex{},
		ModifiedAccts: make(map[solana.PublicKey]bool),
		FinalBankhash: append([]byte{8}, make([]byte, 31)...),
	}
	bankSysvars, err := sealevel.NewBankSysvars(slot, &accounts.Account{
		Key:      sealevel.SysvarClockAddr,
		Lamports: 1,
		Data:     (&sealevel.SysvarClock{Slot: slot}).MustMarshal(),
	})
	require.NoError(t, err)
	require.NoError(t, slotCtx.PublishBankSysvars(bankSysvars))
	return slotCtx
}
