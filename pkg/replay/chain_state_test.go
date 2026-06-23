package replay

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/lthash"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestChainTipTracksLocalLeaderCommit(t *testing.T) {
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
	}
	RegisterLocalLeaderCommit(slotCtx)

	tip := ChainTipParentContext()
	require.Equal(t, uint64(11), tip.PrevNumSigs)
	require.NotNil(t, tip.AcctsLtHash)
	require.True(t, tip.AcctsLtHash.Equals(updated))
	require.True(t, tip.Features.IsActive(features.AccountsLtHash))
}

func TestLocalLeaderCommitPreservesAcctsLtHashForReplay(t *testing.T) {
	parentLtHash := new(lthash.LtHash).InitWithHash(make([]byte, 2048))
	slotCtx := &sealevel.SlotCtx{
		Slot:        300,
		AcctsLtHash: parentLtHash,
	}
	RegisterLocalLeaderCommit(slotCtx)

	commit, ok := TakeLocalLeaderCommit(300)
	require.True(t, ok)
	require.NotNil(t, commit.SlotCtx.AcctsLtHash)
	require.True(t, commit.SlotCtx.AcctsLtHash.Equals(parentLtHash))
}
