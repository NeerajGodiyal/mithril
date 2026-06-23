package rewardcerts

import (
	"fmt"

	bls12381 "github.com/Overclock-Validator/gnark-crypto/ecc/bls12-381"
)

func decodeVoteSignature(data []byte) (bls12381.G2Affine, error) {
	var signature bls12381.G2Affine
	switch len(data) {
	case bls12381.SizeOfG2AffineCompressed:
		if err := signature.Unmarshal(data); err != nil {
			return signature, fmt.Errorf("rewardcerts: decode compressed BLS signature: %w", err)
		}
	case bls12381.SizeOfG2AffineUncompressed:
		if err := signature.Unmarshal(data); err != nil {
			return signature, fmt.Errorf("rewardcerts: decode uncompressed BLS signature: %w", err)
		}
	default:
		return signature, fmt.Errorf("rewardcerts: invalid BLS signature length %d", len(data))
	}
	if signature.IsInfinity() {
		return signature, fmt.Errorf("rewardcerts: invalid BLS signature infinity point")
	}
	return signature, nil
}

func aggregateVoteSignatures(signatures [][]byte) ([blsSignatureCompressedSize]byte, error) {
	var out [blsSignatureCompressedSize]byte
	var aggregate bls12381.G2Affine
	aggregate.SetInfinity()
	for _, raw := range signatures {
		sig, err := decodeVoteSignature(raw)
		if err != nil {
			return out, err
		}
		aggregate.Add(&aggregate, &sig)
	}
	if aggregate.IsInfinity() {
		return out, errEmptyBitmap
	}
	return aggregate.Bytes(), nil
}
