package turbine

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/gossip"
)

func newPacingTestClient(t *testing.T) *repairClient {
	t.Helper()
	c := &repairClient{
		outstanding: make(map[repairRequestKey]outstandingRepairRequest),
		byResponse:  make(map[repairResponseKey]repairRequestKey),
		perPeer:     make(map[repairAddressKey]*peerRecord),
		expiredCur:  make(map[repairResponseKey]time.Time),
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	c.identity = priv
	c.timeoutNanos.Store(int64(repairMinRequestTimeout))
	return c
}

// nonceTrailer builds a packet whose trailing 4 bytes decode to the nonce.
func nonceTrailer(nonce uint32) []byte {
	return []byte{0xAA, 0xBB, byte(nonce), byte(nonce >> 8), byte(nonce >> 16), byte(nonce >> 24)}
}

// Fast responses pin the timeout at the floor; sustained slow responses walk
// it toward factor x latency; the ceiling holds against pathological tails.
func TestAdaptiveTimeoutTracksObservedLatency(t *testing.T) {
	c := newPacingTestClient(t)

	c.mu.Lock()
	for i := 0; i < 50; i++ {
		c.observeLatencyLocked(200 * time.Millisecond)
	}
	c.mu.Unlock()
	if got := time.Duration(c.timeoutNanos.Load()); got != repairMinRequestTimeout {
		t.Fatalf("fast responses: timeout = %s, want floor %s", got, repairMinRequestTimeout)
	}

	c.mu.Lock()
	for i := 0; i < 200; i++ {
		c.observeLatencyLocked(1500 * time.Millisecond)
	}
	c.mu.Unlock()
	got := time.Duration(c.timeoutNanos.Load())
	if got < 4*time.Second || got > 5*time.Second {
		t.Fatalf("1.5s responses: timeout = %s, want ~4.5s (3x EWMA)", got)
	}

	c.mu.Lock()
	for i := 0; i < 200; i++ {
		c.observeLatencyLocked(30 * time.Second)
	}
	c.mu.Unlock()
	if got := time.Duration(c.timeoutNanos.Load()); got != repairMaxRequestTimeout {
		t.Fatalf("pathological responses: timeout = %s, want ceiling %s", got, repairMaxRequestTimeout)
	}
}

// An answer landing after its request expired must be recognized: counted as
// late (not dropped as noise), the peer credited as a responder, and the
// shred attributed as repair-delivered.
func TestLateResponseMatchedAfterExpiry(t *testing.T) {
	c := newPacingTestClient(t)
	from := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 7), Port: 8007}
	addrKey, ok := repairAddressKeyFromUDP(from)
	if !ok {
		t.Fatal("premise: addr key")
	}

	reqKey := repairRequestKey{kind: repairRequestWindowIndex, slot: 42, index: 3}
	sentAt := time.Now().Add(-5 * time.Second)
	c.outstanding[reqKey] = outstandingRepairRequest{key: reqKey, nonce: 777, addr: addrKey, sentAt: sentAt}
	c.byResponse[repairResponseKey{addr: addrKey, nonce: 777}] = reqKey

	c.expireOutstanding(time.Now())
	if c.timeouts.Load() != 1 || len(c.outstanding) != 0 {
		t.Fatalf("expiry: timeouts=%d outstanding=%d, want 1/0", c.timeouts.Load(), len(c.outstanding))
	}

	packet := nonceTrailer(777)
	shred := &Shred{Slot: 42, Index: 3, Type: ShredTypeData}
	if !c.observeShredResponse(nil, packet, from, shred) {
		t.Fatal("late answer must be attributed as a repair delivery")
	}
	if c.lateResponses.Load() != 1 {
		t.Fatalf("lateResponses = %d, want 1", c.lateResponses.Load())
	}
	if rec := c.perPeer[addrKey]; rec == nil || rec.matched != 1 {
		t.Fatal("late-answering peer must earn responder credit")
	}
	if c.respLatencyEWMA < 4.5 {
		t.Fatalf("late latency %.2fs must feed the EWMA (~5s)", c.respLatencyEWMA)
	}

	// Second delivery of the same nonce: entry consumed, ordinary broadcast.
	if c.observeShredResponse(nil, packet, from, shred) {
		t.Fatal("expired entry must be single-use")
	}
}

// Followups draw from the shared token bucket: a drained bucket sends none
// (the scan covers the slot on its own cadence), a full one sends the
// followup cap plus the reserved chained probe and keeps the remainder.
func TestFollowupsAreMeteredByTokenBucket(t *testing.T) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer conn.Close()
	sink, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("sink listen: %v", err)
	}
	defer sink.Close()

	prime := func() (*repairClient, *net.UDPAddr, []byte, *Shred) {
		c := newPacingTestClient(t)
		c.peerCache = []gossip.RepairPeer{{Addr: sink.LocalAddr().(*net.UDPAddr)}}
		c.peerCacheAt = time.Now()

		from := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 9), Port: 8009}
		addrKey, _ := repairAddressKeyFromUDP(from)
		reqKey := repairRequestKey{kind: repairRequestHighestWindowIndex, slot: 50, index: 0}
		c.outstanding[reqKey] = outstandingRepairRequest{key: reqKey, nonce: 5, addr: addrKey, sentAt: time.Now()}
		c.byResponse[repairResponseKey{addr: addrKey, nonce: 5}] = reqKey

		// Highest response reveals a 200-shred gap, not last-in-slot.
		return c, from, nonceTrailer(5), &Shred{Slot: 50, Index: 200, Type: ShredTypeData}
	}

	c, from, packet, shred := prime()
	c.takeRateTokens(repairMaxRequestsPerSecond) // drain the bucket
	if !c.observeShredResponse(conn, packet, from, shred) {
		t.Fatal("response itself must match")
	}
	if got := c.requests.Load(); got != 0 {
		t.Fatalf("drained bucket sent %d followups, want 0", got)
	}

	c, from, packet, shred = prime()
	if !c.observeShredResponse(conn, packet, from, shred) {
		t.Fatal("response itself must match")
	}
	want := uint64(repairMaxFollowupRequests + 1) // window cap + chained highest probe
	if got := c.requests.Load(); got != want {
		t.Fatalf("full bucket sent %d requests, want %d", got, want)
	}
	probeKey := repairRequestKey{kind: repairRequestHighestWindowIndex, slot: 50, index: 201}
	if _, ok := c.outstanding[probeKey]; !ok {
		t.Fatal("chained highest probe must have gone out (its token is reserved)")
	}
	c.mu.Lock()
	remaining := c.rateTokens
	c.mu.Unlock()
	wantTokens := float64(repairMaxRequestsPerSecond) - float64(want)
	if remaining < wantTokens-1 || remaining > wantTokens+1 {
		t.Fatalf("bucket holds %.0f tokens, want ~%.0f (unused grant returned)", remaining, wantTokens)
	}
}
