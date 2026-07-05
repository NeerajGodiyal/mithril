package turbine

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/gossip"
	repairproto "github.com/Overclock-Validator/mithril/pkg/repair"
)

const (
	repairScanInterval        = 100 * time.Millisecond
	repairPeerRefreshInterval = 2 * time.Second
	repairMaxSlotsPerScan     = 32
	repairMaxMissingPerSlot   = 256
	repairMaxFollowupRequests = 64
	repairMaxOutstanding      = 4096
	// Adaptive request timeout: peers serve unstaked requesters from QoS
	// queues whose depth tracks OUR OWN send rate, so a fixed short timeout
	// under load re-requests shreds whose answers are already queued — and
	// each duplicate deepens the queue (observed live: 80% "timeouts" while
	// nearly every request was in fact answered late). The effective timeout
	// is repairTimeoutLatencyFactor x the smoothed observed response latency,
	// clamped to this range.
	repairMinRequestTimeout    = 1500 * time.Millisecond
	repairMaxRequestTimeout    = 6 * time.Second
	repairTimeoutLatencyFactor = 3
	repairLatencyEWMAAlpha     = 0.1
	// Expired-request memory: (responder, nonce) keys of timed-out requests,
	// two rotating generations. A response landing after expiry is matched
	// here instead of vanishing into ignored_old — it counts as a LATE
	// answer, credits the peer as a responder, and feeds its true latency to
	// the adaptive timeout.
	repairExpiredGenCap = 8192
	// Global request-rate ceiling. Serve-repair QoS on Agave-family peers
	// deprioritizes and then effectively BANS heavy unstaked requesters —
	// observed live: ~2000 req/s sustained answered normally for ~90s, then
	// responses froze entirely while requests kept flowing. A bounded rate
	// with a longer per-request timeout yields more than an unbounded flood.
	repairMaxRequestsPerSecond = 500
	// Success-weighted peer selection: a peer that answered within this
	// window counts as a responder, and 3 of 4 requests go to responders.
	// Blind round-robin across a cluster where only a few peers still
	// retain old shreds wastes nearly every request on silence (observed
	// live: 95% timeouts during a resume-gap catchup).
	repairResponderWindow = 60 * time.Second
	repairPeerStatsCap    = 1024
)

type RepairPeerSource func() []gossip.RepairPeer

type RepairStats struct {
	Requests    uint64
	Responses   uint64
	Timeouts    uint64
	Pings       uint64
	Pongs       uint64
	Errors      uint64
	Outstanding int
	Peers       int
	// Peers that answered a request within repairResponderWindow — the
	// number that separates "cluster serves this range" from "only N nodes
	// still retain it".
	RespondingPeers int
	// Answers that arrived AFTER their request expired (already counted in
	// Timeouts). LateResponses tracking Timeouts means peers serve nearly
	// everything but slower than the timeout — a pacing problem, not loss.
	LateResponses uint64
	// The adaptive request timeout currently in effect, and the smoothed
	// response latency driving it.
	TimeoutMillis     int64
	AvgResponseMillis int64
}

type repairRequestKind uint8

const (
	repairRequestWindowIndex repairRequestKind = iota
	repairRequestHighestWindowIndex
)

type repairRequestKey struct {
	kind  repairRequestKind
	slot  uint64
	index uint32
	// attempt distinguishes bounded redundant sends of the SAME shred to
	// different peers (head-slot fanout). 0 for ordinary single-flight.
	attempt uint8
}

type repairAddressKey struct {
	ip   [16]byte
	port int
}

type repairResponseKey struct {
	addr  repairAddressKey
	nonce uint32
}

type outstandingRepairRequest struct {
	key    repairRequestKey
	nonce  uint32
	addr   repairAddressKey
	sentAt time.Time
}

type peerRecord struct {
	sent        uint64
	matched     uint64
	lastMatched time.Time
}

type repairClient struct {
	identity   ed25519.PrivateKey
	peerSource RepairPeerSource

	mu          sync.Mutex
	outstanding map[repairRequestKey]outstandingRepairRequest
	byResponse  map[repairResponseKey]repairRequestKey
	perPeer     map[repairAddressKey]*peerRecord
	peerCursor  uint64

	peerCacheMu sync.Mutex
	peerCache   []gossip.RepairPeer
	peerCacheAt time.Time

	// Token bucket for the global request-rate ceiling (guarded by mu).
	rateTokens   float64
	rateRefillAt time.Time

	// Adaptive timeout state: EWMA of observed response latency (mu-guarded),
	// effective timeout published atomically for the expiry scan.
	respLatencyEWMA float64 // seconds
	timeoutNanos    atomic.Int64

	// Recently-expired requests keyed like byResponse, for late-answer
	// matching (mu-guarded, rotating generations).
	expiredCur  map[repairResponseKey]time.Time
	expiredPrev map[repairResponseKey]time.Time

	requests      atomic.Uint64
	responses     atomic.Uint64
	timeouts      atomic.Uint64
	lateResponses atomic.Uint64
	pings         atomic.Uint64
	pongs         atomic.Uint64
	errors        atomic.Uint64
}

func newRepairClient(identity ed25519.PrivateKey, peerSource RepairPeerSource) (*repairClient, error) {
	var err error
	if len(identity) == 0 {
		_, identity, err = ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("generate repair identity: %w", err)
		}
	}
	if len(identity) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid repair identity size %d", len(identity))
	}
	if peerSource == nil {
		return nil, fmt.Errorf("repair peer source is required")
	}
	c := &repairClient{
		identity:    append(ed25519.PrivateKey(nil), identity...),
		peerSource:  peerSource,
		outstanding: make(map[repairRequestKey]outstandingRepairRequest),
		byResponse:  make(map[repairResponseKey]repairRequestKey),
		perPeer:     make(map[repairAddressKey]*peerRecord),
		expiredCur:  make(map[repairResponseKey]time.Time, repairExpiredGenCap),
	}
	c.timeoutNanos.Store(int64(repairMinRequestTimeout))
	return c, nil
}

func (c *repairClient) run(ctx context.Context, conn *net.UDPConn, assembler *SlotAssembler) {
	ticker := time.NewTicker(repairScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.expireOutstanding(time.Now())
			c.repairOnce(conn, assembler)
		}
	}
}

func (c *repairClient) repairOnce(conn *net.UDPConn, assembler *SlotAssembler) {
	peers := c.peerSnapshot(time.Now())
	if len(peers) == 0 {
		return
	}
	priority, edge := assembler.RepairRequestsTiered(repairMaxSlotsPerScan, repairMaxMissingPerSlot)
	if len(priority)+len(edge) == 0 {
		return
	}

	// Size the token grant by what this scan can actually put on the wire:
	// granted tokens are consumed from the bucket whether used or not, and
	// most scans re-list shreds whose requests are still outstanding (dedup
	// sends nothing for them). Unused grant is returned after the send pass,
	// so the rate cap means "requests sent", not "requests considered".
	edgeDemand := tierSendDemand(edge, false)
	want := tierSendDemand(priority, true) + edgeDemand
	if want > repairMaxOutstanding {
		want = repairMaxOutstanding
	}
	budget := c.takeRateTokens(want)
	if budget <= 0 {
		return
	}

	// Freshness split: most of the budget unblocks replay (the priority head
	// window), but a reserved slice patches LIVE-EDGE holes while peers still
	// serve those shreds hot — a hole repaired at receipt costs one cheap
	// round trip; the same hole discovered at hydration time, minutes later,
	// competes with the head for budget against colder peers. The reserve is
	// sized by the edge's actual demand, so a blocked head is never capped
	// below what the edge can use; leftover head budget flows to the edge.
	spent := c.sendTier(conn, peers, priority, splitRepairBudget(budget, edgeDemand), true)
	spent += c.sendTier(conn, peers, edge, budget-spent, false)
	c.returnRateTokens(budget - spent)
}

// tierSendDemand counts the packets sendTier would emit for a tier given
// unlimited budget (head fanout doubles the first request's sends).
func tierSendDemand(requests []SlotRepairRequest, fanoutHead bool) int {
	demand := 0
	for reqIdx, req := range requests {
		per := len(req.MissingDataShreds)
		if req.NeedHighestDataShred {
			per++
		}
		if fanoutHead && reqIdx == 0 {
			per *= 2
		}
		demand += per
	}
	return demand
}

// splitRepairBudget returns the head tier's share: everything except a
// freshness reserve of a fifth, bounded by the edge's actual demand.
func splitRepairBudget(budget, edgeDemand int) int {
	reserve := budget / 5
	if reserve > edgeDemand {
		reserve = edgeDemand
	}
	return budget - reserve
}

// sendTier sends one tier's requests within budget and returns how many
// were actually sent. fanoutHead gives the FIRST request (the
// emission-gating head) bounded 2-peer redundancy.
func (c *repairClient) sendTier(conn *net.UDPConn, peers []gossip.RepairPeer, requests []SlotRepairRequest, budget int, fanoutHead bool) int {
	sent := 0
	for reqIdx, req := range requests {
		fanout := uint8(1)
		if fanoutHead && reqIdx == 0 {
			fanout = 2
		}
		for _, index := range req.MissingDataShreds {
			for attempt := uint8(0); attempt < fanout; attempt++ {
				if sent >= budget {
					return sent
				}
				if c.sendRequest(conn, peers, repairRequestWindowIndex, req.Slot, index, attempt) {
					sent++
				}
			}
		}
		if req.NeedHighestDataShred {
			for attempt := uint8(0); attempt < fanout; attempt++ {
				if sent >= budget {
					return sent
				}
				if c.sendRequest(conn, peers, repairRequestHighestWindowIndex, req.Slot, req.HighestDataShredIndex, attempt) {
					sent++
				}
			}
		}
	}
	return sent
}

func (c *repairClient) handleRepairPing(conn *net.UDPConn, packet []byte, from *net.UDPAddr) bool {
	ping, ok := repairproto.DecodePing(packet)
	if !ok {
		return false
	}
	c.pings.Add(1)
	pong, err := repairproto.BuildPong(c.identity, ping)
	if err != nil {
		c.errors.Add(1)
		return true
	}
	if _, err := conn.WriteToUDP(pong, from); err != nil {
		c.errors.Add(1)
		return true
	}
	c.pongs.Add(1)
	return true
}

// observeShredResponse matches an incoming packet against outstanding repair
// requests (responder address + nonce). Returns true when the shred was
// delivered BY REPAIR — it answers one of our requests — so the caller can
// attribute it in per-slot repair accounting.
func (c *repairClient) observeShredResponse(conn *net.UDPConn, packet []byte, from *net.UDPAddr, shred *Shred) bool {
	if from == nil || shred == nil {
		return false
	}
	nonce, ok := repairproto.ResponseNonce(packet)
	if !ok {
		return false
	}
	addrKey, ok := repairAddressKeyFromUDP(from)
	if !ok {
		return false
	}
	responseKey := repairResponseKey{addr: addrKey, nonce: nonce}

	c.mu.Lock()
	reqKey, ok := c.byResponse[responseKey]
	if !ok {
		// Not outstanding — but it may be the LATE answer to a request we
		// already expired. Matching it here keeps the peer's responder
		// credit, attributes the shred as repair-delivered, and feeds the
		// true tail latency to the adaptive timeout instead of letting the
		// packet vanish into ignored_old.
		if sentAt, late := c.expiredCur[responseKey]; late {
			delete(c.expiredCur, responseKey)
			c.noteLateResponseLocked(addrKey, sentAt)
			c.mu.Unlock()
			return true
		}
		if sentAt, late := c.expiredPrev[responseKey]; late {
			delete(c.expiredPrev, responseKey)
			c.noteLateResponseLocked(addrKey, sentAt)
			c.mu.Unlock()
			return true
		}
		c.mu.Unlock()
		return false
	}
	outstanding := c.outstanding[reqKey]
	delete(c.byResponse, responseKey)
	delete(c.outstanding, reqKey)
	c.notePeerMatchedLocked(addrKey)
	c.observeLatencyLocked(time.Since(outstanding.sentAt))
	c.mu.Unlock()

	if outstanding.key.slot != shred.Slot {
		return false
	}
	c.responses.Add(1)
	if outstanding.key.kind != repairRequestHighestWindowIndex || shred.Type != ShredTypeData {
		return true
	}

	peers := c.peerSnapshot(time.Now())
	if len(peers) == 0 {
		return true
	}
	start := outstanding.key.index
	gap := 0
	if shred.Index > start {
		gap = int(shred.Index - start)
	}
	ask := gap
	if ask > repairMaxFollowupRequests {
		ask = repairMaxFollowupRequests
	}
	chainProbe := !shred.LastInSlot() && shred.Index < maxDataShredsPerSlot-1
	if chainProbe {
		ask++
	}
	if ask == 0 {
		return true
	}
	// Followups draw from the SAME token bucket as the scan. This path used
	// to be unmetered — with hundreds of probed slots it pushed the total
	// send rate ~70% past the cap, which is exactly the flood the peer-side
	// QoS ban punishes. When the bucket is dry the scan's deficit-aware
	// selection covers the slot on its own cadence.
	grant := c.takeRateTokens(ask)
	if grant <= 0 {
		return true
	}
	windowBudget := grant
	if chainProbe && windowBudget > 0 {
		windowBudget-- // reserve the chained probe's token
	}
	followups := 0
	for index := start; index < shred.Index && followups < windowBudget; index++ {
		if c.sendRequest(conn, peers, repairRequestWindowIndex, shred.Slot, index, 0) {
			followups++
		}
	}
	if chainProbe && followups < grant {
		if c.sendRequest(conn, peers, repairRequestHighestWindowIndex, shred.Slot, shred.Index+1, 0) {
			followups++
		}
	}
	c.returnRateTokens(grant - followups)
	return true
}

// noteLateResponseLocked credits a peer whose answer arrived after the
// request expired: responder status (it DOES serve us — just slower than the
// timeout allowed), late accounting, and the real observed latency.
func (c *repairClient) noteLateResponseLocked(addr repairAddressKey, sentAt time.Time) {
	c.notePeerMatchedLocked(addr)
	c.observeLatencyLocked(time.Since(sentAt))
	c.lateResponses.Add(1)
}

// observeLatencyLocked folds one request->response latency into the EWMA and
// refreshes the effective timeout: repairTimeoutLatencyFactor x the smoothed
// latency, clamped to [repairMinRequestTimeout, repairMaxRequestTimeout].
func (c *repairClient) observeLatencyLocked(lat time.Duration) {
	sec := lat.Seconds()
	if sec <= 0 {
		return
	}
	if c.respLatencyEWMA == 0 {
		c.respLatencyEWMA = sec
	} else {
		c.respLatencyEWMA = (1-repairLatencyEWMAAlpha)*c.respLatencyEWMA + repairLatencyEWMAAlpha*sec
	}
	eff := time.Duration(c.respLatencyEWMA * repairTimeoutLatencyFactor * float64(time.Second))
	if eff < repairMinRequestTimeout {
		eff = repairMinRequestTimeout
	}
	if eff > repairMaxRequestTimeout {
		eff = repairMaxRequestTimeout
	}
	c.timeoutNanos.Store(int64(eff))
}

func (c *repairClient) sendRequest(conn *net.UDPConn, peers []gossip.RepairPeer, kind repairRequestKind, slot uint64, index uint32, attempt uint8) bool {
	if conn == nil || len(peers) == 0 {
		return false
	}
	key := repairRequestKey{kind: kind, slot: slot, index: index, attempt: attempt}

	c.mu.Lock()
	if _, exists := c.outstanding[key]; exists {
		c.mu.Unlock()
		return false
	}
	if len(c.outstanding) >= repairMaxOutstanding {
		c.mu.Unlock()
		return false
	}
	peer, ok := c.nextPeerLocked(peers)
	c.mu.Unlock()
	if !ok {
		return false
	}

	var (
		packet []byte
		nonce  uint32
		err    error
	)
	switch kind {
	case repairRequestWindowIndex:
		packet, nonce, err = repairproto.NewWindowIndexRequest(c.identity, peer.Pubkey, slot, uint64(index))
	case repairRequestHighestWindowIndex:
		packet, nonce, err = repairproto.NewHighestWindowIndexRequest(c.identity, peer.Pubkey, slot, uint64(index))
	default:
		return false
	}
	if err != nil {
		c.errors.Add(1)
		return false
	}

	addrKey, ok := repairAddressKeyFromUDP(peer.Addr)
	if !ok {
		return false
	}
	responseKey := repairResponseKey{addr: addrKey, nonce: nonce}
	c.mu.Lock()
	if _, exists := c.outstanding[key]; exists {
		c.mu.Unlock()
		return false
	}
	if len(c.outstanding) >= repairMaxOutstanding {
		c.mu.Unlock()
		return false
	}
	c.outstanding[key] = outstandingRepairRequest{
		key:    key,
		nonce:  nonce,
		addr:   addrKey,
		sentAt: time.Now(),
	}
	c.byResponse[responseKey] = key
	c.notePeerSentLocked(addrKey)
	c.mu.Unlock()

	if _, err := conn.WriteToUDP(packet, peer.Addr); err != nil {
		c.mu.Lock()
		delete(c.outstanding, key)
		delete(c.byResponse, responseKey)
		c.mu.Unlock()
		c.errors.Add(1)
		return false
	}

	c.requests.Add(1)
	return true
}

// takeRateTokens refills the request-rate bucket and grants up to want
// tokens, returning how many requests this scan may send.
func (c *repairClient) takeRateTokens(want int) int {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rateRefillAt.IsZero() {
		c.rateRefillAt = now
		c.rateTokens = repairMaxRequestsPerSecond
	}
	elapsed := now.Sub(c.rateRefillAt).Seconds()
	c.rateRefillAt = now
	c.rateTokens += elapsed * repairMaxRequestsPerSecond
	if c.rateTokens > repairMaxRequestsPerSecond {
		c.rateTokens = repairMaxRequestsPerSecond // burst cap: one second's worth
	}
	grant := want
	if float64(grant) > c.rateTokens {
		grant = int(c.rateTokens)
	}
	if grant < 0 {
		grant = 0
	}
	c.rateTokens -= float64(grant)
	return grant
}

// returnRateTokens puts the unused portion of a grant back in the bucket
// (bounded by the burst cap) so dedup-suppressed sends don't burn rate.
func (c *repairClient) returnRateTokens(count int) {
	if count <= 0 {
		return
	}
	c.mu.Lock()
	c.rateTokens += float64(count)
	if c.rateTokens > repairMaxRequestsPerSecond {
		c.rateTokens = repairMaxRequestsPerSecond
	}
	c.mu.Unlock()
}

func (c *repairClient) nextPeerLocked(peers []gossip.RepairPeer) (gossip.RepairPeer, bool) {
	if len(peers) == 0 {
		return gossip.RepairPeer{}, false
	}
	c.peerCursor++
	// 3 of 4 requests go to peers that answered recently; every 4th
	// round-robins the FULL set so new or newly-caught-up peers keep being
	// discovered and the responder set can grow back after churn.
	if c.peerCursor%4 != 0 {
		if peer, ok := c.pickResponderLocked(peers, c.peerCursor); ok {
			return peer, true
		}
	}
	for attempts := 0; attempts < len(peers); attempts++ {
		index := int(c.peerCursor % uint64(len(peers)))
		c.peerCursor++
		peer := peers[index]
		if peer.Addr != nil && peer.Addr.Port != 0 {
			return peer, true
		}
	}
	return gossip.RepairPeer{}, false
}

// pickResponderLocked round-robins among peers that answered a repair request
// within repairResponderWindow. ok is false when no responder is known yet
// (fresh start, or the whole set went quiet) — the caller falls back to the
// full round-robin.
func (c *repairClient) pickResponderLocked(peers []gossip.RepairPeer, cursor uint64) (gossip.RepairPeer, bool) {
	now := time.Now()
	responders := make([]gossip.RepairPeer, 0, 16)
	for _, peer := range peers {
		if peer.Addr == nil || peer.Addr.Port == 0 {
			continue
		}
		key, ok := repairAddressKeyFromUDP(peer.Addr)
		if !ok {
			continue
		}
		if rec := c.perPeer[key]; rec != nil && now.Sub(rec.lastMatched) <= repairResponderWindow {
			responders = append(responders, peer)
		}
	}
	if len(responders) == 0 {
		return gossip.RepairPeer{}, false
	}
	return responders[int(cursor%uint64(len(responders)))], true
}

// notePeerSentLocked / notePeerMatchedLocked maintain the per-peer success
// records behind responder-weighted selection.
func (c *repairClient) notePeerSentLocked(addr repairAddressKey) {
	rec := c.perPeer[addr]
	if rec == nil {
		if len(c.perPeer) >= repairPeerStatsCap {
			// Bound memory across peer churn: drop stale non-responders.
			cutoff := time.Now().Add(-repairResponderWindow)
			for key, old := range c.perPeer {
				if old.lastMatched.Before(cutoff) {
					delete(c.perPeer, key)
				}
			}
		}
		rec = &peerRecord{}
		c.perPeer[addr] = rec
	}
	rec.sent++
}

func (c *repairClient) notePeerMatchedLocked(addr repairAddressKey) {
	rec := c.perPeer[addr]
	if rec == nil {
		rec = &peerRecord{}
		c.perPeer[addr] = rec
	}
	rec.matched++
	rec.lastMatched = time.Now()
}

func (c *repairClient) expireOutstanding(now time.Time) {
	timeout := time.Duration(c.timeoutNanos.Load())
	if timeout <= 0 {
		timeout = repairMinRequestTimeout
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, outstanding := range c.outstanding {
		if now.Sub(outstanding.sentAt) < timeout {
			continue
		}
		responseKey := repairResponseKey{addr: outstanding.addr, nonce: outstanding.nonce}
		delete(c.outstanding, key)
		delete(c.byResponse, responseKey)
		c.rememberExpiredLocked(responseKey, outstanding.sentAt)
		c.timeouts.Add(1)
	}
}

// rememberExpiredLocked keeps an expired request's response key so a late
// answer can still be recognized. Two rotating generations bound memory.
func (c *repairClient) rememberExpiredLocked(key repairResponseKey, sentAt time.Time) {
	if c.expiredCur == nil {
		c.expiredCur = make(map[repairResponseKey]time.Time, repairExpiredGenCap)
	}
	if len(c.expiredCur) >= repairExpiredGenCap {
		c.expiredPrev = c.expiredCur
		c.expiredCur = make(map[repairResponseKey]time.Time, repairExpiredGenCap)
	}
	c.expiredCur[key] = sentAt
}

func (c *repairClient) peerSnapshot(now time.Time) []gossip.RepairPeer {
	c.peerCacheMu.Lock()
	if !c.peerCacheAt.IsZero() && now.Sub(c.peerCacheAt) < repairPeerRefreshInterval {
		peers := c.peerCache
		c.peerCacheMu.Unlock()
		return peers
	}
	c.peerCacheMu.Unlock()

	peers := c.peerSource()

	c.peerCacheMu.Lock()
	c.peerCache = peers
	c.peerCacheAt = now
	c.peerCacheMu.Unlock()
	return peers
}

func (c *repairClient) cachedPeerCount() int {
	c.peerCacheMu.Lock()
	count := len(c.peerCache)
	cacheFresh := !c.peerCacheAt.IsZero() && time.Since(c.peerCacheAt) < repairPeerRefreshInterval
	c.peerCacheMu.Unlock()
	if cacheFresh || c.peerSource == nil {
		return count
	}
	return len(c.peerSnapshot(time.Now()))
}

func repairAddressKeyFromUDP(addr *net.UDPAddr) (repairAddressKey, bool) {
	var key repairAddressKey
	if addr == nil {
		return key, false
	}
	ip := addr.IP.To16()
	if ip == nil || addr.Port == 0 {
		return key, false
	}
	copy(key.ip[:], ip)
	key.port = addr.Port
	return key, true
}

func (c *repairClient) stats() RepairStats {
	peers := c.cachedPeerCount()
	cutoff := time.Now().Add(-repairResponderWindow)
	c.mu.Lock()
	outstanding := len(c.outstanding)
	responding := 0
	for _, rec := range c.perPeer {
		if rec.lastMatched.After(cutoff) {
			responding++
		}
	}
	avgResponseMillis := int64(c.respLatencyEWMA * 1000)
	c.mu.Unlock()
	return RepairStats{
		Requests:          c.requests.Load(),
		Responses:         c.responses.Load(),
		Timeouts:          c.timeouts.Load(),
		Pings:             c.pings.Load(),
		Pongs:             c.pongs.Load(),
		Errors:            c.errors.Load(),
		Outstanding:       outstanding,
		Peers:             peers,
		RespondingPeers:   responding,
		LateResponses:     c.lateResponses.Load(),
		TimeoutMillis:     int64(time.Duration(c.timeoutNanos.Load()) / time.Millisecond),
		AvgResponseMillis: avgResponseMillis,
	}
}
