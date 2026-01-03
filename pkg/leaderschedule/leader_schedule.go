package leaderschedule

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"

	"github.com/Overclock-Validator/mithril/pkg/epochstakes"
	"github.com/Overclock-Validator/mithril/pkg/safemath"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/weightedrand"
	"github.com/gagliardetto/solana-go"
)

type LeaderSchedule struct {
	lsMap map[uint64]solana.PublicKey
}

func NewLeaderScheduleFromKeyedSlots(ls map[solana.PublicKey][]uint64, epochStartSlot uint64) *LeaderSchedule {
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

type pubkeyAndStakePair struct {
	pubkey solana.PublicKey
	stake  uint64
}

func New(
	epochVoteAcctsMap map[solana.PublicKey]*epochstakes.VoteAccount,
	epochVoteAcctStakes map[solana.PublicKey]uint64,
	epochSchedule *sealevel.SysvarEpochSchedule,
	epoch uint64,
	length uint64,
	repeat uint64) *LeaderSchedule {

	keyedStakes := make([]pubkeyAndStakePair, 0, len(epochVoteAcctStakes))
	for pubkey, stake := range epochVoteAcctStakes {
		if stake > 0 {
			keyedStakes = append(keyedStakes, pubkeyAndStakePair{pubkey: pubkey, stake: stake})
		}
	}

	leaders := stakeWeightedSlotLeaders(keyedStakes, epoch, length, repeat)
	firstSlotInEpoch := epochSchedule.FirstSlotInEpoch(epoch)
	leaderSchedule := newFromLeaders(leaders, epochVoteAcctsMap, firstSlotInEpoch, length)

	return leaderSchedule
}

func stakeWeightedSlotLeaders(keyedStakes []pubkeyAndStakePair,
	epoch uint64,
	length uint64,
	repeat uint64) []solana.PublicKey {
	keyedStakes = sortStakes(keyedStakes)

	choices := make([]weightedrand.Choice[solana.PublicKey, uint64], 0)

	for _, pair := range keyedStakes {
		choice := weightedrand.NewChoice(pair.pubkey, pair.stake)
		choices = append(choices, choice)
	}

	var seed [32]byte
	binary.LittleEndian.PutUint64(seed[:], epoch)

	chooser, err := weightedrand.NewChaCha20ChooserWithSeed(seed[:], choices...)
	if err != nil {
		panic(err)
	}

	leaders := make([]solana.PublicKey, 0)
	var currentSlotLeader solana.PublicKey

	for i := range length {
		if i%repeat == 0 {
			currentSlotLeader = chooser.PickWithChaCha20()
		}
		leaders = append(leaders, currentSlotLeader)
	}

	return leaders
}

type slotLeaderInfo struct {
	voteAcctAddr          solana.PublicKey
	validatorIdentityAddr solana.PublicKey
}

func newFromLeaders(voteKeyedSlotLeaders []solana.PublicKey,
	voteAcctsMap map[solana.PublicKey]*epochstakes.VoteAccount,
	firstSlotInEpoch uint64,
	length uint64) *LeaderSchedule {

	var defaultPubkey solana.PublicKey
	currentSlotLeaderInfo := slotLeaderInfo{voteAcctAddr: defaultPubkey, validatorIdentityAddr: defaultPubkey}

	slotLeaders := make([]solana.PublicKey, 0)
	for _, voteAcctAddr := range voteKeyedSlotLeaders {
		if voteAcctAddr != currentSlotLeaderInfo.voteAcctAddr {
			validatorIdentityAddr := voteAcctsMap[voteAcctAddr].NodePubkey
			currentSlotLeaderInfo = slotLeaderInfo{voteAcctAddr: voteAcctAddr, validatorIdentityAddr: validatorIdentityAddr}
		}
		slotLeaders = append(slotLeaders, currentSlotLeaderInfo.validatorIdentityAddr)
	}

	leaderScheduleMap := make(map[uint64]solana.PublicKey)
	for i, leader := range slotLeaders {
		slotNum := uint64(i) + firstSlotInEpoch
		leaderScheduleMap[slotNum] = leader
	}

	return &LeaderSchedule{lsMap: leaderScheduleMap}
}

func sortStakes(stakes []pubkeyAndStakePair) []pubkeyAndStakePair {
	slices.SortFunc(stakes, func(l, r pubkeyAndStakePair) int {
		if r.stake != l.stake {
			// Sort by stake descending (matches Agave's r_stake.cmp(l_stake))
			if r.stake > l.stake {
				return 1
			}
			return -1
		}
		// Tiebreak by pubkey descending (matches Agave's r_pubkey.cmp(l_pubkey))
		return bytes.Compare(r.pubkey[:], l.pubkey[:])
	})
	return slices.Compact(stakes)
}

func (ls *LeaderSchedule) LeaderForSlot(slot uint64) (solana.PublicKey, bool) {
	leader, exists := ls.lsMap[slot]
	return leader, exists
}
