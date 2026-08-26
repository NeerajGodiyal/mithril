package turbine

import (
	"context"
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/gagliardetto/solana-go"
)

func TestSlotAssemblerInFlightCompletionRetainsKnownChildHintAcrossPruning(t *testing.T) {
	shreds := generatedAlpenglowDataShreds(t)
	assembler := NewSlotAssembler()
	assembler.SetAlpenglowMode(true)
	started := make(chan struct{})
	release := make(chan struct{})
	assembler.verifyTransactions = func(context.Context, *block.Block) error {
		close(started)
		<-release
		return nil
	}

	for idx, shred := range shreds[:len(shreds)-1] {
		if blk, err := assembler.AddShred(shred); err != nil || blk != nil {
			t.Fatalf("AddShred(%d) = block %v, err %v", idx, blk, err)
		}
	}
	result := make(chan addPacketResult, 1)
	go func() {
		blk, err := assembler.AddShred(shreds[len(shreds)-1])
		result <- addPacketResult{block: blk, err: err}
	}()
	waitSignal(t, started, "in-flight Alpenglow completion")

	const slot = uint64(100)
	contradictory := solana.Hash{0xff}
	assembler.SetKnownAlpenglowBlockID(slot, contradictory)

	// Move the observed edge far enough to exercise completed-ID retention
	// while this slot remains protected as an in-flight completion token.
	futureSlot := slot + maxRetainedCompletedSlotLag + 1
	if blk, err := assembler.AddShred(&Shred{
		Variant:      legacyDataVariant,
		Type:         ShredTypeData,
		Slot:         futureSlot,
		Index:        0,
		Version:      1,
		ParentOffset: 1,
		Data:         []byte{1},
	}); err != nil || blk != nil {
		t.Fatalf("future-edge AddShred = block %v, err %v", blk, err)
	}

	close(release)
	select {
	case got := <-result:
		if got.block != nil || got.err != nil {
			t.Fatalf("completion contradicting retained hint = block %v, err %v", got.block, got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight Alpenglow completion did not drain")
	}
	if assembler.SlotCompleted(slot) {
		t.Fatal("block contradicting retained child hint completed after edge pruning")
	}
	count, gotSlot, _, want := assembler.NonCanonicalBlockIDStats()
	if count != 1 || gotSlot != slot || want != contradictory {
		t.Fatalf("noncanonical stats = count %d slot %d want %s", count, gotSlot, want)
	}
}
