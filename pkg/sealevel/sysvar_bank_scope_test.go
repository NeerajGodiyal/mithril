package sealevel

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/stretchr/testify/require"
)

func TestReadClockSysvarPrefersBankLocalAccount(t *testing.T) {
	previous := SysvarCache.Clock
	t.Cleanup(func() { SysvarCache.Clock = previous })

	parentClock := SysvarClock{Slot: 41, Epoch: 2, UnixTimestamp: 1_700_000_000}
	parentAccount := clockSysvarTestAccount(parentClock)
	SysvarCache.Clock.Sysvar = &parentClock
	SysvarCache.Clock.Acct = parentAccount

	bankClock := SysvarClock{Slot: 42, Epoch: 2, UnixTimestamp: 1_700_000_000}
	bankAccounts := accounts.NewMemAccounts()
	require.NoError(t, bankAccounts.SetAccount(&SysvarClockAddr, clockSysvarTestAccount(bankClock)))

	execCtx := &ExecutionCtx{
		Accounts: accounts.NewMemAccounts(),
		SlotCtx: &SlotCtx{
			Accounts: bankAccounts,
			// Leader banks are deliberately speculative and therefore do not set
			// Replay. Their transactions must still observe their own bank sysvars.
			Replay: false,
		},
	}

	got, err := ReadClockSysvar(execCtx)
	require.NoError(t, err)
	require.Equal(t, bankClock, got)
}

func TestGetSysvarBytesPrefersBankLocalAccount(t *testing.T) {
	previous := SysvarCache.Clock
	t.Cleanup(func() { SysvarCache.Clock = previous })

	parentClock := SysvarClock{Slot: 41, Epoch: 2, UnixTimestamp: 1_700_000_000}
	SysvarCache.Clock.Sysvar = &parentClock
	SysvarCache.Clock.Acct = clockSysvarTestAccount(parentClock)

	bankClock := SysvarClock{Slot: 42, Epoch: 2, UnixTimestamp: 1_700_000_000}
	bankAccounts := accounts.NewMemAccounts()
	require.NoError(t, bankAccounts.SetAccount(&SysvarClockAddr, clockSysvarTestAccount(bankClock)))

	execCtx := &ExecutionCtx{
		Accounts: accounts.NewMemAccounts(),
		SlotCtx:  &SlotCtx{Accounts: bankAccounts, Replay: false},
	}

	got, err := fetchSysvarBytesForPubkey(execCtx, SysvarClockAddr)
	require.NoError(t, err)
	require.Equal(t, bankClock.MustMarshal(), got)
}

func TestReadClockSysvarFallsBackWhenBankHasNoClock(t *testing.T) {
	previous := SysvarCache.Clock
	t.Cleanup(func() { SysvarCache.Clock = previous })

	cachedClock := SysvarClock{Slot: 41, Epoch: 2, UnixTimestamp: 1_700_000_000}
	SysvarCache.Clock.Sysvar = &cachedClock
	SysvarCache.Clock.Acct = clockSysvarTestAccount(cachedClock)

	got, err := ReadClockSysvar(&ExecutionCtx{
		SlotCtx: &SlotCtx{Accounts: accounts.NewMemAccounts(), Replay: false},
	})
	require.NoError(t, err)
	require.Equal(t, cachedClock, got)

	got, err = ReadClockSysvar(nil)
	require.NoError(t, err)
	require.Equal(t, cachedClock, got)
}

func TestReadSlotHashesSysvarPrefersBankLocalAccount(t *testing.T) {
	previous := SysvarCache.SlotHashes
	t.Cleanup(func() { SysvarCache.SlotHashes = previous })

	parentSlotHashes := SysvarSlotHashes{{Slot: 41, Hash: [32]byte{41}}}
	SysvarCache.SlotHashes.Sysvar = &parentSlotHashes
	SysvarCache.SlotHashes.Acct = slotHashesSysvarTestAccount(parentSlotHashes)

	bankSlotHashes := SysvarSlotHashes{{Slot: 42, Hash: [32]byte{42}}}
	bankAccounts := accounts.NewMemAccounts()
	require.NoError(t, bankAccounts.SetAccount(&SysvarSlotHashesAddr, slotHashesSysvarTestAccount(bankSlotHashes)))

	got, err := ReadSlotHashesSysvar(&ExecutionCtx{
		Accounts: accounts.NewMemAccounts(),
		SlotCtx:  &SlotCtx{Accounts: bankAccounts, Replay: false},
	})
	require.NoError(t, err)
	require.Equal(t, bankSlotHashes, got)
}

func clockSysvarTestAccount(clock SysvarClock) *accounts.Account {
	return &accounts.Account{
		Key:      SysvarClockAddr,
		Lamports: 1,
		Data:     clock.MustMarshal(),
	}
}

func slotHashesSysvarTestAccount(slotHashes SysvarSlotHashes) *accounts.Account {
	return &accounts.Account{
		Key:      SysvarSlotHashesAddr,
		Lamports: 1,
		Data:     slotHashes.MustMarshal(),
	}
}
