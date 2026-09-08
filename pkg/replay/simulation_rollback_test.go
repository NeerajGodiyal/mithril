package replay

import (
	"math"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/tpu/txfixture"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/stretchr/testify/require"
)

func TestFailedSimulationPublishesRollbackBalances(t *testing.T) {
	slotCtx, cleanup := newCommitTestSlotCtx()
	defer cleanup()
	payer := txfixture.PayerPubkey()
	destination := txfixture.DestPubkey()

	transfer := system.NewTransferInstruction(100, payer, destination).Build()
	fail := solana.NewInstruction(addresses.SystemProgramAddr, nil, []byte{0xff, 0xff, 0xff, 0xff})
	tx, err := solana.NewTransaction(
		[]solana.Instruction{transfer, fail},
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

	out := LoadAndExecuteTransaction(LoadAndExecuteTransactionInput{
		SlotCtx: slotCtx, Transaction: tx, IsSimulation: true,
	})
	require.NotNil(t, out.ProcessingResult.TransactionError)
	require.Equal(t, TransactionErrorInstructionError, out.ProcessingResult.TransactionError.ErrorType)
	require.NotNil(t, out.ProcessingResult.TransactionError.InstructionIndex)
	require.Equal(t, uint8(1), *out.ProcessingResult.TransactionError.InstructionIndex)
	require.NotNil(t, out.FeeInfo)
	require.Len(t, out.PreAccountSnapshots, len(tx.Message.AccountKeys))
	require.Len(t, out.PostAccountSnapshots, len(tx.Message.AccountKeys))

	payerIndex := indexOfKey(t, tx.Message.AccountKeys, payer)
	destinationIndex := indexOfKey(t, tx.Message.AccountKeys, destination)
	require.Equal(t,
		out.PreAccountSnapshots[payerIndex].Lamports-out.FeeInfo.TotalFee,
		out.PostAccountSnapshots[payerIndex].Lamports,
	)
	require.Equal(t,
		out.PreAccountSnapshots[destinationIndex].Lamports,
		out.PostAccountSnapshots[destinationIndex].Lamports,
	)
	require.Equal(t,
		out.PreAccountSnapshots[destinationIndex].Lamports+100,
		out.ExecCtx.TransactionContext.Accounts.Accounts[destinationIndex].Lamports,
		"failed execution context should retain diagnostics while published post-state rolls back",
	)
	require.ErrorIs(t, out.ProcessingResult.TransactionError.InstructionError, sealevel.InstrErrInvalidInstructionData)
}

func TestProgramLoadFailureIsFeesOnly(t *testing.T) {
	slotCtx, cleanup := newCommitTestSlotCtx()
	defer cleanup()
	slotCtx.Features.EnableFeature(features.DefineLtdsFeeOnlySemantics, 0)
	payer := txfixture.PayerPubkey()
	missingProgram := solana.NewWallet().PublicKey()
	tx, err := solana.NewTransaction(
		[]solana.Instruction{solana.NewInstruction(missingProgram, nil, nil)},
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

	out := LoadAndExecuteTransaction(LoadAndExecuteTransactionInput{
		SlotCtx: slotCtx, Transaction: tx, IsSimulation: true,
	})
	require.True(t, out.FeesOnly)
	require.Nil(t, out.ExecCtx)
	require.NotNil(t, out.FeeInfo)
	require.NotNil(t, out.ProcessingResult.TransactionError)
	require.Equal(t, TransactionErrorProgramAccountNotFound, out.ProcessingResult.TransactionError.ErrorType)
	require.Equal(t, uint32(64), out.LoadedAccountsDataSize)
	require.Equal(t, out.PreBalances[0]-out.FeeInfo.TotalFee, out.PostAccountSnapshots[0].Lamports)

	payerBefore, err := slotCtx.GetAccount(payer)
	require.NoError(t, err)
	require.NoError(t, ApplyFailedTransaction(slotCtx, out))
	payerAfter, err := slotCtx.GetAccount(payer)
	require.NoError(t, err)
	require.Equal(t, payerBefore.Lamports-out.FeeInfo.TotalFee, payerAfter.Lamports)
}

func TestDurableNonceAdvancesForFeesOnlyFailure(t *testing.T) {
	for _, tc := range []struct {
		name         string
		payerIsNonce bool
		parentOnly   bool
	}{
		{"separate payer", false, false},
		{"payer is nonce", true, false},
		{"parent-only nonce", false, true},
		{"parent-only payer is nonce", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slotCtx, cleanup := newCommitTestSlotCtx()
			defer cleanup()
			payer := txfixture.PayerPubkey()
			nonceKey := solana.PublicKey{0xd5}
			if tc.payerIsNonce {
				nonceKey = payer
			}
			initialNonce := [32]byte{0xaa}
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
			nonceAccount := &accounts.Account{
				Key: nonceKey, Lamports: 10_000_000_000, Owner: addresses.SystemProgramAddr,
				Data: nonceData, RentEpoch: 7,
			}
			payerBefore, err := slotCtx.GetAccount(payer)
			require.NoError(t, err)
			payerBefore.RentEpoch = 7
			require.NoError(t, slotCtx.SetAccount(payer, payerBefore))
			if tc.parentOnly {
				delete(slotCtx.Accounts.(accounts.MemAccounts).Map, nonceKey)
				slotCtx.UnrootedRead = rollbackAccountSource{nonceKey: nonceAccount}
			} else {
				require.NoError(t, slotCtx.SetAccount(nonceKey, nonceAccount))
			}
			advance := system.NewAdvanceNonceAccountInstruction(
				nonceKey,
				solana.SysVarRecentBlockHashesPubkey,
				payer,
			).Build()
			missingProgram := solana.PublicKey{0xee, 1}
			tx, err := solana.NewTransaction(
				[]solana.Instruction{advance, solana.NewInstruction(missingProgram, nil, nil)},
				solana.Hash(initialNonce),
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

			out := LoadAndExecuteTransaction(LoadAndExecuteTransactionInput{
				SlotCtx: slotCtx, Transaction: tx, CaptureRollbackAccounts: true,
			})
			require.NoError(t, out.LoadError)
			require.True(t, out.FeesOnly)
			require.NotNil(t, out.FeeInfo)
			require.NotNil(t, out.ProcessingResult.TransactionError)
			require.Equal(t, TransactionErrorProgramAccountNotFound, out.ProcessingResult.TransactionError.ErrorType)
			nonceIndex := indexOfKey(t, tx.Message.AccountKeys, nonceKey)
			before, err := sealevel.UnmarshalNonceStateVersions(out.PreAccountSnapshots[nonceIndex].Data)
			require.NoError(t, err)
			after, err := sealevel.UnmarshalNonceStateVersions(out.PostAccountSnapshots[nonceIndex].Data)
			require.NoError(t, err)
			require.Equal(t, initialNonce, before.State().DurableNonce)
			require.NotEqual(t, initialNonce, after.State().DurableNonce)
			if tc.payerIsNonce {
				require.Equal(t,
					out.PreAccountSnapshots[0].Lamports-out.FeeInfo.TotalFee,
					out.PostAccountSnapshots[0].Lamports,
				)
			}
			require.Equal(t, uint64(math.MaxUint64), out.PostAccountSnapshots[0].RentEpoch)
			require.Equal(t, uint64(7), nonceAccount.RentEpoch)
			require.Equal(t, nonceData, nonceAccount.Data, "processing must not mutate the source account")
			// Compare the new captured rollback path to the existing replay commit semantics.
			comparison, comparisonCleanup := newCommitTestSlotCtx()
			defer comparisonCleanup()
			for _, account := range out.PreAccountSnapshots {
				require.NoError(t, comparison.SetAccount(account.Key, account.Clone()))
			}
			_, err = handleFailedTx(comparison, tx, out.Instrs, out.ComputeBudgetLimits, nil, nil)
			require.NoError(t, err)
			for _, key := range []solana.PublicKey{payer, nonceKey} {
				want, err := comparison.GetAccount(key)
				require.NoError(t, err)
				require.True(t, sameAccountState(want, out.PostAccountSnapshots[indexOfKey(t, tx.Message.AccountKeys, key)]))
			}

			require.NoError(t, ApplyFailedTransaction(slotCtx, out))
			committed, err := slotCtx.GetAccount(nonceKey)
			require.NoError(t, err)
			committedState, err := sealevel.UnmarshalNonceStateVersions(committed.Data)
			require.NoError(t, err)
			require.Equal(t, after.State().DurableNonce, committedState.State().DurableNonce)
		})
	}
}

func TestLoadedAccountsLimitFailureReportsRequestedLimit(t *testing.T) {
	slotCtx, cleanup := newCommitTestSlotCtx()
	defer cleanup()
	slotCtx.Features.EnableFeature(features.DefineLtdsFeeOnlySemantics, 0)
	payer := txfixture.PayerPubkey()
	limitData := []byte{sealevel.ComputeBudgetInstrTypeSetLoadedAccountsDataSizeLimit, 1, 0, 0, 0}
	tx, err := solana.NewTransaction([]solana.Instruction{
		solana.NewInstruction(addresses.ComputeBudgetProgramAddr, nil, limitData),
		system.NewTransferInstruction(1, payer, txfixture.DestPubkey()).Build(),
	}, txfixture.TestBlockhash(), solana.TransactionPayer(payer))
	require.NoError(t, err)
	privateKey := txfixture.PayerPrivateKey()
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key == payer {
			return &privateKey
		}
		return nil
	})
	require.NoError(t, err)

	out := LoadAndExecuteTransaction(LoadAndExecuteTransactionInput{SlotCtx: slotCtx, Transaction: tx, IsSimulation: true})
	require.True(t, out.FeesOnly)
	require.NotNil(t, out.ProcessingResult.TransactionError)
	require.Equal(t, TransactionErrorMaxLoadedAccountsDataSizeExceeded, out.ProcessingResult.TransactionError.ErrorType)
	require.Equal(t, uint32(1), out.LoadedAccountsDataSize)
}

func indexOfKey(t *testing.T, keys []solana.PublicKey, key solana.PublicKey) int {
	t.Helper()
	for i, candidate := range keys {
		if candidate == key {
			return i
		}
	}
	t.Fatalf("key %s missing", key)
	return -1
}

type rollbackAccountSource map[solana.PublicKey]*accounts.Account

func (source rollbackAccountSource) GetAccount(_ uint64, key solana.PublicKey) (*accounts.Account, error) {
	if account := source[key]; account != nil {
		return account, nil
	}
	return nil, accountsdb.ErrNoAccount
}
