package alpenglow

import (
	"encoding/hex"
	"math/big"
	"testing"

	bls12381 "github.com/Overclock-Validator/gnark-crypto/ecc/bls12-381"
	"github.com/gagliardetto/solana-go"
)

func TestVerifyCertificateStakeBase2(t *testing.T) {
	set := testValidatorSet(100, 40, 10, 25)
	cert := Certificate{
		Type:   CertificateSkip,
		Slot:   77,
		Bitmap: testSignerBitmapBase2(3, 0, 2),
	}

	verified, result, err := verifyCertificateStakeWithSet(set, cert)
	if err != nil {
		t.Fatalf("verify certificate stake: %v", err)
	}
	if !verified.StakeVerified || verified.IncludedStake != 65 || verified.TotalStake != 100 {
		t.Fatalf("unexpected verified certificate: %+v", verified)
	}
	if !result.StakeVerified || result.SignerCount != 2 || result.IncludedStake != 65 || result.TotalStake != 100 {
		t.Fatalf("unexpected verify result: %+v", result)
	}
}

func TestVerifyCertificateStakeRejectsInsufficientStake(t *testing.T) {
	set := testValidatorSet(100, 40, 10, 25)
	cert := Certificate{
		Type:   CertificateSkip,
		Slot:   77,
		Bitmap: testSignerBitmapBase2(3, 0, 1),
	}

	verified, result, err := verifyCertificateStakeWithSet(set, cert)
	if err == nil {
		t.Fatalf("expected insufficient stake error")
	}
	if verified.StakeVerified {
		t.Fatalf("insufficient cert should not be marked stake verified: %+v", verified)
	}
	if result.StakeVerified || result.IncludedStake != 50 || result.TotalStake != 100 {
		t.Fatalf("unexpected failed verify result: %+v", result)
	}
}

func TestVerifyCertificateStakeBase3UsesUnionStake(t *testing.T) {
	set := testValidatorSet(100, 40, 10, 25)
	cert := Certificate{
		Type:   CertificateSkip,
		Slot:   77,
		Bitmap: testSignerBitmapBase3(3, map[int]bool{0: true}, map[int]bool{2: true}),
	}

	verified, result, err := verifyCertificateStakeWithSet(set, cert)
	if err != nil {
		t.Fatalf("verify base3 certificate stake: %v", err)
	}
	if !verified.StakeVerified || verified.IncludedStake != 65 {
		t.Fatalf("unexpected verified certificate: %+v", verified)
	}
	if result.SignerCount != 2 || result.IncludedStake != 65 {
		t.Fatalf("unexpected verify result: %+v", result)
	}
}

func TestVerifyCertificateSignatureBase2(t *testing.T) {
	set, keys := testBLSValidatorSet(100, 40, 35, 25)
	var blockHash solana.Hash
	blockHash[0] = 9
	cert := Certificate{
		Type:      CertificateNotarize,
		Slot:      77,
		BlockHash: blockHash,
		Bitmap:    testSignerBitmapBase2(3, 0, 1),
		Signature: testBLSSignature(t, []testBLSVoteSignature{
			{Vote: NewNotarizationVote(77, blockHash), Key: keys[0]},
			{Vote: NewNotarizationVote(77, blockHash), Key: keys[1]},
		}),
	}

	verified, result, err := verifyCertificateWithSet(set, cert, true)
	if err != nil {
		t.Fatalf("verify certificate signature: %v", err)
	}
	if !verified.StakeVerified || !verified.SignatureVerified {
		t.Fatalf("expected fully verified certificate: %+v", verified)
	}
	if !result.StakeVerified || !result.SignatureVerified || result.IncludedStake != 75 {
		t.Fatalf("unexpected verify result: %+v", result)
	}
}

func TestVerifyCertificateSignatureRejectsTamperedPayload(t *testing.T) {
	set, keys := testBLSValidatorSet(100, 40, 35, 25)
	var signedHash solana.Hash
	signedHash[0] = 9
	var tamperedHash solana.Hash
	tamperedHash[0] = 10
	cert := Certificate{
		Type:      CertificateNotarize,
		Slot:      77,
		BlockHash: tamperedHash,
		Bitmap:    testSignerBitmapBase2(3, 0, 1),
		Signature: testBLSSignature(t, []testBLSVoteSignature{
			{Vote: NewNotarizationVote(77, signedHash), Key: keys[0]},
			{Vote: NewNotarizationVote(77, signedHash), Key: keys[1]},
		}),
	}

	verified, result, err := verifyCertificateWithSet(set, cert, true)
	if err == nil {
		t.Fatalf("expected signature verification failure")
	}
	if !verified.StakeVerified || verified.SignatureVerified {
		t.Fatalf("unexpected failed certificate flags: %+v", verified)
	}
	if !result.StakeVerified || result.SignatureVerified {
		t.Fatalf("unexpected failed verify result: %+v", result)
	}
}

func TestVerifyCertificateSignatureBase3MixedVotes(t *testing.T) {
	set, keys := testBLSValidatorSet(100, 40, 35, 25)
	cert := Certificate{
		Type:   CertificateSkip,
		Slot:   88,
		Bitmap: testSignerBitmapBase3(3, map[int]bool{0: true}, map[int]bool{1: true}),
		Signature: testBLSSignature(t, []testBLSVoteSignature{
			{Vote: NewSkipVote(88), Key: keys[0]},
			{Vote: NewSkipFallbackVote(88), Key: keys[1]},
		}),
	}

	verified, result, err := verifyCertificateWithSet(set, cert, true)
	if err != nil {
		t.Fatalf("verify base3 mixed signature: %v", err)
	}
	if !verified.StakeVerified || !verified.SignatureVerified {
		t.Fatalf("expected fully verified certificate: %+v", verified)
	}
	if result.SignerCount != 2 || result.IncludedStake != 75 || !result.SignatureVerified {
		t.Fatalf("unexpected verify result: %+v", result)
	}
}

func TestVerifyVoteMessageSignature(t *testing.T) {
	set, keys := testBLSValidatorSet(100, 40, 35, 25)
	_, err := verifyVoteMessageWithSet(set, VoteMessage{
		Vote:      NewFinalizationVote(91),
		Signature: testBLSSignature(t, []testBLSVoteSignature{{Vote: NewFinalizationVote(91), Key: keys[1]}}),
		Rank:      1,
	})
	if err != nil {
		t.Fatalf("verify vote message: %v", err)
	}

	_, err = verifyVoteMessageWithSet(set, VoteMessage{
		Vote:      NewFinalizationVote(92),
		Signature: testBLSSignature(t, []testBLSVoteSignature{{Vote: NewFinalizationVote(91), Key: keys[1]}}),
		Rank:      1,
	})
	if err == nil {
		t.Fatalf("expected vote signature verification failure for tampered slot")
	}
}

func TestVerifyVoteMessageBindsConfiguredShredVersion(t *testing.T) {
	set, keys := testBLSValidatorSet(100, 100)
	verifier := NewCertificateVerifier()
	verifier.SetShredVersion(7)
	if err := verifier.SetValidatorSet(set); err != nil {
		t.Fatalf("set validator set: %v", err)
	}
	installed, ok := verifier.ValidatorSetForEpoch(set.Epoch)
	if !ok || len(installed.parsedPubkeys) != len(set.Validators) {
		t.Fatalf("installed validator keys were not prevalidated: found=%v parsed=%d validators=%d", ok, len(installed.parsedPubkeys), len(set.Validators))
	}

	vote := NewFinalizationVote(91)
	valid := VoteMessage{
		Vote:      vote,
		Signature: testBLSSignatureWithShredVersion(t, []testBLSVoteSignature{{Vote: vote, Key: keys[0]}}, 7),
		Rank:      0,
	}
	if _, err := verifier.VerifyVoteMessageForEpoch(set.Epoch, valid); err != nil {
		t.Fatalf("verify vote signed for configured shred version: %v", err)
	}

	wrongVersion := valid
	wrongVersion.Signature = testBLSSignatureWithShredVersion(t, []testBLSVoteSignature{{Vote: vote, Key: keys[0]}}, 0)
	if _, err := verifier.VerifyVoteMessageForEpoch(set.Epoch, wrongVersion); err == nil {
		t.Fatal("expected vote signed for another shred version to fail")
	}
}

// TestVerifyRealClusterBlstSignature pins gnark's hash-to-curve + pairing to real
// blst crypto. The pubkey + signature were produced by a solana-bls 3.0.0 cluster's
// blst over the message "mithril-hash-test" — so it pins the matching RO_NUL_ DST
// (not the production default); if gnark's hash-to-curve drifts from blst, it fails.
func TestVerifyRealClusterBlstSignature(t *testing.T) {
	// This vector is 3.0.0 (RO_NUL_) material; select that DST regardless of default.
	defer func(saved string) { blsHashToPointDST = saved }(blsHashToPointDST)
	blsHashToPointDST = "BLS_SIG_BLS12381G2_XMD:SHA-256_SSWU_RO_NUL_"
	pk := mustHex48(t, "95f61236d9fc1ea7bdbfd44109e7223f5f20769863e191fcf50e03cb0e3408dbf2ed74576d259be8acac3226292457d2")
	sig := mustHex(t, "8ba66c044814b8ceacf89647d52f3a0724b456280f826e0eb691010113f271bae599b42026183805f9ed4ade351a183019d701c0a43f033f63448a8d8b8c0ee6bd3287b3633a99348aaff105c0a08f2526c847569dfb07e1989d4d73b46529a1")

	var pubkey bls12381.G1Affine
	if _, err := pubkey.SetBytes(pk[:]); err != nil {
		t.Fatalf("parse pubkey: %v", err)
	}
	var signature bls12381.G2Affine
	if _, err := signature.SetBytes(sig); err != nil {
		t.Fatalf("parse signature: %v", err)
	}
	if err := verifyBLSSignature(pubkey, []byte("mithril-hash-test"), signature); err != nil {
		t.Fatalf("gnark failed to verify a real cluster blst signature (DST/hash-to-curve drift?): %v", err)
	}
}

func TestDecodeSignerStoreBitmapRejectsCorruptPayload(t *testing.T) {
	_, err := DecodeSignerStoreBitmap([]byte{0, 9, 0, 1}, 8)
	if err == nil {
		t.Fatalf("expected corrupt payload error")
	}
}

func TestCertificateBitmapCapacityIsSeparateFromVATCap(t *testing.T) {
	if MaximumVATValidators != 2000 || CertificateBitmapCapacity != 4096 {
		t.Fatalf("unexpected validator limits: VAT=%d bitmap=%d", MaximumVATValidators, CertificateBitmapCapacity)
	}
	base := make([]bool, CertificateBitmapCapacity)
	base[0] = true
	encoded, err := EncodeSignerStoreBitmap(SignerBitmap{
		Encoding: SignerBitmapBase2,
		Length:   CertificateBitmapCapacity,
		Base:     base,
	})
	if err != nil {
		t.Fatalf("encode capacity bitmap: %v", err)
	}
	if _, err := DecodeSignerStoreBitmap(encoded, CertificateBitmapCapacity); err != nil {
		t.Fatalf("decode legal capacity bitmap: %v", err)
	}
	if _, err := DecodeSignerStoreBitmap(encoded, MaximumVATValidators); err == nil {
		t.Fatal("VAT cap unexpectedly accepted as the certificate wire capacity")
	}

	set := testValidatorSet(100, 100)
	cert := Certificate{Type: CertificateSkip, Slot: 77, Bitmap: encoded}
	verified, _, err := verifyCertificateStakeWithSet(set, cert)
	if err != nil {
		t.Fatalf("zero bitmap tail should verify against a smaller active set: %v", err)
	}
	if !verified.StakeVerified || verified.IncludedStake != 100 {
		t.Fatalf("unexpected verified certificate: %+v", verified)
	}
}

func testValidatorSet(totalStake uint64, stakes ...uint64) ValidatorSet {
	validators := make([]ValidatorStake, len(stakes))
	for i, stake := range stakes {
		var voteAcct solana.PublicKey
		voteAcct[0] = byte(i + 1)
		validators[i] = ValidatorStake{
			Rank:        uint16(i),
			VoteAccount: voteAcct,
			Stake:       stake,
		}
	}
	return ValidatorSet{Epoch: 4, Validators: validators, TotalStake: totalStake}
}

func testBLSValidatorSet(totalStake uint64, stakes ...uint64) (ValidatorSet, []*big.Int) {
	validators := make([]ValidatorStake, len(stakes))
	keys := make([]*big.Int, len(stakes))
	for i, stake := range stakes {
		key := big.NewInt(int64(i + 3))
		keys[i] = key
		var pubkey bls12381.G1Affine
		pubkey.ScalarMultiplicationBase(key)
		compressed := pubkey.Bytes()
		uncompressed := pubkey.RawBytes()
		var voteAcct solana.PublicKey
		voteAcct[0] = byte(i + 1)
		validators[i] = ValidatorStake{
			Rank:                  uint16(i),
			VoteAccount:           voteAcct,
			BlsPubkeyCompressed:   compressed,
			BlsPubkeyUncompressed: uncompressed,
			Stake:                 stake,
		}
	}
	return ValidatorSet{Epoch: 4, Validators: validators, TotalStake: totalStake}, keys
}

type testBLSVoteSignature struct {
	Vote Vote
	Key  *big.Int
}

func testBLSSignature(t *testing.T, signatures []testBLSVoteSignature) []byte {
	return testBLSSignatureWithShredVersion(t, signatures, 0)
}

func testBLSSignatureWithShredVersion(t *testing.T, signatures []testBLSVoteSignature, shredVersion uint16) []byte {
	t.Helper()
	var aggregate bls12381.G2Affine
	aggregate.SetInfinity()
	for _, signature := range signatures {
		payload, err := EncodeVotePayloadToSign(signature.Vote, shredVersion)
		if err != nil {
			t.Fatalf("encode vote: %v", err)
		}
		message, err := bls12381.HashToG2(payload, []byte(blsHashToPointDST))
		if err != nil {
			t.Fatalf("hash vote to G2: %v", err)
		}
		var signed bls12381.G2Affine
		signed.ScalarMultiplication(&message, signature.Key)
		aggregate.Add(&aggregate, &signed)
	}
	raw := aggregate.RawBytes()
	return raw[:]
}

func testSignerBitmapBase2(length int, setBits ...int) []byte {
	payload := make([]byte, (length+7)/8)
	for _, bit := range setBits {
		payload[bit/8] |= 1 << uint(bit%8)
	}
	out := []byte{signerStoreVersionBase2, byte(length), byte(length >> 8)}
	return append(out, payload...)
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	out, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("decode hex fixture: %v", err)
	}
	return out
}

func mustHex48(t *testing.T, s string) [48]byte {
	t.Helper()
	raw := mustHex(t, s)
	if len(raw) != 48 {
		t.Fatalf("expected 48-byte fixture, got %d", len(raw))
	}
	var out [48]byte
	copy(out[:], raw)
	return out
}

func testSignerBitmapBase3(length int, base, fallback map[int]bool) []byte {
	payload := make([]byte, (length+base3SymbolsPerByte-1)/base3SymbolsPerByte)
	for chunk := range payload {
		start := chunk * base3SymbolsPerByte
		end := min(start+base3SymbolsPerByte, length)
		var block byte
		for i := end - 1; i >= start; i-- {
			var digit byte
			switch {
			case base[i]:
				digit = 1
			case fallback[i]:
				digit = 2
			}
			block = block*3 + digit
		}
		payload[chunk] = block
	}
	out := []byte{signerStoreVersionBase3, byte(length), byte(length >> 8)}
	return append(out, payload...)
}
