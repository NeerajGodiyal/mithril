package gossip

import (
	"fmt"
	"sort"
	"time"

	"github.com/gagliardetto/solana-go"
)

// DiscoveredContact is the latest verified ContactInfo seen from gossip.
type DiscoveredContact struct {
	Pubkey      Pubkey
	ShredVer    uint16
	Wallclock   uint64
	Gossip      string
	ServeRepair string
	TVU         string
	Sockets     map[uint8]string
	LastSeen    time.Time
}

func (c DiscoveredContact) PubkeyString() string {
	return solana.PublicKey(c.Pubkey).String()
}

func (c DiscoveredContact) HasGossip() bool {
	return c.Gossip != "" && c.Gossip != "none"
}

func (c DiscoveredContact) HasServeRepair() bool {
	return c.ServeRepair != "" && c.ServeRepair != "none"
}

func (c DiscoveredContact) HasTVU() bool {
	return c.TVU != "" && c.TVU != "none"
}

// SocketTagName returns a human-readable socket tag label.
func SocketTagName(tag uint8) string {
	switch tag {
	case socketTagGossip:
		return "gossip"
	case socketTagServeRepair:
		return "repair"
	case 1:
		return "rpc"
	case 2:
		return "rpc_pubsub"
	case 3:
		return "tpu"
	case 5:
		return "tpu_forwards"
	case 6:
		return "tpu_forwards_quic"
	case socketTagTPUQUICForwards:
		return "tpu_forwards_quic"
	case socketTagTPUQUIC:
		return "tpu_quic"
	case socketTagTPUVote:
		return "tpu_vote"
	case socketTagTVU:
		return "tvu"
	case 11:
		return "tvu_quic"
	case socketTagTPUVoteQuic:
		return "tpu_vote_quic"
	case socketTagAlpenglow:
		return "alpenglow"
	default:
		return fmt.Sprintf("tag_%d", tag)
	}
}

func formatDiscoveredEndpoint(endpoint contactEndpoint) string {
	addr := endpoint.UDPAddr()
	if addr == nil {
		return "none"
	}
	return addr.String()
}

func discoveredContactFromRecord(record contactRecord, now time.Time) DiscoveredContact {
	sockets := make(map[uint8]string, len(record.Sockets))
	for tag, endpoint := range record.Sockets {
		sockets[tag] = formatDiscoveredEndpoint(endpoint)
	}
	return DiscoveredContact{
		Pubkey:      record.Pubkey,
		ShredVer:    record.ShredVer,
		Wallclock:   record.Wallclock,
		Gossip:      formatDiscoveredEndpoint(record.GossipAddr),
		ServeRepair: formatDiscoveredEndpoint(record.ServeRepairAddr),
		TVU:         formatDiscoveredEndpoint(record.TVUAddr),
		Sockets:     sockets,
		LastSeen:    now,
	}
}

func (c *Client) recordDiscoveredContact(record contactRecord) {
	now := time.Now()
	contact := discoveredContactFromRecord(record, now)
	c.discoveredMu.Lock()
	if existing, ok := c.discovered[record.Pubkey]; ok && record.Wallclock < existing.Wallclock {
		c.discoveredMu.Unlock()
		return
	}
	c.discovered[record.Pubkey] = contact
	c.discoveredMu.Unlock()
}

// DiscoveredContacts returns verified contacts learned from gossip, sorted by pubkey.
func (c *Client) DiscoveredContacts() []DiscoveredContact {
	if c == nil {
		return nil
	}
	c.discoveredMu.RLock()
	defer c.discoveredMu.RUnlock()
	out := make([]DiscoveredContact, 0, len(c.discovered))
	for _, contact := range c.discovered {
		out = append(out, contact)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].PubkeyString() < out[j].PubkeyString()
	})
	return out
}

// DiscoveredContactSummary counts socket availability across discovered contacts.
type DiscoveredContactSummary struct {
	Total        int
	WithGossip   int
	WithRepair   int
	WithTVU      int
	TVUPeers     int
	UniqueTags   map[string]int
}

// SummarizeDiscoveredContacts aggregates discovery stats for diagnostics.
func (c *Client) SummarizeDiscoveredContacts() DiscoveredContactSummary {
	contacts := c.DiscoveredContacts()
	summary := DiscoveredContactSummary{
		Total:      len(contacts),
		UniqueTags: make(map[string]int),
	}
	for _, contact := range contacts {
		if contact.HasGossip() {
			summary.WithGossip++
		}
		if contact.HasServeRepair() {
			summary.WithRepair++
		}
		if contact.HasTVU() {
			summary.WithTVU++
		}
		for tag := range contact.Sockets {
			summary.UniqueTags[SocketTagName(tag)]++
		}
	}
	summary.TVUPeers = len(c.TVUPeers())
	return summary
}

func formatSocketSummary(sockets map[uint8]string) string {
	if len(sockets) == 0 {
		return "none"
	}
	tags := make([]uint8, 0, len(sockets))
	for tag := range sockets {
		tags = append(tags, tag)
	}
	sort.Slice(tags, func(i, j int) bool { return tags[i] < tags[j] })
	parts := make([]string, 0, len(tags))
	for _, tag := range tags {
		parts = append(parts, fmt.Sprintf("%s=%s", SocketTagName(tag), sockets[tag]))
	}
	return joinStrings(parts, " ")
}

func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, part := range parts[1:] {
		out += sep + part
	}
	return out
}
