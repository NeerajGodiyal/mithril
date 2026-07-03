package turbine

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/gagliardetto/solana-go"
)

// buildVotesAggregate: signature(96) + bitmap(u16 LE len + bytes).
func buildVotesAggregate(sigFill byte, bitmap []byte) []byte {
	out := make([]byte, 96)
	for i := range out {
		out[i] = sigFill
	}
	var lenb [2]byte
	binary.LittleEndian.PutUint16(lenb[:], uint16(len(bitmap)))
	out = append(out, lenb[:]...)
	return append(out, bitmap...)
}

// buildFinalCert: slot(8 LE) + block_id(32) + final_aggregate + notar Option.
func buildFinalCert(slot uint64, blockID [32]byte, notar bool) []byte {
	out := make([]byte, 8)
	binary.LittleEndian.PutUint64(out, slot)
	out = append(out, blockID[:]...)
	out = append(out, buildVotesAggregate(0x11, []byte{1, 2, 3})...)
	if notar {
		out = append(out, 1) // Some
		out = append(out, buildVotesAggregate(0x22, []byte{4, 5})...)
	} else {
		out = append(out, 0) // None
	}
	return out
}

func TestUnmarshalFinalCertificateRoundTrip(t *testing.T) {
	var blockID [32]byte
	for i := range blockID {
		blockID[i] = byte(i + 1)
	}

	for _, notar := range []bool{true, false} {
		blob := buildFinalCert(430276100, blockID, notar)

		// EncodedLen must equal the true blob length (this is what stops the footer
		// option from swallowing trailing reward certs).
		n, err := FinalCertificateEncodedLen(blob)
		if err != nil {
			t.Fatalf("notar=%v: EncodedLen: %v", notar, err)
		}
		if n != len(blob) {
			t.Fatalf("notar=%v: EncodedLen=%d, want %d", notar, n, len(blob))
		}

		// Trailing bytes (simulating reward certs after final_cert) must NOT be
		// consumed — EncodedLen still returns the cert's own length.
		withTrailer := append(append([]byte(nil), blob...), 0xDE, 0xAD, 0xBE, 0xEF)
		if n2, err := FinalCertificateEncodedLen(withTrailer); err != nil || n2 != len(blob) {
			t.Fatalf("notar=%v: EncodedLen with trailer = %d (err %v), want %d — it swallowed trailing bytes", notar, n2, err, len(blob))
		}

		cert, err := UnmarshalFinalCertificate(blob)
		if err != nil {
			t.Fatalf("notar=%v: Unmarshal: %v", notar, err)
		}
		if cert.Slot != 430276100 {
			t.Errorf("slot=%d, want 430276100", cert.Slot)
		}
		if !bytes.Equal(cert.BlockID[:], blockID[:]) {
			t.Errorf("block id mismatch")
		}
		if !bytes.Equal(cert.FinalAggregate.Bitmap, []byte{1, 2, 3}) {
			t.Errorf("final bitmap = %v, want [1 2 3]", cert.FinalAggregate.Bitmap)
		}
		if cert.FinalAggregate.Signature[0] != 0x11 {
			t.Errorf("final sig fill = %x, want 0x11", cert.FinalAggregate.Signature[0])
		}
		if notar {
			if cert.NotarAggregate == nil {
				t.Fatal("notar aggregate missing")
			}
			if !bytes.Equal(cert.NotarAggregate.Bitmap, []byte{4, 5}) {
				t.Errorf("notar bitmap = %v, want [4 5]", cert.NotarAggregate.Bitmap)
			}
			if cert.NotarAggregate.Signature[0] != 0x22 {
				t.Errorf("notar sig fill = %x, want 0x22", cert.NotarAggregate.Signature[0])
			}
		} else if cert.NotarAggregate != nil {
			t.Error("notar aggregate should be nil for None")
		}
	}
}

func TestUnmarshalFinalCertificateRejectsTruncated(t *testing.T) {
	if _, err := UnmarshalFinalCertificate([]byte{1, 2, 3}); err == nil {
		t.Fatal("expected error on truncated final cert")
	}
	if _, err := FinalCertificateEncodedLen([]byte{1, 2, 3}); err == nil {
		t.Fatal("expected error on truncated final cert length")
	}
}

// The footer stores final_cert BEFORE the reward certs. This proves the final_cert
// is length-bounded (FinalCertificateEncodedLen) so the trailing reward certs are
// NOT swallowed — the exact bug that treating final_cert as opaque-to-EOF caused.
func TestFooterFinalCertDoesNotSwallowRewardCerts(t *testing.T) {
	var blockID [32]byte
	for i := range blockID {
		blockID[i] = byte(i + 1)
	}
	finalCert := buildFinalCert(430276100, blockID, true)

	// Minimal valid reward certs: slot(8) [+blockid(32) for notar] + sig(96) + bitmap(shortU16=0).
	skip := make([]byte, 8+96+1) // last byte = shortU16 len 0
	skip[0] = 0x77
	notar := make([]byte, 8+32+96+1)
	notar[0] = 0x88

	footer := BlockFooter{
		BankHash:        solana.HashFromBytes(blockID[:]),
		BlockUserAgent:  []byte("mithril"),
		BlockFinalCert:  finalCert,
		SkipRewardCert:  skip,
		NotarRewardCert: notar,
	}
	raw, err := MarshalBlockComponent(NewBlockFooter(footer))
	if err != nil {
		t.Fatalf("marshal footer: %v", err)
	}
	comp, err := UnmarshalBlockComponent(raw)
	if err != nil {
		t.Fatalf("unmarshal footer: %v", err)
	}
	got := comp.Marker.Footer

	if !bytes.Equal(got.BlockFinalCert, finalCert) {
		t.Fatalf("final cert corrupted: got %d bytes, want %d", len(got.BlockFinalCert), len(finalCert))
	}
	if !bytes.Equal(got.SkipRewardCert, skip) {
		t.Fatalf("skip reward cert swallowed/corrupted: got %d bytes, want %d", len(got.SkipRewardCert), len(skip))
	}
	if !bytes.Equal(got.NotarRewardCert, notar) {
		t.Fatalf("notar reward cert swallowed/corrupted: got %d bytes, want %d", len(got.NotarRewardCert), len(notar))
	}
	// And the bounded final cert still decodes.
	if _, err := UnmarshalFinalCertificate(got.BlockFinalCert); err != nil {
		t.Fatalf("bounded final cert no longer decodes: %v", err)
	}
}
