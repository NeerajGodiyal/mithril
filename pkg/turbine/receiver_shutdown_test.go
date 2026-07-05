package turbine

import (
	"context"
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/fixtures"
)

// Receiver shutdown must JOIN the hydrator before closing the blocks
// channel: an unjoined hydrator emitting a just-completed spooled slot into
// a just-closed channel was a coin-flip "send on closed channel" panic on
// every stream restart and prewarm handover (found by review). This stress
// loop makes the old race fail within a few iterations: each round adopts a
// spool holding a complete slot, starts Run (hydration kicks immediately),
// and cancels while the hydrator is feeding.
func TestReceiverShutdownJoinsHydrator(t *testing.T) {
	rawShreds := fixtures.DataShreds(t, "mainnet", 102815960)
	const slot = 102815960

	dir := t.TempDir()
	seed, err := OpenShredSpool(dir, 0)
	if err != nil {
		t.Fatalf("seed spool: %v", err)
	}
	for _, pkt := range rawShreds {
		seed.Append(slot, pkt)
	}
	seed.Close()

	for round := 0; round < 15; round++ {
		spool, err := OpenShredSpool(dir, 0)
		if err != nil {
			t.Fatalf("round %d open spool: %v", round, err)
		}
		receiver := NewUDPReceiver("127.0.0.1:0")
		receiver.SetShredSpool(spool)
		receiver.SetHydrationWindow(slot, slot+8)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- receiver.Run(ctx) }()
		if rerr := <-receiver.Ready(); rerr != nil {
			cancel()
			t.Fatalf("round %d receiver not ready: %v", round, rerr)
		}
		// Let the hydration kick land somewhere inside the feed, varying
		// the overlap across rounds; then cancel mid-flight.
		time.Sleep(time.Duration(round%5) * 200 * time.Microsecond)
		cancel()
		select {
		case <-done: // a hydrator panic would crash the test process instead
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d: Run did not return after cancel (hydrator join deadlock)", round)
		}
	}
}
