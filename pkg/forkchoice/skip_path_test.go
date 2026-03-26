package forkchoice

import (
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func hash(b byte) solana.Hash {
	return solana.Hash{b}
}

func TestResolvePohPathSingleBlock(t *testing.T) {
	observed := map[uint64]*ObservedBlockMeta{
		10: {Slot: 10, ParentSlot: 9, ParentSlotKnown: true, Blockhash: hash(0x10)},
	}

	result, err := ResolvePohPath(9, 10, observed, nil, 64)
	require.NoError(t, err)
	assert.Equal(t, []bool{true}, result.Path)
	assert.Equal(t, uint64(10), result.MatchedSlot)
}

func TestResolvePohPathMultipleBlocksWithSkips(t *testing.T) {
	observed := map[uint64]*ObservedBlockMeta{
		11: {Slot: 11, ParentSlot: 9, ParentSlotKnown: true, Blockhash: hash(0x11)},
		13: {Slot: 13, ParentSlot: 11, ParentSlotKnown: true, Blockhash: hash(0x13)},
	}

	result, err := ResolvePohPath(9, 13, observed, nil, 64)
	require.NoError(t, err)
	assert.Equal(t, []bool{false, true, false, true}, result.Path)
	assert.Equal(t, uint64(13), result.MatchedSlot)
}

func TestResolvePohPathMissingObservation(t *testing.T) {
	_, err := ResolvePohPath(9, 13, map[uint64]*ObservedBlockMeta{}, nil, 64)
	assert.ErrorIs(t, err, ErrPathIncomplete)
}

func TestResolvePohPathUnknownParent(t *testing.T) {
	observed := map[uint64]*ObservedBlockMeta{
		13: {Slot: 13, ParentSlotKnown: false, ParentBlockhash: hash(0x11), Blockhash: hash(0x13)},
	}

	_, err := ResolvePohPath(9, 13, observed, nil, 64)
	assert.ErrorIs(t, err, ErrPathIncomplete)
}

func TestResolvePohPathDepthExceeded(t *testing.T) {
	_, err := ResolvePohPath(0, 100, nil, nil, 64)
	assert.ErrorIs(t, err, ErrDepthExceeded)
}

func TestResolvePohPathEquivocation(t *testing.T) {
	observed := map[uint64]*ObservedBlockMeta{
		10: {Slot: 10, ParentSlot: 9, ParentSlotKnown: true, Blockhash: hash(0x10)},
	}
	equivocated := map[uint64]struct{}{10: {}}

	_, err := ResolvePohPath(9, 10, observed, equivocated, 64)
	assert.ErrorIs(t, err, ErrEquivocation)
}

func TestResolvePohPathEndBeforeAnchor(t *testing.T) {
	_, err := ResolvePohPath(10, 5, nil, nil, 64)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "leafSlot 5 < anchorSlot 10")
}
