package gossip

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"testing"
)

func TestDiscoveredContactRecordsSockets(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pubkey, err := pubkeyFromPrivateKey(priv)
	if err != nil {
		t.Fatalf("pubkeyFromPrivateKey: %v", err)
	}

	client, err := NewClient(Config{
		Entrypoint: "127.0.0.1:8000",
		BindAddr:   "127.0.0.1:0",
		TVUAddr:    "127.0.0.1:8001",
		Identity:   priv,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	record := contactRecord{
		Pubkey:    pubkey,
		Wallclock: 42,
		ShredVer:  63812,
		GossipAddr: contactEndpoint{
			ip:   ipV4Endpoint(net.ParseIP("203.0.113.10")),
			port: 8000,
			ok:   true,
		},
		ServeRepairAddr: contactEndpoint{
			ip:   ipV4Endpoint(net.ParseIP("203.0.113.10")),
			port: 8008,
			ok:   true,
		},
		TVUAddr: contactEndpoint{
			ip:   ipV4Endpoint(net.ParseIP("203.0.113.10")),
			port: 8001,
			ok:   true,
		},
		Sockets: map[uint8]contactEndpoint{
			socketTagGossip:      {ip: ipV4Endpoint(net.ParseIP("203.0.113.10")), port: 8000, ok: true},
			socketTagServeRepair: {ip: ipV4Endpoint(net.ParseIP("203.0.113.10")), port: 8008, ok: true},
			socketTagTVU:         {ip: ipV4Endpoint(net.ParseIP("203.0.113.10")), port: 8001, ok: true},
		},
	}

	client.handleContactRecord(record, 63812)

	contacts := client.DiscoveredContacts()
	if len(contacts) != 1 {
		t.Fatalf("discovered contacts = %d, want 1", len(contacts))
	}
	got := contacts[0]
	if got.Gossip != "203.0.113.10:8000" {
		t.Fatalf("gossip = %q", got.Gossip)
	}
	if got.TVU != "203.0.113.10:8001" {
		t.Fatalf("tvu = %q", got.TVU)
	}
	if len(got.Sockets) != 3 {
		t.Fatalf("socket tags = %d, want 3", len(got.Sockets))
	}

	summary := client.SummarizeDiscoveredContacts()
	if summary.Total != 1 || summary.WithTVU != 1 || summary.TVUPeers != 1 {
		t.Fatalf("summary = %+v, want total=1 with_tvu=1 tvu_peers=1", summary)
	}
}

func ipV4Endpoint(ip net.IP) [16]byte {
	var out [16]byte
	v4 := ip.To4()
	out[10] = 0xff
	out[11] = 0xff
	copy(out[12:], v4)
	return out
}
