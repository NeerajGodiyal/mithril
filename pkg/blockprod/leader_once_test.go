package blockprod

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/tpu/txfixture"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLeaderLoopHoldsSlotUntilWallAdvances(t *testing.T) {
	leader := txfixture.PayerPubkey()
	var wallSlot uint64 = 42
	global.SetReplayFrontier(41)
	global.SetAlpenglowBlockID(41, solana.Hash{7})
	global.SetAlpenglowChainedMerkleRoot(41, solana.Hash{8})

	loop := NewLeaderLoop(LeaderLoopConfig{
		Controller:  NewController(),
		Identity:    txfixture.PayerPrivateKey(),
		Broadcaster: &captureBroadcaster{},
		CurrentSlot: func() uint64 { return wallSlot },
		LeaderForSlot: func(s uint64) (solana.PublicKey, bool) {
			return leader, s == 42 || s == 43
		},
		ParentContext: func(uint64) ParentContext {
			return ParentContext{ParentSlot: 41, ParentBankhash: solana.Hash{1}}
		},
		ParentBlockID: func(parentSlot uint64) (solana.Hash, bool) {
			return global.AlpenglowBlockID(parentSlot)
		},
	})

	loop.tick()
	require.NotNil(t, loop.activeBank)
	assert.Equal(t, uint64(42), loop.activeSlot)
	assert.False(t, loop.isLeaderSlotFinished(42))

	// Replay still behind; next pending leader slot is 43 but wall has not passed 42.
	loop.tick()
	require.NotNil(t, loop.activeBank)
	assert.Equal(t, uint64(42), loop.activeSlot)
	assert.False(t, loop.isLeaderSlotFinished(42))

	wallSlot = 43
	loop.tick()
	assert.True(t, loop.isLeaderSlotFinished(42))
	assert.Nil(t, loop.activeBank, "slot 43 must wait until locally produced slot 42 passes replay")
}

func TestLeaderLoopDoesNotRestartFinishedWallSlot(t *testing.T) {
	leader := txfixture.PayerPubkey()
	var wallSlot uint64 = 42
	global.SetReplayFrontier(41)
	global.SetAlpenglowBlockID(41, solana.Hash{7})
	global.SetAlpenglowChainedMerkleRoot(41, solana.Hash{8})

	loop := NewLeaderLoop(LeaderLoopConfig{
		Controller:  NewController(),
		Identity:    txfixture.PayerPrivateKey(),
		Broadcaster: &captureBroadcaster{},
		CurrentSlot: func() uint64 { return wallSlot },
		LeaderForSlot: func(s uint64) (solana.PublicKey, bool) {
			return leader, s == 42
		},
		ParentContext: func(uint64) ParentContext {
			return ParentContext{ParentSlot: 41, ParentBankhash: solana.Hash{1}}
		},
		ParentBlockID: func(uint64) (solana.Hash, bool) {
			return global.AlpenglowBlockID(41)
		},
	})

	loop.tick()
	require.NotNil(t, loop.activeBank)

	wallSlot = 43
	loop.tick()
	assert.True(t, loop.isLeaderSlotFinished(42))
	assert.Nil(t, loop.activeBank)

	wallSlot = 42
	loop.tick()
	assert.Nil(t, loop.activeBank)
	assert.True(t, loop.isLeaderSlotFinished(42))
}
