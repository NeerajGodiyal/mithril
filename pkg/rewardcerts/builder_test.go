package rewardcerts

import (
	"math/big"
	"testing"

	bls12381 "github.com/Overclock-Validator/gnark-crypto/ecc/bls12-381"
	"github.com/Overclock-Validator/mithril/pkg/alpenglow"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testBLSHashDST = "BLS_SIG_BLS12381G2_XMD:SHA-256_SSWU_RO_POP_"

func TestEncodeDecodeSkipRewardCertificateRoundTrip(t *testing.T) {
	cert := SkipRewardCertificate{
		Slot: 123,
		Bitmap: mustSignerBitmapBase2(t, 5, 0, 2),
	}
	copy(cert.Signature[:], bytesRepeat(96, 0xab))

	raw, err := EncodeSkipRewardCertificate(cert)
	require.NoError(t, err)
	decoded, err := DecodeSkipRewardCertificate(raw)
	require.NoError(t, err)
	assert.Equal(t, cert, decoded)
}

func TestEncodeDecodeNotarRewardCertificateRoundTrip(t *testing.T) {
	var blockID solana.Hash
	blockID[0] = 7
	cert := NotarRewardCertificate{
		Slot:    456,
		BlockID: blockID,
		Bitmap:  mustSignerBitmapBase2(t, 5, 1, 3),
	}
	copy(cert.Signature[:], bytesRepeat(96, 0xcd))

	raw, err := EncodeNotarRewardCertificate(cert)
	require.NoError(t, err)
	decoded, err := DecodeNotarRewardCertificate(raw)
	require.NoError(t, err)
	assert.Equal(t, cert, decoded)
}

func TestBuilderBuildsSkipRewardCertificate(t *testing.T) {
	keys := testBLSKeys(t, 5)
	slot := uint64(123)
	leaderSlot := slot + SlotsForReward

	builder := NewBuilder(DefaultBuilderConfig())
	vote := testSignedVote(t, alpenglow.NewSkipVote(slot), 0, keys[0])
	builder.AddVote(vote)

	certs := builder.BuildForLeaderSlot(leaderSlot)
	require.NotEmpty(t, certs.Skip)
	assert.Empty(t, certs.Notar)

	decoded, err := DecodeSkipRewardCertificate(certs.Skip)
	require.NoError(t, err)
	assert.Equal(t, slot, decoded.Slot)
	assertBitmapRanks(t, decoded.Bitmap, 5, 0)
	assertAggregateMatchesVotes(t, decoded.Signature[:], []alpenglow.VoteMessage{vote})
}

func TestBuilderBuildsNotarRewardCertificate(t *testing.T) {
	keys := testBLSKeys(t, 5)
	slot := uint64(123)
	leaderSlot := slot + SlotsForReward
	var blockID solana.Hash
	blockID[0] = 9

	builder := NewBuilder(DefaultBuilderConfig())
	var votes []alpenglow.VoteMessage
	for rank := 0; rank < 3; rank++ {
		vote := testSignedVote(t, alpenglow.NewNotarizationVote(slot, blockID), rank, keys[rank])
		builder.AddVote(vote)
		votes = append(votes, vote)
	}

	certs := builder.BuildForLeaderSlot(leaderSlot)
	require.NotEmpty(t, certs.Notar)
	assert.Empty(t, certs.Skip)

	decoded, err := DecodeNotarRewardCertificate(certs.Notar)
	require.NoError(t, err)
	assert.Equal(t, slot, decoded.Slot)
	assert.Equal(t, blockID, decoded.BlockID)
	assertBitmapRanks(t, decoded.Bitmap, 5, 0, 1, 2)
	assertAggregateMatchesVotes(t, decoded.Signature[:], votes)
}

func TestBuilderPicksNotarBlockWithMostVotes(t *testing.T) {
	keys := testBLSKeys(t, 5)
	slot := uint64(50)
	leaderSlot := slot + SlotsForReward

	var blockA, blockB solana.Hash
	blockA[0] = 1
	blockB[0] = 2

	builder := NewBuilder(DefaultBuilderConfig())
	for rank := 0; rank < 2; rank++ {
		builder.AddVote(testSignedVote(t, alpenglow.NewNotarizationVote(slot, blockA), rank, keys[rank]))
	}
	for rank := 2; rank < 5; rank++ {
		builder.AddVote(testSignedVote(t, alpenglow.NewNotarizationVote(slot, blockB), rank, keys[rank]))
	}

	decoded, err := DecodeNotarRewardCertificate(builder.BuildForLeaderSlot(leaderSlot).Notar)
	require.NoError(t, err)
	assert.Equal(t, blockB, decoded.BlockID)
	assertBitmapRanks(t, decoded.Bitmap, 5, 2, 3, 4)
}

func TestBuilderUsesSlotsForRewardOffset(t *testing.T) {
	builder := NewBuilder(DefaultBuilderConfig())
	builder.AddVote(testSignedVote(t, alpenglow.NewSkipVote(10), 0, testBLSKeys(t, 1)[0]))

	assert.Empty(t, builder.BuildForLeaderSlot(17).Skip)
	require.NotEmpty(t, builder.BuildForLeaderSlot(18).Skip)
}

func TestBuilderDeduplicatesVotes(t *testing.T) {
	keys := testBLSKeys(t, 2)
	builder := NewBuilder(DefaultBuilderConfig())
	vote := testSignedVote(t, alpenglow.NewSkipVote(5), 0, keys[0])
	builder.AddVote(vote)
	builder.AddVote(vote)

	certs := builder.BuildForLeaderSlot(5 + SlotsForReward)
	decoded, err := DecodeSkipRewardCertificate(certs.Skip)
	require.NoError(t, err)
	assertBitmapRanks(t, decoded.Bitmap, 2, 0)
}

func testBLSKeys(t *testing.T, n int) []*big.Int {
	t.Helper()
	keys := make([]*big.Int, n)
	for i := range keys {
		keys[i] = big.NewInt(int64(i + 3))
	}
	return keys
}

func testSignedVote(t *testing.T, vote alpenglow.Vote, rank int, key *big.Int) alpenglow.VoteMessage {
	t.Helper()
	payload, err := alpenglow.EncodeVotePayloadToSign(vote, 0)
	require.NoError(t, err)
	message, err := bls12381.HashToG2(payload, []byte(testBLSHashDST))
	require.NoError(t, err)
	var signed bls12381.G2Affine
	signed.ScalarMultiplication(&message, key)
	rawSig := signed.RawBytes()
	return alpenglow.VoteMessage{
		Vote:      vote,
		Signature: rawSig[:],
		Rank:      uint16(rank),
	}
}

func assertAggregateMatchesVotes(t *testing.T, aggregate []byte, votes []alpenglow.VoteMessage) {
	t.Helper()
	set, keys := testValidatorSetForVotes(t, votes)
	verifier := alpenglow.NewCertificateVerifier()
	require.NoError(t, verifier.SetValidatorSet(set))
	verifier.SetShredVersion(0)

	var certType alpenglow.CertificateType
	switch votes[0].Vote.Type {
	case alpenglow.VoteTypeSkip:
		certType = alpenglow.CertificateSkip
	case alpenglow.VoteTypeNotarize:
		certType = alpenglow.CertificateNotarize
	default:
		t.Fatalf("unexpected vote type %q", votes[0].Vote.Type)
	}
	cert := alpenglow.Certificate{
		Type:      certType,
		Slot:      votes[0].Vote.Slot,
		BlockHash: votes[0].Vote.BlockHash,
		Signature: uncompressedSignature(t, aggregate),
		Bitmap:    mustSignerBitmapBase2(t, len(set.Validators), ranksFromVotes(votes)...),
	}
	_, result, err := verifier.VerifyCertificateForEpoch(set.Epoch, cert)
	require.NoError(t, err)
	assert.True(t, result.SignatureVerified)
	_ = keys
}

func testValidatorSetForVotes(t *testing.T, votes []alpenglow.VoteMessage) (alpenglow.ValidatorSet, []*big.Int) {
	t.Helper()
	maxRank := 0
	for _, vote := range votes {
		if int(vote.Rank) > maxRank {
			maxRank = int(vote.Rank)
		}
	}
	keys := testBLSKeys(t, maxRank+1)
	validators := make([]alpenglow.ValidatorStake, maxRank+1)
	for i := range validators {
		var pubkey bls12381.G1Affine
		pubkey.ScalarMultiplicationBase(keys[i])
		var voteAcct solana.PublicKey
		voteAcct[0] = byte(i + 1)
		validators[i] = alpenglow.ValidatorStake{
			Rank:                  uint16(i),
			VoteAccount:           voteAcct,
			BlsPubkeyCompressed:   pubkey.Bytes(),
			BlsPubkeyUncompressed: pubkey.RawBytes(),
			Stake:                 100,
		}
	}
	return alpenglow.ValidatorSet{
		Epoch:      4,
		Validators: validators,
		TotalStake: uint64(len(validators) * 100),
	}, keys
}

func ranksFromVotes(votes []alpenglow.VoteMessage) []int {
	out := make([]int, len(votes))
	for i, vote := range votes {
		out[i] = int(vote.Rank)
	}
	return out
}

func uncompressedSignature(t *testing.T, compressed []byte) []byte {
	t.Helper()
	var sig bls12381.G2Affine
	require.NoError(t, sig.Unmarshal(compressed))
	raw := sig.RawBytes()
	return raw[:]
}

func mustSignerBitmapBase2(t *testing.T, length int, setBits ...int) []byte {
	t.Helper()
	bits := make([]bool, length)
	for _, bit := range setBits {
		bits[bit] = true
	}
	out, err := encodeSignerStoreBase2(bits)
	require.NoError(t, err)
	return out
}

func assertBitmapRanks(t *testing.T, bitmap []byte, maxLen int, ranks ...int) {
	t.Helper()
	decoded, err := alpenglow.DecodeSignerStoreBitmap(bitmap, maxLen)
	require.NoError(t, err)
	union := decoded.Union()
	require.Equal(t, len(ranks), countTrue(union))
	for _, rank := range ranks {
		require.True(t, union[rank])
	}
}

func countTrue(bits []bool) int {
	n := 0
	for _, bit := range bits {
		if bit {
			n++
		}
	}
	return n
}

func bytesRepeat(n int, b byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}
