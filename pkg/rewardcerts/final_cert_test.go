package rewardcerts

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodeFinalCertificateRoundTrip(t *testing.T) {
	raw := buildTestFinalCertWire(t)
	fc, err := DecodeFinalCertificate(raw)
	require.NoError(t, err)
	require.Equal(t, uint64(1234567890), fc.Slot)
	require.Equal(t, bytes.Repeat([]byte{1}, 32), fc.BlockID[:])
	require.Len(t, fc.FinalAggregate.Bitmap, 64)
	require.Nil(t, fc.NotarAggregate)

	_, err = DecodeFinalCertificate(raw[:len(raw)-1])
	require.Error(t, err)
}

func buildTestFinalCertWire(t *testing.T) []byte {
	t.Helper()
	var out []byte
	var slotBuf [8]byte
	binary.LittleEndian.PutUint64(slotBuf[:], 1234567890)
	out = append(out, slotBuf[:]...)
	out = append(out, bytes.Repeat([]byte{1}, 32)...)
	out = append(out, make([]byte, 96)...)
	bitmap := bytes.Repeat([]byte{42}, 64)
	var bitmapLen [2]byte
	binary.LittleEndian.PutUint16(bitmapLen[:], uint16(len(bitmap)))
	out = append(out, bitmapLen[:]...)
	out = append(out, bitmap...)
	out = append(out, 0) // notar_aggregate None
	return out
}
