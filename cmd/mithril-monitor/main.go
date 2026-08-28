// Command mithril-monitor is the off-host deterministic monitor.
//
// It belongs on the OPERATIONS host, not the node host. Its entire purpose is
// to stay observable when the node host is down, which it cannot do if it runs
// there. It does not depend on the node's in-process diagnostics.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Overclock-Validator/mithril/internal/monitor"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	minCollectionInterval = 5 * time.Second
	maxCollectionInterval = 30 * time.Second
)

func main() {
	listen := flag.String("listen", "127.0.0.1:9101", "address to expose /metrics on")
	interval := flag.Duration("interval", 15*time.Second, "collection interval")
	configPath := flag.String("config", monitor.DefaultConfigPath, "absolute path to providers.toml")
	flag.Parse()

	cfg, err := monitor.LoadConfig(*configPath)
	if err != nil {
		// The error never contains a configured URL; see monitor.LoadConfig.
		fmt.Fprintf(os.Stderr, "mithril-monitor: %v\n", err)
		os.Exit(1)
	}

	registry := prometheus.NewRegistry()
	collector := monitor.New(cfg, monitor.NewMetrics(registry))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	server := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    16 << 10,
	}

	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mithril-monitor: metrics listener could not start")
		os.Exit(1)
	}
	if err := runMonitor(ctx, server, listener, collector, *interval); err != nil {
		fmt.Fprintf(os.Stderr, "mithril-monitor: %v\n", err)
		os.Exit(1)
	}
}

type collector interface {
	Collect(context.Context)
}

type metricsHTTPServer interface {
	Serve(net.Listener) error
	Shutdown(context.Context) error
	Close() error
}

func runMonitor(ctx context.Context, server metricsHTTPServer, listener net.Listener, c collector, interval time.Duration) error {
	if interval < minCollectionInterval || interval > maxCollectionInterval {
		_ = listener.Close()
		return fmt.Errorf("collection interval must be between %s and %s", minCollectionInterval, maxCollectionInterval)
	}

	serverResult := make(chan error, 1)
	go func() {
		serverResult <- server.Serve(listener)
	}()

	// Collect immediately so a scrape right after start has real values rather
	// than only the initialized zeros.
	c.Collect(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := server.Shutdown(shutdown); err != nil {
				closeErr := server.Close()
				return fmt.Errorf("metrics server shutdown failed: %w", errors.Join(err, closeErr))
			}
			return nil
		case <-serverResult:
			return fmt.Errorf("metrics server stopped unexpectedly")
		case <-ticker.C:
			c.Collect(ctx)
		}
	}
}
