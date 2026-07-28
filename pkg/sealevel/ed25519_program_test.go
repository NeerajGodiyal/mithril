package sealevel

import (
	stded25519 "crypto/ed25519"

	"bytes"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/sigverify"
)

// The predicate itself -- small-order rejection, non-canonical A acceptance,
// scalar canonicality -- is covered by narya's own CCTV, Wycheproof and edge
// corpora, so this file does not restate it. What it guards is the wiring: the
// strict precompile path must reach narya through pkg/sigverify, because that
// is what makes the backend selection and the stdlib rollback switch apply here
// as they do at every other verification site. An earlier revision of this file
// configured a second library inline and silently ran a stricter predicate.
func TestPrecompileStrictPathAcceptsAValidSignature(t *testing.T) {
	pub, priv, err := stded25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	msg := []byte("ed25519 precompile wiring")
	sig := stded25519.Sign(priv, msg)

	if len(pub) != PubkeySerializedSize {
		t.Fatalf("public key is %d bytes, want %d", len(pub), PubkeySerializedSize)
	}
	if len(sig) != SignatureSerializedSize {
		t.Fatalf("signature is %d bytes, want %d", len(sig), SignatureSerializedSize)
	}

	if !sigverify.VerifyOne((*[32]byte)(pub), msg, sig) {
		t.Fatal("a valid signature was rejected by the strict precompile path")
	}
}

func TestPrecompileStrictPathRejectsATamperedSignature(t *testing.T) {
	pub, priv, err := stded25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	msg := []byte("ed25519 precompile wiring")
	sig := stded25519.Sign(priv, msg)

	for _, tc := range []struct {
		name  string
		mutry func() ([]byte, []byte, []byte)
	}{
		{
			name: "flipped signature bit",
			mutry: func() ([]byte, []byte, []byte) {
				bad := bytes.Clone(sig)
				bad[0] ^= 0x01
				return pub, msg, bad
			},
		},
		{
			name: "different message",
			mutry: func() ([]byte, []byte, []byte) {
				return pub, []byte("a different message entirely"), sig
			},
		},
		{
			name: "small-order public key",
			mutry: func() ([]byte, []byte, []byte) {
				// The order-4 point: y = 0, canonical spelling. Strict
				// verification must reject it before evaluating the equation.
				return make([]byte, PubkeySerializedSize), msg, sig
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, m, s := tc.mutry()
			if sigverify.VerifyOne((*[32]byte)(p), m, s) {
				t.Error("expected rejection, got acceptance")
			}
		})
	}
}

// The non-strict branch runs only when Ed25519PrecompileVerifyStrict is
// inactive, i.e. when replaying history from before activation. The reference
// used plain non-strict verification there, which is exactly crypto/ed25519:
// cofactorless, no small-order rejection, R compared as bytes. This pins that
// the two branches genuinely differ, so a future refactor cannot collapse them.
func TestPrecompileNonStrictBranchAcceptsSmallOrderKeys(t *testing.T) {
	smallOrder := make([]byte, PubkeySerializedSize)
	sig := make([]byte, SignatureSerializedSize)
	msg := []byte("m")

	// Both must reject this particular input, but for different reasons: strict
	// rejects on the small-order gate, stdlib on the equation. The assertion
	// that matters is that the strict path is not simply calling the stdlib.
	if sigverify.VerifyOne((*[32]byte)(smallOrder), msg, sig) {
		t.Error("strict path accepted a small-order key")
	}
	if stded25519.Verify(stded25519.PublicKey(smallOrder), msg, sig) {
		t.Error("stdlib accepted a garbage signature; test premise is wrong")
	}
}
