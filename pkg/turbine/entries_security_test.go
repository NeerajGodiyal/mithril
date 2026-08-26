package turbine

import (
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/txverify"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

const v1GoldenTxFeeHeapHex = "8101000113000000070707070707070707070707070707070707070707070707070707070707070701028a88e3dd7409f195fd52db2d3cba5d72ca6709bf1d94121bf3748801b40f6f5c1010101010101010101010101010101010101010101010101010101010101010010000000000000000800000010100000070083fb1bf5a9e2c90c6903b7328529cdf59e04e2fcf29c718bea8c76fd325f4b67e18feb1f95f9aab171140b81b8c8238c9ffea33d4133bae92af68c6453800"

func TestEntryBatchDecodesCanonicalV1Transaction(t *testing.T) {
	wire, err := hex.DecodeString(v1GoldenTxFeeHeapHex)
	require.NoError(t, err)

	batch := make([]byte, 8+8+32+8, 8+8+32+8+len(wire))
	binary.LittleEndian.PutUint64(batch[:8], 1)
	binary.LittleEndian.PutUint64(batch[8:16], 1)
	binary.LittleEndian.PutUint64(batch[48:56], 1)
	batch = append(batch, wire...)

	entries, err := decodeEntryBatch(batch)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Len(t, entries[0].Txns, 1)
	tx := &entries[0].Txns[0]
	require.Equal(t, solana.MessageVersionV1, tx.Message.GetVersion())
	require.Equal(t, uint64(1), *tx.Message.TransactionConfig.PriorityFee)
	require.Equal(t, uint32(32*1024), *tx.Message.TransactionConfig.HeapSize)
	require.NoError(t, txverify.VerifyTransaction(tx))

	roundTrip, err := marshalEntryBatch(entries)
	require.NoError(t, err)
	require.Equal(t, batch, roundTrip)
}

func TestLegacyEntryBatchRejectsImpossibleEntryCount(t *testing.T) {
	raw := make([]byte, 8)
	binary.LittleEndian.PutUint64(raw, ^uint64(0))
	if _, err := decodeEntryBatch(raw); err == nil {
		t.Fatal("impossible entry count was accepted")
	}
}

func TestLegacyEntryBatchRejectsImpossibleTransactionCount(t *testing.T) {
	raw := make([]byte, 8+minimumEntryWireSize)
	binary.LittleEndian.PutUint64(raw[:8], 1)
	binary.LittleEndian.PutUint64(raw[8+8+32:], ^uint64(0))
	if _, err := decodeEntryBatch(raw); err == nil {
		t.Fatal("impossible transaction count was accepted")
	}
}

func TestLegacyEntryBatchRejectsTrailingBytes(t *testing.T) {
	raw := make([]byte, 8+minimumEntryWireSize+1)
	binary.LittleEndian.PutUint64(raw[:8], 1)
	if _, err := decodeEntryBatch(raw); err == nil {
		t.Fatal("entry batch with trailing bytes was accepted")
	}
}
