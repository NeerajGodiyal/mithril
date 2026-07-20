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
	t.Cleanup(ResetChainTip)
	parentLtHash := new(lthash.LtHash).InitWithHash(make([]byte, 2048))
	feats := features.NewFeaturesDefault()
	feats.EnableFeature(features.AccountsLtHash, 1)
	InitChainTip(parentLtHash, feats, 10, solana.Hash{})
	initialGeneration := ChainTipParentContext().Generation

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
	statuses := NewTransactionStatusCache().View()
	identity := ChainTipIdentity{
		AlpenglowBlockID:              solana.Hash{3},
		HasAlpenglowBlockID:           true,
		AlpenglowChainedMerkleRoot:    solana.Hash{4},
		HasAlpenglowChainedMerkleRoot: true,
	}
	UpdateChainTipFromSlotCtx(slotCtx, feats, statuses, identity)

	tip := ChainTipParentContext()
	require.Greater(t, tip.Generation, initialGeneration)
	require.Equal(t, uint64(200), tip.Slot)
	require.Equal(t, solana.Hash{7}, tip.Bankhash)
	require.Equal(t, identity.AlpenglowBlockID, tip.AlpenglowBlockID)
	require.True(t, tip.HasAlpenglowBlockID)
	require.Equal(t, identity.AlpenglowChainedMerkleRoot, tip.AlpenglowChainedMerkleRoot)
	require.True(t, tip.HasAlpenglowChainedMerkleRoot)
	require.Equal(t, solana.Hash{9}, tip.LastEntryHash)
	require.Equal(t, uint64(11), tip.PrevNumSigs)
	require.NotNil(t, tip.AcctsLtHash)
	require.True(t, tip.AcctsLtHash.Equals(updated))
	require.True(t, tip.Features.IsActive(features.AccountsLtHash))
	require.Same(t, statuses, tip.TransactionStatuses)
}

func TestResetChainTipClearsTransactionStatuses(t *testing.T) {
	statuses := NewTransactionStatusCache().View()
	UpdateChainTipFromSlotCtx(&sealevel.SlotCtx{Slot: 1}, nil, statuses, ChainTipIdentity{
		AlpenglowBlockID:              solana.Hash{1},
		HasAlpenglowBlockID:           true,
		AlpenglowChainedMerkleRoot:    solana.Hash{2},
		HasAlpenglowChainedMerkleRoot: true,
	})
	before := ChainTipParentContext()
	require.Same(t, statuses, before.TransactionStatuses)

	ResetChainTip()
	after := ChainTipParentContext()
	require.Greater(t, after.Generation, before.Generation)
	require.Nil(t, after.TransactionStatuses)
	require.False(t, after.HasAlpenglowBlockID)
	require.False(t, after.HasAlpenglowChainedMerkleRoot)
}

func TestInitChainTipFailsClosedWithoutCompleteReplayParent(t *testing.T) {
	t.Cleanup(ResetChainTip)
	statuses := NewTransactionStatusCache().View()
	InitChainTip(nil, nil, 0, solana.Hash{}, statuses)

	tip := ChainTipParentContext()
	require.NotZero(t, tip.Generation)
	require.Same(t, statuses, tip.TransactionStatuses)
	require.Zero(t, tip.Slot)
	require.Zero(t, tip.Bankhash)
	require.False(t, tip.HasAlpenglowBlockID)
	require.False(t, tip.HasAlpenglowChainedMerkleRoot)
	require.Nil(t, tip.PrevFeeGovernor)
}
