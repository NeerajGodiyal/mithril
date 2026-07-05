package sealevel

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

// TestGetAccountLiveOrPersistedPrefersLiveState guards the fix for footer/reward code
// reading stale parent state: when a same-slot write exists in the working set, the
// live-first accessor must return it (e.g. a Vote Withdraw that reduced lamports),
// not the persisted parent version.
func TestGetAccountLiveOrPersistedPrefersLiveState(t *testing.T) {
	mem := accounts.NewMemAccounts()
	slotCtx := &SlotCtx{Slot: 100, Accounts: mem}

	pk := solana.PublicKey{1, 2, 3}
	require.NoError(t, slotCtx.SetAccount(pk, &accounts.Account{Key: pk, Lamports: 100}))

	got, err := slotCtx.GetAccountLiveOrPersisted(pk)
	require.NoError(t, err)
	require.Equal(t, uint64(100), got.Lamports)

	// A same-slot mutation (as a transaction would do) must be visible.
	require.NoError(t, slotCtx.SetAccount(pk, &accounts.Account{Key: pk, Lamports: 50}))
	got, err = slotCtx.GetAccountLiveOrPersisted(pk)
	require.NoError(t, err)
	require.Equal(t, uint64(50), got.Lamports)

	// The returned account must be an independent copy: mutating it in place (as the reward
	// path does) must not corrupt the backing store, or a later parent-baseline read would
	// observe the mutation and zero out the lt-hash delta.
	got.Lamports = 999
	got.Data = []byte{1, 2, 3}
	fresh, err := slotCtx.GetAccountLiveOrPersisted(pk)
	require.NoError(t, err)
	require.Equal(t, uint64(50), fresh.Lamports)
	require.Empty(t, fresh.Data)
}

// TestGetAccountLiveOrPersistedFallsBackWhenAbsent verifies that an account not in the
// working set falls through to the persisted store (here nil AccountsDb -> error),
// which is the path reward-only vote accounts take.
func TestGetAccountLiveOrPersistedFallsBackWhenAbsent(t *testing.T) {
	slotCtx := &SlotCtx{Slot: 100, Accounts: accounts.NewMemAccounts()}
	_, err := slotCtx.GetAccountLiveOrPersisted(solana.PublicKey{9, 9, 9})
	require.Error(t, err)
}
