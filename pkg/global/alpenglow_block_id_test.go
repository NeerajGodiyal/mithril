package global

import (
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
)

func TestAlpenglowBlockIDRoundTrip(t *testing.T) {
	ResetAlpenglowChainMetadata()
	slot := uint64(42)
	id := solana.Hash{1, 2, 3}
	SetAlpenglowBlockID(slot, id)
	got, ok := AlpenglowBlockID(slot)
	assert.True(t, ok)
	assert.Equal(t, id, got)
	_, ok = AlpenglowBlockID(99)
	assert.False(t, ok)
}

func TestPruneAlpenglowBlockIDsBeforeKeepsRoot(t *testing.T) {
	ResetAlpenglowChainMetadata()
	SetAlpenglowBlockID(41, solana.Hash{1})
	SetAlpenglowBlockID(42, solana.Hash{2})
	PruneAlpenglowBlockIDsBefore(42)
	_, oldOK := AlpenglowBlockID(41)
	root, rootOK := AlpenglowBlockID(42)
	assert.False(t, oldOK)
	assert.True(t, rootOK)
	assert.Equal(t, solana.Hash{2}, root)
}
