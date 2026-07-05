package replay

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/stretchr/testify/require"
)

func TestUpdateClockSysvarUsesBankEpochSchedule(t *testing.T) {
	prevCalcUnixTime := global.CalcUnixTimeForClockSysvar()
	t.Cleanup(func() {
		global.SetCalcUnixTimeForClockSysvar(prevCalcUnixTime)
	})

	bankEpochSchedule := &sealevel.SysvarEpochSchedule{
		SlotsPerEpoch:            432000,
		LeaderScheduleSlotOffset: 432000,
	}
	global.SetCalcUnixTimeForClockSysvar(false)

	clock := &sealevel.SysvarClock{
		Slot:                463624424,
		Epoch:               1073,
		LeaderScheduleEpoch: 1074,
		EpochStartTimestamp: 1779227894,
		UnixTimestamp:       1779261346,
	}
	blk := &block.Block{
		Slot:          463624425,
		Epoch:         1073,
		UnixTimestamp: 1779261347,
	}

	err := updateClockSysvar(clock, blk, bankEpochSchedule)
	require.NoError(t, err)

	require.Equal(t, blk.Slot, clock.Slot)
	require.Equal(t, blk.Epoch, clock.Epoch)
	require.Equal(t, blk.UnixTimestamp, clock.UnixTimestamp)
	require.Equal(t, uint64(1074), clock.LeaderScheduleEpoch)
	require.Equal(t, int64(1779227894), clock.EpochStartTimestamp)
}

func TestUpdateClockSysvarForAlpenglowPreservesParentTimestampAtBankStart(t *testing.T) {
	bankEpochSchedule := &sealevel.SysvarEpochSchedule{
		SlotsPerEpoch:            54000,
		LeaderScheduleSlotOffset: 54000,
	}

	clock := &sealevel.SysvarClock{
		Slot:                2751720,
		Epoch:               50,
		LeaderScheduleEpoch: 51,
		EpochStartTimestamp: 1779232800,
		UnixTimestamp:       1779232849,
	}
	blk := &block.Block{
		Slot:          2751721,
		ParentSlot:    2751720,
		Epoch:         50,
		UnixTimestamp: 1779232900,
	}

	err := updateClockSysvarForMode(clock, blk, bankEpochSchedule, true)
	require.NoError(t, err)

	require.Equal(t, blk.Slot, clock.Slot)
	require.Equal(t, blk.Epoch, clock.Epoch)
	require.Equal(t, uint64(51), clock.LeaderScheduleEpoch)
	require.Equal(t, int64(1779232800), clock.EpochStartTimestamp)
	require.Equal(t, int64(1779232849), clock.UnixTimestamp)
}

func TestUpdateClockSysvarForAlpenglowRefreshesLeaderScheduleEpochInEpochZero(t *testing.T) {
	bankEpochSchedule := &sealevel.SysvarEpochSchedule{
		SlotsPerEpoch:            54000,
		LeaderScheduleSlotOffset: 54000,
	}

	clock := &sealevel.SysvarClock{
		Slot:                25664,
		Epoch:               0,
		LeaderScheduleEpoch: 0,
		EpochStartTimestamp: 1779232800,
		UnixTimestamp:       1779232849,
	}
	blk := &block.Block{
		Slot:          25665,
		ParentSlot:    25664,
		Epoch:         0,
		UnixTimestamp: 1779232900,
	}

	err := updateClockSysvarForMode(clock, blk, bankEpochSchedule, true)
	require.NoError(t, err)

	require.Equal(t, blk.Slot, clock.Slot)
	require.Equal(t, blk.Epoch, clock.Epoch)
	require.Equal(t, uint64(1), clock.LeaderScheduleEpoch)
	require.Equal(t, int64(1779232800), clock.EpochStartTimestamp)
	require.Equal(t, int64(1779232849), clock.UnixTimestamp)
}

func TestUpdateClockSysvarFromAlpenglowFooterAppliesFooterTimestamp(t *testing.T) {
	bankEpochSchedule := &sealevel.SysvarEpochSchedule{
		SlotsPerEpoch:            54000,
		LeaderScheduleSlotOffset: 54000,
	}

	clock := &sealevel.SysvarClock{
		Slot:                2751721,
		Epoch:               50,
		LeaderScheduleEpoch: 51,
		EpochStartTimestamp: 1779232800,
		UnixTimestamp:       1779232849,
	}
	blk := &block.Block{
		Slot:          2751721,
		ParentSlot:    2751720,
		Epoch:         50,
		UnixTimestamp: 1779232900,
	}

	err := updateClockSysvarFromAlpenglowFooter(clock, blk, bankEpochSchedule)
	require.NoError(t, err)

	require.Equal(t, blk.Slot, clock.Slot)
	require.Equal(t, blk.Epoch, clock.Epoch)
	require.Equal(t, uint64(51), clock.LeaderScheduleEpoch)
	require.Equal(t, int64(1779232800), clock.EpochStartTimestamp)
	require.Equal(t, blk.UnixTimestamp, clock.UnixTimestamp)
}

func TestAlpenglowFooterUnixTimestampFromProducerTimeNanos(t *testing.T) {
	blk := &block.Block{
		FooterProducerTimeNanos: 1782227493876242864,
	}
	ts, ok, err := alpenglowFooterUnixTimestamp(blk)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(1782227493), ts)
}

func TestAlpenglowFooterUnixTimestampPrefersBlockUnixTimestamp(t *testing.T) {
	blk := &block.Block{
		UnixTimestamp:           1779232900,
		FooterProducerTimeNanos: 1782227493876242864,
	}
	ts, ok, err := alpenglowFooterUnixTimestamp(blk)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(1779232900), ts)
}

func TestUpdateClockSysvarFromAlpenglowFooterUsesProducerTimeNanos(t *testing.T) {
	bankEpochSchedule := &sealevel.SysvarEpochSchedule{
		SlotsPerEpoch:            54000,
		LeaderScheduleSlotOffset: 54000,
	}

	clock := &sealevel.SysvarClock{
		Slot:                3737279,
		Epoch:               69,
		LeaderScheduleEpoch: 70,
		EpochStartTimestamp: 1782222727,
		UnixTimestamp:       1782227468,
	}
	blk := &block.Block{
		Slot:                    3737280,
		ParentSlot:              3737279,
		Epoch:                   69,
		FooterProducerTimeNanos: 1782227493876242864,
	}

	err := updateClockSysvarFromAlpenglowFooter(clock, blk, bankEpochSchedule)
	require.NoError(t, err)

	require.Equal(t, int64(1782227493), clock.UnixTimestamp)
	require.Equal(t, blk.Slot, clock.Slot)
	require.Equal(t, blk.Epoch, clock.Epoch)
}

func TestUpdateClockSysvarRejectsMismatchedEpochFrame(t *testing.T) {
	clock := &sealevel.SysvarClock{Slot: 463624424, Epoch: 1073}
	blk := &block.Block{Slot: 463624425, Epoch: 56592}
	err := updateClockSysvar(clock, blk, &sealevel.SysvarEpochSchedule{SlotsPerEpoch: 8192, LeaderScheduleSlotOffset: 8192})
	require.Error(t, err)
}
