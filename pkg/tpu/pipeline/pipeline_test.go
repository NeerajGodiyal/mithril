package pipeline

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/tpu/packet"
	"github.com/Overclock-Validator/mithril/pkg/tpu/sink"
	"github.com/Overclock-Validator/mithril/pkg/tpu/wire"
)

func TestWireSanitizeAcceptsValidFixture(t *testing.T) {
	wireTx := mustValidTestWire(t)
	if _, err := wire.Sanitize(wireTx); err != nil {
		t.Fatalf("sanitize valid fixture: %v", err)
	}
}

func TestPipelineEndToEnd(t *testing.T) {
	wireTx := mustValidTestWire(t)
	ctx := context.Background()
	noop := &sink.Noop{}
	p, ingress := Start(ctx, Config{SigverifyWorkers: 2, Sink: noop})
	defer p.Stop()

	requireEnqueue(t, ingress, packet.Owned(wireTx))

	deadline := time.Now().Add(2 * time.Second)
	for noop.Stats.InPackets == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for sink, stats=%+v", p.Stats())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func requireEnqueue(t *testing.T, ingress chan<- packet.Packet, pkt packet.Packet) {
	t.Helper()
	var stats IngressStats
	deadline := time.Now().Add(time.Second)
	for !TryEnqueue(ingress, pkt, &stats) {
		if time.Now().After(deadline) {
			t.Fatal("ingress channel full")
		}
		runtime.Gosched()
	}
}
