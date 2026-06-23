package gossip

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"testing"
	"time"
)

func TestVarintAndShortLenEncoding(t *testing.T) {
	var e encoder
	if err := e.shortLen(0xffff); err != nil {
		t.Fatalf("shortLen returned error: %v", err)
	}
	if got, want := e.bytes(), []byte{0xff, 0xff, 0x03}; string(got) != string(want) {
		t.Fatalf("shortLen bytes = %x, want %x", got, want)
	}

	d := newDecoder(e.bytes())
	got, err := d.shortLen()
	if err != nil {
		t.Fatalf("shortLen decode returned error: %v", err)
	}
	if got != 0xffff {
		t.Fatalf("shortLen decode = %d, want %d", got, 0xffff)
	}
}

func TestContactInfoRoundTripAndSignature(t *testing.T) {
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
		1234,
		&net.UDPAddr{IP: net.ParseIP("203.0.113.10"), Port: 65400},
		&net.UDPAddr{IP: net.ParseIP("203.0.113.10"), Port: 8001},
	)
	if err != nil {
		t.Fatalf("NewContactInfo returned error: %v", err)
	}
	if err := contact.SetSocket(socketTagServeRepair, &net.UDPAddr{IP: net.ParseIP("203.0.113.10"), Port: 8008}); err != nil {
		t.Fatalf("SetSocket serve repair returned error: %v", err)
	}
	if err := contact.SetAlpenglowAddr(&net.UDPAddr{IP: net.ParseIP("203.0.113.10"), Port: 8002}); err != nil {
		t.Fatalf("SetAlpenglowAddr returned error: %v", err)
	}
	if err := contact.SetTPUVoteAddr(&net.UDPAddr{IP: net.ParseIP("203.0.113.10"), Port: 8002}); err != nil {
		t.Fatalf("SetTPUVoteAddr returned error: %v", err)
	}
	if err := contact.SetTPUVoteQuicAddr(&net.UDPAddr{IP: net.ParseIP("203.0.113.10"), Port: 8002}); err != nil {
		t.Fatalf("SetTPUVoteQuicAddr returned error: %v", err)
	}

	value, err := signCrdsContactInfo(contact, priv)
	if err != nil {
		t.Fatalf("signCrdsContactInfo returned error: %v", err)
	}
	if !value.Verify() {
		t.Fatalf("signed CRDS value did not verify")
	}

	var e encoder
	value.encode(&e)
	decoded, err := decodeCrdsValue(newDecoder(e.bytes()))
	if err != nil {
		t.Fatalf("decodeCrdsValue returned error: %v", err)
	}
	if !decoded.Verify() {
		t.Fatalf("decoded CRDS value did not verify")
	}
	if decoded.ContactInfo.ShredVer != 1234 {
		t.Fatalf("shred version = %d, want 1234", decoded.ContactInfo.ShredVer)
	}
	if decoded.ContactInfo.GossipAddr.String() != "203.0.113.10:65400" {
		t.Fatalf("gossip addr = %s", decoded.ContactInfo.GossipAddr.String())
	}
	if decoded.ContactInfo.TVUAddr.String() != "203.0.113.10:8001" {
		t.Fatalf("tvu addr = %s", decoded.ContactInfo.TVUAddr.String())
	}
	if decoded.ContactInfo.ServeRepairAddr.String() != "203.0.113.10:8008" {
		t.Fatalf("serve repair addr = %s", decoded.ContactInfo.ServeRepairAddr.String())
	}
	if decoded.ContactInfo.AlpenglowAddr.String() != "203.0.113.10:8002" {
		t.Fatalf("alpenglow addr = %s", decoded.ContactInfo.AlpenglowAddr.String())
	}
	if decoded.ContactInfo.TPUVoteAddr.String() != "203.0.113.10:8002" {
		t.Fatalf("tpu vote addr = %s", decoded.ContactInfo.TPUVoteAddr.String())
	}
	if decoded.ContactInfo.TPUVoteQuicAddr.String() != "203.0.113.10:8002" {
		t.Fatalf("tpu vote quic addr = %s", decoded.ContactInfo.TPUVoteQuicAddr.String())
	}
}

func TestClientSetTPUQUICBeforeInitialize(t *testing.T) {
	client, err := NewClient(Config{
		Entrypoint:   "127.0.0.1:8000",
		BindAddr:     "0.0.0.0:0",
		TVUAddr:      "0.0.0.0:8001",
		AdvertisedIP: "203.0.113.10",
		ShredVersion: 4321,
	})
	if err != nil {
		t.Fatalf("NewClient returned error: %v", err)
	}
	tpuAddr := &net.UDPAddr{IP: net.ParseIP("203.0.113.10"), Port: 9007}
	if err := client.SetTPUQUIC(tpuAddr); err != nil {
		t.Fatalf("SetTPUQUIC before initializeContact returned error: %v", err)
	}
	if err := client.initializeContact(&net.UDPAddr{IP: net.ParseIP("0.0.0.0"), Port: 65400}); err != nil {
		t.Fatalf("initializeContact returned error: %v", err)
	}

	client.contactMu.RLock()
	contact := client.contact
	client.contactMu.RUnlock()
	if contact == nil {
		t.Fatalf("expected contact info")
	}
	var foundForwards, foundTPU bool
	for _, socket := range contact.Sockets {
		switch socket.Key {
		case socketTagTPUQUICForwards:
			foundForwards = socket.Port == 9007
		case socketTagTPUQUIC:
			foundTPU = socket.Port == 9007
		}
	}
	if !foundForwards || !foundTPU {
		t.Fatalf("expected TPU QUIC socket tags 7 and 8 on port 9007, sockets=%v", contact.Sockets)
	}
}

func TestClientAdvertisesAlpenglowSocket(t *testing.T) {
	client, err := NewClient(Config{
		Entrypoint:    "127.0.0.1:8000",
		BindAddr:      "0.0.0.0:0",
		TVUAddr:       "0.0.0.0:8001",
		AlpenglowAddr: "0.0.0.0:8002",
		AdvertisedIP:  "203.0.113.10",
		ShredVersion:  4321,
	})
	if err != nil {
		t.Fatalf("NewClient returned error: %v", err)
	}
	if err := client.initializeContact(&net.UDPAddr{IP: net.ParseIP("0.0.0.0"), Port: 65400}); err != nil {
		t.Fatalf("initializeContact returned error: %v", err)
	}

	client.contactMu.RLock()
	contact := client.contact
	client.contactMu.RUnlock()
	if contact == nil {
		t.Fatalf("expected contact info")
	}
	if contact.AlpenglowAddr == nil {
		t.Fatalf("expected alpenglow socket to be advertised")
	}
	if got, want := contact.AlpenglowAddr.String(), "203.0.113.10:8002"; got != want {
		t.Fatalf("alpenglow addr = %s, want %s", got, want)
	}
	if contact.TPUVoteAddr == nil {
		t.Fatalf("expected TPU vote socket to be advertised for Alpenglow Votor compatibility")
	}
	if got, want := contact.TPUVoteAddr.String(), "203.0.113.10:8002"; got != want {
		t.Fatalf("tpu vote addr = %s, want %s", got, want)
	}
	if contact.TPUVoteQuicAddr == nil {
		t.Fatalf("expected TPU vote QUIC socket to be advertised for Alpenglow Votor compatibility")
	}
	if got, want := contact.TPUVoteQuicAddr.String(), "203.0.113.10:8002"; got != want {
		t.Fatalf("tpu vote quic addr = %s, want %s", got, want)
	}
}

func TestPingPongRoundTrip(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey returned error: %v", err)
	}
	ping, err := newPing(priv)
	if err != nil {
		t.Fatalf("newPing returned error: %v", err)
	}
	if !ping.Verify() {
		t.Fatalf("ping did not verify")
	}
	pong, err := newPong(ping, priv)
	if err != nil {
		t.Fatalf("newPong returned error: %v", err)
	}
	if !pong.Verify() {
		t.Fatalf("pong did not verify")
	}

	decoded, err := decodePacket(encodePingMessage(ping))
	if err != nil {
		t.Fatalf("decodePacket ping returned error: %v", err)
	}
	if decoded.Kind != packetPing || !decoded.Ping.Verify() {
		t.Fatalf("decoded ping did not verify")
	}

	decoded, err = decodePacket(encodePongMessage(pong))
	if err != nil {
		t.Fatalf("decodePacket pong returned error: %v", err)
	}
	if decoded.Kind != packetPong || !decoded.Pong.Verify() {
		t.Fatalf("decoded pong did not verify")
	}
}

func TestQueryEntrypointParsesEchoResponse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen returned error: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		buf := make([]byte, ipEchoHeaderLength+16+1)
		_, _ = conn.Read(buf)
		var e encoder
		e.fixed([]byte{0, 0, 0, 0})
		if err := encodeIP(&e, net.ParseIP("198.51.100.7")); err != nil {
			t.Errorf("encodeIP returned error: %v", err)
			return
		}
		e.bool(true)
		e.u16(4321)
		_, _ = conn.Write(e.bytes())
	}()

	addr, err := net.ResolveUDPAddr("udp", listener.Addr().String())
	if err != nil {
		t.Fatalf("ResolveUDPAddr returned error: %v", err)
	}
	resp, err := QueryEntrypoint(addr, time.Second)
	if err != nil {
		t.Fatalf("QueryEntrypoint returned error: %v", err)
	}
	if resp.ShredVersion != 4321 {
		t.Fatalf("shred version = %d, want 4321", resp.ShredVersion)
	}
	if !resp.Address.Equal(net.ParseIP("198.51.100.7")) {
		t.Fatalf("address = %s", resp.Address.String())
	}
}
