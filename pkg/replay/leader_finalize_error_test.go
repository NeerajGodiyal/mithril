package replay

import (
	"errors"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

var errTestLeaderParentRead = errors.New("test leader parent read failure")

type failingLeaderParentReader struct{}

func (failingLeaderParentReader) GetAccount(uint64, solana.PublicKey) (*accounts.Account, error) {
	return nil, errTestLeaderParentRead
}

func TestEnsureParentAccountsRejectsStorageFailure(t *testing.T) {
	feats := features.NewFeaturesDefault()
	feats.EnableFeature(features.AccountsLtHash, 0)
	key := solana.PublicKey{1}
	slotCtx := &sealevel.SlotCtx{
		Features:     feats,
		ParentAccts:  accounts.NewMemAccounts(),
		UnrootedRead: failingLeaderParentReader{},
	}

	err := ensureParentAccountsForModified(slotCtx, []*accounts.Account{{Key: key, Lamports: 1}})
	require.ErrorIs(t, err, errTestLeaderParentRead)
	_, parentErr := slotCtx.GetParentAccount(key)
	require.Error(t, parentErr)
}
