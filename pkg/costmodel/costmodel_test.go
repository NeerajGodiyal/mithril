package costmodel

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
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

func TestLoadedAccountsDataSizeCostProtocolPages(t *testing.T) {
	assert.Equal(t, uint64(0), loadedAccountsDataSizeCost(0))
	assert.Equal(t, uint64(HeapCost), loadedAccountsDataSizeCost(1))
	assert.Equal(t, uint64(HeapCost), loadedAccountsDataSizeCost(AccountDataCostPageSize))
	assert.Equal(t, uint64(2*HeapCost), loadedAccountsDataSizeCost(AccountDataCostPageSize+1))

	defaultLimitPages := uint64(sealevel.MaxLoadedAccountsDataSizeBytes) / AccountDataCostPageSize
	assert.Equal(t, defaultLimitPages*HeapCost, loadedAccountsDataSizeCost(sealevel.MaxLoadedAccountsDataSizeBytes))
	assert.Equal(t, uint64(16_384), loadedAccountsDataSizeCost(sealevel.MaxLoadedAccountsDataSizeBytes))
}

func TestWritableAccountLimitAllowsManyDefaultLoadedSizeTxs(t *testing.T) {
	tracker := NewCostTracker(DefaultLimits())
	payer := txfixture.PayerPubkey()
	cost := TransactionCost{
		SignatureCost:              SignatureCost,
		WriteLockCost:              WriteLockUnits,
		ProgramsExecutionCost:      450,
		LoadedAccountsDataSizeCost: loadedAccountsDataSizeCost(sealevel.MaxLoadedAccountsDataSizeBytes),
		WritableAccounts:           []solana.PublicKey{payer},
	}

	for i := 0; i < 6; i++ {
		require.Equal(t, ExceedNone, tracker.WouldExceed(cost), "tx %d should fit under the 24M writable-account cap", i+1)
		tracker.Record(cost)
	}
	assert.LessOrEqual(t, tracker.WritableAccountCost(payer), uint64(MaxWritableAccountUnits))
}

func TestCostTrackerRebateDropsUnusedLoadedAndExec(t *testing.T) {
	tracker := NewCostTracker(DefaultLimits())
	payer := txfixture.PayerPubkey()
	estimated := TransactionCost{
		SignatureCost:              SignatureCost,
		WriteLockCost:              WriteLockUnits,
		DataBytesCost:              7,
		ProgramsExecutionCost:      450,
		LoadedAccountsDataSizeCost: loadedAccountsDataSizeCost(sealevel.MaxLoadedAccountsDataSizeBytes),
		WritableAccounts:           []solana.PublicKey{payer},
	}
	actualExec := uint64(150)
	actualLoaded := loadedAccountsDataSizeCost(200)

	require.Equal(t, ExceedNone, tracker.WouldExceed(estimated))
	tracker.Record(estimated)
	assert.Equal(t, estimated.Sum(), tracker.BlockCost())
	tracker.Rebate(estimated, actualExec, actualLoaded)

	want := estimated.SignatureCost + estimated.WriteLockCost + estimated.DataBytesCost + actualExec + actualLoaded
	assert.Equal(t, want, tracker.BlockCost())
	assert.Equal(t, want, tracker.WritableAccountCost(payer))
	assert.Less(t, tracker.BlockCost(), estimated.Sum())
}

func TestCostTrackerRebateAllowsManySamePayerDefaultLoadedTxs(t *testing.T) {
	tracker := NewCostTracker(DefaultLimits())
	payer := txfixture.PayerPubkey()
	estimated := TransactionCost{
		SignatureCost:              SignatureCost,
		WriteLockCost:              WriteLockUnits,
		DataBytesCost:              7,
		ProgramsExecutionCost:      450,
		LoadedAccountsDataSizeCost: loadedAccountsDataSizeCost(sealevel.MaxLoadedAccountsDataSizeBytes),
		WritableAccounts:           []solana.PublicKey{payer},
	}
	actualExec := uint64(150)
	actualLoaded := loadedAccountsDataSizeCost(200)
	actualSum := estimated.SignatureCost + estimated.WriteLockCost + estimated.DataBytesCost + actualExec + actualLoaded

	const n = 2000
	for i := 0; i < n; i++ {
		require.Equal(t, ExceedNone, tracker.WouldExceed(estimated), "tx %d should fit after rebates", i+1)
		tracker.Record(estimated)
		tracker.Rebate(estimated, actualExec, actualLoaded)
	}
	assert.Equal(t, actualSum*n, tracker.BlockCost())
	assert.Less(t, tracker.WritableAccountCost(payer), uint64(MaxWritableAccountUnits))
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
