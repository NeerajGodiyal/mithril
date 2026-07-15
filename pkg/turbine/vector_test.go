package turbine

import (
	_ "embed"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

//go:embed testdata/agave_turbine_broadcast_peers_chacha8_mixed_stake.json
var agaveTurbineBroadcastPeersChaCha8MixedStakeJSON []byte

func TestEmitVectorDefaultFixture(t *testing.T) {
	var fixture VectorFixture
	require.NoError(t, json.Unmarshal(agaveTurbineBroadcastPeersChaCha8MixedStakeJSON, &fixture))
	require.True(t, fixture.UseChaCha8, "Alpenglow/mainnet use ChaCha8 turbine RNG")

	out, err := EmitVector(fixture, "mithril")
	require.NoError(t, err)

	var decoded VectorOutput
	require.NoError(t, json.Unmarshal(out, &decoded))
	require.Len(t, decoded.BroadcastPeers, int(fixture.MaxShredIndex)+1)
	require.Len(t, decoded.Nodes, 7)

	// Golden raw_node_index values for shred 0..7 on the default fixture.
	// Regenerate with compare.sh after Agave emitter changes.
	want := []int{1, 4, 1, 0, 3, 4, 1, 3}
	for i, index := range want {
		require.Equal(t, index, decoded.BroadcastPeers[i].RawNodeIndex, "shred_index=%d", i)
	}
}
