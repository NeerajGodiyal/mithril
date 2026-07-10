package gossip

import (
	"net"
	"testing"
	"time"
)

func TestResolveShredVersionPrefersEntrypoint(t *testing.T) {
	addr, err := net.ResolveUDPAddr("udp", "64.130.37.11:8000")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got, err := ResolveShredVersion(addr, 63812, 3*time.Second)
	if err != nil {
		t.Fatalf("ResolveShredVersion returned error: %v", err)
	}
	if got == 63812 {
		t.Fatalf("expected entrypoint shred version to override configured 63812, got %d", got)
	}
	if got == 0 {
		t.Fatalf("expected non-zero shred version from entrypoint")
	}
	t.Logf("resolved shred version %d", got)
}
