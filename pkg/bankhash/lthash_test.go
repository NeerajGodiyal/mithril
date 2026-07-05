package bankhash

import (
	"sync"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/lthash"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestApplyInPlaceAccountLtHashDeltaMatchesBatchDelta(t *testing.T) {
	featuresMap := features.NewFeaturesDefault()
	featuresMap.EnableFeature(features.AccountsLtHash, 0)

	parent := &accounts.Account{
		Key:      solana.NewWallet().PublicKey(),
		Owner:    solana.NewWallet().PublicKey(),
		Lamports: 1,
		Data:     []byte("parent"),
	}
	parentAccts := accounts.NewMemAccountsWithLen(1)
	require.NoError(t, parentAccts.SetAccountWithoutLock([32]byte(parent.Key), parent.Clone()))

	slotCtx := &sealevel.SlotCtx{
		ParentAccts:  parentAccts,
		AcctsLtHash:  &lthash.LtHash{},
		Features:     featuresMap,
		AcctMapsMu:   &sync.Mutex{},
		ModifiedAccts: make(map[solana.PublicKey]bool),
	}

	acct := parent.Clone()
	err := ApplyInPlaceAccountLtHashDelta(slotCtx, acct, func() error {
		acct.Data = []byte("modified")
		return nil
	})
	require.NoError(t, err)
	require.True(t, slotCtx.LtHashAlreadyApplied(acct.Key))

	var expected lthash.LtHash
	var old lthash.LtHash
	old.InitWithAcct(parent)
	expected.Sub(&old)
	var new lthash.LtHash
	new.InitWithAcct(acct)
	expected.Add(&new)

	require.True(t, slotCtx.AcctsLtHash.Equals(&expected))

	batchDelta := calculateSingleDeltaLtHash(slotCtx, acct)
	require.True(t, batchDelta.Equals(&lthash.LtHash{}))
}
