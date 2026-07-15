package global

import (
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
)

func TestAlpenglowChainedMerkleRootRoundTrip(t *testing.T) {
	ResetAlpenglowChainMetadata()
	slot := uint64(42)
	root := solana.Hash{4, 5, 6}
	SetAlpenglowChainedMerkleRoot(slot, root)
	got, ok := AlpenglowChainedMerkleRoot(slot)
	assert.True(t, ok)
	assert.Equal(t, root, got)
	_, ok = AlpenglowChainedMerkleRoot(99)
	assert.False(t, ok)
}

func TestPruneAlpenglowChainedRootsBeforeKeepsRoot(t *testing.T) {
	ResetAlpenglowChainMetadata()
	SetAlpenglowChainedMerkleRoot(41, solana.Hash{1})
	SetAlpenglowChainedMerkleRoot(42, solana.Hash{2})
	PruneAlpenglowChainedRootsBefore(42)
	_, oldOK := AlpenglowChainedMerkleRoot(41)
	root, rootOK := AlpenglowChainedMerkleRoot(42)
	assert.False(t, oldOK)
	assert.True(t, rootOK)
	assert.Equal(t, solana.Hash{2}, root)
}
