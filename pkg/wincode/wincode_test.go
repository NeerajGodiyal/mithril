package wincode

import (
	"bytes"
	"testing"
)

func TestWriterReaderPrimitives(t *testing.T) {
	w := NewWriter(0)
	w.WriteU16(0x0102)
	w.WriteU32(0x03040506)
	w.WriteU64(0x0708090a0b0c0d0e)
	w.WriteByteVec([]byte{0xaa, 0xbb})

	want := []byte{
		0x02, 0x01,
		0x06, 0x05, 0x04, 0x03,
		0x0e, 0x0d, 0x0c, 0x0b, 0x0a, 0x09, 0x08, 0x07,
		0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0xaa, 0xbb,
	}
	if !bytes.Equal(w.Bytes(), want) {
		t.Fatalf("encoded bytes mismatch:\n got %x\nwant %x", w.Bytes(), want)
	}

	r := NewReader(w.Bytes())
	if got, err := r.ReadU16(); err != nil || got != 0x0102 {
		t.Fatalf("ReadU16 got %x err %v", got, err)
	}
	if got, err := r.ReadU32(); err != nil || got != 0x03040506 {
		t.Fatalf("ReadU32 got %x err %v", got, err)
	}
	if got, err := r.ReadU64(); err != nil || got != 0x0708090a0b0c0d0e {
		t.Fatalf("ReadU64 got %x err %v", got, err)
	}
	gotVec, err := r.ReadByteVec(2)
	if err != nil {
		t.Fatalf("ReadByteVec: %v", err)
	}
	if !bytes.Equal(gotVec, []byte{0xaa, 0xbb}) {
		t.Fatalf("byte vec mismatch: %x", gotVec)
	}
	if err := r.EnsureEOF(); err != nil {
		t.Fatalf("EnsureEOF: %v", err)
	}
}

func TestReaderRejectsOversizedByteVec(t *testing.T) {
	w := NewWriter(0)
	w.WriteByteVec([]byte{0xaa, 0xbb})

	r := NewReader(w.Bytes())
	if _, err := r.ReadByteVec(1); err == nil {
		t.Fatalf("expected byte vector limit error")
	}
}
