package replay

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/stretchr/testify/require"
)

func TestInitialReplayEpochUsesRootedParentAcrossSkippedBoundary(t *testing.T) {
	schedule := &sealevel.SysvarEpochSchedule{
		SlotsPerEpoch:            100,
		LeaderScheduleSlotOffset: 100,
	}

	require.Equal(t, uint64(0), initialReplayEpoch(
		schedule, 100, 0, &ResumeState{ParentSlot: 99},
	))
	require.Equal(t, uint64(0), initialReplayEpoch(
		schedule, 100, 99, nil,
	))
	require.Equal(t, uint64(1), initialReplayEpoch(
		schedule, 100, 0, nil,
	))
}

func TestEpochBoundaryParentCtxUsesConfiguredParentState(t *testing.T) {
	lastBlockhash := [32]byte{1, 2, 3}
	ctx := epochBoundaryParentCtx(nil, &block.Block{
		ParentSlot:    99,
		LastBlockhash: lastBlockhash,
	}, 0, nil)

	require.Equal(t, uint64(99), ctx.Slot)
	require.Equal(t, uint64(0), ctx.Epoch)
	require.Equal(t, lastBlockhash, ctx.Blockhash)
	require.NotNil(t, ctx.Accounts)
}

func TestEpochBoundaryRequiresDurableParentBeforeAccountScans(t *testing.T) {
	require.NoError(t, requireDurableEpochBoundaryParent(100, 99, 99))
	require.NoError(t, requireDurableEpochBoundaryParent(102, 99, 101))

	err := requireDurableEpochBoundaryParent(100, 99, 98)
	require.EqualError(t, err,
		"epoch boundary at slot 100 requires durable parent 99 before AccountsDB-wide scans; durable state is only at slot 98")
}
