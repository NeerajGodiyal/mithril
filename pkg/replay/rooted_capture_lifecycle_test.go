package replay

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/rootedevents"
	"github.com/stretchr/testify/require"
)

func TestRootedCaptureRetainsOwnedEvidenceUntilFoldOrUnwind(t *testing.T) {
	tail := rootedCaptureTestTail(t)
	observations := []rootedevents.TransactionObservation{{Transaction: []byte{1, 2}, Logs: []string{"original"}}}
	require.NoError(t, tail.RecordRootedEventSlot(testRootedEventIdentity(20, 19), observations))
	retainedBytes := tail.transactionBytes
	observations[0].Transaction[0] = 9
	observations[0].Logs[0] = "changed"
	require.Equal(t, byte(1), tail.transactions[20][0].Transaction[0])
	require.Equal(t, "original", tail.transactions[20][0].Logs[0])

	require.Error(t, tail.recordRootedEventSlotWithLimit(testRootedEventIdentity(21, 20), observations, retainedBytes))
	require.NotContains(t, tail.identities, uint64(21))
	require.NotContains(t, tail.transactions, uint64(21))
	require.Equal(t, retainedBytes, tail.transactionBytes)

	require.NoError(t, tail.RecordRootedEventSlot(testRootedEventIdentity(21, 20), nil))
	require.Contains(t, tail.transactions, uint64(21), "empty slots retain capture presence")
	tail.unwind(21)
	require.NotContains(t, tail.transactions, uint64(21))
	require.Contains(t, tail.transactions, uint64(20))
	require.Equal(t, retainedBytes, tail.transactionBytes)

	tail.applyFoldJob(&foldJob{through: 20})
	require.Empty(t, tail.identities)
	require.Empty(t, tail.transactions)
	require.Empty(t, tail.transactionSizes)
	require.Zero(t, tail.transactionBytes)
}
