package alpenglow

import (
	"context"
	"crypto/ed25519"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestVotorBroadcasterDeliversOverIdentityQUIC(t *testing.T) {
	serverIdentity := ed25519.NewKeyFromSeed(bytesOf(31, ed25519.SeedSize))
	clientIdentity := ed25519.NewKeyFromSeed(bytesOf(32, ed25519.SeedSize))
	received := make(chan Message, 1)
	receiver, err := NewReceiver(ReceiverConfig{
		BindAddr:     "127.0.0.1:0",
		Identity:     serverIdentity,
		ShredVersion: 0x1234,
		LogInterval:  -1,
		AdmitMessage: func(message Message) (Message, bool) {
			received <- message
			return Message{}, false
		},
	}, NewObserver())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- receiver.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, receiver.Close())
		require.NoError(t, <-runErr)
	})

	peer, err := net.ResolveUDPAddr("udp", receiver.Addr().String())
	require.NoError(t, err)
	broadcaster, err := NewVotorBroadcaster(VotorBroadcasterConfig{
		Identity:     clientIdentity,
		ShredVersion: 0x1234,
		Peers:        func(uint64) []*net.UDPAddr { return []*net.UDPAddr{peer} },
		Workers:      1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, broadcaster.Close()) })

	vote := NewVoteMessage(NewSkipVote(77), testSignatureSeq(0x41), 3)
	require.NoError(t, broadcaster.Enqueue(vote))
	select {
	case message := <-received:
		require.NotNil(t, message.Vote)
		require.EqualValues(t, 77, message.Vote.Vote.Slot)
		require.EqualValues(t, 0x1234, message.ShredVersion)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for outbound Votor message")
	}
	require.Eventually(t, func() bool {
		return broadcaster.Stats().PeerSends == 1
	}, time.Second, 10*time.Millisecond)
}
