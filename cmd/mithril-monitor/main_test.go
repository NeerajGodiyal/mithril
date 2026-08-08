package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type countingCollector struct {
	calls atomic.Int64
}

func (c *countingCollector) Collect(context.Context) {
	c.calls.Add(1)
}

type failingListener struct{}

func (failingListener) Accept() (net.Conn, error) { return nil, errors.New("listener failed") }
func (failingListener) Close() error              { return nil }
func (failingListener) Addr() net.Addr            { return testAddr("failed") }

type testAddr string

func (a testAddr) Network() string { return string(a) }
func (a testAddr) String() string  { return string(a) }

type shutdownFailServer struct {
	release chan struct{}
	once    sync.Once
}

func (s *shutdownFailServer) Serve(net.Listener) error {
	<-s.release
	return http.ErrServerClosed
}

func (*shutdownFailServer) Shutdown(context.Context) error {
	return errors.New("injected shutdown failure")
}

func (s *shutdownFailServer) Close() error {
	s.once.Do(func() { close(s.release) })
	return nil
}

func TestRunMonitorReturnsListenerFailure(t *testing.T) {
	c := &countingCollector{}
	err := runMonitor(
		context.Background(),
		&http.Server{Handler: http.NewServeMux()},
		failingListener{},
		c,
		15*time.Second,
	)
	if err == nil {
		t.Fatal("listener failure returned nil")
	}
	if got := c.calls.Load(); got != 1 {
		t.Fatalf("immediate collection calls = %d, want 1", got)
	}
}

func TestRunMonitorRejectsNonPositiveInterval(t *testing.T) {
	c := &countingCollector{}
	err := runMonitor(
		context.Background(),
		&http.Server{Handler: http.NewServeMux()},
		failingListener{},
		c,
		0,
	)
	if err == nil {
		t.Fatal("zero interval returned nil")
	}
	if got := c.calls.Load(); got != 0 {
		t.Fatalf("collection calls = %d, want 0", got)
	}
}

func TestRunMonitorRejectsTooFrequentInterval(t *testing.T) {
	c := &countingCollector{}
	err := runMonitor(
		context.Background(),
		&http.Server{Handler: http.NewServeMux()},
		failingListener{},
		c,
		minCollectionInterval-time.Nanosecond,
	)
	if err == nil {
		t.Fatal("too-frequent interval returned nil")
	}
	if got := c.calls.Load(); got != 0 {
		t.Fatalf("collection calls = %d, want 0", got)
	}
}

func TestRunMonitorRejectsIntervalBeyondFreshnessSLO(t *testing.T) {
	c := &countingCollector{}
	err := runMonitor(
		context.Background(),
		&http.Server{Handler: http.NewServeMux()},
		failingListener{},
		c,
		maxCollectionInterval+time.Second,
	)
	if err == nil {
		t.Fatal("overlong interval returned nil")
	}
	if got := c.calls.Load(); got != 0 {
		t.Fatalf("collection calls = %d, want 0", got)
	}
}

func TestRunMonitorStopsCleanlyWithContext(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := &countingCollector{}
	if err := runMonitor(
		ctx,
		&http.Server{Handler: http.NewServeMux()},
		listener,
		c,
		15*time.Second,
	); err != nil {
		t.Fatalf("context shutdown returned %v", err)
	}
	if got := c.calls.Load(); got != 1 {
		t.Fatalf("immediate collection calls = %d, want 1", got)
	}
}

func TestRunMonitorReportsShutdownFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	server := &shutdownFailServer{release: make(chan struct{})}
	c := &countingCollector{}

	err := runMonitor(ctx, server, failingListener{}, c, 15*time.Second)
	if err == nil || !strings.Contains(err.Error(), "shutdown failed") {
		t.Fatalf("shutdown failure = %v", err)
	}
	if got := c.calls.Load(); got != 1 {
		t.Fatalf("immediate collection calls = %d, want 1", got)
	}
}
