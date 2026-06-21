package bench

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/tpu"
	"github.com/Overclock-Validator/mithril/pkg/tpu/txfixture"
)

// SigverifyOptions configures the sigverify benchmark stage.
type SigverifyOptions struct {
	Options
	Workers int
}

func (o SigverifyOptions) normalized() SigverifyOptions {
	out := o
	out.Options = out.Options.normalized()
	if out.Workers <= 0 {
		out.Workers = runtime.GOMAXPROCS(0)
	}
	return out
}

// RunSigverify benchmarks signature verification on signed transfer transactions.
func RunSigverify(ctx context.Context, opts SigverifyOptions) Result {
	opts = opts.normalized()
	const name = "sigverify"

	fmt.Fprintf(os.Stderr, "[%s] precomputing %d signed transfers...\n", name, opts.PoolSize)
	pool := txfixture.NewPool(opts.PoolSize)
	wires := pool.Slice()
	if len(wires) == 0 {
		wires = [][]byte{txfixture.MustSignedTransferWire(0)}
	}

	var packets atomic.Uint64
	var bytes atomic.Uint64
	var seq atomic.Uint64

	start := time.Now()
	benchCtx, benchCancel := context.WithTimeout(ctx, opts.Duration)
	defer benchCancel()

	var lastPackets, lastBytes uint64
	lastAt := start

	var wg sync.WaitGroup
	for i := 0; i < opts.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for benchCtx.Err() == nil {
				i := seq.Add(1) - 1
				wire := wires[i%uint64(len(wires))]
				if tpu.VerifyPacket(wire) {
					packets.Add(1)
					bytes.Add(uint64(len(wire)))
				}
			}
		}()
	}

	if opts.ProgressEvery > 0 {
		go func() {
			ticker := time.NewTicker(opts.ProgressEvery)
			defer ticker.Stop()
			for {
				select {
				case <-benchCtx.Done():
					return
				case now := <-ticker.C:
					curPackets := packets.Load()
					curBytes := bytes.Load()
					elapsed := now.Sub(lastAt).Seconds()
					if elapsed <= 0 {
						continue
					}
					printProgress(name, curPackets-lastPackets, curBytes-lastBytes, elapsed, curPackets, curBytes)
					lastPackets = curPackets
					lastBytes = curBytes
					lastAt = now
				}
			}
		}()
	}

	wg.Wait()

	result := finalizeResult(name, packets.Load(), bytes.Load(), start)
	printDone(result)
	return result
}
