package fees

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func emptyTransactionAccounts() *sealevel.TransactionAccounts {
	return sealevel.NewTransactionAccountsFromRefs([]*accounts.Account{}, []bool{})
}

func newEmptyTransaction() *solana.Transaction {
	return &solana.Transaction{
		Message: solana.Message{
			Header: solana.MessageHeader{NumRequiredSignatures: 1},
		},
	}
}

func TestCalculateAndDeductTxFees_Simulation_ReturnsErrorWithoutPanic(t *testing.T) {
	require.NotPanics(t, func() {
		fee, _, err := CalculateAndDeductTxFees(
			newEmptyTransaction(), nil, nil,
			emptyTransactionAccounts(),
			&sealevel.ComputeBudgetLimits{},
			features.NewFeaturesDefault(),
			sealevel.NewDefaultRentSysvar(),
			true,
		)
		assert.Nil(t, fee)
		assert.Error(t, err)
	})
}

func TestCalculateAndDeductTxFees_BlockReplay_PanicsOnMissingFeePayer(t *testing.T) {
	require.Panics(t, func() {
		_, _, _ = CalculateAndDeductTxFees(
			newEmptyTransaction(), nil, nil,
			emptyTransactionAccounts(),
			&sealevel.ComputeBudgetLimits{},
			features.NewFeaturesDefault(),
			sealevel.NewDefaultRentSysvar(),
			false,
		)
	})
}

func TestCalculateAndDeductTxFees_BlockReplay_PanicMessageContainsContext(t *testing.T) {
	defer func() {
		r := recover()
		require.NotNil(t, r)
		msg, ok := r.(string)
		require.True(t, ok)
		assert.Contains(t, msg, "CalculateAndDeductTxFees")
		assert.Contains(t, msg, "feePayer")
	}()

	_, _, _ = CalculateAndDeductTxFees(
		newEmptyTransaction(), nil, nil,
		emptyTransactionAccounts(),
		&sealevel.ComputeBudgetLimits{},
		features.NewFeaturesDefault(),
		sealevel.NewDefaultRentSysvar(),
		false,
	)
}

func TestCalculateTxFeesUsesV1DirectPriorityFee(t *testing.T) {
	tx := newEmptyTransaction()
	limits := &sealevel.ComputeBudgetLimits{
		ComputeUnitLimit:          200_000,
		PrioritizationFeeLamports: 777,
	}
	fee := CalculateTxFees(tx, nil, limits, features.NewFeaturesDefault())
	require.Equal(t, uint64(5_000), fee.ExecutionFee)
	require.Equal(t, uint64(777), fee.PriorityFee)
	require.Equal(t, uint64(5_777), fee.TotalFee)
}
