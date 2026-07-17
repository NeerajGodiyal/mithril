package alpenglow

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"math/big"

	bls12381 "github.com/Overclock-Validator/gnark-crypto/ecc/bls12-381"
	blsfr "github.com/Overclock-Validator/gnark-crypto/ecc/bls12-381/fr"
)

var (
	blsKeyDeriveMessage = []byte("bls-key-derive-alpenglow")
	blsKeygenSalt       = []byte("BLS-SIG-KEYGEN-SALT-")
)

// BLSSigner is the Alpenglow BLS key deterministically derived from a Solana
// authorized-voter keypair. The derivation and signature encodings match
// solana-bls-signatures 3.3.0.
type BLSSigner struct {
	secret *big.Int
	public [48]byte
}

// DeriveBLSSigner mirrors solana_bls_signatures::Keypair::derive_from_signer:
// Ed25519-sign the public derivation label, then run the IETF BLS KeyGen
// procedure over that deterministic 64-byte signature.
func DeriveBLSSigner(authorizedVoter ed25519.PrivateKey) (*BLSSigner, error) {
	if len(authorizedVoter) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("alpenglow BLS signer: invalid authorized-voter private key size %d", len(authorizedVoter))
	}
	ikm := ed25519.Sign(authorizedVoter, blsKeyDeriveMessage)
	secret, err := deriveBLSSecretKey(ikm)
	if err != nil {
		return nil, err
	}
	var public bls12381.G1Affine
	public.ScalarMultiplicationBase(secret)
	if public.IsInfinity() {
		return nil, fmt.Errorf("alpenglow BLS signer: derived public key is infinity")
	}
	return &BLSSigner{secret: secret, public: public.Bytes()}, nil
}

func deriveBLSSecretKey(ikm []byte) (*big.Int, error) {
	if len(ikm) < 32 {
		return nil, fmt.Errorf("alpenglow BLS signer: key material is only %d bytes", len(ikm))
	}
	// KeyGen from draft-irtf-cfrg-bls-signature. blst_keygen, used by
	// solana-bls-signatures, hashes the salt before every attempt and reduces a
	// 48-byte HKDF output modulo the BLS12-381 scalar-field order.
	salt := append([]byte(nil), blsKeygenSalt...)
	ikmZero := make([]byte, len(ikm)+1)
	copy(ikmZero, ikm)
	info := []byte{0, 48} // I2OSP(L, 2), where L = ceil(1.5 * ceil(log2(r))/8)
	for attempts := 0; attempts < 255; attempts++ {
		digest := sha256.Sum256(salt)
		salt = digest[:]
		prk := hkdfExtractSHA256(salt, ikmZero)
		okm := hkdfExpandSHA256(prk, info, 48)
		secret := new(big.Int).SetBytes(okm)
		secret.Mod(secret, blsfr.Modulus())
		if secret.Sign() != 0 {
			return secret, nil
		}
	}
	return nil, fmt.Errorf("alpenglow BLS signer: key derivation produced zero repeatedly")
}

func hkdfExtractSHA256(salt, ikm []byte) []byte {
	mac := hmac.New(sha256.New, salt)
	_, _ = mac.Write(ikm)
	return mac.Sum(nil)
}

func hkdfExpandSHA256(prk, info []byte, length int) []byte {
	out := make([]byte, 0, length)
	var previous []byte
	for counter := byte(1); len(out) < length; counter++ {
		mac := hmac.New(sha256.New, prk)
		_, _ = mac.Write(previous)
		_, _ = mac.Write(info)
		_, _ = mac.Write([]byte{counter})
		previous = mac.Sum(nil)
		remaining := length - len(out)
		if remaining < len(previous) {
			out = append(out, previous[:remaining]...)
		} else {
			out = append(out, previous...)
		}
	}
	return out
}

func (s *BLSSigner) PublicKeyCompressed() [48]byte {
	if s == nil {
		return [48]byte{}
	}
	return s.public
}

// Sign returns Agave's 192-byte uncompressed G2Affine signature encoding.
func (s *BLSSigner) Sign(message []byte) ([]byte, error) {
	if s == nil || s.secret == nil || s.secret.Sign() == 0 {
		return nil, fmt.Errorf("alpenglow BLS signer: signer is not initialized")
	}
	point, err := bls12381.HashToG2(message, []byte(blsHashToPointDST))
	if err != nil {
		return nil, fmt.Errorf("alpenglow BLS signer: hash vote payload: %w", err)
	}
	var signature bls12381.G2Affine
	signature.ScalarMultiplication(&point, s.secret)
	if signature.IsInfinity() {
		return nil, fmt.Errorf("alpenglow BLS signer: signature is infinity")
	}
	raw := signature.RawBytes()
	return append([]byte(nil), raw[:]...), nil
}
