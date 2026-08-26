package replay

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func validShapeTransaction() *solana.Transaction {
	return &solana.Transaction{
		Signatures: []solana.Signature{{}},
		Message: solana.Message{
			Header: solana.MessageHeader{
				NumRequiredSignatures:       1,
				NumReadonlyUnsignedAccounts: 1,
			},
			AccountKeys: []solana.PublicKey{{1}, {2}},
			Instructions: []solana.CompiledInstruction{{
				ProgramIDIndex: 1,
				Accounts:       []uint16{0},
			}},
		},
	}
}

func TestValidateTransactionShapeCanonicalRules(t *testing.T) {
	feats := features.NewFeaturesDefault()
	require.NoError(t, ValidateTransactionShape(validShapeTransaction(), feats))

	tests := []struct {
		name   string
		mutate func(*solana.Transaction)
	}{
		{"extra signature", func(tx *solana.Transaction) { tx.Signatures = append(tx.Signatures, solana.Signature{}) }},
		{"readonly unsigned overflow", func(tx *solana.Transaction) { tx.Message.Header.NumReadonlyUnsignedAccounts = 2 }},
		{"payer as program", func(tx *solana.Transaction) { tx.Message.Instructions[0].ProgramIDIndex = 0 }},
		{"empty lookup", func(tx *solana.Transaction) {
			tx.Message.SetAddressTableLookups([]solana.MessageAddressTableLookup{{AccountKey: solana.PublicKey{3}}})
		}},
		{"more than 256 keys", func(tx *solana.Transaction) {
			tx.Message.SetAddressTableLookups([]solana.MessageAddressTableLookup{{
				AccountKey: solana.PublicKey{3}, WritableIndexes: make([]uint8, 255),
			}})
		}},
		{"dynamic program", func(tx *solana.Transaction) {
			tx.Message.SetAddressTableLookups([]solana.MessageAddressTableLookup{{
				AccountKey: solana.PublicKey{3}, WritableIndexes: []uint8{0},
			}})
			tx.Message.Instructions[0].ProgramIDIndex = 2
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tx := validShapeTransaction()
			test.mutate(tx)
			require.ErrorIs(t, ValidateTransactionShape(tx, feats), TxErrSanitizeFailure)
		})
	}
}

func TestValidateTransactionShapeAllowsResolvedV0Account(t *testing.T) {
	tx := validShapeTransaction()
	table := solana.PublicKey{3}
	loaded := solana.PublicKey{4}
	tx.Message.SetAddressTableLookups([]solana.MessageAddressTableLookup{{
		AccountKey: table, WritableIndexes: []uint8{0},
	}})
	tx.Message.Instructions[0].Accounts = []uint16{0, 2}
	require.NoError(t, ValidateTransactionShape(tx, features.NewFeaturesDefault()))
	require.NoError(t, tx.Message.SetAddressTables(map[solana.PublicKey]solana.PublicKeySlice{table: {loaded}}))
	require.NoError(t, tx.Message.ResolveLookups())
	require.NoError(t, ValidateTransactionShape(tx, features.NewFeaturesDefault()))
}

func TestValidateTransactionShapeAppliesV1Limits(t *testing.T) {
	tx := validShapeTransaction()
	_, err := tx.Message.SetVersion(solana.MessageVersionV1)
	require.NoError(t, err)
	inactive := features.NewFeaturesDefault()
	require.ErrorIs(t, ValidateTransactionShape(tx, inactive), TxErrSanitizeFailure)
	active := features.NewFeaturesDefault()
	active.EnableFeature(features.EnableTransactionV1, 0)
	require.NoError(t, ValidateTransactionShape(tx, active))
	require.NoError(t, ValidateTransactionShape(tx, nil), "bank-independent validation may decode v1")

	tx.Message.AccountKeys = append(tx.Message.AccountKeys, tx.Message.AccountKeys[0])
	require.ErrorIs(t, ValidateTransactionShape(tx, active), TxErrSanitizeFailure)
}

func v1TransactionWithWireSize(t *testing.T, target int) *solana.Transaction {
	t.Helper()
	tx := validShapeTransaction()
	_, err := tx.Message.SetVersion(solana.MessageVersionV1)
	require.NoError(t, err)

	for low, high := 0, target; low <= high; {
		dataLen := low + (high-low)/2
		tx.Message.Instructions[0].Data = make([]byte, dataLen)
		wire, marshalErr := tx.MarshalBinary()
		require.NoError(t, marshalErr)
		switch {
		case len(wire) < target:
			low = dataLen + 1
		case len(wire) > target:
			high = dataLen - 1
		default:
			return tx
		}
	}
	t.Fatalf("could not construct a v1 transaction with %d-byte wire size", target)
	return nil
}

func TestValidateTransactionShapeAppliesExactV1WireLimit(t *testing.T) {
	active := features.NewFeaturesDefault()
	active.EnableFeature(features.EnableTransactionV1, 0)
	require.NoError(t, ValidateTransactionShape(v1TransactionWithWireSize(t, solana.MaxTransactionSizeV1), active))
	require.ErrorIs(t,
		ValidateTransactionShape(v1TransactionWithWireSize(t, solana.MaxTransactionSizeV1+1), active),
		TxErrSanitizeFailure,
	)
}

func transactionWithDistinctKeys(count int) *solana.Transaction {
	keys := make([]solana.PublicKey, count)
	for i := range keys {
		keys[i][0] = byte(i + 1)
		keys[i][1] = byte((i + 1) >> 8)
	}
	return &solana.Transaction{
		Signatures: []solana.Signature{{}},
		Message: solana.Message{
			Header:      solana.MessageHeader{NumRequiredSignatures: 1},
			AccountKeys: keys,
		},
	}
}

func TestTransactionAccountLocksMatchAgaveOrderingAndLimit(t *testing.T) {
	legacy := features.NewFeaturesDefault()
	_, failed := transactionAccountLockError(transactionWithDistinctKeys(legacyMaxTransactionAccountLocks), legacy)
	require.False(t, failed)
	errorType, failed := transactionAccountLockError(transactionWithDistinctKeys(legacyMaxTransactionAccountLocks+1), legacy)
	require.True(t, failed)
	require.Equal(t, TransactionErrorTooManyAccountLocks, errorType)

	increased := features.NewFeaturesDefault()
	increased.EnableFeature(features.IncreaseTxAccountLockLimit, 0)
	_, failed = transactionAccountLockError(transactionWithDistinctKeys(maxTransactionAccountLocks), increased)
	require.False(t, failed)
	tooMany := transactionWithDistinctKeys(maxTransactionAccountLocks + 1)
	errorType, failed = transactionAccountLockError(tooMany, increased)
	require.True(t, failed)
	require.Equal(t, TransactionErrorTooManyAccountLocks, errorType)

	tooMany.Message.AccountKeys[1] = tooMany.Message.AccountKeys[0]
	errorType, failed = transactionAccountLockError(tooMany, increased)
	require.True(t, failed)
	require.Equal(t, TransactionErrorAccountLoadedTwice, errorType, "duplicates take precedence over the lock limit")
}

func TestTransactionAccountLocksDetectStaticLoadedDuplicate(t *testing.T) {
	tx := validShapeTransaction()
	table := solana.PublicKey{3}
	tx.Message.SetAddressTableLookups([]solana.MessageAddressTableLookup{{
		AccountKey: table, ReadonlyIndexes: []uint8{0},
	}})
	require.NoError(t, tx.Message.SetAddressTables(map[solana.PublicKey]solana.PublicKeySlice{
		table: {tx.Message.AccountKeys[0]},
	}))
	require.NoError(t, tx.Message.ResolveLookups())

	errorType, failed := transactionAccountLockError(tx, features.NewFeaturesDefault())
	require.True(t, failed)
	require.Equal(t, TransactionErrorAccountLoadedTwice, errorType)
}

func TestLoadAndExecuteTransactionReturnsAccountLockErrors(t *testing.T) {
	slotCtx := &sealevel.SlotCtx{Features: features.NewFeaturesDefault()}
	for _, test := range []struct {
		name string
		tx   *solana.Transaction
		want TransactionErrorType
	}{
		{"duplicate", transactionWithDistinctKeys(2), TransactionErrorAccountLoadedTwice},
		{"too many", transactionWithDistinctKeys(legacyMaxTransactionAccountLocks + 1), TransactionErrorTooManyAccountLocks},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.want == TransactionErrorAccountLoadedTwice {
				test.tx.Message.AccountKeys[1] = test.tx.Message.AccountKeys[0]
			}
			out := LoadAndExecuteTransaction(LoadAndExecuteTransactionInput{
				SlotCtx: slotCtx, Transaction: test.tx, IsSimulation: true,
			})
			require.NotNil(t, out.ProcessingResult.TransactionError)
			require.Equal(t, test.want, out.ProcessingResult.TransactionError.ErrorType)
		})
	}
}

func TestLoadAndExecuteTransactionRejectsUnresolvedV0Lookups(t *testing.T) {
	tx := validShapeTransaction()
	tx.Message.SetAddressTableLookups([]solana.MessageAddressTableLookup{{
		AccountKey: solana.PublicKey{3}, ReadonlyIndexes: []uint8{0},
	}})
	out := LoadAndExecuteTransaction(LoadAndExecuteTransactionInput{
		SlotCtx:      &sealevel.SlotCtx{Features: features.NewFeaturesDefault()},
		Transaction:  tx,
		IsSimulation: true,
	})
	require.NotNil(t, out.ProcessingResult.TransactionError)
	require.Equal(t, TransactionErrorSanitizeFailure, out.ProcessingResult.TransactionError.ErrorType)
}
