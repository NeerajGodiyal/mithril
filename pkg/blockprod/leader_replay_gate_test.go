package blockprod

import (
	"testing"

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
			return ParentContext{ParentSlot: selected.Slot, ParentBankhash: solana.Hash{5}}
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
			return ParentContext{ParentSlot: 44, ParentBankhash: solana.Hash{2}}
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
			return ParentContext{ParentSlot: 44, ParentBankhash: solana.Hash{3}}
		},
		ParentBlockID: global.AlpenglowBlockID,
	})

	loop.tick()
	assert.Nil(t, loop.activeBank)
	assert.False(t, loop.isLeaderSlotFinished(45))
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
			return ParentContext{ParentSlot: actualParent, ParentBankhash: solana.Hash{3}}
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
			return ParentContext{ParentSlot: leaderSlot - 1, ParentBankhash: parentHash}
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
			return ParentContext{ParentSlot: leaderSlot - 1, ParentBankhash: solana.Hash{3}}
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
