package replay

import (
	"encoding/binary"
	"math"
	"sync"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/metrics"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/tpu/txfixture"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplySuccessfulTransactionCommitsTransfer(t *testing.T) {
	slotCtx, cleanup := newCommitTestSlotCtx()
	defer cleanup()

	wire := txfixture.MustSignedTransferWire(0)
	tx, err := solana.TransactionFromBytes(wire)
	require.NoError(t, err)

	output := LoadAndExecuteTransaction(LoadAndExecuteTransactionInput{
		SlotCtx:     slotCtx,
		Transaction: tx,
	})
	require.Nil(t, output.ProcessingResult.TransactionError)
	require.NotNil(t, output.ProcessingResult.ProcessedTransaction)
	require.NotNil(t, output.ProcessingResult.ProcessedTransaction.Executed)
	require.Len(t, output.PreBalances, len(tx.Message.AccountKeys))
	require.NotEmpty(t, output.ExecutionResult.AccountUpdates)
	require.Len(t, output.ProcessingResult.ProcessedTransaction.Executed.LoadedTransaction.Accounts, len(tx.Message.AccountKeys))

	payerBefore, err := slotCtx.GetAccount(txfixture.PayerPubkey())
	require.NoError(t, err)
	destBefore, err := slotCtx.GetAccount(txfixture.DestPubkey())
	require.NoError(t, err)

	require.NoError(t, ApplySuccessfulTransaction(slotCtx, output))

	payerAfter, err := slotCtx.GetAccount(txfixture.PayerPubkey())
	require.NoError(t, err)
	destAfter, err := slotCtx.GetAccount(txfixture.DestPubkey())
	require.NoError(t, err)

	assert.Less(t, payerAfter.Lamports, payerBefore.Lamports)
	assert.Greater(t, destAfter.Lamports, destBefore.Lamports)
}

func TestApplySuccessfulTransactionCommitsLeanResult(t *testing.T) {
	slotCtx, cleanup := newCommitTestSlotCtx()
	defer cleanup()

	tx, err := solana.TransactionFromBytes(txfixture.MustSignedTransferWire(0))
	require.NoError(t, err)

	output := LoadAndExecuteTransaction(LoadAndExecuteTransactionInput{
		SlotCtx:     slotCtx,
		Transaction: tx,
		LeanResult:  true,
	})
	require.Nil(t, output.ProcessingResult.TransactionError)
	assert.Nil(t, output.ProcessingResult.ProcessedTransaction)
	require.NotNil(t, output.ExecutionResult)
	assert.Nil(t, output.ExecutionResult.AccountUpdates)
	assert.Nil(t, output.ExecutionResult.ModifiedVoteAccounts)
	assert.Nil(t, output.PreBalances)
	assert.IsType(t, discardLogger{}, output.ExecCtx.Log)
	assert.Nil(t, output.ExecCtx.ModifiedVoteStates)
	assert.ElementsMatch(t,
		[]solana.PublicKey{txfixture.PayerPubkey(), txfixture.DestPubkey()},
		output.ExecutionResult.WritableAccounts,
	)
	assert.Len(t, output.ExecutionResult.WritableAccounts, len(output.ExecutionResult.WritableAccountSet), "lean writable list must be deduplicated")

	destBefore, err := slotCtx.GetAccount(txfixture.DestPubkey())
	require.NoError(t, err)
	require.NoError(t, ApplySuccessfulTransaction(slotCtx, output))
	destAfter, err := slotCtx.GetAccount(txfixture.DestPubkey())
	require.NoError(t, err)
	assert.Greater(t, destAfter.Lamports, destBefore.Lamports)
}

func TestLeanResultCanCaptureDiagnostics(t *testing.T) {
	slotCtx, cleanup := newCommitTestSlotCtx()
	defer cleanup()

	tx, err := solana.TransactionFromBytes(txfixture.MustSignedTransferWire(0))
	require.NoError(t, err)

	output := LoadAndExecuteTransaction(LoadAndExecuteTransactionInput{
		SlotCtx:            slotCtx,
		Transaction:        tx,
		LeanResult:         true,
		CapturePreBalances: true,
		RecordLogs:         true,
	})
	require.Nil(t, output.ProcessingResult.TransactionError)
	require.Len(t, output.PreBalances, len(tx.Message.AccountKeys))
	assert.Equal(t, uint64(10_000_000_000), output.PreBalances[0])
	assert.IsType(t, &sealevel.LogRecorder{}, output.ExecCtx.Log)
	assert.Nil(t, output.ProcessingResult.ProcessedTransaction)
}

func TestProcessTransactionPublicationMetrics(t *testing.T) {
	for _, test := range []struct {
		name      string
		replay    bool
		removeADH bool
	}{
		{name: "replay-rich", replay: true},
		{name: "replay-lean-without-adh", replay: true, removeADH: true},
		{name: "block-production-is-not-recorded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			previousMetrics := metrics.GlobalBlockReplay
			defer func() { metrics.GlobalBlockReplay = previousMetrics }()
			slotCtx, cleanup := newCommitTestSlotCtx()
			defer cleanup()
			slotCtx.Replay = test.replay
			if test.removeADH {
				slotCtx.Features.EnableFeature(features.RemoveAccountsDeltaHash, 0)
			}
			tx, err := solana.TransactionFromBytes(txfixture.MustSignedTransferWire(0))
			require.NoError(t, err)
			metrics.GlobalBlockReplay = metrics.BlockReplay{}
			var sigverify sync.WaitGroup
			feeInfo, computeUnits, err := ProcessTransaction(slotCtx, &sigverify, tx, nil, nil, nil, false)
			sigverify.Wait()
			require.NoError(t, err)
			require.NotNil(t, feeInfo)
			assert.NotZero(t, computeUnits)

			got := metrics.GlobalBlockReplay
			if !test.replay {
				assert.Zero(t, got.TxUpdateAccounts.Count)
				assert.Zero(t, got.TxPublishRecordWritableAcct.Count)
				assert.Zero(t, got.TxPublishTouchedAccountState.Count)
				assert.Zero(t, got.TxPublishStakeVoteBookkeeping.Count)
				assert.Zero(t, got.TxPublicationTouchedAccounts)
				assert.Zero(t, got.TxPublicationTouchedAccountBytes)
				return
			}
			assert.Equal(t, uint64(1), got.TxUpdateAccounts.Count)
			if test.removeADH {
				assert.Zero(t, got.TxPublishRecordWritableAcct.Count)
			} else {
				assert.Equal(t, uint64(1), got.TxPublishRecordWritableAcct.Count)
			}
			assert.Equal(t, uint64(1), got.TxPublishTouchedAccountState.Count)
			assert.Equal(t, uint64(1), got.TxPublishStakeVoteBookkeeping.Count)
			assert.Equal(t, uint64(2), got.TxPublicationTouchedAccounts)
			assert.Zero(t, got.TxPublicationTouchedAccountBytes)
			children := got.TxPublishRecordWritableAcct.SumNanoseconds +
				got.TxPublishTouchedAccountState.SumNanoseconds +
				got.TxPublishStakeVoteBookkeeping.SumNanoseconds
			assert.LessOrEqual(t, children, got.TxUpdateAccounts.SumNanoseconds)
		})
	}
}

func TestProcessTransactionFailedPublicationMetrics(t *testing.T) {
	for _, replay := range []bool{true, false} {
		name := "block-production-is-not-recorded"
		if replay {
			name = "replay"
		}
		t.Run(name, func(t *testing.T) {
			previousMetrics := metrics.GlobalBlockReplay
			defer func() { metrics.GlobalBlockReplay = previousMetrics }()
			slotCtx, cleanup := newCommitTestSlotCtx()
			defer cleanup()
			slotCtx.Replay = replay

			tx, err := solana.TransactionFromBytes(txfixture.MustSignedTransferWire(0))
			require.NoError(t, err)
			require.GreaterOrEqual(t, len(tx.Message.Instructions[0].Data), 12)
			binary.LittleEndian.PutUint64(tx.Message.Instructions[0].Data[4:], math.MaxUint64)
			payerBefore, err := slotCtx.GetAccount(txfixture.PayerPubkey())
			require.NoError(t, err)

			metrics.GlobalBlockReplay = metrics.BlockReplay{}
			var sigverify sync.WaitGroup
			feeInfo, _, processErr := ProcessTransaction(slotCtx, &sigverify, tx, nil, nil, nil, false)
			sigverify.Wait()
			require.Error(t, processErr)
			require.NotNil(t, feeInfo)
			payerAfter, err := slotCtx.GetAccount(txfixture.PayerPubkey())
			require.NoError(t, err)
			assert.Less(t, payerAfter.Lamports, payerBefore.Lamports)

			got := metrics.GlobalBlockReplay
			if !replay {
				assert.Zero(t, got.TxFailedUpdateAccounts.Count)
				assert.Zero(t, got.TxFailedPublicationPreparation.Count)
				assert.Zero(t, got.TxFailedPayerPublication.Count)
				assert.Zero(t, got.TxFailedNoncePublication.Count)
				return
			}
			assert.Equal(t, uint64(1), got.TxFailedUpdateAccounts.Count)
			assert.Equal(t, uint64(1), got.TxFailedPublicationPreparation.Count)
			assert.Equal(t, uint64(1), got.TxFailedPayerPublication.Count)
			assert.Zero(t, got.TxFailedNoncePublication.Count)
			children := got.TxFailedPublicationPreparation.SumNanoseconds +
				got.TxFailedPayerPublication.SumNanoseconds +
				got.TxFailedNoncePublication.SumNanoseconds
			assert.LessOrEqual(t, children, got.TxFailedUpdateAccounts.SumNanoseconds)
			assert.Zero(t, got.TxUpdateAccounts.Count)
		})
	}
}

func TestHandleFailedTxNoncePublicationMetrics(t *testing.T) {
	previousMetrics := metrics.GlobalBlockReplay
	defer func() { metrics.GlobalBlockReplay = previousMetrics }()
	slotCtx, cleanup := newCommitTestSlotCtx()
	defer cleanup()
	slotCtx.Replay = true

	previousRecent := sealevel.SysvarCache.RecentBlockHashes.Sysvar
	emptyRecent := sealevel.SysvarRecentBlockhashes{}
	sealevel.SysvarCache.RecentBlockHashes.Sysvar = &emptyRecent
	defer func() { sealevel.SysvarCache.RecentBlockHashes.Sysvar = previousRecent }()

	authority := txfixture.PayerPubkey()
	nonceKey := solana.PublicKey{0xD5}
	durableNonce := [32]byte{0xAA}
	nonceState := sealevel.NonceStateVersions{
		Type: sealevel.NonceVersionCurrent,
		Current: sealevel.NonceData{
			IsInitialized: true,
			Authority:     authority,
			DurableNonce:  durableNonce,
			FeeCalculator: sealevel.FeeCalculator{LamportsPerSignature: 5000},
		},
	}
	nonceData, err := nonceState.Marshal()
	require.NoError(t, err)
	require.NoError(t, slotCtx.SetAccount(nonceKey, &accounts.Account{
		Key: nonceKey, Lamports: 1, Owner: addresses.SystemProgramAddr,
		Data: nonceData, RentEpoch: math.MaxUint64,
	}))
	slotCtx.LastBlockhash = [32]byte{0x77}

	tx, err := solana.TransactionFromBytes(txfixture.MustSignedTransferWire(0))
	require.NoError(t, err)
	tx.Message.RecentBlockhash = durableNonce
	instructionData := make([]byte, 4)
	binary.LittleEndian.PutUint32(instructionData, sealevel.SystemProgramInstrTypeAdvanceNonceAccount)
	instruction := sealevel.Instruction{
		ProgramId: addresses.SystemProgramAddr,
		Accounts: []sealevel.AccountMeta{
			{Pubkey: nonceKey, IsWritable: true},
			{Pubkey: authority, IsSigner: true},
		},
		Data: instructionData,
	}

	metrics.GlobalBlockReplay = metrics.BlockReplay{}
	feeInfo, handleErr := handleFailedTx(
		slotCtx,
		tx,
		[]sealevel.Instruction{instruction},
		&sealevel.ComputeBudgetLimits{},
		sealevel.InstrErrInvalidArgument,
		nil,
	)
	require.ErrorIs(t, handleErr, sealevel.InstrErrInvalidArgument)
	require.NotNil(t, feeInfo)
	assert.Contains(t, slotCtx.ModifiedAccts, txfixture.PayerPubkey())
	assert.Contains(t, slotCtx.ModifiedAccts, nonceKey)
	got := metrics.GlobalBlockReplay
	assert.Equal(t, uint64(1), got.TxFailedUpdateAccounts.Count)
	assert.Equal(t, uint64(1), got.TxFailedPublicationPreparation.Count)
	assert.Equal(t, uint64(1), got.TxFailedPayerPublication.Count)
	assert.Equal(t, uint64(1), got.TxFailedNoncePublication.Count)
	children := got.TxFailedPublicationPreparation.SumNanoseconds +
		got.TxFailedPayerPublication.SumNanoseconds + got.TxFailedNoncePublication.SumNanoseconds
	assert.LessOrEqual(t, children, got.TxFailedUpdateAccounts.SumNanoseconds)
}

func BenchmarkLoadAndExecuteTransferResultMode(b *testing.B) {
	slotCtx, cleanup := newCommitTestSlotCtx()
	defer cleanup()

	tx, err := solana.TransactionFromBytes(txfixture.MustSignedTransferWire(0))
	require.NoError(b, err)

	for _, bench := range []struct {
		name        string
		lean        bool
		preBalances bool
	}{
		{name: "rich"},
		{name: "lean", lean: true},
		{name: "lean-with-pre-balances", lean: true, preBalances: true},
	} {
		b.Run(bench.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				output := LoadAndExecuteTransaction(LoadAndExecuteTransactionInput{
					SlotCtx:            slotCtx,
					Transaction:        tx,
					LeanResult:         bench.lean,
					CapturePreBalances: bench.preBalances,
				})
				if output.ProcessingResult.TransactionError != nil {
					b.Fatal(output.ProcessingResult.TransactionError)
				}
			}
		})
	}
}

func BenchmarkApplySuccessfulTransactionPublicationMetrics(b *testing.B) {
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		b.Run(name, func(b *testing.B) {
			previousMetrics := metrics.GlobalBlockReplay
			defer func() { metrics.GlobalBlockReplay = previousMetrics }()
			slotCtx, cleanup := newCommitTestSlotCtx()
			defer cleanup()
			slotCtx.Replay = enabled

			tx, err := solana.TransactionFromBytes(txfixture.MustSignedTransferWire(0))
			require.NoError(b, err)
			output := LoadAndExecuteTransaction(LoadAndExecuteTransactionInput{
				SlotCtx: slotCtx, Transaction: tx,
			})
			require.Nil(b, output.ProcessingResult.TransactionError)
			require.NotNil(b, output.ExecCtx)
			require.NotNil(b, output.ExecutionResult)

			metrics.GlobalBlockReplay = metrics.BlockReplay{}
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if err := applySuccessfulTransactionState(slotCtx, output.ExecCtx, output.ExecutionResult); err != nil {
						panic(err)
					}
				}
			})
			b.StopTimer()
			b.ReportMetric(2, "touched/op")
		})
	}
}

func TestApplySuccessfulTransactionRejectsFailedOutput(t *testing.T) {
	slotCtx, cleanup := newCommitTestSlotCtx()
	defer cleanup()

	output := sanitizeFailureOutput()
	err := ApplySuccessfulTransaction(slotCtx, output)
	require.Error(t, err)
}

func newCommitTestSlotCtx() (*sealevel.SlotCtx, func()) {
	feats := features.NewFeaturesDefault()
	feats.EnableFeature(features.FormalizeLoadedTransactionDataSize, 0)

	mem := accounts.NewMemAccounts()
	systemAcct := &accounts.Account{
		Key:        addresses.SystemProgramAddr,
		Lamports:   1,
		Owner:      addresses.NativeLoaderAddr,
		Executable: true,
		RentEpoch:  math.MaxUint64,
	}
	_ = mem.SetAccountWithoutLock(addresses.SystemProgramAddr, systemAcct)
	_ = mem.SetAccountWithoutLock(txfixture.PayerPubkey(), &accounts.Account{
		Key: txfixture.PayerPubkey(), Lamports: 10_000_000_000, Owner: addresses.SystemProgramAddr, RentEpoch: math.MaxUint64,
	})
	_ = mem.SetAccountWithoutLock(txfixture.DestPubkey(), &accounts.Account{
		Key: txfixture.DestPubkey(), Lamports: 10_000_000, Owner: addresses.SystemProgramAddr, RentEpoch: math.MaxUint64,
	})

	prevRBH := sealevel.SysvarCache.RecentBlockHashes.Sysvar
	rbh := sealevel.SysvarRecentBlockhashes{{Blockhash: txfixture.TestBlockhash(), FeeCalculator: sealevel.FeeCalculator{LamportsPerSignature: 5000}}}
	sealevel.SysvarCache.RecentBlockHashes.Sysvar = &rbh

	prevRent := sealevel.SysvarCache.Rent.Sysvar
	rent := sealevel.NewDefaultRentSysvar()
	sealevel.SysvarCache.Rent.Sysvar = &rent

	slotCtx := &sealevel.SlotCtx{
		Slot:            42,
		Features:        feats,
		Accounts:        mem,
		FeeRateGovernor: &sealevel.FeeRateGovernor{PrevLamportsPerSignature: 5000},
		LastBlockhash:   txfixture.TestBlockhash(),
		AcctMapsMu:      &sync.Mutex{},
		ModifiedAccts:   make(map[solana.PublicKey]bool),
		WritableAccts:   make(map[solana.PublicKey]bool),
	}
	return slotCtx, func() {
		sealevel.SysvarCache.RecentBlockHashes.Sysvar = prevRBH
		sealevel.SysvarCache.Rent.Sysvar = prevRent
	}
}
