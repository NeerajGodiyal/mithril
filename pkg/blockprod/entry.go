package blockprod

import (
	"bytes"

	"github.com/Overclock-Validator/mithril/pkg/costmodel"
	"github.com/Overclock-Validator/mithril/pkg/turbine"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
)

// EntryBuilder accumulates forged transactions into Alpenglow-style entry batches.
type EntryBuilder struct {
	limits costmodel.Limits

	pendingTxns []solana.Transaction
	pendingWire int
	entryHash   solana.Hash
}

func NewEntryBuilder(limits costmodel.Limits, entryHash solana.Hash) *EntryBuilder {
	return &EntryBuilder{
		limits:    limits,
		entryHash: entryHash,
	}
}

func (b *EntryBuilder) PendingCount() int {
	return len(b.pendingTxns)
}

func (b *EntryBuilder) PendingWireBytes() int {
	return b.pendingWire
}

// Append adds a forged transaction. When the batch byte budget is exceeded it
// returns the flushed entry batch and resets the pending buffer.
func (b *EntryBuilder) Append(tx solana.Transaction, wireSize int) ([]turbine.Entry, int, bool) {
	if wireSize <= 0 {
		wire, err := tx.MarshalBinary()
		if err != nil {
			return nil, 0, false
		}
		wireSize = len(wire)
	}

	nextBytes, err := estimateBatchBytes(b.pendingTxns, tx)
	if err != nil {
		return nil, 0, false
	}
	if len(b.pendingTxns) > 0 && nextBytes > int(b.limits.MaxBatchBytes) {
		flushed, batchBytes := b.flushLocked()
		b.pendingTxns = append(b.pendingTxns[:0], tx)
		b.pendingWire = wireSize
		return flushed, batchBytes, true
	}

	b.pendingTxns = append(b.pendingTxns, tx)
	b.pendingWire += wireSize
	return nil, 0, false
}

// Flush emits the current pending transactions as a single PoH entry.
func (b *EntryBuilder) Flush() ([]turbine.Entry, int) {
	if len(b.pendingTxns) == 0 {
		return nil, 0
	}
	return b.flushLocked()
}

func (b *EntryBuilder) flushLocked() ([]turbine.Entry, int) {
	entries := []turbine.Entry{{
		NumHashes: 1,
		Hash:      b.entryHash,
		Txns:      append([]solana.Transaction(nil), b.pendingTxns...),
	}}
	batchBytes, err := marshalEntryBatchBytes(entries)
	if err != nil {
		return nil, 0
	}
	b.pendingTxns = b.pendingTxns[:0]
	b.pendingWire = 0
	return entries, len(batchBytes)
}

func estimateBatchBytes(pending []solana.Transaction, next solana.Transaction) (int, error) {
	txns := append(append([]solana.Transaction(nil), pending...), next)
	entries := []turbine.Entry{{
		NumHashes: 1,
		Txns:      txns,
	}}
	out, err := marshalEntryBatchBytes(entries)
	if err != nil {
		return 0, err
	}
	return len(out), nil
}

func marshalEntryBatchBytes(entries []turbine.Entry) ([]byte, error) {
	var buf bytes.Buffer
	enc := bin.NewEncoderWithEncoding(&buf, bin.EncodingBin)
	if err := enc.WriteUint64(uint64(len(entries)), bin.LE); err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if err := enc.WriteUint64(entry.NumHashes, bin.LE); err != nil {
			return nil, err
		}
		if err := enc.WriteBytes(entry.Hash[:], false); err != nil {
			return nil, err
		}
		if err := enc.WriteUint64(uint64(len(entry.Txns)), bin.LE); err != nil {
			return nil, err
		}
		for i := range entry.Txns {
			if err := entry.Txns[i].MarshalWithEncoder(enc); err != nil {
				return nil, err
			}
		}
	}
	return buf.Bytes(), nil
}
