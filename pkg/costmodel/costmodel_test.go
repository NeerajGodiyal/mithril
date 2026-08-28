package costmodel

import (
	"bytes"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/tpu/txfixture"
	bin "github.com/gagliardetto/binary"
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
	assert.Equal(t, []solana.PublicKey{txfixture.PayerPubkey(), txfixture.DestPubkey()}, cost.WritableAccounts)
	assert.NotContains(t, cost.WritableAccounts, tx.Message.AccountKeys[len(tx.Message.AccountKeys)-1], "readonly program must not consume the destination's writable-account budget")
	assert.Greater(t, cost.Sum(), uint64(0))
}

func TestEstimateTransactionCostV1ReservesOneLoadedDataPage(t *testing.T) {
	tx := mustParseTransferTx(t, 0)
	_, err := tx.Message.SetVersion(solana.MessageVersionV1)
	require.NoError(t, err)

	cost, err := EstimateTransactionCost(tx, features.NewFeaturesDefault())
	require.NoError(t, err)
	require.Equal(t, uint64(HeapCost), cost.LoadedAccountsDataSizeCost)
}

func TestEstimateTransactionCostV0CountsResolvedWritableLookups(t *testing.T) {
	payer := solana.PublicKey{1}
	program := solana.PublicKey(addresses.SystemProgramAddr)
	table := solana.PublicKey{2}
	loadedWritable := solana.PublicKey{3}
	loadedReadonly := solana.PublicKey{4}
	tx := &solana.Transaction{
		Signatures: []solana.Signature{{}},
		Message: solana.Message{
			Header: solana.MessageHeader{
				NumRequiredSignatures:       1,
				NumReadonlyUnsignedAccounts: 1,
			},
			AccountKeys: []solana.PublicKey{payer, program},
			Instructions: []solana.CompiledInstruction{{
				ProgramIDIndex: 1,
				Accounts:       []uint16{0, 2, 3},
			}},
		},
	}
	tx.Message.SetAddressTableLookups([]solana.MessageAddressTableLookup{{
		AccountKey:      table,
		WritableIndexes: []uint8{0},
		ReadonlyIndexes: []uint8{1},
	}})
	require.NoError(t, tx.Message.SetAddressTables(map[solana.PublicKey]solana.PublicKeySlice{
		table: {loadedWritable, loadedReadonly},
	}))
	require.NoError(t, tx.Message.ResolveLookups())

	cost, err := EstimateTransactionCost(tx, features.NewFeaturesDefault())
	require.NoError(t, err)
	require.Equal(t, uint64(2*WriteLockUnits), cost.WriteLockCost)
	require.Contains(t, cost.WritableAccounts, loadedWritable)
	require.NotContains(t, cost.WritableAccounts, loadedReadonly)
}

func TestSignatureCostIncludesPrecompiles(t *testing.T) {
	tx := &solana.Transaction{
		Signatures: []solana.Signature{{}},
		Message: solana.Message{
			AccountKeys: []solana.PublicKey{
				solana.PublicKey(addresses.Ed25519PrecompileAddr),
				solana.PublicKey(addresses.Secp256kPrecompileAddr),
				solana.PublicKey(addresses.Secp256r1PrecompileAddr),
			},
			Instructions: []solana.CompiledInstruction{
				{ProgramIDIndex: 0, Data: []byte{2}},
				{ProgramIDIndex: 1, Data: []byte{3}},
				{ProgramIDIndex: 2, Data: []byte{4}},
			},
		},
	}
	feats := features.NewFeaturesDefault()
	require.Equal(t, uint64(SignatureCost+2*Ed25519VerifyStrictCost+3*Secp256k1VerifyCost), signatureCost(tx, feats))
	feats.EnableFeature(features.EnableSecp256r1Precompile, 0)
	require.Equal(t, uint64(SignatureCost+2*Ed25519VerifyStrictCost+3*Secp256k1VerifyCost+4*Secp256r1VerifyCost), signatureCost(tx, feats))
}

func marshalAllocateCostTest(t *testing.T, space uint64) []byte {
	t.Helper()
	var buffer bytes.Buffer
	require.NoError(t, (&sealevel.SystemInstrAllocate{Space: space}).MarshalWithEncoder(bin.NewBorshEncoder(&buffer)))
	return buffer.Bytes()
}

func TestEstimateAllocDeltaSystemAllocations(t *testing.T) {
	feats := features.NewFeaturesDefault()
	instrs := []sealevel.Instruction{
		{ProgramId: addresses.SystemProgramAddr, Data: marshalAllocateCostTest(t, 10)},
		{ProgramId: addresses.SystemProgramAddr, Data: marshalAllocateCostTest(t, 20)},
	}
	require.Equal(t, uint64(30), estimateAllocDelta(instrs, feats))
	instrs = append(instrs, sealevel.Instruction{ProgramId: addresses.SystemProgramAddr, Data: marshalAllocateCostTest(t, sealevel.SystemProgMaxPermittedDataLen)})
	require.Equal(t, uint64(sealevel.SystemProgMaxPermittedDataLen), estimateAllocDelta(instrs, feats), "sum is capped per transaction")
	require.Zero(t, estimateAllocDelta([]sealevel.Instruction{{ProgramId: addresses.SystemProgramAddr, Data: marshalAllocateCostTest(t, sealevel.SystemProgMaxPermittedDataLen+1)}}, feats))
	require.Zero(t, estimateAllocDelta([]sealevel.Instruction{{ProgramId: addresses.SystemProgramAddr, Data: []byte{1}}}, feats))
}

func TestCostTrackerChargesActualTransferDestination(t *testing.T) {
	tx := mustParseTransferTx(t, 0)
	cost, err := EstimateTransactionCost(tx, features.NewFeaturesDefault())
	require.NoError(t, err)
	limits := DefaultLimits()
	limits.BlockCost = ^uint64(0)
	limits.WritableAccountCost = cost.Sum()
	tracker := NewCostTracker(limits)
	require.Equal(t, ExceedNone, tracker.WouldExceed(cost))
	tracker.Record(cost)
	require.Equal(t, cost.Sum(), tracker.WritableAccountCost(txfixture.DestPubkey()))
	require.Equal(t, ExceedWritableAccountCost, tracker.WouldExceed(cost))
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

func TestPackEntryBytesMaxProtocolBindsAtDefaultShreds(t *testing.T) {
	shredSafe := PackEntryBytesMax(DefaultMaxDataShredsPerSlot, EntryHeaderBytes+PacketDataSize)
	require.Greater(t, shredSafe, uint64(DefaultMaxEntryBytesPerSlot))
	require.Equal(t, uint64(DefaultMaxEntryBytesPerSlot-EntryHeaderBytes), DefaultPackEntryBytes())
}

func TestPackEntryBytesMaxRejectsMicroblockLargerThanWatermark(t *testing.T) {
	wmark := uint64(FECSetsPerBatch)*uint64(TypicalFECSetPayloadBytes) - 8
	assert.Zero(t, PackEntryBytesMax(DefaultMaxDataShredsPerSlot, wmark))
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
