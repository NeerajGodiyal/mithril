package blockprod

import (
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/alpenglow"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/tpu/txfixture"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLeaderWindowRequiresVerifiedParentReady(t *testing.T) {
	const leaderSlot = uint64(212)
	global.SetReplayFrontier(leaderSlot - 1)
	loop := NewLeaderLoop(LeaderLoopConfig{
		ProductionParent: func(uint64) alpenglow.BlockProductionParent {
			return alpenglow.BlockProductionParent{Kind: alpenglow.BlockProductionParentNotReady}
		},
	})

	ready, err := loop.leaderSlotReplayReady(leaderSlot)
	assert.False(t, ready)
	require.ErrorIs(t, err, errParentNotReady)
}

func TestLeaderWindowAcceptsVerifiedOlderParentReady(t *testing.T) {
	const leaderSlot = uint64(212)
	selected := alpenglow.BlockID{Slot: 208, Hash: solana.Hash{3}}
	global.SetReplayFrontier(selected.Slot)
	global.SetAlpenglowBlockID(selected.Slot, selected.Hash)
	global.SetAlpenglowChainedMerkleRoot(selected.Slot, solana.Hash{4})

	loop := NewLeaderLoop(LeaderLoopConfig{
		Controller:  NewController(),
		Identity:    txfixture.PayerPrivateKey(),
		Broadcaster: &captureBroadcaster{},
		CurrentSlot: func() uint64 { return leaderSlot },
		LeaderForSlot: func(slot uint64) (solana.PublicKey, bool) {
			return txfixture.PayerPubkey(), slot == leaderSlot
		},
		ProductionParent: func(uint64) alpenglow.BlockProductionParent {
			return alpenglow.BlockProductionParent{Kind: alpenglow.BlockProductionParentReady, Parent: selected, ReadyAt: time.Now()}
		},
		ParentContext: func(uint64) ParentContext {
			return coherentTestParentContext(selected.Slot, solana.Hash{5})
		},
		ParentBlockID: global.AlpenglowBlockID,
	})

	ready, err := loop.leaderSlotReplayReady(leaderSlot)
	assert.True(t, ready)
	assert.NoError(t, err)

	loop.tick()
	require.NotNil(t, loop.activeBank)
	loop.tick()
	assert.NotNil(t, loop.activeBank, "older verified ParentReady parent must remain canonical while the window opens")
}

func TestLeaderWindowUsesParentReadyDeadlineInsteadOfLiveSlot(t *testing.T) {
	const leaderSlot = uint64(212)
	selected := alpenglow.BlockID{Slot: 208, Hash: solana.Hash{3}}
	readyAt := time.Unix(1_700_000_000, 0)
	now := readyAt
	wallSlot := leaderSlot
	global.SetReplayFrontier(selected.Slot)
	global.SetAlpenglowBlockID(selected.Slot, selected.Hash)
	global.SetAlpenglowChainedMerkleRoot(selected.Slot, solana.Hash{4})

	loop := NewLeaderLoop(LeaderLoopConfig{
		Controller:  NewController(),
		Identity:    txfixture.PayerPrivateKey(),
		Broadcaster: &captureBroadcaster{},
		CurrentSlot: func() uint64 { return wallSlot },
		Now:         func() time.Time { return now },
		LeaderForSlot: func(slot uint64) (solana.PublicKey, bool) {
			return txfixture.PayerPubkey(), slot >= leaderSlot && slot < leaderSlot+alpenglow.LeaderWindowSlots
		},
		ProductionParent: func(uint64) alpenglow.BlockProductionParent {
			return alpenglow.BlockProductionParent{Kind: alpenglow.BlockProductionParentReady, Parent: selected, ReadyAt: readyAt}
		},
		ParentContext: func(uint64) ParentContext {
			return coherentTestParentContext(selected.Slot, solana.Hash{5})
		},
		ParentBlockID: global.AlpenglowBlockID,
	})

	loop.tick()
	require.NotNil(t, loop.activeBank)

	// Authenticated traffic can jump the live-slot estimate across the window.
	// The block still owns its ParentReady-derived time budget.
	wallSlot = leaderSlot + 3
	now = readyAt.Add(100 * time.Millisecond)
	loop.tick()
	assert.NotNil(t, loop.activeBank)
	assert.Equal(t, leaderSlot, loop.activeSlot)
}

func TestLeaderWindowReservesObservedFinalizationAndFanoutBudget(t *testing.T) {
	const leaderSlot = uint64(212)
	readyAt := time.Unix(1_700_000_000, 0)
	loop := NewLeaderLoop(LeaderLoopConfig{SlotDuration: AlpenglowSlotDuration})
	loop.productionWindow = leaderProductionWindow{
		active:    true,
		startSlot: leaderSlot,
		endSlot:   leaderSlot + alpenglow.LeaderWindowSlots - 1,
		nextSlot:  leaderSlot,
		readyAt:   readyAt,
	}

	// The incident trace's slowest successful finalize+footer path consumed
	// about 55ms after the freeze cutoff. The old 25ms reserve completed 30ms
	// late; the new cutoff leaves another 20ms for scheduler/fanout jitter.
	const observedWorstFinalization = 55 * time.Millisecond
	for offset := uint64(0); offset < alpenglow.LeaderWindowSlots; offset++ {
		slot := leaderSlot + offset
		protocolDeadline := loop.productionWindowProtocolDeadlineLocked(slot)
		freezeAt := loop.productionWindowDeadlineLocked(slot)
		require.Equal(t, readyAt.Add(time.Duration(offset+1)*AlpenglowSlotDuration), protocolDeadline)
		require.Equal(t, leaderBlockCompletionReserve, protocolDeadline.Sub(freezeAt))
		require.Equal(t, 20*time.Millisecond, protocolDeadline.Sub(freezeAt.Add(observedWorstFinalization)))
	}
}

func TestLeaderProductionTimingStopsAtNetworkCompletion(t *testing.T) {
	const leaderSlot = uint64(212)
	readyAt := time.Unix(1_700_000_000, 0)
	loop := NewLeaderLoop(LeaderLoopConfig{SlotDuration: AlpenglowSlotDuration})
	loop.productionWindow = leaderProductionWindow{
		active:    true,
		startSlot: leaderSlot,
		endSlot:   leaderSlot + alpenglow.LeaderWindowSlots - 1,
		nextSlot:  leaderSlot,
		readyAt:   readyAt,
	}

	finalizationStartedAt := readyAt.Add(125 * time.Millisecond)
	networkCompletedAt := readyAt.Add(190 * time.Millisecond)
	require.Equal(t,
		" window_elapsed_ms=190 finalization_ms=65 deadline_margin_ms=10",
		loop.productionTimingDetailLocked(leaderSlot, finalizationStartedAt, networkCompletedAt))

	// A blocked local handoff after the final shred must not be charged to the
	// protocol deadline or finalization path.
	localHandoffCompletedAt := networkCompletedAt.Add(40 * time.Millisecond)
	require.Equal(t,
		" window_elapsed_ms=230 finalization_ms=105 deadline_margin_ms=-30",
		loop.productionTimingDetailLocked(leaderSlot, finalizationStartedAt, localHandoffCompletedAt),
		"this is the contaminated value that recording after OnBlock would report")
}

func TestLeaderProductionTimingRequiresActiveWindow(t *testing.T) {
	loop := NewLeaderLoop(LeaderLoopConfig{SlotDuration: AlpenglowSlotDuration})
	now := time.Unix(1_700_000_000, 0)
	require.Empty(t, loop.productionTimingDetailLocked(212, now, now))
}

func TestLeaderWindowStartsNextSlotDespiteLiveSlotJump(t *testing.T) {
	const leaderSlot = uint64(212)
	readyAt := time.Unix(1_700_000_000, 0)
	now := readyAt.Add(250 * time.Millisecond)
	selected := alpenglow.BlockID{Slot: 208, Hash: solana.Hash{3}}
	global.SetReplayFrontier(leaderSlot)
	global.SetAlpenglowBlockID(leaderSlot, solana.Hash{6})
	global.SetAlpenglowChainedMerkleRoot(leaderSlot, solana.Hash{7})

	loop := NewLeaderLoop(LeaderLoopConfig{
		Controller:  NewController(),
		Identity:    txfixture.PayerPrivateKey(),
		Broadcaster: &captureBroadcaster{},
		CurrentSlot: func() uint64 { return leaderSlot + 3 },
		Now:         func() time.Time { return now },
		LeaderForSlot: func(slot uint64) (solana.PublicKey, bool) {
			return txfixture.PayerPubkey(), slot >= leaderSlot && slot < leaderSlot+alpenglow.LeaderWindowSlots
		},
		ProductionParent: func(uint64) alpenglow.BlockProductionParent {
			return alpenglow.BlockProductionParent{Kind: alpenglow.BlockProductionParentReady, Parent: selected, ReadyAt: readyAt}
		},
		ParentContext: func(uint64) ParentContext {
			return coherentTestParentContext(leaderSlot, solana.Hash{8})
		},
		ParentBlockID: global.AlpenglowBlockID,
	})
	loop.productionWindow = leaderProductionWindow{
		active:    true,
		startSlot: leaderSlot,
		endSlot:   leaderSlot + alpenglow.LeaderWindowSlots - 1,
		nextSlot:  leaderSlot + 1,
		readyAt:   readyAt,
	}
	loop.markLeaderSlotFinished(leaderSlot)

	loop.tick()
	require.NotNil(t, loop.activeBank)
	assert.Equal(t, leaderSlot+1, loop.activeSlot)
}

func TestLeaderWindowRejectsParentReadyForkMismatch(t *testing.T) {
	const leaderSlot = uint64(216)
	selected := alpenglow.BlockID{Slot: 212, Hash: solana.Hash{3}}
	global.SetReplayFrontier(leaderSlot - 1)
	global.SetAlpenglowBlockID(selected.Slot, solana.Hash{4})

	loop := NewLeaderLoop(LeaderLoopConfig{
		ProductionParent: func(uint64) alpenglow.BlockProductionParent {
			return alpenglow.BlockProductionParent{Kind: alpenglow.BlockProductionParentReady, Parent: selected}
		},
		ParentContext: func(uint64) ParentContext {
			return coherentTestParentContext(selected.Slot, solana.Hash{5})
		},
		ParentBlockID: global.AlpenglowBlockID,
	})

	err := loop.startSlotLocked(leaderSlot)
	require.ErrorIs(t, err, errParentNotReady)
	assert.Nil(t, loop.activeBank)
}

func TestLeaderSlotReplayReadyOtherLeaderParent(t *testing.T) {
	self := txfixture.PayerPubkey()
	other := solana.NewWallet().PublicKey()

	loop := &LeaderLoop{
		identity: txfixture.PayerPrivateKey(),
		leaderForSlot: func(slot uint64) (solana.PublicKey, bool) {
			switch slot {
			case 44:
				return other, true
			case 45:
				return self, true
			default:
				return solana.PublicKey{}, false
			}
		},
	}

	global.SetReplayFrontier(43)
	ready, err := loop.leaderSlotReplayReady(45)
	assert.False(t, ready)
	require.ErrorIs(t, err, errParentNotReady)

	global.SetReplayFrontier(44)
	global.SetAlpenglowBlockID(44, solana.Hash{1})
	global.SetAlpenglowChainedMerkleRoot(44, solana.Hash{2})
	ready, err = loop.leaderSlotReplayReady(45)
	assert.True(t, ready)
	assert.NoError(t, err)
}

func TestLeaderSlotReplayReadyOwnParentRequiresReplay(t *testing.T) {
	self := txfixture.PayerPubkey()

	loop := &LeaderLoop{
		identity:            txfixture.PayerPrivateKey(),
		finishedLeaderSlots: map[uint64]struct{}{42: {}},
		leaderForSlot: func(slot uint64) (solana.PublicKey, bool) {
			if slot == 42 || slot == 43 {
				return self, true
			}
			return solana.PublicKey{}, false
		},
	}

	global.SetAlpenglowBlockID(42, solana.Hash{1})
	global.SetAlpenglowChainedMerkleRoot(42, solana.Hash{2})
	global.SetReplayFrontier(40)
	ready, err := loop.leaderSlotReplayReady(43)
	assert.False(t, ready)
	require.ErrorIs(t, err, errParentNotReady)

	global.SetReplayFrontier(42)
	ready, err = loop.leaderSlotReplayReady(43)
	assert.True(t, ready)
	assert.NoError(t, err)
}

func TestLeaderLoopDoesNotAttemptLeaderUntilOtherParentReplayed(t *testing.T) {
	self := txfixture.PayerPubkey()
	other := solana.NewWallet().PublicKey()
	var wallSlot uint64 = 45

	global.SetReplayFrontier(43)
	global.SetAlpenglowBlockID(43, solana.Hash{1})
	global.SetAlpenglowChainedMerkleRoot(43, solana.Hash{2})

	loop := NewLeaderLoop(LeaderLoopConfig{
		Controller:  NewController(),
		Identity:    txfixture.PayerPrivateKey(),
		Broadcaster: &captureBroadcaster{},
		CurrentSlot: func() uint64 { return wallSlot },
		LeaderForSlot: func(slot uint64) (solana.PublicKey, bool) {
			switch slot {
			case 44:
				return other, true
			case 45:
				return self, true
			default:
				return solana.PublicKey{}, false
			}
		},
		ParentContext: func(uint64) ParentContext {
			return coherentTestParentContext(44, solana.Hash{2})
		},
		ParentBlockID: func(parentSlot uint64) (solana.Hash, bool) {
			return global.AlpenglowBlockID(parentSlot)
		},
	})

	loop.tick()
	assert.Nil(t, loop.activeBank)

	global.SetReplayFrontier(44)
	global.SetAlpenglowBlockID(44, solana.Hash{3})
	global.SetAlpenglowChainedMerkleRoot(44, solana.Hash{4})
	loop.tick()
	assert.NotNil(t, loop.activeBank)
}

func TestLeaderLoopNeverBackfillsMissedLeaderSlot(t *testing.T) {
	self := txfixture.PayerPubkey()
	global.SetReplayFrontier(44)
	global.SetAlpenglowBlockID(44, solana.Hash{1})
	global.SetAlpenglowChainedMerkleRoot(44, solana.Hash{2})

	loop := NewLeaderLoop(LeaderLoopConfig{
		Controller:  NewController(),
		Identity:    txfixture.PayerPrivateKey(),
		Broadcaster: &captureBroadcaster{},
		CurrentSlot: func() uint64 { return 50 },
		LeaderForSlot: func(slot uint64) (solana.PublicKey, bool) {
			return self, slot == 45
		},
		ParentContext: func(uint64) ParentContext {
			return coherentTestParentContext(44, solana.Hash{3})
		},
		ParentBlockID: global.AlpenglowBlockID,
	})

	loop.tick()
	assert.Nil(t, loop.activeBank)
	assert.False(t, loop.isLeaderSlotFinished(45))
}

func TestLeaderLoopRecordsExpiredLocalLeaderGateFailure(t *testing.T) {
	self := txfixture.PayerPubkey()
	var wallSlot uint64 = 45
	global.SetReplayFrontier(43)

	loop := NewLeaderLoop(LeaderLoopConfig{
		Identity:    txfixture.PayerPrivateKey(),
		CurrentSlot: func() uint64 { return wallSlot },
		LeaderForSlot: func(slot uint64) (solana.PublicKey, bool) {
			return self, slot == 45
		},
	})

	loop.tick()
	require.Contains(t, loop.pendingFailures, uint64(45))
	assert.Equal(t, leaderReasonReplayNotReady, loop.pendingFailures[45].reason)
	assert.False(t, loop.isLeaderSlotFinished(45))

	wallSlot = 46
	loop.tick()
	assert.True(t, loop.isLeaderSlotFinished(45))
	assert.NotContains(t, loop.pendingFailures, uint64(45))
}

func TestLeaderAfterSkippedSlotsUsesActualReplayedParent(t *testing.T) {
	self := txfixture.PayerPubkey()
	const leaderSlot = uint64(43)
	const actualParent = uint64(40)
	global.SetReplayFrontier(leaderSlot - 1) // slots 41 and 42 were resolved skipped
	global.SetAlpenglowBlockID(actualParent, solana.Hash{1})
	global.SetAlpenglowChainedMerkleRoot(actualParent, solana.Hash{2})

	loop := NewLeaderLoop(LeaderLoopConfig{
		Controller:  NewController(),
		Identity:    txfixture.PayerPrivateKey(),
		Broadcaster: &captureBroadcaster{},
		CurrentSlot: func() uint64 { return leaderSlot },
		LeaderForSlot: func(slot uint64) (solana.PublicKey, bool) {
			return self, slot == leaderSlot
		},
		ParentContext: func(uint64) ParentContext {
			return coherentTestParentContext(actualParent, solana.Hash{3})
		},
		ParentBlockID: func(parentSlot uint64) (solana.Hash, bool) {
			return global.AlpenglowBlockID(parentSlot)
		},
	})

	loop.tick()
	require.NotNil(t, loop.activeBank)
	require.Equal(t, actualParent, loop.parentCtx.ParentSlot)
	require.Equal(t, actualParent, loop.activeBank.SlotCtx().ParentSlot)
}

func TestLeaderRestartsWhenReplayParentChangesBeforeFreeze(t *testing.T) {
	self := txfixture.PayerPubkey()
	const leaderSlot = uint64(52)
	global.SetReplayFrontier(leaderSlot - 1)
	global.SetAlpenglowBlockID(leaderSlot-1, solana.Hash{1})
	global.SetAlpenglowChainedMerkleRoot(leaderSlot-1, solana.Hash{2})
	parentHash := solana.Hash{3}

	loop := NewLeaderLoop(LeaderLoopConfig{
		Controller:  NewController(),
		Identity:    txfixture.PayerPrivateKey(),
		Broadcaster: &captureBroadcaster{},
		CurrentSlot: func() uint64 { return leaderSlot },
		LeaderForSlot: func(slot uint64) (solana.PublicKey, bool) {
			return self, slot == leaderSlot
		},
		ParentContext: func(uint64) ParentContext {
			return coherentTestParentContext(leaderSlot-1, parentHash)
		},
		ParentBlockID: func(parentSlot uint64) (solana.Hash, bool) {
			return global.AlpenglowBlockID(parentSlot)
		},
	})

	loop.tick()
	require.NotNil(t, loop.activeBank)
	parentHash = solana.Hash{9}
	loop.tick()
	require.NotNil(t, loop.activeBank)
	require.Equal(t, parentHash, loop.parentCtx.ParentBankhash)
	require.False(t, loop.isLeaderSlotFinished(leaderSlot))
}

func TestLeaderAbortsWhenOwnSlotAlreadyResolved(t *testing.T) {
	self := txfixture.PayerPubkey()
	const leaderSlot = uint64(62)
	global.SetReplayFrontier(leaderSlot - 1)
	global.SetAlpenglowBlockID(leaderSlot-1, solana.Hash{1})
	global.SetAlpenglowChainedMerkleRoot(leaderSlot-1, solana.Hash{2})

	loop := NewLeaderLoop(LeaderLoopConfig{
		Controller:  NewController(),
		Identity:    txfixture.PayerPrivateKey(),
		Broadcaster: &captureBroadcaster{},
		CurrentSlot: func() uint64 { return leaderSlot },
		LeaderForSlot: func(slot uint64) (solana.PublicKey, bool) {
			return self, slot == leaderSlot
		},
		ParentContext: func(uint64) ParentContext {
			return coherentTestParentContext(leaderSlot-1, solana.Hash{3})
		},
		ParentBlockID: global.AlpenglowBlockID,
	})

	loop.tick()
	require.NotNil(t, loop.activeBank)
	global.SetReplayFrontier(leaderSlot) // e.g. a certified skip won the slot
	loop.tick()
	assert.Nil(t, loop.activeBank)
	assert.False(t, loop.isLeaderSlotFinished(leaderSlot))
}
