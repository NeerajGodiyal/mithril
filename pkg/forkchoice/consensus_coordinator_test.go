package forkchoice

import (
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestForkChoiceState(totalStake uint64) *ForkChoiceService {
	state := &forkChoiceState{
		voteStakeTotals:     make(map[uint64]*slotVoteAccumulator),
		observedBlocks:      make(map[uint64]*ObservedBlockMeta),
		blockhashToSlot:     make(map[solana.Hash]uint64),
		pendingParentByHash: make(map[solana.Hash][]uint64),
		equivocatedSlots:    make(map[uint64]struct{}),
		totalEpochStake:     totalStake,
	}
	return &ForkChoiceService{state: state}
}

func injectSupermajority(fc *ForkChoiceService, slot uint64, winningHash solana.Hash, stake uint64) {
	acc := newSlotVoteAccumulator(fc.state.totalEpochStake, slot)
	tracker := &voteStakeTracker{
		voted: make(map[solana.PublicKey]struct{}),
		stake: stake,
	}
	acc.trackers[winningHash] = tracker
	acc.confirmed = true
	acc.confirmedHash = winningHash
	fc.state.voteStakeTotals[slot] = acc
}

func TestResolveFromAnchorSuccess(t *testing.T) {
	fc := newTestForkChoiceState(100)

	injectSupermajority(fc, 13, hash(0xAA), 70)
	fc.state.observedBlocks[11] = &ObservedBlockMeta{Slot: 11, ParentSlot: 9, ParentSlotKnown: true, Blockhash: hash(0x11)}
	fc.state.observedBlocks[13] = &ObservedBlockMeta{Slot: 13, ParentSlot: 11, ParentSlotKnown: true, Blockhash: hash(0x13)}
	fc.state.latestObservedSlot = 13

	cc := NewConsensusCoordinator(fc, 64, "halt")
	resolved, err := cc.ResolveFromAnchor(9)
	require.NoError(t, err)
	require.NotNil(t, resolved)
	assert.Equal(t, uint64(13), resolved.LeafSlot)
	assert.Equal(t, hash(0xAA), resolved.LeafBankhash)
	assert.Equal(t, []SlotDecision{
		{Slot: 10, UseBlock: false},
		{Slot: 11, UseBlock: true},
		{Slot: 12, UseBlock: false},
		{Slot: 13, UseBlock: true},
	}, resolved.SlotDecisions)
}

func TestResolveFromAnchorNeedWait(t *testing.T) {
	fc := newTestForkChoiceState(100)

	cc := NewConsensusCoordinator(fc, 64, "halt")
	_, err := cc.ResolveFromAnchor(10)
	assert.ErrorIs(t, err, ErrNeedWait)
}

func TestResolveFromAnchorDepthExceeded(t *testing.T) {
	fc := newTestForkChoiceState(100)

	fc.state.latestObservedSlot = 100
	injectSupermajority(fc, 100, hash(0xEE), 70)

	cc := NewConsensusCoordinator(fc, 16, "halt")
	_, err := cc.ResolveFromAnchor(0)
	assert.ErrorIs(t, err, ErrDepthExceeded)
}

func TestResolveFromAnchorWaitsWhenObservedDepthExceedsLimitWithoutConfirmedLeaf(t *testing.T) {
	fc := newTestForkChoiceState(100)

	fc.state.latestObservedSlot = 100

	cc := NewConsensusCoordinator(fc, 16, "halt")
	_, err := cc.ResolveFromAnchor(0)
	assert.ErrorIs(t, err, ErrNeedWait)
}

func TestResolveFromAnchorPathIncomplete(t *testing.T) {
	fc := newTestForkChoiceState(100)

	injectSupermajority(fc, 15, hash(0xCC), 70)
	fc.state.observedBlocks[15] = &ObservedBlockMeta{Slot: 15, ParentSlotKnown: false, ParentBlockhash: hash(0x02)}
	fc.state.latestObservedSlot = 15

	cc := NewConsensusCoordinator(fc, 64, "halt")
	_, err := cc.ResolveFromAnchor(10)
	assert.ErrorIs(t, err, ErrPathIncomplete)
}

func TestResolveFromAnchorSkipsDisconnectedConfirmedLeaf(t *testing.T) {
	fc := newTestForkChoiceState(100)

	// Newest confirmed leaf does not connect back to the anchor.
	injectSupermajority(fc, 14, hash(0xDD), 70)
	fc.state.observedBlocks[14] = &ObservedBlockMeta{Slot: 14, ParentSlot: 8, ParentSlotKnown: true, Blockhash: hash(0x14)}

	// Slightly older confirmed leaf is valid and should be selected instead.
	injectSupermajority(fc, 13, hash(0xCC), 70)
	fc.state.observedBlocks[11] = &ObservedBlockMeta{Slot: 11, ParentSlot: 9, ParentSlotKnown: true, Blockhash: hash(0x11)}
	fc.state.observedBlocks[13] = &ObservedBlockMeta{Slot: 13, ParentSlot: 11, ParentSlotKnown: true, Blockhash: hash(0x13)}
	fc.state.latestObservedSlot = 14

	cc := NewConsensusCoordinator(fc, 64, "halt")
	resolved, err := cc.ResolveFromAnchor(9)
	require.NoError(t, err)
	require.NotNil(t, resolved)
	assert.Equal(t, uint64(13), resolved.LeafSlot)
	assert.Equal(t, hash(0xCC), resolved.LeafBankhash)
	assert.Equal(t, []SlotDecision{
		{Slot: 10, UseBlock: false},
		{Slot: 11, UseBlock: true},
		{Slot: 12, UseBlock: false},
		{Slot: 13, UseBlock: true},
	}, resolved.SlotDecisions)
}

func TestCoordinatorPolicy(t *testing.T) {
	fc := newTestForkChoiceState(100)
	cc := NewConsensusCoordinator(fc, 64, "halt")
	assert.Equal(t, "halt", cc.Policy())

	cc2 := NewConsensusCoordinator(fc, 64, "warn")
	assert.Equal(t, "warn", cc2.Policy())
}
