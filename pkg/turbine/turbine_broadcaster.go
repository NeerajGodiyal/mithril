package turbine

import (
	"fmt"
	"net"
	"sync"

	"github.com/Overclock-Validator/mithril/pkg/gossip"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/gagliardetto/solana-go"
)

const defaultBroadcastLeaderSlots = 4 // protocol default consecutive leader window

// TVUPeerSource supplies gossip TVU peers for turbine routing.
type TVUPeerSource interface {
	TVUPeers() []gossip.TVUPeer
	SelfTVUAddr() *net.UDPAddr
	LookupTVU(pubkey solana.PublicKey) (*net.UDPAddr, bool)
}

// TurbineBroadcaster sends each shred to the turbine root peer for the slot.
type TurbineBroadcaster struct {
	mu sync.Mutex

	conn   *net.UDPConn
	self   solana.PublicKey
	peers  TVUPeerSource
	stakes func(uint64) map[solana.PublicKey]uint64
	epoch  func(uint64) uint64

	leaderForSlot func(slot uint64) (solana.PublicKey, bool)
	leaderSlots   uint64

	useChaCha8 bool
	dedupAddrs bool

	cacheEpoch uint64
	cacheNodes *ClusterNodes
}

type TurbineBroadcasterConfig struct {
	BindAddr      string
	Self          solana.PublicKey
	Peers         TVUPeerSource
	Stakes        func(slot uint64) map[solana.PublicKey]uint64
	EpochForSlot  func(slot uint64) uint64
	LeaderForSlot func(slot uint64) (solana.PublicKey, bool)
	LeaderSlots   uint64
	UseChaCha8    bool
	DedupAddrs    bool
}

func NewTurbineBroadcaster(cfg TurbineBroadcasterConfig) (*TurbineBroadcaster, error) {
	var conn *net.UDPConn
	if cfg.BindAddr != "" {
		addr, err := net.ResolveUDPAddr("udp", cfg.BindAddr)
		if err != nil {
			return nil, fmt.Errorf("resolve turbine broadcast bind %q: %w", cfg.BindAddr, err)
		}
		conn, err = net.ListenUDP("udp", addr)
		if err != nil {
			return nil, fmt.Errorf("listen turbine broadcast udp %q: %w", cfg.BindAddr, err)
		}
	}
	leaderSlots := cfg.LeaderSlots
	if leaderSlots == 0 {
		leaderSlots = defaultBroadcastLeaderSlots
	}
	return &TurbineBroadcaster{
		conn:          conn,
		self:          cfg.Self,
		peers:         cfg.Peers,
		stakes:        cfg.Stakes,
		epoch:         cfg.EpochForSlot,
		leaderForSlot: cfg.LeaderForSlot,
		leaderSlots:   leaderSlots,
		useChaCha8:    cfg.UseChaCha8,
		dedupAddrs:    true,
	}, nil
}

func (b *TurbineBroadcaster) Close() error {
	if b.conn == nil {
		return nil
	}
	return b.conn.Close()
}

func (b *TurbineBroadcaster) Broadcast(packets [][]byte) error {
	for _, packet := range packets {
		if err := b.broadcastPacket(packet); err != nil {
			return err
		}
	}
	return nil
}

func (b *TurbineBroadcaster) broadcastPacket(packet []byte) error {
	shredID, err := ShredIDFromPacket(packet)
	if err != nil {
		return fmt.Errorf("turbine broadcast: parse shred id: %w", err)
	}
	nodes := b.clusterNodesForSlot(shredID.Slot)
	dests := b.broadcastDestinations(shredID, nodes)
	if len(dests) == 0 {
		if pick, ok := nodes.BroadcastPeerPick(shredID); ok {
			// cavey TODO: maybe remove
			mlog.Log.Infof("cavey debug: turbine broadcast skipped shred slot=%d index=%d weighted_pick=%s",
				shredID.Slot, shredID.Index, pick)
		}
		return nil
	}
	for _, peer := range dests {
		if err := b.sendTo(packet, peer); err != nil {
			return err
		}
	}
	return nil
}

// broadcastDestinations selects turbine broadcast peers: weighted root plus
// optional next-leader TVU when distinct.
func (b *TurbineBroadcaster) broadcastDestinations(shredID ShredID, nodes *ClusterNodes) []*net.UDPAddr {
	var dests []*net.UDPAddr
	if addr, ok := nodes.BroadcastPeer(shredID); ok {
		dests = append(dests, addr)
	}
	if b.leaderForSlot != nil && b.peers != nil {
		nextSlot := shredID.Slot + b.leaderSlots
		nextLeader, ok := b.leaderForSlot(nextSlot)
		if ok && nextLeader != b.self {
			if addr, ok := b.peers.LookupTVU(nextLeader); ok {
				if addr, ok := broadcastTVUUDP(addr); ok {
					if len(dests) == 0 || dests[0].String() != addr.String() {
						dests = append(dests, addr)
					}
				}
			}
		}
	}
	return dests
}

func (b *TurbineBroadcaster) clusterNodesForSlot(slot uint64) *ClusterNodes {
	epoch := uint64(0)
	if b.epoch != nil {
		epoch = b.epoch(slot)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cacheNodes != nil && b.cacheEpoch == epoch {
		return b.cacheNodes
	}
	stakes := map[solana.PublicKey]uint64{}
	if b.stakes != nil {
		stakes = b.stakes(slot)
	}
	var tvuPeers []gossip.TVUPeer
	var selfTVU *net.UDPAddr
	if b.peers != nil {
		tvuPeers = b.peers.TVUPeers()
		selfTVU = b.peers.SelfTVUAddr()
	}
	b.cacheNodes = NewBroadcastClusterNodes(ClusterNodesConfig{
		Self:         b.self,
		SelfTVU:      selfTVU,
		TVUPeers:     tvuPeers,
		Stakes:       stakes,
		DedupTVUAddr: b.dedupAddrs,
		UseChaCha8:   b.useChaCha8,
	})
	b.cacheEpoch = epoch
	logClusterNodesWithoutTVU(b.cacheNodes, epoch, slot, len(tvuPeers), len(stakes))
	return b.cacheNodes
}

// cavey TODO: maybe remove after reporting ag nodes
func logClusterNodesWithoutTVU(nodes *ClusterNodes, epoch, slot uint64, gossipTVUPeers, stakedEntries int) {
	if nodes == nil {
		return
	}
	without := nodes.StakedWithoutBroadcastTVU()
	withTVU := 0
	for _, node := range nodes.nodes {
		if node.stake > 0 && node.canBroadcastTVU() {
			withTVU++
		}
	}
	mlog.Log.Infof("cavey debug: turbine cluster table epoch=%d slot=%d nodes=%d staked=%d gossip_tvu_peers=%d staked_with_broadcast_tvu=%d staked_without_broadcast_tvu=%d",
		epoch, slot, len(nodes.nodes), stakedEntries, gossipTVUPeers, withTVU, len(without))
	for _, node := range without {
		mlog.Log.Infof("cavey debug:   no_tvu %s", node)
	}
}

func (b *TurbineBroadcaster) sendTo(packet []byte, peer *net.UDPAddr) error {
	if peer == nil {
		return fmt.Errorf("turbine broadcast: nil peer")
	}
	if b.conn != nil {
		_, err := b.conn.WriteToUDP(packet, peer)
		return err
	}
	conn, err := net.DialUDP("udp", nil, peer)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write(packet)
	return err
}
