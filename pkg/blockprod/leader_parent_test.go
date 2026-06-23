package blockprod

import (
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/tpu/txfixture"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLeaderLoopWaitsForParentBlockID(t *testing.T) {
	bc := &captureBroadcaster{}
	controller := NewController()
	leader := txfixture.PayerPubkey()

	const slot = uint64(210)
	const parentSlot = slot - 1
	var wallSlot uint64 = slot
	loop := NewLeaderLoop(LeaderLoopConfig{
		Controller:  controller,
		Identity:    txfixture.PayerPrivateKey(),
		Broadcaster: bc,
		CurrentSlot: func() uint64 { return wallSlot },
		LeaderForSlot: func(s uint64) (solana.PublicKey, bool) {
			if s == slot {
				return leader, true
			}
			return solana.PublicKey{}, false
		},
		ParentContext: func(uint64) ParentContext {
			return ParentContext{ParentBankhash: solana.Hash{9}}
		},
		ParentBlockID: func(uint64) (solana.Hash, bool) {
			return global.AlpenglowBlockID(parentSlot)
		},
		BankHash:     DefaultBankHash,
		PollInterval: 5 * time.Millisecond,
	})

	global.SetSlot(parentSlot)
	stop := make(chan struct{})
	go loop.Run(stop)
	time.Sleep(25 * time.Millisecond)
	assert.Nil(t, controller.WorkingBank())
	assert.Equal(t, 0, bc.count())

	global.SetAlpenglowBlockID(parentSlot, solana.Hash{7})
	global.SetAlpenglowChainedMerkleRoot(parentSlot, solana.Hash{8})
	time.Sleep(25 * time.Millisecond)
	require.NotNil(t, controller.WorkingBank())
	assert.Greater(t, bc.count(), 0)

	close(stop)
}
