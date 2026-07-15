package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/tpu/bench"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("bench-tpu", flag.ExitOnError)
	duration := fs.Duration("duration", 5*time.Second, "Per-stage benchmark duration")
	progressEvery := fs.Duration("progress-every", time.Second, "Progress reporting interval; 0 disables")
	connections := fs.Int("connections", 4, "Concurrent QUIC connections for the quic stage")
	sigverifyWorkers := fs.Int("sigverify-workers", 0, "Sigverify workers for the sigverify stage; 0 uses GOMAXPROCS")
	poolSize := fs.Int("pool-size", 4096, "Precomputed signed transfer pool size")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "bench-tpu: %v\n", err)
		return 2
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	opts := bench.Options{
		Duration:      *duration,
		ProgressEvery: *progressEvery,
		PoolSize:      *poolSize,
	}

	fmt.Println("bench-tpu: signed system-transfer transactions")

	if _, err := bench.RunQUIC(ctx, bench.QUICOptions{
		Options:     opts,
		Connections: *connections,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "bench-tpu: quic stage failed: %v\n", err)
		return 1
	}

	bench.RunDedupSanitize(ctx, opts)
	bench.RunSigverify(ctx, bench.SigverifyOptions{
		Options: opts,
		Workers: *sigverifyWorkers,
	})
	return 0
}
