package consensus

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/alpenglow"
	"github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestExecutedReplayProofsAreCardinalityAndRootBounded(t *testing.T) {
	engine, err := NewEngine(Config{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, engine.Close()) })
	parent := alpenglow.BlockID{Slot: 39, Hash: solana.Hash{0x39}}

	engine.observedReplayMu.Lock()
	for i := 0; i < maxRecentAlpenglowBlockIDs+32; i++ {
		var hash solana.Hash
		binary.BigEndian.PutUint64(hash[:8], uint64(i+1))
		retainExecutedBlockID(engine.executedReplayBlocks, alpenglow.BlockID{Slot: 40, Hash: hash}, parent)
	}
	require.Len(t, engine.executedReplayBlocks, maxRecentAlpenglowBlockIDs)
	newer := alpenglow.BlockID{Slot: 41, Hash: solana.Hash{0x41}}
	newerParent := alpenglow.BlockID{Slot: 40, Hash: solana.Hash{0x40}}
	retainExecutedBlockID(engine.executedReplayBlocks, newer, newerParent)
	require.Len(t, engine.executedReplayBlocks, maxRecentAlpenglowBlockIDs)
	require.Equal(t, newerParent, engine.executedReplayBlocks[newer])
	tooOld := alpenglow.BlockID{Slot: 38, Hash: solana.Hash{0xff}}
	retainExecutedBlockID(engine.executedReplayBlocks, tooOld, alpenglow.BlockID{})
	require.NotContains(t, engine.executedReplayBlocks, tooOld)
	engine.observedReplayMu.Unlock()

	engine.pruneExecutedReplayBlocksAtOrBelow(40)
	engine.observedReplayMu.Lock()
	require.Equal(t, map[alpenglow.BlockID]alpenglow.BlockID{newer: newerParent}, engine.executedReplayBlocks)
	engine.observedReplayMu.Unlock()
}

func TestRepeatedReplayDoesNotDowngradeExactParentProof(t *testing.T) {
	engine, err := NewEngine(Config{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, engine.Close()) })
	parent := alpenglow.BlockID{Slot: 39, Hash: solana.Hash{0x39}}
	child := alpenglow.BlockID{Slot: 40, Hash: solana.Hash{0x40}}

	exact := &block.Block{
		Slot:                      child.Slot,
		ParentSlot:                parent.Slot,
		AlpenglowBlockID:          [32]byte(child.Hash),
		HasAlpenglowBlockID:       true,
		AlpenglowParentBlockID:    [32]byte(parent.Hash),
		HasAlpenglowParentBlockID: true,
	}
	require.NoError(t, engine.ObserveBlock(context.Background(), BlockObservation{Block: exact, Source: "exact"}))
	require.NoError(t, engine.OnReplayResult(context.Background(), SlotReplayResult{Slot: child.Slot, Source: "exact"}))

	hashless := &block.Block{
		Slot:                child.Slot,
		ParentSlot:          parent.Slot,
		AlpenglowBlockID:    [32]byte(child.Hash),
		HasAlpenglowBlockID: true,
	}
	require.NoError(t, engine.ObserveBlock(context.Background(), BlockObservation{Block: hashless, Source: "hashless"}))
	require.NoError(t, engine.OnReplayResult(context.Background(), SlotReplayResult{Slot: child.Slot, Source: "hashless"}))

	engine.observedReplayMu.Lock()
	require.Equal(t, parent, engine.executedReplayBlocks[child])
	engine.observedReplayMu.Unlock()
}

func TestExecutedReplayParentEvidenceRefinesAndRejectsConflicts(t *testing.T) {
	child := alpenglow.BlockID{Slot: 40, Hash: solana.Hash{0x40}}
	unknown := alpenglow.BlockID{Slot: 39}
	exact := alpenglow.BlockID{Slot: 39, Hash: solana.Hash{0x39}}
	proofs := map[alpenglow.BlockID]alpenglow.BlockID{child: unknown}

	require.NoError(t, retainExecutedReplayParent(proofs, child, exact))
	require.Equal(t, exact, proofs[child])
	require.NoError(t, retainExecutedReplayParent(proofs, child, unknown))
	require.Equal(t, exact, proofs[child])
	require.ErrorContains(t, retainExecutedReplayParent(proofs, child,
		alpenglow.BlockID{Slot: exact.Slot, Hash: solana.Hash{0xff}}), "conflicting exact parents")
	require.ErrorContains(t, retainExecutedReplayParent(proofs, child,
		alpenglow.BlockID{Slot: exact.Slot - 1}), "conflicting parent slots")
}
