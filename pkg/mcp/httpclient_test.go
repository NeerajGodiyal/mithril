package mcp

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestReadCappedBody(t *testing.T) {
	mk := func(body string, contentLength int64) *http.Response {
		return &http.Response{Body: io.NopCloser(strings.NewReader(body)), ContentLength: contentLength}
	}
	if body, err := readCappedBody(mk("hello", 5), 100); err != nil || string(body) != "hello" {
		t.Errorf("under cap = %q, %v", body, err)
	}
	if _, err := readCappedBody(mk("hello", 1000), 100); err == nil {
		t.Error("over-cap Content-Length must be rejected")
	}
	if _, err := readCappedBody(mk(strings.Repeat("x", 200), -1), 100); err == nil {
		t.Error("body exceeding cap must be rejected when Content-Length is absent")
	}
}

type fixedResolver struct {
	addresses []net.IPAddr
	err       error
}

func (r fixedResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return r.addresses, r.err
}

func TestResolvePinnedIPsFailsClosed(t *testing.T) {
	u, _ := url.Parse("https://example.test/path")
	if _, err := resolvePinnedIPs(context.Background(), fixedResolver{err: errors.New("dns down")}, u); err == nil {
		t.Fatal("DNS failure must be rejected")
	}
	if _, err := resolvePinnedIPs(context.Background(), fixedResolver{}, u); err == nil {
		t.Fatal("empty DNS result must be rejected")
	}
	if _, err := resolvePinnedIPs(context.Background(), fixedResolver{addresses: []net.IPAddr{
		{IP: net.ParseIP("8.8.8.8")},
		{IP: net.ParseIP("169.254.169.254")},
	}}, u); err == nil {
		t.Fatal("one blocked address must reject the whole mixed answer")
	}
}

func TestPinnedDialIgnoresReboundHostname(t *testing.T) {
	var target string
	dial := pinnedDialContext([]net.IP{net.ParseIP("127.0.0.1")}, func(_ context.Context, _, address string) (net.Conn, error) {
		target = address
		return nil, errors.New("stop after capture")
	})
	_, _ = dial(context.Background(), "tcp", "attacker-controlled.example:8443")
	if target != "127.0.0.1:8443" {
		t.Fatalf("dial target = %q, want pinned IP", target)
	}
}

func TestPinnedRequestRequiresTLSOffLoopback(t *testing.T) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://8.8.8.8/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := doPinnedRequest(context.Background(), req, time.Second); err == nil || !strings.Contains(err.Error(), "only for loopback") {
		t.Fatalf("public cleartext HTTP error = %v", err)
	}
}
