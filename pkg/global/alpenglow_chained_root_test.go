package global

import (
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
)

func TestAlpenglowChainedMerkleRootRoundTrip(t *testing.T) {
	slot := uint64(42)
	root := solana.Hash{4, 5, 6}
	SetAlpenglowChainedMerkleRoot(slot, root)
	got, ok := AlpenglowChainedMerkleRoot(slot)
	assert.True(t, ok)
	assert.Equal(t, root, got)
	_, ok = AlpenglowChainedMerkleRoot(99)
	assert.False(t, ok)
}
