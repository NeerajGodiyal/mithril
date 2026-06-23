package blockprod

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/lthash"
	"github.com/stretchr/testify/require"
)

func TestNewLeaderSlotCtxInheritsAcctsLtHashAndFeatures(t *testing.T) {
	parentLtHash := new(lthash.LtHash).InitWithHash(make([]byte, 2048))
	parentFeatures := features.NewFeaturesDefault()
	parentFeatures.EnableFeature(features.AccountsLtHash, 1)
	parentFeatures.EnableFeature(features.RemoveAccountsDeltaHash, 2)

	slotCtx, err := NewLeaderSlotCtx(100, 99, nil, ParentContext{
		PrevNumSigs: 42,
		AcctsLtHash: parentLtHash,
		Features:    parentFeatures,
	})
	require.NoError(t, err)
	require.NotNil(t, slotCtx.AcctsLtHash)
	require.True(t, slotCtx.AcctsLtHash.Equals(parentLtHash))
	require.True(t, slotCtx.Features.IsActive(features.AccountsLtHash))
	require.True(t, slotCtx.Features.IsActive(features.RemoveAccountsDeltaHash))
	require.True(t, slotCtx.Features.IsActive(features.FormalizeLoadedTransactionDataSize))
	require.Equal(t, uint64(42), slotCtx.NumSignatures)
}
