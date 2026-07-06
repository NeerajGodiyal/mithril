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
		expiredCur:  make(map[repairResponseKey]outstandingRepairRequest),
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
	if rec := c.perPeer[addrKey]; rec == nil || rec.late != 1 {
		t.Fatal("late-answering peer must earn responder credit (late count)")
	}
	if c.respLatencyEWMA < 4.5 {
		t.Fatalf("late latency %.2fs must feed the EWMA (~5s)", c.respLatencyEWMA)
	}

	// Second delivery of the same nonce: entry consumed, ordinary broadcast.
	if c.observeShredResponse(nil, packet, from, shred) {
		t.Fatal("expired entry must be single-use")
	}
}

// A late answer carrying a shred for the WRONG slot is rejected exactly like
// a timely mismatch would be — the expired record keeps the request's slot,
// so a peer cannot ride an old nonce to repair-attribute arbitrary data.
func TestLateResponseWrongSlotRejected(t *testing.T) {
	c := newPacingTestClient(t)
	from := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 8), Port: 8008}
	addrKey, _ := repairAddressKeyFromUDP(from)

	reqKey := repairRequestKey{kind: repairRequestWindowIndex, slot: 42, index: 3}
	c.outstanding[reqKey] = outstandingRepairRequest{key: reqKey, nonce: 900, addr: addrKey, sentAt: time.Now().Add(-5 * time.Second)}
	c.byResponse[repairResponseKey{addr: addrKey, nonce: 900}] = reqKey
	c.expireOutstanding(time.Now())

	if c.observeShredResponse(nil, nonceTrailer(900), from, &Shred{Slot: 43, Index: 3, Type: ShredTypeData}) {
		t.Fatal("wrong-slot late answer must not be attributed as repair")
	}
	if c.lateResponses.Load() != 0 {
		t.Fatalf("lateResponses = %d, want 0 (slot check precedes counting)", c.lateResponses.Load())
	}
}

// A LATE HighestWindowIndex answer fires the same metered gap followups as a
// timely one — the revealed missing range is the whole point of the probe.
func TestLateHighestResponseFiresFollowups(t *testing.T) {
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

	c := newPacingTestClient(t)
	c.peerCache = []gossip.RepairPeer{{Addr: sink.LocalAddr().(*net.UDPAddr)}}
	c.peerCacheAt = time.Now()
	from := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 9), Port: 8009}
	addrKey, _ := repairAddressKeyFromUDP(from)
	reqKey := repairRequestKey{kind: repairRequestHighestWindowIndex, slot: 50, index: 0}
	c.outstanding[reqKey] = outstandingRepairRequest{key: reqKey, nonce: 6, addr: addrKey, sentAt: time.Now().Add(-5 * time.Second)}
	c.byResponse[repairResponseKey{addr: addrKey, nonce: 6}] = reqKey
	c.expireOutstanding(time.Now())

	if !c.observeShredResponse(conn, nonceTrailer(6), from, &Shred{Slot: 50, Index: 200, Type: ShredTypeData}) {
		t.Fatal("late HWI answer must match")
	}
	if c.lateResponses.Load() != 1 || c.responses.Load() != 0 {
		t.Fatalf("late=%d responses=%d, want 1/0", c.lateResponses.Load(), c.responses.Load())
	}
	// Single sink peer: the per-peer inflight cap binds (same as the timely
	// path) — parity is the point, the exact count is the cap.
	want := uint64(repairPerPeerInflightCap)
	if got := c.requests.Load(); got != want {
		t.Fatalf("late HWI sent %d followups, want %d (same as timely)", got, want)
	}
}

// The head is strictly first in line but holds at most its admission
// SHARE; the fanout decision comes from the FULL listing (endgame is about
// the slot's closeness to completion, not this scan's room), and the fill
// SKIPS indices already in flight — a plain prefix truncation would
// re-select the mostly-outstanding prefix and under-fill the share.
func TestBoundHeadToAdmissionShare(t *testing.T) {
	share := int(float64(repairMaxRequestsPerSecond) * repairAdmissionQueueSeconds * repairHeadAdmissionShare)

	c := newPacingTestClient(t)
	for i := 0; i < 100; i++ {
		key := repairRequestKey{kind: repairRequestWindowIndex, slot: 70, index: uint32(i)}
		c.outstanding[key] = outstandingRepairRequest{key: key}
	}
	head := SlotRepairRequest{Slot: 70, MissingDataShreds: seq(100, 1099)}
	if fanout := c.boundHeadToAdmissionShare(&head); fanout != 1 {
		t.Fatal("bulk head must stay single-flight")
	}
	if got := len(head.MissingDataShreds); got != share-100 {
		t.Fatalf("head listing = %d, want %d (share %d minus 100 in flight)", got, share-100, share)
	}

	// Overlap case (the codex-caught bug): the listing's prefix IS in
	// flight. The fill must skip it and still use the whole room with NEW
	// indices instead of returning a mostly-dedup'd prefix.
	c2 := newPacingTestClient(t)
	inflight := share / 2
	for i := 0; i < inflight; i++ {
		key := repairRequestKey{kind: repairRequestWindowIndex, slot: 72, index: uint32(i)}
		c2.outstanding[key] = outstandingRepairRequest{key: key}
	}
	overlap := SlotRepairRequest{Slot: 72, MissingDataShreds: seq(0, 999)}
	c2.boundHeadToAdmissionShare(&overlap)
	room := share - inflight
	if got := len(overlap.MissingDataShreds); got != room {
		t.Fatalf("overlap fill = %d indices, want the full room %d", got, room)
	}
	if overlap.MissingDataShreds[0] != uint32(inflight) {
		t.Fatalf("overlap fill starts at %d, want %d (first NOT-in-flight index)", overlap.MissingDataShreds[0], inflight)
	}

	// Endgame: fanout 2 survives the share bound, and the listing shrinks
	// to the remaining room.
	c3 := newPacingTestClient(t)
	for i := 0; i < share-4; i++ {
		key := repairRequestKey{kind: repairRequestWindowIndex, slot: 71, index: uint32(i)}
		c3.outstanding[key] = outstandingRepairRequest{key: key}
	}
	small := SlotRepairRequest{Slot: 71, MissingDataShreds: seq(2000, 2009)}
	if fanout := c3.boundHeadToAdmissionShare(&small); fanout != 2 {
		t.Fatal("endgame head must keep 2-peer fanout after the share bound")
	}
	if got := len(small.MissingDataShreds); got != 4 {
		t.Fatalf("endgame listing = %d, want 4 (room left in the share)", got)
	}
}

// Selection skips peers at their inflight cap: a slow peer's admission
// slots recycle 5-8x slower than a fast one's, so unbounded per-peer
// queueing lets the slow population clog the whole window.
func TestSelectionSkipsSaturatedPeers(t *testing.T) {
	client := &repairClient{perPeer: make(map[repairAddressKey]*peerRecord)}
	peers := []gossip.RepairPeer{
		{Addr: &net.UDPAddr{IP: net.IPv4(10, 0, 0, 41), Port: 9401}},
		{Addr: &net.UDPAddr{IP: net.IPv4(10, 0, 0, 42), Port: 9402}},
	}
	k1, _ := repairAddressKeyFromUDP(peers[0].Addr)
	k2, _ := repairAddressKeyFromUDP(peers[1].Addr)
	client.perPeer[k1] = &peerRecord{lastMatched: time.Now(), inflight: repairPerPeerInflightCap}
	client.perPeer[k2] = &peerRecord{lastMatched: time.Now()}

	client.mu.Lock()
	for i := 0; i < 50; i++ {
		peer, ok := client.nextPeerLocked(peers)
		if !ok {
			t.Fatal("pick must succeed while an unsaturated peer exists")
		}
		if peer.Addr.String() == peers[0].Addr.String() {
			t.Fatal("saturated peer must be skipped")
		}
	}
	client.perPeer[k2].inflight = repairPerPeerInflightCap
	client.rankedAt = time.Time{} // force ranked rebuild after mutation
	if _, ok := client.nextPeerLocked(peers); ok {
		t.Fatal("all peers saturated must yield no pick (tokens return, retry next scan)")
	}
	client.mu.Unlock()
}

// The inflight counter follows the outstanding entry's lifecycle: one
// decrement when the entry leaves (timely match or expiry) — a late answer
// must NOT decrement again.
func TestPeerInflightLifecycle(t *testing.T) {
	c := &repairClient{perPeer: make(map[repairAddressKey]*peerRecord)}
	addr := repairAddressKey{port: 9500}
	c.mu.Lock()
	c.notePeerSentLocked(addr)
	c.notePeerSentLocked(addr)
	if c.perPeer[addr].inflight != 2 {
		t.Fatalf("inflight after 2 sends = %d, want 2", c.perPeer[addr].inflight)
	}
	c.notePeerTimelyLocked(addr, 100*time.Millisecond)
	c.notePeerTimeoutLocked(addr)
	if c.perPeer[addr].inflight != 0 {
		t.Fatalf("inflight after match+expiry = %d, want 0", c.perPeer[addr].inflight)
	}
	c.notePeerLateLocked(addr, time.Second)
	if c.perPeer[addr].inflight != 0 {
		t.Fatalf("late answer double-decremented inflight: %d", c.perPeer[addr].inflight)
	}
	c.mu.Unlock()
}

// Admission cap: outstanding never exceeds ~1s of service rate no matter
// what the token bucket would allow — queue DEPTH, not the timeout, bounds
// peer-side queueing (the bufferbloat spiral observed live: outstanding
// ~3000 at 500/s pushed avg latency to 4.4s with zero throughput gain).
func TestAdmissionCapBoundsOutstanding(t *testing.T) {
	c := newPacingTestClient(t)
	admission := int(float64(repairMaxRequestsPerSecond) * repairAdmissionQueueSeconds)
	for i := 0; i < admission-10; i++ {
		key := repairRequestKey{kind: repairRequestWindowIndex, slot: 1, index: uint32(i)}
		c.outstanding[key] = outstandingRepairRequest{key: key}
	}
	if got := c.takeRateTokens(100); got != 10 {
		t.Fatalf("grant = %d, want 10 (admission room, not bucket balance)", got)
	}
	for i := admission - 10; i < admission; i++ {
		key := repairRequestKey{kind: repairRequestWindowIndex, slot: 1, index: uint32(i)}
		c.outstanding[key] = outstandingRepairRequest{key: key}
	}
	if got := c.takeRateTokens(100); got != 0 {
		t.Fatalf("grant at cap = %d, want 0 (full token bucket must not override admission)", got)
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
	// Drain the bucket. Two calls: the admission cap trims the first grant
	// by the primed outstanding entry, leaving a stray token.
	c.takeRateTokens(repairMaxRequestsPerSecond)
	c.takeRateTokens(repairMaxRequestsPerSecond)
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
	// A SINGLE available peer binds on the per-peer inflight cap before the
	// followup cap: 16 sends land, the rest (and the chained probe) are
	// refused by saturation-aware selection, and their tokens return.
	want := uint64(repairPerPeerInflightCap)
	if got := c.requests.Load(); got != want {
		t.Fatalf("full bucket sent %d requests, want %d (single peer capped at its inflight limit)", got, want)
	}
	c.mu.Lock()
	remaining := c.rateTokens
	c.mu.Unlock()
	wantTokens := float64(repairMaxRequestsPerSecond) - float64(want)
	if remaining < wantTokens-1 || remaining > wantTokens+1 {
		t.Fatalf("bucket holds %.0f tokens, want ~%.0f (unused grant returned)", remaining, wantTokens)
	}
}
