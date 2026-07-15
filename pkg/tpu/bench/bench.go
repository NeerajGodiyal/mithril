package bench

import (
	"fmt"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/tpu"
)

// Result summarizes a completed benchmark stage.
type Result struct {
	Name    string
	Packets uint64
	Bytes   uint64
	Elapsed time.Duration
	Pps     float64
	Gbps    float64
}

// Options configures timed microbenchmark stages.
type Options struct {
	Duration      time.Duration
	ProgressEvery time.Duration
	PoolSize      int
}

func (o Options) normalized() Options {
	out := o
	if out.Duration <= 0 {
		out.Duration = 5 * time.Second
	}
	if out.PoolSize <= 0 {
		out.PoolSize = 4096
	}
	return out
}

func finalizeResult(name string, packets, bytes uint64, start time.Time) Result {
	elapsed := time.Since(start)
	result := Result{
		Name:    name,
		Packets: packets,
		Bytes:   bytes,
		Elapsed: elapsed,
	}
	if secs := elapsed.Seconds(); secs > 0 {
		result.Pps, result.Gbps = tpu.AverageRates(packets, bytes, secs)
	}
	return result
}

func printProgress(name string, intervalPackets, intervalBytes uint64, elapsedSec float64, totalPackets, totalBytes uint64) {
	pps, gbps := tpu.IntervalRates(intervalPackets, intervalBytes, elapsedSec)
	fmt.Printf(
		"[%s] tps=%.0f gbps=%.3f interval_packets=%d total_packets=%d total_bytes=%d\n",
		name,
		pps,
		gbps,
		intervalPackets,
		totalPackets,
		totalBytes,
	)
}

func printDone(result Result) {
	fmt.Printf(
		"[%s] done tps=%.0f gbps=%.3f packets=%d bytes=%d elapsed=%s\n",
		result.Name,
		result.Pps,
		result.Gbps,
		result.Packets,
		result.Bytes,
		result.Elapsed,
	)
}
