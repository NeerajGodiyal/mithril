package bench

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/tpu/dedup"
	"github.com/Overclock-Validator/mithril/pkg/tpu/txfixture"
)

// RunDedupSanitize benchmarks the single-threaded dedup+sanitize stage.
func RunDedupSanitize(ctx context.Context, opts Options) Result {
	opts = opts.normalized()
	const name = "dedup-sanitize"

	fmt.Fprintf(os.Stderr, "[%s] precomputing %d signed transfers...\n", name, opts.PoolSize)
	pool := txfixture.NewPool(opts.PoolSize)
	wires := pool.Slice()
	if len(wires) == 0 {
		wires = [][]byte{txfixture.MustSignedTransferWire(0)}
	}

	cache := dedup.NewCache(dedup.DefaultCacheCapacity)
	var stats dedup.Stats
	var seq uint64

	start := time.Now()
	deadline := start.Add(opts.Duration)
	var lastPackets, lastBytes uint64
	lastAt := start

	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			break
		}
		dedup.FilterWire(wires[seq%uint64(len(wires))], cache, &stats, nil)
		seq++

		if opts.ProgressEvery > 0 {
			now := time.Now()
			if elapsed := now.Sub(lastAt).Seconds(); elapsed >= opts.ProgressEvery.Seconds() {
				printProgress(
					name,
					stats.InPackets-lastPackets,
					stats.InBytes-lastBytes,
					elapsed,
					stats.InPackets,
					stats.InBytes,
				)
				lastPackets = stats.InPackets
				lastBytes = stats.InBytes
				lastAt = now
			}
		}
	}

	result := finalizeResult(name, stats.InPackets, stats.InBytes, start)
	printDone(result)
	return result
}
