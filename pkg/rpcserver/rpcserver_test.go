package rpcserver

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestServeHTTPQuietlyHandlesUnsupportedMethod(t *testing.T) {
	rpcServer := &RpcServer{}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"jsonrpc":"2.0","method":"getSlot","id":7}`))
	rec := httptest.NewRecorder()

	rpcServer.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d", http.StatusInternalServerError, rec.Code)
	}

	var resp struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.JSONRPC != "2.0" || resp.ID != 7 {
		t.Fatalf("unexpected response identity: %+v", resp)
	}
	if resp.Error.Code != -32601 || resp.Error.Message != "method 'getSlot' not found" {
		t.Fatalf("unexpected error response: %+v", resp.Error)
	}
}

func TestServeHTTPQuietlyHandlesInvalidRequest(t *testing.T) {
	rpcServer := &RpcServer{}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
	rec := httptest.NewRecorder()

	rpcServer.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rec.Code)
	}

	var resp struct {
		JSONRPC string `json:"jsonrpc"`
		ID      any    `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.JSONRPC != "2.0" || resp.ID != nil {
		t.Fatalf("unexpected response identity: %+v", resp)
	}
	if resp.Error.Code != -32600 || resp.Error.Message != "Invalid request" {
		t.Fatalf("unexpected error response: %+v", resp.Error)
	}
}

func TestServeHTTPQuietlyHandlesNonRPCProbe(t *testing.T) {
	rpcServer := &RpcServer{}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	rpcServer.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d", http.StatusNotFound, rec.Code)
	}
}

func TestRPCServerShutdownStopsListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rpcServer := &RpcServer{listener: listener}
	rpcServer.httpServer = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})}
	rpcServer.Start()

	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + listener.Addr().String())
	if err != nil {
		t.Fatalf("request running RPC server: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d", http.StatusNoContent, resp.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := rpcServer.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown RPC server: %v", err)
	}
	if _, err := client.Get("http://" + listener.Addr().String()); err == nil {
		t.Fatal("listener still accepted requests after shutdown")
	}
}

func TestRPCServerShutdownTimesOutWhileHandlerIsActive(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	rpcServer := &RpcServer{listener: listener}
	rpcServer.httpServer = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	})}
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
