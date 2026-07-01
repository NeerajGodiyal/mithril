package forkchoice

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/epochstakes"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRootingService builds a service and injects confirmed+observed slots.
func newRootingService() *ForkChoiceService {
	return NewForkChoiceService(0, map[solana.PublicKey]uint64{}, 100, epochstakes.NewEpochAuthorizedVotersCache())
}

// injectConfirmedSlot marks slot as both observed and supermajority-confirmed.
func injectConfirmedSlot(s *ForkChoiceService, slot, parentSlot uint64, winningHash solana.Hash) {
	acc := newSlotVoteAccumulator(100, slot)
	acc.trackers[winningHash] = &voteStakeTracker{voted: make(map[solana.PublicKey]struct{}), stake: 70}
	acc.confirmed = true
	acc.confirmedHash = winningHash
	s.state.voteStakeTotals[slot] = acc
	s.state.observedBlocks[slot] = &ObservedBlockMeta{Slot: slot, ParentSlot: parentSlot, ParentSlotKnown: true, Blockhash: winningHash}
}

// Applying votes records each validator's latest explicit root
// (monotonic; nil roots ignored).
func TestApplyVoteUpdatesRecordsRoots(t *testing.T) {
	s := newRootingService()
	rp := func(v uint64) *uint64 { return &v }
	s.applyVoteUpdatesLocked([]voteUpdate{
		{voteInfo: &voteInfo{slot: 110, bankHash: hash(110), votePubkey: vk(1), rootSlot: rp(78)}, stake: 50},
		{voteInfo: &voteInfo{slot: 111, bankHash: hash(111), votePubkey: vk(2), rootSlot: rp(80)}, stake: 30},
		{voteInfo: &voteInfo{slot: 109, bankHash: hash(109), votePubkey: vk(1), rootSlot: rp(70)}, stake: 50}, // older root -> ignored
		{voteInfo: &voteInfo{slot: 112, bankHash: hash(112), votePubkey: vk(3), rootSlot: nil}, stake: 20},    // no root
	}, 100)

	assert.Equal(t, uint64(78), s.state.validatorRoots[vk(1)], "keeps the max root, not the later-but-lower one")
	assert.Equal(t, uint64(80), s.state.validatorRoots[vk(2)])
	_, ok := s.state.validatorRoots[vk(3)]
	assert.False(t, ok, "nil root not recorded")
}

func vk(b byte) solana.PublicKey { return solana.PublicKey{b} }

// highestRootedSlot: the highest slot a >2/3 supermajority has rooted past, exact arithmetic.
func TestHighestRootedSlot(t *testing.T) {
	// total 120, threshold = floor(120*2/3) = 80, need stake STRICTLY > 80.
	stakes := map[solana.PublicKey]uint64{vk(1): 40, vk(2): 40, vk(3): 40}

	// roots 100/90/80: rooted >=90 is 80 (not >80); >=80 is 120 (>80) -> 80.
	got, ok := highestRootedSlot(map[solana.PublicKey]uint64{vk(1): 100, vk(2): 90, vk(3): 80}, stakes, 120)
	assert.True(t, ok)
	assert.Equal(t, uint64(80), got)

	// Skewed stake 50/30/40: >=90 is 80 (not >80); >=80 is 120 -> still 80.
	got, ok = highestRootedSlot(map[solana.PublicKey]uint64{vk(1): 100, vk(2): 90, vk(3): 80},
		map[solana.PublicKey]uint64{vk(1): 50, vk(2): 30, vk(3): 40}, 120)
	assert.True(t, ok)
	assert.Equal(t, uint64(80), got)

	// One validator with >2/3 alone: its own root is the watermark.
	got, ok = highestRootedSlot(map[solana.PublicKey]uint64{vk(1): 200, vk(2): 50},
		map[solana.PublicKey]uint64{vk(1): 90, vk(2): 30}, 120)
	assert.True(t, ok)
	assert.Equal(t, uint64(200), got)
}

func TestHighestRootedSlotNoQuorum(t *testing.T) {
	// Only 80 of 120 stake has any root; 80 is NOT > threshold(80).
	_, ok := highestRootedSlot(map[solana.PublicKey]uint64{vk(1): 100, vk(2): 100},
		map[solana.PublicKey]uint64{vk(1): 40, vk(2): 40, vk(3): 40}, 120)
	assert.False(t, ok, "only 2/3 (not >2/3) rooted -> not final")
}

func TestHighestRootedSlotIgnoresUnstaked(t *testing.T) {
	// vk(9) has a root but no stake entry -> ignored. vk(1)=100 > threshold 80.
	got, ok := highestRootedSlot(map[solana.PublicKey]uint64{vk(1): 500, vk(9): 999},
		map[solana.PublicKey]uint64{vk(1): 100, vk(2): 20}, 120)
	assert.True(t, ok)
	assert.Equal(t, uint64(500), got)
}

func TestHighestRootedSlotEmpty(t *testing.T) {
	_, ok := highestRootedSlot(nil, nil, 120)
	assert.False(t, ok)
}

// makeRooted makes 3 validators (40 each, total 120) all explicitly root at rootSlot.
func makeRooted(s *ForkChoiceService, rootSlot uint64) {
	s.state.epochStakes = map[solana.PublicKey]uint64{vk(1): 40, vk(2): 40, vk(3): 40}
	s.state.totalEpochStake = 120
	s.state.validatorRoots = map[solana.PublicKey]uint64{vk(1): rootSlot, vk(2): rootSlot, vk(3): rootSlot}
}

// rootedChain observes+confirms slots (anchor, top] linked by parent, so a path
// resolves from anchor to any slot in the chain.
func rootedChain(s *ForkChoiceService, anchor, top uint64) {
	for slot := anchor + 1; slot <= top; slot++ {
		injectConfirmedSlot(s, slot, slot-1, hash(byte(slot)))
	}
	s.state.latestObservedSlot = top
}

// Returns the slot a >2/3 supermajority has explicitly rooted past, when it is
// observed, confirmed, and path-resolves to the anchor.
func TestFindRootedSlotReturnsExplicitRoot(t *testing.T) {
	s := newRootingService()
	rootedChain(s, 100, 110)
	makeRooted(s, 110)

	r, err := s.FindRootedSlot(100, 32)
	require.NoError(t, err)
	assert.Equal(t, uint64(110), r.Slot)
	assert.Equal(t, hash(byte(110)), r.Bankhash)
}

// No durable root until a >2/3 supermajority has rooted (not just confirmed).
func TestFindRootedSlotNeedWaitWhenNoSupermajorityRoot(t *testing.T) {
	s := newRootingService()
	rootedChain(s, 100, 110)
	s.state.epochStakes = map[solana.PublicKey]uint64{vk(1): 40, vk(2): 40, vk(3): 40}
	s.state.totalEpochStake = 120
	s.state.validatorRoots = map[solana.PublicKey]uint64{vk(1): 110} // only 40/120 rooted

	_, err := s.FindRootedSlot(100, 32)
	assert.ErrorIs(t, err, ErrNeedWait)
}

// With no explicit roots at all, nothing is rooted.
func TestFindRootedSlotNeedWaitWhenNoRoots(t *testing.T) {
	s := newRootingService()
	rootedChain(s, 100, 110) // confirmed, but no roots recorded

	_, err := s.FindRootedSlot(100, 32)
	assert.ErrorIs(t, err, ErrNeedWait)
}

// The rooted watermark slot being unobserved falls back to the highest observed
// confirmed slot below it.
func TestFindRootedSlotSkipsUnobserved(t *testing.T) {
	s := newRootingService()
	rootedChain(s, 100, 109) // observe+confirm 101..109
	// slot 110 confirmed but NOT observed
	acc := newSlotVoteAccumulator(100, 110)
	acc.confirmed = true
	acc.confirmedHash = hash(byte(110))
	s.state.voteStakeTotals[110] = acc
	s.state.latestObservedSlot = 110
	makeRooted(s, 110) // watermark 110, but 110 unobserved

	r, err := s.FindRootedSlot(100, 32)
	require.NoError(t, err)
	assert.Equal(t, uint64(109), r.Slot)
}

// Equivocation at the rooted slot fails the gate.
func TestFindRootedSlotEquivocation(t *testing.T) {
	s := newRootingService()
	rootedChain(s, 100, 110)
	makeRooted(s, 110)
	s.state.equivocatedSlots[110] = struct{}{}

	_, err := s.FindRootedSlot(100, 32)
	assert.ErrorIs(t, err, ErrEquivocation)
}

// A broken (unresolvable) path from anchor to the rooted slot fails CLOSED:
// a slot that can't be tied to the canonical fork is never durably committed.
func TestFindRootedSlotFailsClosedOnBrokenPath(t *testing.T) {
	s := newRootingService()
	rootedChain(s, 100, 110)
	makeRooted(s, 110)
	delete(s.state.observedBlocks, 105) // sever the ancestry chain mid-way

	_, err := s.FindRootedSlot(100, 32)
	require.Error(t, err, "must not return a rooted slot whose path is unresolvable")
}
