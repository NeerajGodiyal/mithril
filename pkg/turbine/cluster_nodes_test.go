package turbine

import (
	"net"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/gossip"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestShredIDSeedMatchesAgaveLayout(t *testing.T) {
	leader := solana.PublicKey{1}
	id := ShredID{Slot: 42, Index: 7, Type: ShredTypeData}
	seed := id.Seed(leader)
	require.NotEqual(t, [32]byte{}, seed)

	id2 := ShredID{Slot: 42, Index: 7, Type: ShredTypeCode}
	require.NotEqual(t, id.Seed(leader), id2.Seed(leader))
}

func TestBroadcastClusterNodesDeterministicRoot(t *testing.T) {
	self := solana.MustPublicKeyFromBase58("11111111111111111111111111111112")
	peerA := solana.MustPublicKeyFromBase58("11111111111111111111111111111113")
	peerB := solana.MustPublicKeyFromBase58("11111111111111111111111111111114")

	nodes := NewBroadcastClusterNodes(ClusterNodesConfig{
		Self: self,
		TVUPeers: []gossip.TVUPeer{
			{Pubkey: gossip.Pubkey(peerA), TVUAddr: &net.UDPAddr{IP: net.ParseIP("203.0.113.1"), Port: 8001}},
			{Pubkey: gossip.Pubkey(peerB), TVUAddr: &net.UDPAddr{IP: net.ParseIP("203.0.113.2"), Port: 8001}},
		},
		Stakes: map[solana.PublicKey]uint64{
			peerA: 100,
			peerB: 50,
		},
		UseChaCha8: true,
	})

	shred := ShredID{Slot: 100, Index: 3, Type: ShredTypeData}
	addr1, ok := nodes.BroadcastPeer(shred)
	require.True(t, ok)
	require.NotNil(t, addr1)

	addr2, ok := nodes.BroadcastPeer(shred)
	require.True(t, ok)
	require.Equal(t, addr1.String(), addr2.String())
}
