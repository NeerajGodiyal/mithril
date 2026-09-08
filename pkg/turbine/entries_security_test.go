package turbine

import (
	"encoding/binary"
	"encoding/hex"
	"strings"
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

func TestEntryBatchEnforcesTransactionWireSizeByVersion(t *testing.T) {
	tests := []struct {
		name    string
		version solana.MessageVersion
		limit   int
	}{
		{name: "legacy", version: solana.MessageVersionLegacy, limit: legacyTransactionWireLimit},
		{name: "v0", version: solana.MessageVersionV0, limit: legacyTransactionWireLimit},
		{name: "v1", version: solana.MessageVersionV1, limit: solana.MaxTransactionSizeV1},
	}

	for _, test := range tests {
		t.Run(test.name+"_at_limit", func(t *testing.T) {
			tx := transactionWithWireSize(t, test.version, test.limit)
			raw, err := marshalEntryBatch([]Entry{{NumHashes: 1, Txns: []solana.Transaction{tx}}})
			if err != nil {
				t.Fatalf("marshal entry batch: %v", err)
			}
			if _, err := decodeEntryBatch(raw); err != nil {
				t.Fatalf("transaction at wire-size limit was rejected: %v", err)
			}
		})

		t.Run(test.name+"_over_limit", func(t *testing.T) {
			tx := transactionWithWireSize(t, test.version, test.limit+1)
			raw, err := marshalEntryBatch([]Entry{{NumHashes: 1, Txns: []solana.Transaction{tx}}})
			if err != nil {
				t.Fatalf("marshal entry batch: %v", err)
			}
			if _, err := decodeEntryBatch(raw); err == nil {
				t.Fatal("transaction over wire-size limit was accepted")
			} else if !strings.Contains(err.Error(), "wire size") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func transactionWithWireSize(t *testing.T, version solana.MessageVersion, target int) solana.Transaction {
	t.Helper()
	tx := solana.Transaction{
		Signatures: []solana.Signature{{}},
		Message: solana.Message{
			Header: solana.MessageHeader{
				NumRequiredSignatures:       1,
				NumReadonlyUnsignedAccounts: 1,
			},
			AccountKeys:     solana.PublicKeySlice{{1}, {2}},
			RecentBlockhash: solana.Hash{3},
			Instructions: []solana.CompiledInstruction{{
				ProgramIDIndex: 1,
				Accounts:       []uint16{0},
			}},
		},
	}
	if _, err := tx.Message.SetVersion(version); err != nil {
		t.Fatalf("set message version %d: %v", version, err)
	}

	dataSize := target
	for attempts := 0; attempts < 8; attempts++ {
		if dataSize < 0 {
			t.Fatalf("target wire size %d is smaller than transaction overhead", target)
		}
		tx.Message.Instructions[0].Data = make([]byte, dataSize)
		wire, err := tx.MarshalBinary()
		if err != nil {
			t.Fatalf("marshal transaction: %v", err)
		}
		if len(wire) == target {
			return tx
		}
		dataSize += target - len(wire)
	}
	t.Fatalf("could not construct version %d transaction with wire size %d", version, target)
	return solana.Transaction{}
}
