package turbine

import (
	"crypto/ed25519"
	"net"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func testBroadcastLeader(t *testing.T) solana.PrivateKey {
	t.Helper()
	var seed [32]byte
	for i := range seed {
		seed[i] = byte(i + 3)
	}
	return solana.PrivateKey(ed25519.NewKeyFromSeed(seed[:]))
}

func TestBroadcastSessionHeaderAndFooter(t *testing.T) {
	capture := &packetCapture{}
	leader := testBroadcastLeader(t)
	parentBlockID := solana.Hash{0xaa}
	parentChainedRoot := solana.Hash{0xbb}
	session := NewBroadcastSession(BroadcastSessionConfig{
		Leader:                  leader,
		Slot:                    100,
		ParentSlot:              99,
		ParentBlockID:           parentBlockID,
		ParentChainedMerkleRoot: parentChainedRoot,
		Version:                 7,
		Broadcaster:             capture,
	})
	require.NoError(t, session.BroadcastHeader(parentBlockID))
	require.NoError(t, session.BroadcastFooter(solana.Hash{0xcc}, 1234, nil, nil))
	require.Greater(t, capture.len(), 0)

	var firstData *Shred
	for _, packet := range capture.packets {
		shred, err := ParseShred(packet)
		require.NoError(t, err)
		if shred.Type == ShredTypeData && shred.Index == 0 {
			firstData = shred
			break
		}
	}
	require.NotNil(t, firstData)
	chainedRoot, err := firstData.EmbeddedChainedMerkleRoot()
	require.NoError(t, err)
	require.Equal(t, parentChainedRoot, chainedRoot)
	require.NotEqual(t, parentBlockID, chainedRoot)
}

func TestUDPBroadcasterLoopback(t *testing.T) {
	recvAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	require.NoError(t, err)
	recvConn, err := net.ListenUDP("udp", recvAddr)
	require.NoError(t, err)
	defer recvConn.Close()

	bc, err := NewUDPBroadcaster("")
	require.NoError(t, err)
	bc.AddPeer(recvConn.LocalAddr().(*net.UDPAddr))
	require.NoError(t, bc.Broadcast([][]byte{{1, 2, 3}}))

	buf := make([]byte, 16)
	n, _, err := recvConn.ReadFromUDP(buf)
	require.NoError(t, err)
	require.Equal(t, 3, n)
}

type packetCapture struct {
	packets [][]byte
}

func (p *packetCapture) Broadcast(packets [][]byte) error {
	for _, pkt := range packets {
		p.packets = append(p.packets, append([]byte(nil), pkt...))
	}
	return nil
}

func (p *packetCapture) len() int {
	return len(p.packets)
}
