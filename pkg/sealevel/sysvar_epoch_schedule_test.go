package sealevel

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSysvarEpochScheduleWarmupEpochAndSlotIndexMatchesAgave(t *testing.T) {
	schedule := SysvarEpochSchedule{
		SlotsPerEpoch:            512,
		LeaderScheduleSlotOffset: 256,
		Warmup:                   true,
		FirstNormalEpoch:         4,
		FirstNormalSlot:          480,
	}

	tests := []struct {
		slot      uint64
		epoch     uint64
		slotIndex uint64
	}{
		{slot: 0, epoch: 0, slotIndex: 0},
		{slot: 31, epoch: 0, slotIndex: 31},
		{slot: 32, epoch: 1, slotIndex: 0},
		{slot: 95, epoch: 1, slotIndex: 63},
		{slot: 96, epoch: 2, slotIndex: 0},
		{slot: 223, epoch: 2, slotIndex: 127},
		{slot: 224, epoch: 3, slotIndex: 0},
		{slot: 479, epoch: 3, slotIndex: 255},
		{slot: 480, epoch: 4, slotIndex: 0},
		{slot: 991, epoch: 4, slotIndex: 511},
		{slot: 992, epoch: 5, slotIndex: 0},
	}

	for _, tt := range tests {
		epoch, slotIndex := schedule.GetEpochAndSlotIndex(tt.slot)
		require.Equalf(t, tt.epoch, epoch, "slot %d epoch", tt.slot)
		require.Equalf(t, tt.slotIndex, slotIndex, "slot %d slot index", tt.slot)
		require.Equalf(t, tt.epoch, schedule.GetEpoch(tt.slot), "slot %d GetEpoch", tt.slot)
	}
}

func TestSysvarEpochScheduleWarmupFirstSlotAndLeaderScheduleEpochMatchesAgave(t *testing.T) {
	schedule := SysvarEpochSchedule{
		SlotsPerEpoch:            512,
		LeaderScheduleSlotOffset: 256,
		Warmup:                   true,
		FirstNormalEpoch:         4,
		FirstNormalSlot:          480,
	}

	require.Equal(t, uint64(0), schedule.FirstSlotInEpoch(0))
	require.Equal(t, uint64(32), schedule.FirstSlotInEpoch(1))
	require.Equal(t, uint64(96), schedule.FirstSlotInEpoch(2))
	require.Equal(t, uint64(224), schedule.FirstSlotInEpoch(3))
	require.Equal(t, uint64(480), schedule.FirstSlotInEpoch(4))
	require.Equal(t, uint64(992), schedule.FirstSlotInEpoch(5))

	require.Equal(t, uint64(1), schedule.LeaderScheduleEpoch(0))
	require.Equal(t, uint64(1), schedule.LeaderScheduleEpoch(31))
	require.Equal(t, uint64(2), schedule.LeaderScheduleEpoch(32))
	require.Equal(t, uint64(3), schedule.LeaderScheduleEpoch(96))
	require.Equal(t, uint64(4), schedule.LeaderScheduleEpoch(224))
	require.Equal(t, uint64(4), schedule.LeaderScheduleEpoch(480))
	require.Equal(t, uint64(5), schedule.LeaderScheduleEpoch(736))
	require.Equal(t, uint64(5), schedule.LeaderScheduleEpoch(991))
}
