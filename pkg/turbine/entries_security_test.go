package turbine

import (
	"encoding/binary"
	"testing"
)

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
