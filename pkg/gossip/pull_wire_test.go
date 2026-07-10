package gossip

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"net"
	"testing"
)

func TestPullRequestWireSize(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey returned error: %v", err)
	}
	pubkey, err := pubkeyFromPrivateKey(priv)
	if err != nil {
		t.Fatalf("pubkeyFromPrivateKey returned error: %v", err)
	}
	contact, err := NewContactInfo(
		pubkey,
		63812,
		&net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 18000},
		&net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 18001},
	)
	if err != nil {
		t.Fatalf("NewContactInfo returned error: %v", err)
	}
	value, err := signCrdsContactInfo(contact, priv)
	if err != nil {
		t.Fatalf("signCrdsContactInfo returned error: %v", err)
	}
	packet, err := encodePullRequest(value)
	if err != nil {
		t.Fatalf("encodePullRequest returned error: %v", err)
	}
	t.Logf("pull len=%d hex=%s", len(packet), hex.EncodeToString(packet))
	data, err := encodeCrdsDataContactInfo(contact)
	if err != nil {
		t.Fatalf("encodeCrdsDataContactInfo returned error: %v", err)
	}
	t.Logf("crds data len=%d hex=%s", len(data), hex.EncodeToString(data))
}
