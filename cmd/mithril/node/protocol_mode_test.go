package node

import (
	"testing"

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

func TestDefaultBlockSourceForProtocolMode(t *testing.T) {
	require.Equal(t, "turbine", defaultBlockSourceForMode(true))
	require.Equal(t, "rpc", defaultBlockSourceForMode(false))
}
