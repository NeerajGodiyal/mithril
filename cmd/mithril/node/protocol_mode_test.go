package node

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/gossip"
	"github.com/Overclock-Validator/mithril/pkg/replay"
	"github.com/Overclock-Validator/mithril/pkg/state"
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

func TestValidatorModeUsesLocalFooterBankHashInsteadOfRPCVerifier(t *testing.T) {
	cfg := replay.TrailingVerifierDefaults()
	got := verifierConfigForConsensusMode("validator", cfg)
	require.False(t, got.Enabled)
	require.False(t, got.Required)
	require.True(t, got.ValidatorFooterHash)

	require.Equal(t, cfg, verifierConfigForConsensusMode("verifying", cfg), "verifying mode must remain unchanged")
}

func TestRPCVerificationGateStartsClosedWhenVerificationIsRequired(t *testing.T) {
	t.Cleanup(func() { replay.ResetVerificationStatus(false, 0) })
	initializeVerificationStatusForRPC(
		replay.VerifierConfig{Enabled: true, Required: true},
		&state.MithrilState{LastRootedSlot: 42},
		true,
		true,
	)
	got, required, verified, eligible := replay.VerificationSnapshot()
	require.Equal(t, replay.VerificationUnavailable, got)
	require.True(t, required)
	require.Equal(t, uint64(42), verified)
	require.Equal(t, uint64(42), eligible)
}

func TestClassicRootedRPCVerificationGateStartsClosed(t *testing.T) {
	t.Cleanup(func() { replay.ResetVerificationStatus(false, 0) })
	initializeVerificationStatusForRPC(
		replay.VerifierConfig{Enabled: true, Required: true},
		&state.MithrilState{SnapshotSlot: 21, RootedDurable: true},
		true,
		false,
	)
	got, required, verified, eligible := replay.VerificationSnapshot()
	require.Equal(t, replay.VerificationUnavailable, got)
	require.True(t, required)
	require.Equal(t, uint64(21), verified)
	require.Equal(t, uint64(21), eligible)
}

func TestValidateClassicRootedVerifier(t *testing.T) {
	require.NoError(t, validateClassicRootedVerifier(false, replay.VerifierConfig{}))
	require.NoError(t, validateClassicRootedVerifier(true, replay.VerifierConfig{Enabled: true, Required: true}))
	require.Error(t, validateClassicRootedVerifier(true, replay.VerifierConfig{Enabled: true}))
}

func TestValidateRootedDurableProfile(t *testing.T) {
	require.NoError(t, validateRootedDurableProfile(nil, false))
	require.Error(t, validateRootedDurableProfile(nil, true))
	require.NoError(t, validateRootedDurableProfile(&state.MithrilState{RootedDurable: true}, true))
	require.NoError(t, validateRootedDurableProfile(&state.MithrilState{}, false))
	require.Error(t, validateRootedDurableProfile(&state.MithrilState{RootedDurable: true}, false))

	s := &state.MithrilState{StateSchemaVersion: state.CurrentStateSchemaVersion, Stage: "ready", SnapshotSlot: 100}
	require.Error(t, validateRootedDurableProfile(s, true))
	require.False(t, s.RootedDurable)
}

func TestBindRootRunID(t *testing.T) {
	root := t.TempDir()
	s := &state.MithrilState{StateSchemaVersion: state.CurrentStateSchemaVersion, Stage: "ready"}
	const first = "00112233445566778899aabbccddeeff"
	require.NoError(t, bindRootRunID(s, root, first))
	require.Equal(t, first, s.RootRunID)
	require.NoError(t, bindRootRunID(s, root, "ffeeddccbbaa99887766554433221100"))
	require.Equal(t, first, s.RootRunID)

	loaded, err := state.LoadState(root)
	require.NoError(t, err)
	require.Equal(t, first, loaded.RootRunID)
	require.Error(t, bindRootRunID(&state.MithrilState{Stage: "ready"}, root, "NOT-HEX"))

	legacy := &state.MithrilState{StateSchemaVersion: state.CurrentStateSchemaVersion, Stage: "ready", CurrentRunID: "aabbccdd"}
	require.NoError(t, bindRootRunID(legacy, t.TempDir(), first))
	require.Equal(t, "aabbccdd", legacy.RootRunID)
}

func TestTxParallelismForMode(t *testing.T) {
	require.Equal(t, int64(32), txParallelismForMode("validator", 0, false, 16))
	require.Equal(t, int64(0), txParallelismForMode("validator", 0, true, 16), "explicit sequential validator mode must be preserved")
	require.Equal(t, int64(7), txParallelismForMode("validator", 7, true, 16))
	require.Equal(t, int64(0), txParallelismForMode("verifying", 0, false, 16), "non-Alpenglow verifying flow must remain unchanged")
	require.Equal(t, int64(2), txParallelismForMode("validator", 0, false, 0), "invalid CPU discovery gets a safe minimum")
}
