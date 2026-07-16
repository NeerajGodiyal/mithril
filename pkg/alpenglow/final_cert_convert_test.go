package alpenglow

import (
	"testing"

	bls12381 "github.com/Overclock-Validator/gnark-crypto/ecc/bls12-381"
	"github.com/gagliardetto/solana-go"
)

// compressG2 turns a 192-byte uncompressed aggregate signature into the 96-byte
// compressed form a block footer carries.
func compressG2(t *testing.T, sig192 []byte) []byte {
	t.Helper()
	var p bls12381.G2Affine
	if _, err := p.SetBytes(sig192); err != nil {
		t.Fatalf("parse 192-byte sig: %v", err)
	}
	b := p.Bytes()
	return b[:]
}

// A footer FinalCertificate with a notar aggregate must convert into a Notarize +
// Finalize pair that both pass the REAL verifier — proving decompress (96→192) +
// conversion end-to-end against genuine BLS material.
func TestFinalCertToCertificatesSlowPathVerifies(t *testing.T) {
	set, keys := testBLSValidatorSet(100, 40, 35, 25)
	var blockHash solana.Hash
	blockHash[0] = 9
	const slot = uint64(77)

	notar96 := compressG2(t, testBLSSignature(t, []testBLSVoteSignature{
		{Vote: NewNotarizationVote(slot, blockHash), Key: keys[0]},
		{Vote: NewNotarizationVote(slot, blockHash), Key: keys[1]},
	}))
	final96 := compressG2(t, testBLSSignature(t, []testBLSVoteSignature{
		{Vote: NewFinalizationVote(slot), Key: keys[0]},
		{Vote: NewFinalizationVote(slot), Key: keys[1]},
	}))
	bitmap := testSignerBitmapBase2(3, 0, 1)

	certs, err := FinalCertToCertificates(slot, blockHash, final96, bitmap, notar96, bitmap)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(certs) != 2 || certs[0].Type != CertificateNotarize || certs[1].Type != CertificateFinalize {
		t.Fatalf("slow path must yield [Notarize, Finalize], got %+v", certs)
	}
	// Finalize is slot-scoped: no block hash (matches HasBlock + QUIC dedup key).
	if !certs[1].BlockHash.IsZero() {
		t.Errorf("Finalize cert must carry no block hash, got %s", certs[1].BlockHash)
	}
	for _, c := range certs {
		verified, result, err := verifyCertificateWithSet(set, c, true)
		if err != nil {
			t.Fatalf("verify %s: %v", c.Type, err)
		}
		if !verified.SignatureVerified || !verified.StakeVerified {
			t.Fatalf("%s cert did not verify: %+v (result %+v)", c.Type, verified, result)
		}
	}
}

// Fast path: no notar aggregate → a single FinalizeFast cert that verifies.
func TestFinalCertToCertificatesFastPathVerifies(t *testing.T) {
	set, keys := testBLSValidatorSet(100, 40, 35, 25)
	var blockHash solana.Hash
	blockHash[0] = 7
	const slot = uint64(88)

	// FinalizeFast needs 80% — sign with all three validators (100%).
	final96 := compressG2(t, testBLSSignature(t, []testBLSVoteSignature{
		{Vote: NewNotarizationVote(slot, blockHash), Key: keys[0]},
		{Vote: NewNotarizationVote(slot, blockHash), Key: keys[1]},
		{Vote: NewNotarizationVote(slot, blockHash), Key: keys[2]},
	}))
	bitmap := testSignerBitmapBase2(3, 0, 1, 2)

	certs, err := FinalCertToCertificates(slot, blockHash, final96, bitmap, nil, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(certs) != 1 || certs[0].Type != CertificateFinalizeFast {
		t.Fatalf("fast path must yield [FinalizeFast], got %+v", certs)
	}
	verified, _, err := verifyCertificateWithSet(set, certs[0], true)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !verified.SignatureVerified || !verified.StakeVerified {
		t.Fatalf("FinalizeFast cert did not verify: %+v", verified)
	}
}

// A corrupt 96-byte signature must fail decompression, not silently pass.
func TestFinalCertToCertificatesRejectsBadSignature(t *testing.T) {
	var blockHash solana.Hash
	bad := make([]byte, 96)
	for i := range bad {
		bad[i] = 0xFF
	}
	if _, err := FinalCertToCertificates(5, blockHash, bad, []byte{1}, nil, nil); err == nil {
		t.Fatal("expected decompress error on corrupt signature")
	}
}
func TestFooterFinalizeFastReachesFinality(t *testing.T) {
	set, keys := testBLSValidatorSet(100, 40, 35, 25)
	var blockHash solana.Hash
	blockHash[0] = 0xB
	const slot = uint64(120)

	final96 := compressG2(t, testBLSSignature(t, []testBLSVoteSignature{
		{Vote: NewNotarizationVote(slot, blockHash), Key: keys[0]},
		{Vote: NewNotarizationVote(slot, blockHash), Key: keys[1]},
		{Vote: NewNotarizationVote(slot, blockHash), Key: keys[2]},
	}))
	bitmap := testSignerBitmapBase2(3, 0, 1, 2)

	certs, err := FinalCertToCertificates(slot, blockHash, final96, bitmap, nil, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	tracker := NewChainTracker()
	for _, c := range certs {
		verified, _, err := verifyCertificateWithSet(set, c, true)
		if err != nil {
			t.Fatalf("verify %s: %v", c.Type, err)
		}
		if _, err := tracker.ObserveCertificate(verified); err != nil {
			t.Fatalf("observe %s: %v", c.Type, err)
		}
		if verified.Type == CertificateFinalizeFast {
			block, ok := verified.Block()
			if !ok {
				t.Fatal("verified finalize-fast certificate has no block")
			}
			if err := tracker.ObserveFinalized(block, verified.Type); err != nil {
				t.Fatalf("apply pool finalization: %v", err)
			}
		}
	}

	snap := tracker.Snapshot()
	if snap.LatestDirectFinalizedBlock.Slot != slot {
		t.Fatalf("finalized slot = %d, want %d (footer FinalizeFast did not finalize)",
			snap.LatestDirectFinalizedBlock.Slot, slot)
	}
	if !snap.LatestDirectFinalizedBlock.Hash.Equals(blockHash) {
		t.Fatalf("finalized block hash mismatch: got %s", snap.LatestDirectFinalizedBlock.Hash)
	}
}
