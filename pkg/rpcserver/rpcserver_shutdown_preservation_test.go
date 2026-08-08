package rpcserver

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestShutdownTimesOutWhileHandlerIsActive(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	server := newRPCHTTPServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	rpcServer := &RpcServer{
		listeners:   []rpcBoundListener{{listener: listener, bindIP: net.ParseIP(DefaultRPCBindAddress)}},
		httpServers: []*http.Server{server},
	}
	rpcServer.Start()

	requestDone := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + listener.Addr().String())
		if err == nil {
			resp.Body.Close()
		}
		requestDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err = rpcServer.Shutdown(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v, want deadline exceeded", err)
	}

	close(release)
	select {
	case err := <-requestDone:
		if err != nil {
			t.Fatalf("active request after shutdown timeout: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("active request did not finish after release")
	}
}
