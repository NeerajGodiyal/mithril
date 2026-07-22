package turbine

import (
	"context"
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/fixtures"
)

func TestCompletionPoolMarksPendingBeforeResultConsumption(t *testing.T) {
	const slot = uint64(102815960)
	receiver := NewUDPReceiver("127.0.0.1:0")
	pool := newSlotCompletionPool(
		receiver.assembler,
		&receiver.slotResetMu,
		receiver.startPendingBlock,
		1,
		1,
	)
	defer pool.closeAndWait()

	var work *slotCompletionWork
	for idx, packet := range fixtures.DataShreds(t, "mainnet", slot) {
		shred, err := ParseShred(packet)
		if err != nil {
			t.Fatalf("ParseShred(%d): %v", idx, err)
		}
		candidate, err := receiver.assembler.addShredFrom(shred, false)
		if err != nil {
			t.Fatalf("addShredFrom(%d): %v", idx, err)
		}
		if candidate != nil {
			work = candidate
			break
		}
	}
	if work == nil {
		t.Fatal("fixture did not produce completion work")
	}
	if !pool.enqueue(context.Background(), work, false) {
		t.Fatal("completion work was not enqueued")
	}

	deadline := time.Now().Add(5 * time.Second)
	for len(pool.results) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(pool.results) == 0 {
		t.Fatal("completion worker did not finalize a buffered result")
	}
	// No goroutine has received from pool.results yet. The pending marker must
	// already cover this ownership gap so repair/stall logic cannot mistake the
	// finalized block for a lost delivery.
	if !receiver.BlockPendingDelivery(slot) {
		t.Fatal("finalized result was not marked pending before result consumption")
	}

	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		receiver.consumeCompletionResults(context.Background(), pool.results)
	}()

	var deliveredSlot uint64
	select {
	case delivered := <-receiver.Blocks():
		if delivered == nil {
			t.Fatal("receiver delivered a nil block")
		}
		deliveredSlot = delivered.Slot
	case <-time.After(5 * time.Second):
		t.Fatal("result consumer did not deliver the finalized block")
	}
	if deliveredSlot != slot {
		t.Fatalf("delivered block slot = %d, want %d", deliveredSlot, slot)
	}
	if !receiver.BlockPendingDelivery(slot) {
		t.Fatal("pending marker cleared before downstream acknowledgement")
	}
	receiver.AcknowledgeBlockDelivery(slot)
	if receiver.BlockPendingDelivery(slot) {
		t.Fatal("pending marker survived downstream acknowledgement")
	}

	pool.closeAndWait()
	select {
	case <-consumerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("result consumer did not stop after pool shutdown")
	}
}
