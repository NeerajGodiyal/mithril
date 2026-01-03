package leaderschedule

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/bits"
	"slices"
	"sort"

	"github.com/Overclock-Validator/mithril/pkg/epochstakes"
	"github.com/Overclock-Validator/mithril/pkg/safemath"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"github.com/nixberg/chacha-rng-go"
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
	if repeat == 0 {
		panic("stakeWeightedSlotLeaders: repeat cannot be 0")
	}

	keyedStakes = sortStakes(keyedStakes)

	// Build cumulative weights, preserving input order (stake desc, pubkey desc)
	// This matches Agave's WeightedU64Index which does NOT re-sort
	cumulative := make([]uint64, len(keyedStakes))
	var total uint64
	for i, pair := range keyedStakes {
		newTotal, err := safemath.CheckedAddU64(total, pair.stake)
		if err != nil {
			panic(fmt.Sprintf("stakeWeightedSlotLeaders: cumulative stake overflow at index %d", i))
		}
		total = newTotal
		cumulative[i] = total
	}

	if total == 0 {
		panic("stakeWeightedSlotLeaders: total stake is zero")
	}

	// Create ChaCha20 RNG with epoch as seed, matching Agave's seeding:
	// let mut seed = [0u8; 32];
	// seed[0..8].copy_from_slice(&epoch.to_le_bytes());
	// let rng = &mut ChaChaRng::from_seed(seed);
	//
	// The nixberg/chacha-rng-go library takes [8]uint32 which is the same 32 bytes
	// interpreted as little-endian uint32s.
	var seedBytes [32]byte
	binary.LittleEndian.PutUint64(seedBytes[:], epoch)
	var seed [8]uint32
	for i := 0; i < 8; i++ {
		seed[i] = binary.LittleEndian.Uint32(seedBytes[i*4:])
	}
	rng := chacha.Seeded20(seed, 0) // stream=0 matches default

	leaders := make([]solana.PublicKey, 0, length)
	var currentSlotLeader solana.PublicKey

	for i := range length {
		if i%repeat == 0 {
			// Generate random in [0, total) using rejection sampling
			// This matches Rust's gen_range behavior
			r := uint64n(rng, total)
			idx := sort.Search(len(cumulative), func(j int) bool {
				return cumulative[j] > r
			})
			currentSlotLeader = keyedStakes[idx].pubkey
		}
		leaders = append(leaders, currentSlotLeader)
	}

	return leaders
}

// uint64n generates a uniform random uint64 in [0,n) using Lemire's method.
// This matches Rust's rand 0.8.x uniform distribution algorithm.
// See: https://lemire.me/blog/2019/06/06/nearly-divisionless-random-integer-generation-on-various-systems/
func uint64n(rng *chacha.ChaCha, n uint64) uint64 {
	if n == 0 {
		panic("uint64n: n cannot be 0")
	}

	// Lemire's nearly divisionless method
	// 1. Generate random x
	// 2. Compute wide multiply: m = x * n (as 128-bit)
	// 3. l = low 64 bits of m
	// 4. If l < n, need rejection check
	// 5. Return high 64 bits of m

	x := rng.Uint64()

	// Compute 128-bit product: m = x * n
	// mHi = high 64 bits, mLo = low 64 bits
	mHi, mLo := bits.Mul64(x, n)

	if mLo < n {
		// Rejection threshold: (2^64 - n) % n = (-n) % n
		threshold := -n % n
		for mLo < threshold {
			x = rng.Uint64()
			mHi, mLo = bits.Mul64(x, n)
		}
	}

	return mHi
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
