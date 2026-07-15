package replay

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/lthash"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestChainTipTracksReplayedSlot(t *testing.T) {
	parentLtHash := new(lthash.LtHash).InitWithHash(make([]byte, 2048))
	feats := features.NewFeaturesDefault()
	feats.EnableFeature(features.AccountsLtHash, 1)
	InitChainTip(parentLtHash, feats, 10, solana.Hash{})

	updated := new(lthash.LtHash).InitWithHash(make([]byte, 2048))
	updated.Add(parentLtHash)
	slotCtx := &sealevel.SlotCtx{
		Slot:          200,
		NumSignatures: 11,
		AcctsLtHash:   updated,
		Features:      feats,
		FinalBankhash: append([]byte{7}, make([]byte, 31)...),
		Blockhash:     solana.Hash{9},
	}
	UpdateChainTipFromSlotCtx(slotCtx, feats)

	tip := ChainTipParentContext()
	require.Equal(t, uint64(200), tip.Slot)
	require.Equal(t, solana.Hash{7}, tip.Bankhash)
	require.Equal(t, solana.Hash{9}, tip.LastEntryHash)
	require.Equal(t, uint64(11), tip.PrevNumSigs)
	require.NotNil(t, tip.AcctsLtHash)
	require.True(t, tip.AcctsLtHash.Equals(updated))
	require.True(t, tip.Features.IsActive(features.AccountsLtHash))
}
