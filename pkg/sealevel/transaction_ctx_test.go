package sealevel

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewTransactionCtxUsesMinimumHeapFrame(t *testing.T) {
	txCtx := NewTransactionCtx(TransactionAccounts{}, 1, 1)

	require.NotNil(t, txCtx.ComputeBudgetLimits)
	require.Equal(t, uint32(MinHeapFrameBytes), txCtx.ComputeBudgetLimits.UpdatedHeapBytes)
}
