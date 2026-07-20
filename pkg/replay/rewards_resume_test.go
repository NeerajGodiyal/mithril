package replay

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/rewards"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/stretchr/testify/require"
)

func TestValidatePartitionedRewardsResumeAllowsCompletedDistribution(t *testing.T) {
	schedule := &sealevel.SysvarEpochSchedule{
		SlotsPerEpoch:            54000,
		LeaderScheduleSlotOffset: 54000,
	}
	require.True(t, rewards.IsWithinRewardsPeriod(87, 4698088, schedule),
		"incident slot must exercise the old calendar-only rejection")

	require.NoError(t, validatePartitionedRewardsResume(4698088, &sealevel.SysvarEpochRewards{
		DistributionStartingBlockHeight: 1234,
		NumPartitions:                   1,
		DistributedRewards:              99,
		TotalRewards:                    99,
		Active:                          false,
	}))
}

func TestValidatePartitionedRewardsResumeRejectsActiveDistribution(t *testing.T) {
	err := validatePartitionedRewardsResume(4698001, &sealevel.SysvarEpochRewards{
		DistributionStartingBlockHeight: 1234,
		NumPartitions:                   4,
		Active:                          true,
	})
	require.EqualError(t, err,
		"cannot resume replay at slot 4698001: persisted EpochRewards is still active (distribution_starting_block_height=1234 num_partitions=4), but partition spool progress is not restored")
}

func TestValidatePartitionedRewardsResumeRejectsMissingSysvar(t *testing.T) {
	err := validatePartitionedRewardsResume(4698001, nil)
	require.EqualError(t, err,
		"cannot resume replay at slot 4698001: persisted EpochRewards sysvar is unavailable")
}
