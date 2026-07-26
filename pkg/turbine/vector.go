package turbine

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/Overclock-Validator/mithril/pkg/gossip"
	"github.com/gagliardetto/solana-go"
)

const VectorVersion = 1

type VectorFixture struct {
	Version         int               `json:"version"`
	SelfPubkey      string            `json:"self_pubkey"`
	SelfTVU         string            `json:"self_tvu,omitempty"`
	SelfStake       uint64            `json:"self_stake,omitempty"`
	UseChaCha8      bool              `json:"use_cha_cha_8"`
	DedupTVUAddrs   bool              `json:"dedup_tvu_addrs"`
	Slot            uint64            `json:"slot"`
	ShredType       uint8             `json:"shred_type"`
	MaxShredIndex   uint32            `json:"max_shred_index"`
	Peers           []VectorPeer      `json:"peers"`
	StakedNoContact []VectorStakeOnly `json:"staked_no_contact"`
}

type VectorPeer struct {
	Pubkey string `json:"pubkey"`
	Stake  uint64 `json:"stake"`
	TVU    string `json:"tvu"`
}

type VectorStakeOnly struct {
	Pubkey string `json:"pubkey"`
	Stake  uint64 `json:"stake"`
}

type VectorNode struct {
	Pubkey     string `json:"pubkey"`
	Stake      uint64 `json:"stake"`
	HasContact bool   `json:"has_contact"`
	TVU        string `json:"tvu,omitempty"`
}

type VectorBroadcastEntry struct {
	ShredIndex      uint32 `json:"shred_index"`
	RawNodeIndex    int    `json:"raw_node_index"`
	RawPubkey       string `json:"raw_pubkey,omitempty"`
	BroadcastPubkey string `json:"broadcast_pubkey,omitempty"`
	HasBroadcastTVU bool   `json:"has_broadcast_tvu"`
}

type VectorOutput struct {
	Version        int                    `json:"version"`
	Implementation string                 `json:"implementation"`
	Fixture        VectorFixture          `json:"fixture"`
	Nodes          []VectorNode           `json:"nodes"`
	BroadcastPeers []VectorBroadcastEntry `json:"broadcast_peers"`
}

func LoadVectorFixture(path string) (VectorFixture, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return VectorFixture{}, err
	}
	var fixture VectorFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		return VectorFixture{}, err
	}
	if fixture.Version == 0 {
		fixture.Version = 1
	}
	if fixture.MaxShredIndex == 0 {
		fixture.MaxShredIndex = 1000
	}
	if !fixture.DedupTVUAddrs {
		fixture.DedupTVUAddrs = true
	}
	return fixture, nil
}

func EmitVector(fixture VectorFixture, implementation string) ([]byte, error) {
	nodes, err := clusterNodesFromFixture(fixture)
	if err != nil {
		return nil, err
	}

	out := VectorOutput{
		Version:        VectorVersion,
		Implementation: implementation,
		Fixture:        fixture,
		Nodes:          snapshotVectorNodes(nodes),
		BroadcastPeers: make([]VectorBroadcastEntry, 0, fixture.MaxShredIndex+1),
	}

	shredType := ShredType(fixture.ShredType)
	for index := uint32(0); index <= fixture.MaxShredIndex; index++ {
		shred := ShredID{Slot: fixture.Slot, Index: index, Type: shredType}
		rawIndex, rawOK := nodes.BroadcastPeerRawIndex(shred)
		entry := VectorBroadcastEntry{ShredIndex: index}
		if rawOK {
			entry.RawNodeIndex = rawIndex
			entry.RawPubkey = hex.EncodeToString(nodes.nodes[rawIndex].pubkey[:])
		} else {
			entry.RawNodeIndex = -1
		}
		if _, ok := nodes.BroadcastPeer(shred); ok {
			entry.HasBroadcastTVU = true
			entry.BroadcastPubkey = entry.RawPubkey
		}
		out.BroadcastPeers = append(out.BroadcastPeers, entry)
	}

	return json.MarshalIndent(out, "", "  ")
}

func clusterNodesFromFixture(fixture VectorFixture) (*ClusterNodes, error) {
	self, err := selfPubkeyFromSeed(fixture.SelfPubkey)
	if err != nil {
		return nil, fmt.Errorf("self_pubkey: %w", err)
	}
	stakes := map[solana.PublicKey]uint64{}
	if fixture.SelfStake > 0 {
		stakes[self] = fixture.SelfStake
	}
	var selfTVU *net.UDPAddr
	if fixture.SelfTVU != "" {
		selfTVU, err = parseUDPAddr(fixture.SelfTVU)
		if err != nil {
			return nil, fmt.Errorf("self_tvu: %w", err)
		}
	}
	tvuPeers := make([]gossip.TVUPeer, 0, len(fixture.Peers))
	for i, peer := range fixture.Peers {
		pk, err := parseHexPubkey(peer.Pubkey)
		if err != nil {
			return nil, fmt.Errorf("peers[%d].pubkey: %w", i, err)
		}
		tvu, err := parseUDPAddr(peer.TVU)
		if err != nil {
			return nil, fmt.Errorf("peers[%d].tvu: %w", i, err)
		}
		if peer.Stake > 0 {
			stakes[pk] = peer.Stake
		}
		tvuPeers = append(tvuPeers, gossip.TVUPeer{
			Pubkey:  gossip.Pubkey(pk),
			TVUAddr: tvu,
		})
	}
	for i, peer := range fixture.StakedNoContact {
		pk, err := parseHexPubkey(peer.Pubkey)
		if err != nil {
			return nil, fmt.Errorf("staked_no_contact[%d].pubkey: %w", i, err)
		}
		if peer.Stake > 0 {
			stakes[pk] = peer.Stake
		}
	}
	return NewBroadcastClusterNodes(ClusterNodesConfig{
		Self:         self,
		SelfTVU:      selfTVU,
		TVUPeers:     tvuPeers,
		Stakes:       stakes,
		DedupTVUAddr: fixture.DedupTVUAddrs,
		UseChaCha8:   fixture.UseChaCha8,
	}), nil
}

func snapshotVectorNodes(nodes *ClusterNodes) []VectorNode {
	if nodes == nil {
		return nil
	}
	out := make([]VectorNode, len(nodes.nodes))
	for i, node := range nodes.nodes {
		tvu := ""
		if node.tvuAddr != nil {
			tvu = node.tvuAddr.String()
		}
		out[i] = VectorNode{
			Pubkey:     hex.EncodeToString(node.pubkey[:]),
			Stake:      node.stake,
			HasContact: node.hasContact,
			TVU:        tvu,
		}
	}
	return out
}

func parseHexPubkey(s string) (solana.PublicKey, error) {
	s = strings.TrimSpace(s)
	raw, err := hex.DecodeString(s)
	if err != nil {
		return solana.PublicKey{}, err
	}
	if len(raw) != 32 {
		return solana.PublicKey{}, fmt.Errorf("want 32 bytes, got %d", len(raw))
	}
	return solana.PublicKeyFromBytes(raw), nil
}

func selfPubkeyFromSeed(seedHex string) (solana.PublicKey, error) {
	seed, err := parseHexPubkey(seedHex)
	if err != nil {
		return solana.PublicKey{}, fmt.Errorf("self_pubkey seed: %w", err)
	}
	priv := ed25519.NewKeyFromSeed(seed[:])
	return solana.PublicKeyFromBytes(priv.Public().(ed25519.PublicKey)), nil
}

func parseUDPAddr(s string) (*net.UDPAddr, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(s))
	if err != nil {
		return nil, err
	}
	return net.ResolveUDPAddr("udp", net.JoinHostPort(host, port))
}
