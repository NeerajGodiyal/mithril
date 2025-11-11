package leaderschedule

import (
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/safemath"
	"github.com/gagliardetto/solana-go"
)

type LeaderSchedule struct {
	lsMap map[uint64]solana.PublicKey
}

func NewLeaderSchedule(ls map[solana.PublicKey][]uint64, epochStartSlot uint64) *LeaderSchedule {
	lsMap := make(map[uint64]solana.PublicKey)

	for pubkey, epochIndices := range ls {
		for _, idx := range epochIndices {
			slot, err := safemath.CheckedAddU64(idx, epochStartSlot)
			if err != nil {
				panic(fmt.Sprintf("overflow for %s, idx %d, epochStartSlot = %d", pubkey, idx, epochStartSlot))
			}
			existingPubkey, exists := lsMap[slot]
			if exists {
				panic(fmt.Sprintf("error adding %s as leader for slot %d - there's already an entry for %s", pubkey, slot, existingPubkey))
			}
			lsMap[slot] = pubkey
		}
	}

	return &LeaderSchedule{lsMap: lsMap}
}

func (ls *LeaderSchedule) LeaderForSlot(slot uint64) solana.PublicKey {
	return ls.lsMap[slot]
}
