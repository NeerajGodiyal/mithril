package rent

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerifyRentStateChangesReturnsOffendingAccountIndex(t *testing.T) {
	rent := sealevel.NewDefaultRentSysvar()
	first := solana.PublicKey{1, 1}
	second := solana.PublicKey{2, 2}
	txAccts := sealevel.NewTransactionAccounts([]accounts.Account{
		{Key: first},
		{Key: second, Lamports: rent.MinimumBalance(100), Data: make([]byte, 100)},
	})
	txAccts.AcctMetas = []*sealevel.AccountMeta{{Pubkey: first, IsWritable: true}, {Pubkey: second, IsWritable: true}}
	txCtx := sealevel.NewTransactionCtx(*txAccts, 1, 1)
	feats := features.NewFeaturesDefault()
	pre := NewRentStateInfo(&rent, txCtx, feats)
	txCtx.Accounts.Accounts[1].Lamports = 1
	post := NewRentStateInfo(&rent, txCtx, feats)

	idx, err := VerifyRentStateChanges(pre, post, txCtx)
	require.Error(t, err)
	assert.Equal(t, uint8(1), idx)
}
