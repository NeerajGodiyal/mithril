package global

import (
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
)

func TestAlpenglowBlockIDRoundTrip(t *testing.T) {
	slot := uint64(42)
	id := solana.Hash{1, 2, 3}
	SetAlpenglowBlockID(slot, id)
	got, ok := AlpenglowBlockID(slot)
	assert.True(t, ok)
	assert.Equal(t, id, got)
	_, ok = AlpenglowBlockID(99)
	assert.False(t, ok)
}
