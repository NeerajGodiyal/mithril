package rewardcerts

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/alpenglow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadinessForLeaderSlot(t *testing.T) {
	builder := NewBuilder(DefaultBuilderConfig())
	leaderSlot := uint64(18)
	rewardSlot, ok := RewardSlotForLeader(leaderSlot)
	require.True(t, ok)
	require.Equal(t, uint64(10), rewardSlot)

	readiness := builder.ReadinessForLeaderSlot(leaderSlot)
	assert.Equal(t, rewardSlot, readiness.RewardSlot)
	assert.False(t, readiness.SkipReady)
	assert.Contains(t, readiness.Format(false), "footer_certs=none")

	keys := testBLSKeys(t, 5)
	builder.AddVote(testSignedVote(t, alpenglow.NewSkipVote(rewardSlot), 0, keys[0]))
	readiness = builder.ReadinessForLeaderSlot(leaderSlot)
	assert.True(t, readiness.SkipReady)
	assert.Equal(t, 1, readiness.SkipVotes)
	assert.Contains(t, readiness.Format(true), "footer_certs=skip")
}
