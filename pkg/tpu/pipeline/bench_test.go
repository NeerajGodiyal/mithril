package pipeline

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/tpu/dedup"
	"github.com/Overclock-Validator/mithril/pkg/tpu/packet"
	"github.com/Overclock-Validator/mithril/pkg/tpu/rates"
	"github.com/Overclock-Validator/mithril/pkg/tpu/sink"
	"github.com/Overclock-Validator/mithril/pkg/tpu/txfixture"
)

func validTestWire(b *testing.B) []byte {
	b.Helper()
	return mustValidTestWire(b)
}

func reportThroughput(b *testing.B, packets, bytes uint64, elapsed time.Duration) {
	secs := elapsed.Seconds()
	pps, gbps := rates.IntervalRates(packets, bytes, secs)
	b.ReportMetric(pps, "tx/s")
	b.ReportMetric(gbps, "Gbps")
}

func BenchmarkDedupSanitize(b *testing.B) {
	pool := txfixture.NewPool(4096)
	wires := pool.Slice()
	cache := dedup.NewCache(len(wires))
	var stats dedup.Stats

	b.SetBytes(int64(len(wires[0])))
	b.ReportAllocs()
	b.ResetTimer()

	start := time.Now()
	for i := 0; i < b.N; i++ {
		dedup.FilterWire(wires[i%len(wires)], cache, &stats, nil)
	}
	reportThroughput(b, stats.InPackets, stats.InBytes, time.Since(start))
}

func BenchmarkSigverify(b *testing.B) {
	pool := txfixture.NewPool(4096)
	wires := pool.Slice()

	b.SetBytes(int64(len(wires[0])))
	b.ReportAllocs()
	b.ResetTimer()

	start := time.Now()
	var bytes uint64
	for i := 0; i < b.N; i++ {
		wire := wires[i%len(wires)]
		if verifyPacket(wire) {
			bytes += uint64(len(wire))
		}
	}
	reportThroughput(b, uint64(b.N), bytes, time.Since(start))
}

func BenchmarkSink(b *testing.B) {
	wireTx := validTestWire(b)
	noop := &sink.Noop{}

	b.SetBytes(int64(len(wireTx)))
	b.ReportAllocs()
	b.ResetTimer()

	start := time.Now()
	var bytes uint64
	for i := 0; i < b.N; i++ {
		pkt := packet.Owned(wireTx)
		noop.Receive(pkt)
		bytes += uint64(len(wireTx))
	}
	reportThroughput(b, uint64(b.N), bytes, time.Since(start))
}

func BenchmarkPipeline(b *testing.B) {
	wireTx := validTestWire(b)
	workers := runtime.GOMAXPROCS(0)

	ctx := context.Background()
	noop := &sink.Noop{}
	p, ingress := Start(ctx, Config{
		SigverifyWorkers: workers,
		IngressCap:       1 << 14,
		DedupOutCap:      1 << 14,
		VerifiedCap:      1 << 14,
		Sink:             noop,
	})
	b.Cleanup(p.Stop)

	b.SetBytes(int64(len(wireTx)))
	b.ReportAllocs()
	b.ResetTimer()

	start := time.Now()
	for i := 0; i < b.N; i++ {
		pkt := packet.Owned(wireTx)
		for !TryEnqueue(ingress, pkt, &p.stats.Ingress) {
			runtime.Gosched()
		}
	}
	elapsed := time.Since(start)

	deadline := time.Now().Add(2 * time.Second)
	for noop.Snapshot().InPackets < uint64(b.N) && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	stats := noop.Snapshot()
	reportThroughput(b, stats.InPackets, stats.InBytes, elapsed)
}
