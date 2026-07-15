package turbine

import (
	"context"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/block"
)

func TestAssembledBlockRemainsPendingUntilConsumerAcknowledges(t *testing.T) {
	receiver := NewUDPReceiver("127.0.0.1:0")
	blk := &block.Block{Slot: 42}

	if !receiver.emitAssembled(context.Background(), blk) {
		t.Fatalf("emitAssembled unexpectedly stopped")
	}
	if !receiver.BlockPendingDelivery(blk.Slot) {
		t.Fatalf("assembled block must be pending while queued in Blocks")
	}
	if got := <-receiver.Blocks(); got != blk {
		t.Fatalf("received block %p, want %p", got, blk)
	}
	if !receiver.BlockPendingDelivery(blk.Slot) {
		t.Fatalf("channel receive alone must not create an ownership gap")
	}
	receiver.AcknowledgeBlockDelivery(blk.Slot)
	if receiver.BlockPendingDelivery(blk.Slot) {
		t.Fatalf("acknowledged block remained pending")
	}
}

func TestReceiverCountsReplacementDeliveryForSameSlot(t *testing.T) {
	receiver := NewUDPReceiver("127.0.0.1:0")
	blk := &block.Block{Slot: 77}

	if !receiver.emitAssembled(context.Background(), blk) || !receiver.emitAssembled(context.Background(), blk) {
		t.Fatal("queue replacement deliveries")
	}
	receiver.AcknowledgeBlockDelivery(blk.Slot)
	if !receiver.BlockPendingDelivery(blk.Slot) {
		t.Fatal("acknowledging the old variant must retain replacement ownership")
	}
	receiver.AcknowledgeBlockDelivery(blk.Slot)
	if receiver.BlockPendingDelivery(blk.Slot) {
		t.Fatal("all same-slot deliveries were acknowledged")
	}
}
