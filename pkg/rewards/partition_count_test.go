package rewards

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCalculateNumRewardPartitionsSchedulesEmptyPartition(t *testing.T) {
	require.Equal(t, uint64(1), CalculateNumRewardPartitions(0))
	require.Equal(t, uint64(1), CalculateNumRewardPartitions(1))
	require.Equal(t, uint64(1), CalculateNumRewardPartitions(4096))
	require.Equal(t, uint64(2), CalculateNumRewardPartitions(4097))
}
