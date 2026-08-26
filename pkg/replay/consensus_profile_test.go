package replay

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateConsensusReplayProfile(t *testing.T) {
	verified := TrailingVerifierDefaults()

	require.NoError(t, validateConsensusReplayProfile(false, nil, VerifierConfig{}, false, false))
	require.NoError(t, validateConsensusReplayProfile(true, &ConsensusOpts{Alpenglow: true, RootedDurable: true}, VerifierConfig{}, false, true))
	require.NoError(t, validateConsensusReplayProfile(true, &ConsensusOpts{RootedDurable: true, FinalizedRPC: true, RootedEvents: true}, verified, false, false))

	tests := []struct {
		name           string
		accountsRooted bool
		opts           *ConsensusOpts
		verifier       VerifierConfig
		lightbringer   bool
		turbine        bool
		want           string
	}{
		{"profile mismatch", true, nil, VerifierConfig{}, false, false, "does not match"},
		{"events without rooted storage", false, &ConsensusOpts{RootedEvents: true}, VerifierConfig{}, false, false, "rooted events require"},
		{"Alpenglow without rooted storage", false, &ConsensusOpts{Alpenglow: true}, VerifierConfig{}, false, false, "Alpenglow replay requires"},
		{"Alpenglow with finalized RPC", true, &ConsensusOpts{Alpenglow: true, RootedDurable: true, FinalizedRPC: true}, verified, false, false, "cannot use classic"},
		{"Alpenglow over RPC", true, &ConsensusOpts{Alpenglow: true, RootedDurable: true}, verified, false, false, "requires the Turbine block source"},
		{"Alpenglow over Lightbringer", true, &ConsensusOpts{Alpenglow: true, RootedDurable: true}, verified, true, false, "requires the Turbine block source"},
		{"finalized RPC without rooted storage", false, &ConsensusOpts{FinalizedRPC: true}, verified, false, false, "requires rooted-durable"},
		{"finalized RPC over Lightbringer", true, &ConsensusOpts{RootedDurable: true, FinalizedRPC: true}, verified, true, false, "requires the RPC block source"},
		{"finalized RPC over Turbine", true, &ConsensusOpts{RootedDurable: true, FinalizedRPC: true}, verified, false, true, "requires the RPC block source"},
		{"classic rooted without finalized input", true, &ConsensusOpts{RootedDurable: true}, verified, false, false, "requires finalized RPC"},
		{"classic rooted without verifier", true, &ConsensusOpts{RootedDurable: true, FinalizedRPC: true}, VerifierConfig{}, false, false, "verifier enabled and required"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateConsensusReplayProfile(test.accountsRooted, test.opts, test.verifier, test.lightbringer, test.turbine)
			require.ErrorContains(t, err, test.want)
		})
	}
}
