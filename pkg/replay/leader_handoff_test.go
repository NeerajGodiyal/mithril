package replay

import (
	"testing"

	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/leaderschedule"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestConfigureBlockSetsLeaderAfterLocalLeaderHandoff(t *testing.T) {
	const slotsPerEpoch = 54000
	const epochStartSlot = slotsPerEpoch * 115
	const parentSlot = epochStartSlot + 1483
	const nextSlot = parentSlot + 1

	leader := solana.NewWallet().PublicKey()
	schedule := leaderschedule.NewLeaderScheduleFromKeyedSlots(map[solana.PublicKey][]uint64{
		leader: {nextSlot - epochStartSlot},
	}, epochStartSlot)
	global.SetLeaderSchedule(schedule)
	global.SetManageLeaderSchedule(true)
	t.Cleanup(func() {
		global.SetLeaderSchedule(nil)
		global.SetManageLeaderSchedule(false)
	})

	epochSchedule := &sealevel.SysvarEpochSchedule{SlotsPerEpoch: slotsPerEpoch}
	// Local leader commits historically left Epoch unset on SlotCtx.
	lastSlotCtx := &sealevel.SlotCtx{
		Slot:       parentSlot,
		Epoch:      0,
		Blockhash:  solana.Hash{1},
		FinalBankhash: []byte{2},
	}

	block := &b.Block{Slot: nextSlot}
	err := configureBlock(block, lastSlotCtx, epochSchedule)
	require.NoError(t, err)
	require.Equal(t, leader, block.Leader)
}
