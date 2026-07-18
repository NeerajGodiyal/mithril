package replay

import (
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestPlanBlockTransactionExecutionRejectsDuplicateMessages(t *testing.T) {
	message := solana.Message{
		Header: solana.MessageHeader{
			NumRequiredSignatures: 2,
		},
		AccountKeys:     []solana.PublicKey{{1}, {2}},
		RecentBlockhash: solana.Hash{3},
	}
	first := &solana.Transaction{
		Signatures: []solana.Signature{{4}, {5}},
		Message:    message,
	}
	// Agave keys AlreadyProcessed by message hash, so changing the wire
	// signature does not make this a different transaction for this check.
	duplicateMessage := &solana.Transaction{
		Signatures: []solana.Signature{{6}, {7}},
		Message:    message,
	}
	differentMessage := &solana.Transaction{
		Signatures: []solana.Signature{{8}},
		Message: solana.Message{
			Header:          solana.MessageHeader{NumRequiredSignatures: 1},
			AccountKeys:     []solana.PublicKey{{1}},
			RecentBlockhash: solana.Hash{9},
		},
	}

	plan, err := planBlockTransactionExecution([]*solana.Transaction{
		first,
		duplicateMessage,
		differentMessage,
	})
	require.NoError(t, err)
	require.Equal(t, []bool{true, false, true}, plan.execute)
	require.Equal(t, uint64(2), plan.processedTxCount)
	require.Equal(t, uint64(3), plan.processedSignatures)
	require.Equal(t, uint64(1), plan.duplicateTxCount)
}

func TestPlanBlockTransactionExecutionRejectsNilTransaction(t *testing.T) {
	_, err := planBlockTransactionExecution([]*solana.Transaction{nil})
	require.ErrorContains(t, err, "transaction 0 is nil")
}
