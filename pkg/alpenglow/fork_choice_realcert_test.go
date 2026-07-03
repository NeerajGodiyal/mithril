package alpenglow

import (
	"testing"

	"github.com/gagliardetto/solana-go"
)

// Fork-choice driven by REAL BLS certificates. The ChainTracker unit tests set
// SignatureVerified/StakeVerified by hand; these instead sign genuine aggregate
// BLS certificates, run them through the real CertificateVerifier, and only feed
// the verified result to the tracker — so pick / ditch / halt is exercised end to
// end with real crypto, the way a live fork would drive it.

// realVerifiedCert signs nothing itself; it runs cert through the real verifier and
// fails the test unless the signature + stake actually verify.
func realVerifiedCert(t *testing.T, set ValidatorSet, cert Certificate) Certificate {
	t.Helper()
	verified, result, err := verifyCertificateWithSet(set, cert, true)
	if err != nil {
		t.Fatalf("verify %s cert: %v", cert.Type, err)
	}
	if !verified.SignatureVerified || !verified.StakeVerified {
		t.Fatalf("%s cert failed real verification: %+v (result %+v)", cert.Type, verified, result)
	}
	return verified
}

func observeCert(t *testing.T, tracker *ChainTracker, cert Certificate) {
	t.Helper()
	if _, err := tracker.ObserveCertificate(cert); err != nil {
		t.Fatalf("observe %s cert: %v", cert.Type, err)
	}
}

// Pick: real notarize + finalize certs for block A resolve the slot to A — the
// certified branch — through the real verifier, not a hand-set flag.
func TestForkChoicePicksCertifiedBranchRealBLS(t *testing.T) {
	set, keys := testBLSValidatorSet(100, 40, 35, 25)
	const slot = uint64(42)
	blockA := solana.Hash{0xA}
	bitmap := testSignerBitmapBase2(3, 0, 1) // validators 0+1 = 75% stake (>60%)

	notar := realVerifiedCert(t, set, Certificate{
		Type: CertificateNotarize, Slot: slot, BlockHash: blockA,
		Signature: testBLSSignature(t, []testBLSVoteSignature{
			{Vote: NewNotarizationVote(slot, blockA), Key: keys[0]},
			{Vote: NewNotarizationVote(slot, blockA), Key: keys[1]},
		}),
		Bitmap: bitmap,
	})
	final := realVerifiedCert(t, set, Certificate{
		Type: CertificateFinalize, Slot: slot,
		Signature: testBLSSignature(t, []testBLSVoteSignature{
			{Vote: NewFinalizationVote(slot), Key: keys[0]},
			{Vote: NewFinalizationVote(slot), Key: keys[1]},
		}),
		Bitmap: bitmap,
	})

	tracker := NewChainTracker()
	observeCert(t, tracker, notar)
	observeCert(t, tracker, final)

	dec, ok := tracker.NextDecision(slot - 1)
	if !ok || dec.Kind != ChainDecisionKindBlock || dec.Slot != slot {
		t.Fatalf("want block decision at slot %d, got %+v (ok=%v)", slot, dec, ok)
	}
	if dec.Block.Hash != blockA {
		t.Fatalf("fork-choice picked %s, want certified block %s", dec.Block.Hash, blockA)
	}
	t.Logf("PROOF: real notarize+finalize BLS certs -> slot %d resolves to certified block 0x%X", slot, blockA[0])
}

// Ditch/halt: two DIFFERENT blocks each carry a genuinely-signed notarize cert for
// the same slot (equivocation). The tracker must surface a conflict, never silently
// pick one — the safety case a real fork would hit.
func TestForkChoiceHaltsOnEquivocationRealBLS(t *testing.T) {
	set, keys := testBLSValidatorSet(100, 40, 35, 25)
	const slot = uint64(55)
	blockA := solana.Hash{0xA}
	blockB := solana.Hash{0xB}
	bitmap := testSignerBitmapBase2(3, 0, 1)

	notarA := realVerifiedCert(t, set, Certificate{
		Type: CertificateNotarize, Slot: slot, BlockHash: blockA,
		Signature: testBLSSignature(t, []testBLSVoteSignature{
			{Vote: NewNotarizationVote(slot, blockA), Key: keys[0]},
			{Vote: NewNotarizationVote(slot, blockA), Key: keys[1]},
		}),
		Bitmap: bitmap,
	})
	notarB := realVerifiedCert(t, set, Certificate{
		Type: CertificateNotarize, Slot: slot, BlockHash: blockB,
		Signature: testBLSSignature(t, []testBLSVoteSignature{
			{Vote: NewNotarizationVote(slot, blockB), Key: keys[0]},
			{Vote: NewNotarizationVote(slot, blockB), Key: keys[1]},
		}),
		Bitmap: bitmap,
	})

	tracker := NewChainTracker()
	observeCert(t, tracker, notarA)
	observeCert(t, tracker, notarB)

	dec, ok := tracker.NextDecision(slot - 1)
	if !ok || dec.Kind != ChainDecisionKindConflict {
		t.Fatalf("two certified blocks for slot %d must be a conflict, got %+v (ok=%v)", slot, dec, ok)
	}
	t.Logf("PROOF: two real-signed notarize certs (0x%X vs 0x%X) -> conflict, no silent pick", blockA[0], blockB[0])
}

// A tampered signature must not drive fork-choice: real verification rejects it, so
// a strict tracker never lets it produce a decision.
func TestForkChoiceRejectsTamperedCertRealBLS(t *testing.T) {
	set, keys := testBLSValidatorSet(100, 40, 35, 25)
	const slot = uint64(60)
	blockA := solana.Hash{0xA}

	sig := testBLSSignature(t, []testBLSVoteSignature{
		{Vote: NewNotarizationVote(slot, blockA), Key: keys[0]},
		{Vote: NewNotarizationVote(slot, blockA), Key: keys[1]},
	})
	sig[10] ^= 0xFF // corrupt the aggregate signature

	cert := Certificate{
		Type: CertificateNotarize, Slot: slot, BlockHash: blockA,
		Signature: sig, Bitmap: testSignerBitmapBase2(3, 0, 1),
	}
	verified, _, err := verifyCertificateWithSet(set, cert, true)
	if err == nil && verified.SignatureVerified {
		t.Fatal("tampered signature must not verify")
	}

	tracker := NewChainTrackerWithConfig(ChainConfig{
		RequireVerifiedCertificates:      true,
		RequireStakeVerifiedCertificates: true,
	})
	_, _ = tracker.ObserveCertificate(verified) // untrusted -> ignored
	if _, ok := tracker.NextDecision(slot - 1); ok {
		t.Fatal("a tampered/unverified cert must not drive a fork-choice decision")
	}
	t.Logf("PROOF: tampered BLS sig fails real verification -> no decision (verification gates pick/ditch)")
}
