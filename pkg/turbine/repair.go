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
	repairRequestTimeout      = 1500 * time.Millisecond
	repairPeerRefreshInterval = 2 * time.Second
	repairMaxSlotsPerScan     = 32
	repairMaxMissingPerSlot   = 256
	repairMaxFollowupRequests = 256
	repairMaxOutstanding      = 2048
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

	requests  atomic.Uint64
	responses atomic.Uint64
	timeouts  atomic.Uint64
	pings     atomic.Uint64
	pongs     atomic.Uint64
	errors    atomic.Uint64
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
	return &repairClient{
		identity:    append(ed25519.PrivateKey(nil), identity...),
		peerSource:  peerSource,
		outstanding: make(map[repairRequestKey]outstandingRepairRequest),
		byResponse:  make(map[repairResponseKey]repairRequestKey),
		perPeer:     make(map[repairAddressKey]*peerRecord),
	}, nil
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
	requests := assembler.RepairRequests(repairMaxSlotsPerScan, repairMaxMissingPerSlot)
	if len(requests) == 0 {
		return
	}

	budget := repairMaxSlotsPerScan * (repairMaxMissingPerSlot + 1)
	if budget > repairMaxOutstanding {
		budget = repairMaxOutstanding
	}
	if rate := c.takeRateTokens(budget); rate < budget {
		budget = rate
	}
	if budget <= 0 {
		return
	}
	for reqIdx, req := range requests {
		// The FIRST request is the emission-gating head (priority order puts
		// it first): its shreds get bounded 2-peer redundancy so one silent
		// pick cannot cost the whole pipeline a request-timeout of latency.
		// Everything else stays single-flight.
		fanout := uint8(1)
		if reqIdx == 0 {
			fanout = 2
		}
		for _, index := range req.MissingDataShreds {
			for attempt := uint8(0); attempt < fanout; attempt++ {
				if budget <= 0 {
					return
				}
				if c.sendRequest(conn, peers, repairRequestWindowIndex, req.Slot, index, attempt) {
					budget--
				}
			}
		}
		if req.NeedHighestDataShred {
			for attempt := uint8(0); attempt < fanout; attempt++ {
				if budget <= 0 {
					return
				}
				if c.sendRequest(conn, peers, repairRequestHighestWindowIndex, req.Slot, req.HighestDataShredIndex, attempt) {
					budget--
				}
			}
		}
	}
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
		c.mu.Unlock()
		return false
	}
	outstanding := c.outstanding[reqKey]
	delete(c.byResponse, responseKey)
	delete(c.outstanding, reqKey)
	c.notePeerMatchedLocked(addrKey)
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
	followups := 0
	for index := start; index < shred.Index && followups < repairMaxFollowupRequests; index++ {
		if c.sendRequest(conn, peers, repairRequestWindowIndex, shred.Slot, index, 0) {
			followups++
		}
	}
	if !shred.LastInSlot() && followups < repairMaxFollowupRequests && shred.Index < maxDataShredsPerSlot-1 {
		c.sendRequest(conn, peers, repairRequestHighestWindowIndex, shred.Slot, shred.Index+1, 0)
	}
	return true
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
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, outstanding := range c.outstanding {
		if now.Sub(outstanding.sentAt) < repairRequestTimeout {
			continue
		}
		delete(c.outstanding, key)
		delete(c.byResponse, repairResponseKey{addr: outstanding.addr, nonce: outstanding.nonce})
		c.timeouts.Add(1)
	}
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
	c.mu.Unlock()
	return RepairStats{
		Requests:        c.requests.Load(),
		Responses:       c.responses.Load(),
		Timeouts:        c.timeouts.Load(),
		Pings:           c.pings.Load(),
		Pongs:           c.pongs.Load(),
		Errors:          c.errors.Load(),
		Outstanding:     outstanding,
		Peers:           peers,
		RespondingPeers: responding,
	}
}
