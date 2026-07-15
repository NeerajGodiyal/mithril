package node

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/gossip"
	"github.com/stretchr/testify/require"
)

func TestAlpenglowModeForCluster(t *testing.T) {
	alpenglow, err := alpenglowModeForCluster("alpenglow")
	require.NoError(t, err)
	require.True(t, alpenglow)

	for _, cluster := range []string{"mainnet-beta", "testnet", "devnet"} {
		classic, err := alpenglowModeForCluster(cluster)
		require.NoError(t, err)
		require.False(t, classic, cluster)
	}

	_, err = alpenglowModeForCluster("localnet")
	require.Error(t, err)
}

func TestResolveTurbineShredVersion(t *testing.T) {
	called := false
	query := func(addr *net.UDPAddr, timeout time.Duration) (gossip.EchoResponse, error) {
		called = true
		require.Equal(t, "127.0.0.1:9000", addr.String())
		require.Equal(t, 5*time.Second, timeout)
		return gossip.EchoResponse{ShredVersion: 10638}, nil
	}

	got, err := resolveTurbineShredVersion(0, "127.0.0.1:9000", query)
	require.NoError(t, err)
	require.Equal(t, 10638, got)
	require.True(t, called)

	called = false
	got, err = resolveTurbineShredVersion(4242, "", query)
	require.NoError(t, err)
	require.Equal(t, 4242, got)
	require.False(t, called, "an explicitly configured version must not query gossip")
}

func TestResolveTurbineShredVersionReportsDiscoveryErrors(t *testing.T) {
	_, err := resolveTurbineShredVersion(0, "", nil)
	require.ErrorContains(t, err, "entrypoint is required")

	want := errors.New("echo unavailable")
	_, err = resolveTurbineShredVersion(0, "127.0.0.1:9000", func(*net.UDPAddr, time.Duration) (gossip.EchoResponse, error) {
		return gossip.EchoResponse{}, want
	})
	require.ErrorIs(t, err, want)
}

func TestDefaultBlockSourceForProtocolMode(t *testing.T) {
	require.Equal(t, "turbine", defaultBlockSourceForMode(true))
	require.Equal(t, "rpc", defaultBlockSourceForMode(false))
}
