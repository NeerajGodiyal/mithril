package costmodel

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/tpu/txfixture"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustParseTransferTx(t *testing.T, seq uint64) *solana.Transaction {
	t.Helper()
	wire := txfixture.MustSignedTransferWire(seq)
	tx, err := solana.TransactionFromBytes(wire)
	require.NoError(t, err)
	return tx
}

func TestEstimateTransactionCostTransfer(t *testing.T) {
	tx := mustParseTransferTx(t, 0)
	feats := features.NewFeaturesDefault()

	cost, err := EstimateTransactionCost(tx, feats)
	require.NoError(t, err)

	assert.Equal(t, uint64(SignatureCost), cost.SignatureCost)
	assert.Equal(t, uint64(2*WriteLockUnits), cost.WriteLockCost)
	assert.Greater(t, cost.ProgramsExecutionCost, uint64(0))
	assert.Len(t, cost.WritableAccounts, 2)
	assert.Greater(t, cost.Sum(), uint64(0))
}

func TestCostTrackerBlockLimit(t *testing.T) {
	limits := DefaultLimits()
	limits.BlockCost = 1000
	tracker := NewCostTracker(limits)

	cost := TransactionCost{
		SignatureCost:         720,
		WriteLockCost:         300,
		ProgramsExecutionCost: 200_000,
		WritableAccounts:      []solana.PublicKey{txfixture.PayerPubkey()},
	}

	assert.Equal(t, ExceedBlockCost, tracker.WouldExceed(cost))
	tracker.Record(cost)
	assert.Equal(t, ExceedBlockCost, tracker.WouldExceed(cost))
}

func TestCostTrackerWritableAccountLimit(t *testing.T) {
	limits := DefaultLimits()
	limits.WritableAccountCost = 1000
	tracker := NewCostTracker(limits)

	cost := TransactionCost{
		SignatureCost:         720,
		WriteLockCost:         300,
		ProgramsExecutionCost: 200_000,
		WritableAccounts:      []solana.PublicKey{txfixture.PayerPubkey()},
	}

	assert.Equal(t, ExceedWritableAccountCost, tracker.WouldExceed(cost))
}

func TestCostTrackerAcceptsUnderLimits(t *testing.T) {
	tracker := NewCostTracker(DefaultLimits())
	tx := mustParseTransferTx(t, 1)
	feats := features.NewFeaturesDefault()
	cost, err := EstimateTransactionCost(tx, feats)
	require.NoError(t, err)

	wire := txfixture.MustSignedTransferWire(1)
	assert.Equal(t, ExceedNone, tracker.WouldExceed(cost))
	tracker.Record(cost)
	assert.Equal(t, ExceedNone, tracker.WouldExceed(cost))
	_ = wire
}
