package turbine

import (
	"fmt"
	"net"

	"github.com/gagliardetto/solana-go"
)

// cavey TODO: maybe remove this in future when we're done debugging
// ClusterNodeNoTVU describes a staked cluster node that cannot receive broadcast shreds.
type ClusterNodeNoTVU struct {
	Index      int
	Pubkey     solana.PublicKey
	Stake      uint64
	HasContact bool
	TVURaw     string
	Reason     string
}

func (n ClusterNodeNoTVU) String() string {
	contact := "stake_only"
	if n.HasContact {
		contact = "contact"
	}
	return fmt.Sprintf("index=%d pubkey=%s stake=%d contact=%s tvu=%s reason=%s",
		n.Index, n.Pubkey, n.Stake, contact, n.TVURaw, n.Reason)
}

func (n clusterNode) canBroadcastTVU() bool {
	if !n.hasContact {
		return false
	}
	_, ok := broadcastTVUUDP(n.tvuAddr)
	return ok
}

func noBroadcastTVUReason(n clusterNode) string {
	if !n.hasContact {
		return "stake_only"
	}
	if n.tvuAddr == nil {
		return "missing_tvu"
	}
	if n.tvuAddr.IP == nil || n.tvuAddr.IP.To4() == nil {
		return "non_ipv4_tvu"
	}
	return "unknown"
}

func formatTVURaw(addr *net.UDPAddr) string {
	if addr == nil {
		return "none"
	}
	return addr.String()
}

func snapshotNoTVU(index int, n clusterNode) ClusterNodeNoTVU {
	return ClusterNodeNoTVU{
		Index:      index,
		Pubkey:     n.pubkey,
		Stake:      n.stake,
		HasContact: n.hasContact,
		TVURaw:     formatTVURaw(n.tvuAddr),
		Reason:     noBroadcastTVUReason(n),
	}
}

// StakedWithoutBroadcastTVU lists staked nodes in the sorted cluster table that
// weighted shuffle may pick but cannot receive IPv4 TVU shreds.
func (c *ClusterNodes) StakedWithoutBroadcastTVU() []ClusterNodeNoTVU {
	if c == nil {
		return nil
	}
	out := make([]ClusterNodeNoTVU, 0)
	for i, node := range c.nodes {
		if node.stake == 0 {
			continue
		}
		if node.canBroadcastTVU() {
			continue
		}
		out = append(out, snapshotNoTVU(i, node))
	}
	return out
}

// BroadcastPeerPick returns the weighted-shuffle node for a shred before TVU filtering.
func (c *ClusterNodes) BroadcastPeerPick(shred ShredID) (ClusterNodeNoTVU, bool) {
	if c == nil || len(c.nodes) == 0 {
		return ClusterNodeNoTVU{}, false
	}
	index, ok := c.BroadcastPeerRawIndex(shred)
	if !ok || index < 0 || index >= len(c.nodes) {
		return ClusterNodeNoTVU{}, false
	}
	return snapshotNoTVU(index, c.nodes[index]), true
}
