package replay

import (
	"crypto/sha256"
	"math"
	"sync"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/fees"
	"github.com/Overclock-Validator/mithril/pkg/metrics"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/tpu/txfixture"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessTransactionFailedDurableNoncePublicationMetrics(t *testing.T) {
	previousMetrics := metrics.GlobalBlockReplay
	defer func() { metrics.GlobalBlockReplay = previousMetrics }()

	slotCtx, cleanup := newCommitTestSlotCtx()
	defer cleanup()
	slotCtx.Replay = true

	previousRecent := sealevel.SysvarCache.RecentBlockHashes.Sysvar
	// Keep the queue non-empty so AdvanceNonceAccount itself is valid, while
	// excluding the transaction's durable nonce so age validation takes the
	// nonce-account path.
	recent := sealevel.SysvarRecentBlockhashes{{
		Blockhash:     [32]byte{0xBB},
		FeeCalculator: sealevel.FeeCalculator{LamportsPerSignature: 5000},
	}}
	sealevel.SysvarCache.RecentBlockHashes.Sysvar = &recent
	defer func() { sealevel.SysvarCache.RecentBlockHashes.Sysvar = previousRecent }()

	payer := txfixture.PayerPubkey()
	nonceKey := solana.PublicKey{0xD5}
	initialNonce := [32]byte{0xAA}
	nonceState := sealevel.NonceStateVersions{
		Type: sealevel.NonceVersionCurrent,
		Current: sealevel.NonceData{
			IsInitialized: true,
			Authority:     payer,
			DurableNonce:  initialNonce,
			FeeCalculator: sealevel.FeeCalculator{LamportsPerSignature: 5000},
		},
	}
	nonceData, err := nonceState.Marshal()
	require.NoError(t, err)
	require.NoError(t, slotCtx.SetAccount(nonceKey, &accounts.Account{
		Key:       nonceKey,
		Lamports:  10_000_000,
		Owner:     addresses.SystemProgramAddr,
		Data:      nonceData,
		RentEpoch: math.MaxUint64,
	}))
	slotCtx.LastBlockhash = [32]byte{0x77}

	advanceNonce := system.NewAdvanceNonceAccountInstruction(
		nonceKey,
		solana.SysVarRecentBlockHashesPubkey,
		payer,
	).Build()
	failAfterAdvance := solana.NewInstruction(
		addresses.SystemProgramAddr,
		nil,
		[]byte{0xff, 0xff, 0xff, 0xff},
	)
	tx, err := solana.NewTransaction(
		[]solana.Instruction{advanceNonce, failAfterAdvance},
		solana.Hash(initialNonce),
		solana.TransactionPayer(payer),
	)
	require.NoError(t, err)
	payerPrivateKey := txfixture.PayerPrivateKey()
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key == payer {
			return &payerPrivateKey
		}
		return nil
	})
	require.NoError(t, err)

	payerBefore, err := slotCtx.GetAccount(payer)
	require.NoError(t, err)
	metrics.GlobalBlockReplay = metrics.BlockReplay{}
	var sigverify sync.WaitGroup
	feeInfo, _, processErr := ProcessTransaction(
		slotCtx,
		&sigverify,
		tx,
		nil,
		nil,
		nil,
		false,
	)
	sigverify.Wait()
	require.ErrorIs(t, processErr, sealevel.InstrErrInvalidInstructionData)
	require.NotNil(t, feeInfo)

	payerAfter, err := slotCtx.GetAccount(payer)
	require.NoError(t, err)
	require.Positive(t, feeInfo.TotalFee)
	assert.Less(t, payerAfter.Lamports, payerBefore.Lamports)
	assert.Equal(t, payerBefore.Lamports-feeInfo.TotalFee, payerAfter.Lamports)

	nonceAfter, err := slotCtx.GetAccount(nonceKey)
	require.NoError(t, err)
	decodedNonce, err := sealevel.UnmarshalNonceStateVersions(nonceAfter.Data)
	require.NoError(t, err)
	require.Equal(t, uint32(sealevel.NonceVersionCurrent), decodedNonce.Type)
	state := decodedNonce.State()
	require.True(t, state.IsInitialized)
	assert.Equal(t, payer, state.Authority)
	expectedNonce := sha256.Sum256(append([]byte("DURABLE_NONCE"), slotCtx.LastBlockhash[:]...))
	assert.Equal(t, expectedNonce, state.DurableNonce)
	assert.NotEqual(t, initialNonce, state.DurableNonce)
	assert.Equal(t, slotCtx.FeeRateGovernor.PrevLamportsPerSignature, state.FeeCalculator.LamportsPerSignature)

	assert.Contains(t, slotCtx.ModifiedAccts, payer)
	assert.Contains(t, slotCtx.ModifiedAccts, nonceKey)
	got := metrics.GlobalBlockReplay
	assert.Equal(t, uint64(1), got.TxFailedUpdateAccounts.Count)
	assert.Equal(t, uint64(1), got.TxFailedPublicationPreparation.Count)
	assert.Equal(t, uint64(1), got.TxFailedPayerPublication.Count)
	assert.Equal(t, uint64(1), got.TxFailedNoncePublication.Count)
	assert.Zero(t, got.TxUpdateAccounts.Count)
	children := got.TxFailedPublicationPreparation.SumNanoseconds +
		got.TxFailedPayerPublication.SumNanoseconds +
		got.TxFailedNoncePublication.SumNanoseconds
	assert.LessOrEqual(t, children, got.TxFailedUpdateAccounts.SumNanoseconds)
}

func TestProcessTransactionMalformedComputeBudgetDoesNotChargeFee(t *testing.T) {
	slotCtx, cleanup := newCommitTestSlotCtx()
	defer cleanup()

	payer := txfixture.PayerPubkey()
	malformed := solana.NewInstruction(
		addresses.ComputeBudgetProgramAddr,
		nil,
		[]byte{sealevel.ComputeBudgetInstrTypeSetComputeUnitLimit},
	)
	tx, err := solana.NewTransaction(
		[]solana.Instruction{malformed},
		txfixture.TestBlockhash(),
		solana.TransactionPayer(payer),
	)
	require.NoError(t, err)
	privateKey := txfixture.PayerPrivateKey()
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key == payer {
			return &privateKey
		}
		return nil
	})
	require.NoError(t, err)

	payerBefore, err := slotCtx.GetAccount(payer)
	require.NoError(t, err)
	var sigverify sync.WaitGroup
	feeInfo, _, processErr := ProcessTransaction(slotCtx, &sigverify, tx, nil, nil, nil, false)
	sigverify.Wait()
	require.ErrorIs(t, processErr, sealevel.InstrErrInvalidInstructionData)
	assert.Nil(t, feeInfo)
	payerAfter, err := slotCtx.GetAccount(payer)
	require.NoError(t, err)
	assert.Equal(t, payerBefore.Lamports, payerAfter.Lamports)
	assert.NotContains(t, slotCtx.ModifiedAccts, payer)
}

func TestProcessTransactionFeePayerFailuresDoNotChargeFee(t *testing.T) {
	for _, tc := range []struct {
		name     string
		lamports func(*sealevel.SlotCtx) uint64
		wantType TransactionErrorType
	}{
		{
			name:     "insufficient fee",
			lamports: func(*sealevel.SlotCtx) uint64 { return 1 },
			wantType: TransactionErrorInsufficientFundsForFee,
		},
		{
			name: "rent reserve",
			lamports: func(slotCtx *sealevel.SlotCtx) uint64 {
				rent := fees.RentForSlot(slotCtx)
				return rent.MinimumBalance(0) + 5000 - 1
			},
			wantType: TransactionErrorInsufficientFundsForRent,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slotCtx, cleanup := newCommitTestSlotCtx()
			defer cleanup()
			tx, err := solana.TransactionFromBytes(txfixture.MustSignedTransferWire(0))
			require.NoError(t, err)
			payer, err := slotCtx.GetAccount(txfixture.PayerPubkey())
			require.NoError(t, err)
			payer.Lamports = tc.lamports(slotCtx)
			require.NoError(t, slotCtx.SetAccount(txfixture.PayerPubkey(), payer))
			before := payer.Lamports

			var sigverify sync.WaitGroup
			feeInfo, _, processErr := ProcessTransaction(slotCtx, &sigverify, tx, nil, nil, nil, false)
			sigverify.Wait()
			require.Nil(t, feeInfo)
			var txErr *TransactionError
			require.ErrorAs(t, processErr, &txErr)
			require.Equal(t, tc.wantType, txErr.ErrorType)
			after, err := slotCtx.GetAccount(txfixture.PayerPubkey())
			require.NoError(t, err)
			require.Equal(t, before, after.Lamports)
			require.NotContains(t, slotCtx.ModifiedAccts, txfixture.PayerPubkey())
		})
	}
}
