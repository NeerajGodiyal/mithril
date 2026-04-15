package sealevel

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMarshalEpochScheduleSyscallBytes_UsesReprCLayout(t *testing.T) {
	epochSchedule := SysvarEpochSchedule{
		SlotsPerEpoch:            432_000,
		LeaderScheduleSlotOffset: 432_000,
		Warmup:                   true,
		FirstNormalEpoch:         14,
		FirstNormalSlot:          524_256,
	}

	data := marshalEpochScheduleSysvar(epochSchedule)

	require.Equal(t, epochSchedule.SlotsPerEpoch, binary.LittleEndian.Uint64(data[0:8]))
	require.Equal(t, epochSchedule.LeaderScheduleSlotOffset, binary.LittleEndian.Uint64(data[8:16]))
	require.Equal(t, byte(1), data[16])
	require.Equal(t, [7]byte{}, [7]byte(data[17:24]))
	require.Equal(t, epochSchedule.FirstNormalEpoch, binary.LittleEndian.Uint64(data[24:32]))
	require.Equal(t, epochSchedule.FirstNormalSlot, binary.LittleEndian.Uint64(data[32:40]))
}
