package replay

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/stretchr/testify/require"
)

func TestAlpenglowMetadataAddresses(t *testing.T) {
	require.Equal(t, features.AlpenglowFeatureGateAddress, AlpenglowFeatureGatePubkey)
	require.Equal(t, "A1PeNGc3D8SQmKwdYf4qj1XG7XgWVSuFQaiJSCQj775h", alpenglowFeatureGatePubkey.String())
	require.Equal(t, "CzbDGGjhn3JhaaukmLjnmb3kcC8ohGs98GqZR6GcrkQf", VoteRewardAccountAddr().String())
	require.Equal(t, "ErF9JEo3jKD5kWfvgagixVHRfJwa6qFVnaEYDdi7Wdrk", NanosecondClockAccountAddr().String())
	require.Equal(t, "EEJkUCpugoK7DnYjxv3msztqhEJ45r8MKZwfBUV57pug", RewardEpochDelegatedStakesAccountAddr().String())
}
