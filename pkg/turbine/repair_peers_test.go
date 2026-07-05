package turbine

import (
	"net"
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/gossip"
)

func repairTestPeer(ip byte, port int) gossip.RepairPeer {
	return gossip.RepairPeer{Addr: &net.UDPAddr{IP: net.IPv4(10, 0, 0, ip), Port: port}}
}

// Success-weighted selection: once one peer has answered, roughly 3 of 4
// requests concentrate on it while the full set keeps being explored — the
// contract that turns "95% of requests wasted on peers without the data"
// into "most requests go where answers come from".
func TestNextPeerPrefersRecentResponders(t *testing.T) {
	client := &repairClient{
		outstanding: make(map[repairRequestKey]outstandingRepairRequest),
		byResponse:  make(map[repairResponseKey]repairRequestKey),
		perPeer:     make(map[repairAddressKey]*peerRecord),
	}
	peers := make([]gossip.RepairPeer, 0, 8)
	for i := byte(1); i <= 8; i++ {
		peers = append(peers, repairTestPeer(i, 8000+int(i)))
	}

	responder := peers[2]
	key, ok := repairAddressKeyFromUDP(responder.Addr)
	if !ok {
		t.Fatalf("premise: responder addr key")
	}
	client.perPeer[key] = &peerRecord{matched: 1, lastMatched: time.Now()}

	hits := 0
	const picks = 400
	client.mu.Lock()
	for i := 0; i < picks; i++ {
		peer, ok := client.nextPeerLocked(peers)
		if !ok {
			t.Fatalf("selection must succeed with live peers")
		}
		if peer.Addr.String() == responder.Addr.String() {
			hits++
		}
	}
	client.mu.Unlock()

	// 3/4 of picks target the sole responder; the remaining exploration
	// quarter round-robins all 8 (occasionally landing on it too).
	if hits < picks/2 {
		t.Fatalf("responder got %d/%d picks; expected concentration (>=%d)", hits, picks, picks/2)
	}
	if hits == picks {
		t.Fatalf("exploration must still reach non-responders")
	}

	// A stale responder record (outside the window) drops back to blind
	// round-robin: no concentration.
	client.perPeer[key].lastMatched = time.Now().Add(-2 * repairResponderWindow)
	stale := 0
	client.mu.Lock()
	for i := 0; i < picks; i++ {
		peer, _ := client.nextPeerLocked(peers)
		if peer.Addr.String() == responder.Addr.String() {
			stale++
		}
	}
	client.mu.Unlock()
	if stale > picks/4 {
		t.Fatalf("stale responder still concentrated: %d/%d", stale, picks)
	}
}
