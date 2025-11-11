package replay

import (
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/base58"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
)

type unconfirmedBankhashState struct {
	slot         uint64
	totalStake   uint64
	votedStake   uint64
	slotsElapsed uint64
}

const maxSlotsElapsedForVoting = 32

func addStakesAndConfirmBankhashes(b map[[32]byte]*unconfirmedBankhashState, slotCtx *sealevel.SlotCtx) {
	// add the last replayed slot bankhash
	var currentBankhash [32]byte
	if n := copy(currentBankhash[:], slotCtx.FinalBankhash); n != 32 {
		panic(fmt.Sprintf("bankhash had unexpected size %d, expected 32", n))
	}
	b[currentBankhash] = &unconfirmedBankhashState{slot: slotCtx.Slot, totalStake: slotCtx.TotalEpochStake}

	for h, stake := range slotCtx.BankhashStakes {
		u, ok := b[h]
		if !ok {
			// someone voted on a bankhash we are not tracking?
			continue
		}

		// already got a supermajority voted on this slot
		if global.SlotConfirmed(u.slot) {
			delete(slotCtx.BankhashStakes, h)
			continue
		}

		threshold := (u.totalStake * 2) / 3
		u.votedStake += stake

		if u.votedStake < threshold {
			mlog.Log.Debugf("bankhash %s voting still below threshold. votedStake %d < threshold %d", base58.Encode(h[:]), u.votedStake, threshold)
			u.slotsElapsed++
			continue
		}

		// this bankhash has been confirmed by supermajority, so remove it from the maps and mark the slot as confirmed
		// in the global context
		percentage := float64(u.votedStake) * 100.0 / float64(slotCtx.TotalEpochStake)
		mlog.Log.Debugf("bankhash confirmed: %s (%.1f%% stake, %d/%d lamports) in slot %d (%d slot latency); len(b)=%d",
			base58.Encode(h[:]), percentage, stake, u.totalStake, slotCtx.Slot, slotCtx.Slot-u.slot, len(b))

		global.PutSlotConfirmed(slotCtx.Slot)
		delete(b, h)
		delete(slotCtx.BankhashStakes, h)
	}

	for h, u := range b {
		// check that bankhash has been voted as valid for a slot by a supermajority by the time 'maxSlotsElapsedForVoting'
		// subsequent slots have been produced.
		if u.slotsElapsed >= maxSlotsElapsedForVoting {
			if !global.SlotConfirmed(u.slot) {
				panic(fmt.Sprintf("slot %d with bankhash %s not 'confirmed' by votes after %d slots", u.slot, base58.Encode(h[:]), u.slotsElapsed))
			} else {
				delete(b, h)
				delete(slotCtx.BankhashStakes, h)
			}
		}
	}
}
