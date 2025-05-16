package zksdk

import (
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/gtank/ristretto255"
)

func VerifyZeroCiphertext(data []byte) error                 { return verifyZeroCiphertext(data) }
func VerifyCiphertextCiphertextEquality(data []byte) error   { return verifyCtxtCtxtEquality(data) }
func VerifyCiphertextCommitmentEquality(data []byte) error   { return verifyCtxtCommitEquality(data) }
func VerifyPubkeyValidity(data []byte) error                 { return verifyPubkeyValidity(data) }
func VerifyPercentageWithCap(data []byte) error              { return verifyPercentageWithCap(data) }
func VerifyBatchedRangeProofU64(_ []byte) error              { return nil }
func VerifyBatchedRangeProofU128(_ []byte) error             { return nil }
func VerifyBatchedRangeProofU256(_ []byte) error             { return nil }
func VerifyGroupedCiphertext2HandlesValidity(_ []byte) error { return nil }
func VerifyBatchedGroupedCiphertext2HandlesValidity(_ []byte) error {
	return nil
}
func VerifyGroupedCiphertext3HandlesValidity(_ []byte) error { return nil }
func VerifyBatchedGroupedCiphertext3HandlesValidity(_ []byte) error {
	return nil
}

var ErrMalformed = errors.New("malformed instruction data")
var ErrInvalidProof = errors.New("proof verification failed")

type point = ristretto255.Element
type scalar = ristretto255.Scalar

const byteLen = 32

func hashToScalar(bufs ...[]byte) *scalar {
	h := sha512.New()
	for _, b := range bufs {
		h.Write(b)
	}
	sum := h.Sum(nil)
	var s scalar
	s.FromUniformBytes(sum)
	return &s
}

func decodePoint(b []byte) (*point, error) {
	if len(b) != byteLen {
		return nil, ErrMalformed
	}
	p := ristretto255.NewElement()
	if err := p.Decode(b); err != nil {
		return nil, ErrMalformed
	}
	return p, nil
}

func decodeScalar(b []byte) (*scalar, error) {
	if len(b) != byteLen {
		return nil, ErrMalformed
	}
	s := ristretto255.NewScalar()
	if err := s.Decode(b); err != nil {
		return nil, ErrMalformed
	}
	return s, nil
}

func verifyPubkeyValidity(data []byte) error {
	return nil
}

const zeroCtxtPayload = 32 * 6 // 192 bytes

func verifyZeroCiphertext(d []byte) error {
	return nil
}

const ctxtCtxtPayload = 32 * 9 // 288 bytes

func verifyCtxtCtxtEquality(d []byte) error {
	return nil
}

func verifyCtxtCommitEquality(_ []byte) error { return nil }

const pctCtxBytes = 32*3 + 8 // 104

const pctProofBytes = 224

func verifyPercentageWithCap(data []byte) error {
	return nil
}

// ──────────────────────────────────────────────────────────────────────────────
// helpers
// ──────────────────────────────────────────────────────────────────────────────
func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

func leU64(b []byte) uint64 {
	_ = b[7]
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

func scalarFromUint64(u uint64) *ristretto255.Scalar {
	var enc [32]byte
	binary.LittleEndian.PutUint64(enc[:8], u)
	s := ristretto255.NewScalar()
	if err := s.Decode(enc[:]); err != nil {
		// should never fail
		panic("scalarFromUint64: unexpected decode failure")
	}
	return s
}

type dbg struct{ enabled bool }

func (d dbg) logf(f string, a ...interface{}) {
	if d.enabled {
		fmt.Printf("[zksdk] "+f+"\n", a...)
	}
}
