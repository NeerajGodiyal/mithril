package gossip

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"testing"
)

func TestContactInfoSetTPUQUIC(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pubkey, err := pubkeyFromPrivateKey(priv)
	if err != nil {
		t.Fatalf("pubkeyFromPrivateKey: %v", err)
	}
	contact, err := NewContactInfo(
		pubkey,
		1234,
		&net.UDPAddr{IP: net.ParseIP("203.0.113.10"), Port: 8001},
		&net.UDPAddr{IP: net.ParseIP("203.0.113.10"), Port: 8002},
	)
	if err != nil {
		t.Fatalf("NewContactInfo: %v", err)
	}
	tpuAddr := &net.UDPAddr{IP: net.ParseIP("203.0.113.10"), Port: 9001}
	if err := contact.SetTPUQUIC(tpuAddr); err != nil {
		t.Fatalf("SetTPUQUIC: %v", err)
	}
	var foundForwards, foundTPU bool
	for _, socket := range contact.Sockets {
		switch socket.Key {
		case socketTagTPUQUICForwards:
			foundForwards = socket.Port == 9001
		case socketTagTPUQUIC:
			foundTPU = socket.Port == 9001
		}
	}
	if !foundForwards || !foundTPU {
		t.Fatalf("expected TPU QUIC socket tags 7 and 8 on port 9001, sockets=%v", contact.Sockets)
	}
}
