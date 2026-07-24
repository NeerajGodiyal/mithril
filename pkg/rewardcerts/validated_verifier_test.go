package rewardcerts

import (
	"testing"

	bls12381 "github.com/Overclock-Validator/gnark-crypto/ecc/bls12-381"
	"github.com/Overclock-Validator/mithril/pkg/alpenglow"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateRewardCertificatesWithVerifierRetainsExactBLSChecks(t *testing.T) {
	const rewardSlot = uint64(123)
	vote := testSignedVote(t, alpenglow.NewSkipVote(rewardSlot), 0, testBLSKeys(t, 1)[0])
	builder := NewBuilder(DefaultBuilderConfig())
	builder.AddVote(vote)
	raw := builder.BuildForLeaderSlot(rewardSlot + SlotsForReward).Skip
	require.NotEmpty(t, raw)

	validatorSet, _ := testValidatorSetForVotes(t, []alpenglow.VoteMessage{vote})
	verifier := alpenglow.NewCertificateVerifier()
	require.NoError(t, verifier.SetValidatorSet(validatorSet))
	verifier.SetShredVersion(0)

	validated, timings, err := ValidateRewardCertificatesWithVerifier(
		rewardSlot+SlotsForReward,
		raw,
		nil,
		validatorSet.Epoch,
		verifier,
		true,
	)
	require.NoError(t, err)
	require.NotNil(t, validated)
	assert.Contains(t, validated.Validators, validatorSet.Validators[0].VoteAccount)
	assert.Positive(t, timings.Skip)

	tampered, err := DecodeSkipRewardCertificate(raw)
	require.NoError(t, err)
	tampered.Signature[0] ^= 1
	tamperedRaw, err := EncodeSkipRewardCertificate(tampered)
	require.NoError(t, err)
	_, _, err = ValidateRewardCertificatesWithVerifier(
		rewardSlot+SlotsForReward,
		tamperedRaw,
		nil,
		validatorSet.Epoch,
		verifier,
		true,
	)
	require.Error(t, err)

	wrongVersionVerifier := alpenglow.NewCertificateVerifier()
	require.NoError(t, wrongVersionVerifier.SetValidatorSet(validatorSet))
	wrongVersionVerifier.SetShredVersion(1)
	_, _, err = ValidateRewardCertificatesWithVerifier(
		rewardSlot+SlotsForReward,
		raw,
		nil,
		validatorSet.Epoch,
		wrongVersionVerifier,
		true,
	)
	require.Error(t, err)
}

func TestValidateDecodedBlockFinalCertificateWithVerifierRetainsExactBLSChecks(t *testing.T) {
	const slot = uint64(456)
	var blockID solana.Hash
	blockID[0] = 9

	vote := testSignedVote(t, alpenglow.NewNotarizationVote(slot, blockID), 0, testBLSKeys(t, 1)[0])
	validatorSet, _ := testValidatorSetForVotes(t, []alpenglow.VoteMessage{vote})
	verifier := alpenglow.NewCertificateVerifier()
	require.NoError(t, verifier.SetValidatorSet(validatorSet))
	verifier.SetShredVersion(0)

	var signature bls12381.G2Affine
	_, err := signature.SetBytes(vote.Signature)
	require.NoError(t, err)
	finalCertificate := FinalCertificate{
		Slot:    slot,
		BlockID: blockID,
		FinalAggregate: VotesAggregateWire{
			Signature: signature.Bytes(),
			Bitmap:    mustSignerBitmapBase2(t, 1, 0),
		},
	}

	validated, err := ValidateDecodedBlockFinalCertificateWithVerifier(
		finalCertificate,
		validatorSet.Epoch,
		verifier,
	)
	require.NoError(t, err)
	require.NotNil(t, validated)
	assert.Contains(t, validated.Signers, validatorSet.Validators[0].VoteAccount)

	tampered := finalCertificate
	tampered.BlockID[0] ^= 1
	_, err = ValidateDecodedBlockFinalCertificateWithVerifier(tampered, validatorSet.Epoch, verifier)
	require.Error(t, err)

	wrongVersionVerifier := alpenglow.NewCertificateVerifier()
	require.NoError(t, wrongVersionVerifier.SetValidatorSet(validatorSet))
	wrongVersionVerifier.SetShredVersion(1)
	_, err = ValidateDecodedBlockFinalCertificateWithVerifier(
		finalCertificate,
		validatorSet.Epoch,
		wrongVersionVerifier,
	)
	require.Error(t, err)
}
