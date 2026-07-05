package replay

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/base58"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/stretchr/testify/require"
)

func TestApplyAlpenglowRuntimeFeatureOverridesEnablesAgave4VoteSemantics(t *testing.T) {
	ft := features.NewFeaturesDefault()

	applyAlpenglowRuntimeFeatureOverrides(ft, 32897)

	require.True(t, ft.IsActive(features.VoteStateV4))
	require.True(t, ft.IsActive(features.TimelyVoteCredits))
	require.True(t, ft.IsActive(features.DeprecateUnusedLegacyVotePlumbing))

	slot, ok := ft.ActivationSlot(features.TimelyVoteCredits)
	require.True(t, ok)
	require.Equal(t, uint64(32897), slot)
}

func TestAlpenglowClockFeatureRequiresAgaveFeatureGate(t *testing.T) {
	ft := features.NewFeaturesDefault()

	require.False(t, alpenglowClockFeatureActive(ft))

	ft.EnableFeature(features.Alpenglow, 42)
	require.True(t, alpenglowClockFeatureActive(ft))

	ft.DisableFeature(features.Alpenglow)
	ft.EnableFeature(features.AlpenglowDevContext, 43)
	require.True(t, alpenglowClockFeatureActive(ft))
}

func TestAlpenglowFeatureGateMatchesVoteRewardProgram(t *testing.T) {
	require.Equal(t, AlpenglowFeatureGatePubkey, base58.Encode(features.Alpenglow.Address[:]))
}

func TestAlpenglowReplayModeForcesAlpenglowClockSemantics(t *testing.T) {
	ft := features.NewFeaturesDefault()

	require.False(t, alpenglowClockFeatureActive(ft))
	require.False(t, useAlpenglowClockSemantics(false, ft))
	require.True(t, useAlpenglowClockSemantics(true, ft))
}
