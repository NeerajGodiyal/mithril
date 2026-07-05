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

func TestVerifyRewardCertificateAllowsLowStakeFraction(t *testing.T) {
	set, keys := testBLSValidatorSet(100, 40, 35, 25)
	cert := Certificate{
		Type:   CertificateSkip,
		Slot:   88,
		Bitmap: testSignerBitmapBase2(3, 2),
		Signature: testBLSSignature(t, []testBLSVoteSignature{
			{Vote: NewSkipVote(88), Key: keys[2]},
		}),
	}

	if err := verifyRewardCertificateWithSet(set, cert); err != nil {
		t.Fatalf("verify reward certificate: %v", err)
	}

	_, _, err := verifyCertificateWithSet(set, cert, true)
	if err == nil {
		t.Fatalf("expected full chain verification to reject low-stake skip cert")
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

func TestVerifyVoteMessageSignatureRustFixture(t *testing.T) {
	pubkeyCompressed := mustHex48(t, "82423626eecef17f3e3d53b6f8254c946d1fc0451c1dd6551ee9aea7b8cbca624c3a4ee8d1e5a9be5ef83a407d30ea44")
	signature := mustHex(t, "0035fa868927897593efb1e3385b51d2df47dd9daca035a3f386509a4df49050bf51f5cde5b2b2fe85958d056819fcf10e77254213e3d16053418356f8d4a1f593e2739cac2fd1632bb0ea62fb97591e73002914f7b948d4cd1e4ffc3eb146ab0f9f9fd5809fcce6134a20d7f121ab57f6180d1025c4ad8d6ed50087b902643fe1c50791dab65076216d90984cb4a8e61873f9ff13234d682542a5cec62f7364b59f313bd8f9831f3df4342e94a109cf8ce640defc6477033168318ca063009a")
	var blockHash solana.Hash
	for i := range blockHash {
		blockHash[i] = 0x42 + byte(i)
	}
	set := ValidatorSet{
		Epoch:      52,
		TotalStake: 100,
		Validators: []ValidatorStake{{
			Rank:                0,
			BlsPubkeyCompressed: pubkeyCompressed,
			Stake:               100,
		}},
	}

	_, err := verifyVoteMessageWithSet(set, VoteMessage{
		Vote:      NewNotarizationVote(0x2223_2425_2627_2829, blockHash),
		Signature: signature,
		Rank:      0,
	})
	if err != nil {
		t.Fatalf("verify Rust-produced vote signature: %v", err)
	}
}

func TestVerifyVoteMessageSignatureLiveVotorFixture(t *testing.T) {
	pubkeyCompressed := mustHex48(t, "afa97fe11e43589b2b4f96e0b505d2a0a52a2c413df83019a3af05ba5d8bbac06ba7b6a0ba7892180354748a74d81b96")
	signature := mustHex(t, "034ebb8bd92a5abfdd071e4e037c8c59513dd1f0c2b11b6139f54abb76f39234dbdce082324cd8a495c0f1b010549bc00df5e2f6a8405b3b1edf808c5e467370175f3b343d751a940e9ff4398f213c2a458f41f0b85062ba641f520a282f62c30a401bb44f8b75c79a38c059ec2f9997837c5f1fbb86a9be127f33ce21a8c42985eccfbabeb1334fa54c382c0dccd4650dda79e7c835c6e0bcc2452819c22ba6bba85dba51ea84c9d59aa96239ee8c15773aa705159f663b4daf92a89a0dea92")
	set := ValidatorSet{
		Epoch:      3,
		TotalStake: 100,
		Validators: []ValidatorStake{{
			Rank:                0,
			BlsPubkeyCompressed: pubkeyCompressed,
			Stake:               100,
		}},
	}

	_, err := verifyVoteMessageWithSet(set, VoteMessage{
		Vote:      NewFinalizationVote(208215),
		Signature: signature,
		Rank:      0,
	})
	if err != nil {
		t.Fatalf("verify live Votor vote signature: %v", err)
	}
}

func TestDecodeSignerStoreBitmapRejectsCorruptPayload(t *testing.T) {
	_, err := DecodeSignerStoreBitmap([]byte{0, 9, 0, 1}, 8)
	if err == nil {
		t.Fatalf("expected corrupt payload error")
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
	t.Helper()
	var aggregate bls12381.G2Affine
	aggregate.SetInfinity()
	for _, signature := range signatures {
		payload, err := EncodeVote(signature.Vote)
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
