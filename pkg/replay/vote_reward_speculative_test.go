package replay

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestLoadAccountLiveOrParentForReplayUsesSpeculativeStore(t *testing.T) {
	spec := NewSpeculativeReplay()
	spec.Enable()
	spec.store.SetFinalizedSlot(100)

	parentSlot := uint64(101)
	pk := solana.PublicKey{7, 7, 7}
	parentAcct := &accounts.Account{Key: pk, Lamports: 42}

	layer := &SpeculativeLayer{
		Slot:       parentSlot,
		ParentSlot: 100,
		Deltas:     map[solana.PublicKey]*accounts.Account{pk: parentAcct},
	}
	spec.store.mu.Lock()
	spec.store.layers[parentSlot] = layer
	spec.store.mu.Unlock()

	slotCtx := &sealevel.SlotCtx{
		Slot:       102,
		ParentSlot: parentSlot,
		Accounts:   accounts.NewMemAccounts(),
	}

	got, err := loadAccountLiveOrParentForReplay(nil, spec, slotCtx, pk)
	require.NoError(t, err)
	require.Equal(t, uint64(42), got.Lamports)

	require.NoError(t, slotCtx.SetAccount(pk, &accounts.Account{Key: pk, Lamports: 99}))
	live, err := loadAccountLiveOrParentForReplay(nil, spec, slotCtx, pk)
	require.NoError(t, err)
	require.Equal(t, uint64(99), live.Lamports)
}
