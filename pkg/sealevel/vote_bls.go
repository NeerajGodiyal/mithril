package sealevel

import (
	bls12381 "github.com/Overclock-Validator/gnark-crypto/ecc/bls12-381"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
)

const (
	VoteAuthorizeTypeVoterWithBLS        = 2
	blsPublicKeyCompressedSize           = 48
	blsProofOfPossessionCompressedSize   = 96
	blsProofOfPossessionDST              = "BLS_POP_BLS12381G2_XMD:SHA-256_SSWU_RO_POP_"
	voteBLSProofOfPossessionComputeUnits = 34_500
)

type VoterWithBLSArgs struct {
	BlsPubkeyCompressed  [blsPublicKeyCompressedSize]byte
	BlsProofOfPossession [blsProofOfPossessionCompressedSize]byte
}

type VoteAuthorizeKind struct {
	Type         uint32
	VoterWithBLS *VoterWithBLSArgs
}

func (authorize *VoteAuthorizeKind) UnmarshalWithDecoder(decoder *bin.Decoder) error {
	var err error
	authorize.Type, err = decoder.ReadUint32(bin.LE)
	if err != nil {
		return err
	}

	switch authorize.Type {
	case VoteAuthorizeTypeVoter, VoteAuthorizeTypeWithdrawer:
		authorize.VoterWithBLS = nil
		return nil
	case VoteAuthorizeTypeVoterWithBLS:
		var args VoterWithBLSArgs
		pubkey, err := decoder.ReadBytes(blsPublicKeyCompressedSize)
		if err != nil {
			return err
		}
		copy(args.BlsPubkeyCompressed[:], pubkey)

		proof, err := decoder.ReadBytes(blsProofOfPossessionCompressedSize)
		if err != nil {
			return err
		}
		copy(args.BlsProofOfPossession[:], proof)

		authorize.VoterWithBLS = &args
		return nil
	default:
		return invalidEnumValue
	}
}

// generateVoteBLSPopMessage mirrors solana-bls-signatures' bound PoP input:
// custom payload "ALPENGLOW" || vote account, followed by the compressed BLS
// public key before hashing to G2 with the proof-of-possession domain.
func generateVoteBLSPopMessage(voteAccount solana.PublicKey, blsPubkey []byte) []byte {
	msg := make([]byte, 0, 9+solana.PublicKeyLength+blsPublicKeyCompressedSize)
	msg = append(msg, []byte("ALPENGLOW")...)
	msg = append(msg, voteAccount[:]...)
	msg = append(msg, blsPubkey...)
	return msg
}

func verifyVoteBLSProofOfPossession(voteAccount solana.PublicKey, args *VoterWithBLSArgs) error {
	if args == nil {
		return InstrErrInvalidInstructionData
	}

	var pubkey bls12381.G1Affine
	if _, err := pubkey.SetBytes(args.BlsPubkeyCompressed[:]); err != nil {
		return InstrErrInvalidArgument
	}
	if pubkey.IsInfinity() {
		return InstrErrInvalidArgument
	}

	var proof bls12381.G2Affine
	if _, err := proof.SetBytes(args.BlsProofOfPossession[:]); err != nil {
		return InstrErrInvalidArgument
	}

	msg := generateVoteBLSPopMessage(voteAccount, args.BlsPubkeyCompressed[:])
	message, err := bls12381.HashToG2(msg, []byte(blsProofOfPossessionDST))
	if err != nil {
		return InstrErrInvalidArgument
	}

	_, _, g1Generator, _ := bls12381.Generators()
	var negGenerator bls12381.G1Affine
	negGenerator.Neg(&g1Generator)

	ok, err := bls12381.PairingCheck(
		[]bls12381.G1Affine{pubkey, negGenerator},
		[]bls12381.G2Affine{message, proof},
	)
	if err != nil || !ok {
		return InstrErrInvalidArgument
	}
	return nil
}

func voteStateHasBLSPubkey(voteState *VoteState) bool {
	bls := voteState.v4BlsPubkeyCompressed
	if bls == nil {
		return false
	}
	for _, value := range bls[:] {
		if value != 0 {
			return true
		}
	}
	return false
}
