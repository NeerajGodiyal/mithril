package replay

import (
	"testing"

	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/stretchr/testify/require"
)

func TestVerifyFooterBankHashMatch(t *testing.T) {
	var hash [32]byte
	hash[0] = 0xab
	block := &b.Block{Slot: 42, HasExpectedBankhash: true, ExpectedBankhash: hash}
	slotCtx := &sealevel.SlotCtx{Slot: 42, FinalBankhash: hash[:]}
	require.NoError(t, verifyFooterBankHash(slotCtx, block))
}

func TestVerifyFooterBankHashMismatch(t *testing.T) {
	var expected, computed [32]byte
	expected[0] = 0x01
	computed[0] = 0x02
	block := &b.Block{Slot: 42, HasExpectedBankhash: true, ExpectedBankhash: expected}
	slotCtx := &sealevel.SlotCtx{Slot: 42, FinalBankhash: computed[:]}
	err := verifyFooterBankHash(slotCtx, block)
	require.Error(t, err)
	require.Contains(t, err.Error(), "footer bank hash mismatch")
}

func TestVerifyFooterBankHashSkippedWhenUnset(t *testing.T) {
	block := &b.Block{Slot: 42}
	slotCtx := &sealevel.SlotCtx{Slot: 42, FinalBankhash: []byte{1}}
	require.NoError(t, verifyFooterBankHash(slotCtx, block))
	require.NoError(t, verifyFooterBankHash(nil, block))
}

func TestRequireAlpenglowBlockFooter(t *testing.T) {
	require.False(t, requireAlpenglowBlockFooter(&b.Block{Slot: 1, FromLiveStream: true}, false))
	require.False(t, requireAlpenglowBlockFooter(&b.Block{Slot: 1, FromLiveStream: true, IsSkipped: true}, true))
	require.False(t, requireAlpenglowBlockFooter(&b.Block{Slot: 1}, true))
	require.True(t, requireAlpenglowBlockFooter(&b.Block{Slot: 1, FromLiveStream: true}, true))
}

func TestVerifyAlpenglowBlockFooterMissing(t *testing.T) {
	slotCtx := &sealevel.SlotCtx{Slot: 99, FinalBankhash: []byte{1}}
	block := &b.Block{Slot: 99, FromLiveStream: true}

	err := verifyAlpenglowBlockFooter(slotCtx, block, true)
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing block footer")
}

func TestVerifyAlpenglowBlockFooterRequiresBankHash(t *testing.T) {
	slotCtx := &sealevel.SlotCtx{Slot: 99, FinalBankhash: []byte{1}}
	block := &b.Block{Slot: 99, FromLiveStream: true, HasAlpenglowFooter: true}

	err := verifyAlpenglowBlockFooter(slotCtx, block, true)
	require.ErrorContains(t, err, "block footer is missing its bank hash")
}

func TestVerifyAlpenglowBlockFooterAllowsLeaderBlocks(t *testing.T) {
	slotCtx := &sealevel.SlotCtx{Slot: 99, FinalBankhash: []byte{1}}
	block := &b.Block{Slot: 99}

	require.NoError(t, verifyAlpenglowBlockFooter(slotCtx, block, true))
}
