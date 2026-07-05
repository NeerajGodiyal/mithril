package replay

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"sync"
	"testing"

	"github.com/gagliardetto/solana-go"
)

func signedTestSnapshot(t *testing.T, corrupt bool) *sigverifySnapshot {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	message := []byte("sigverify pool test message")
	sig := ed25519.Sign(priv, message)
	if corrupt {
		sig[0] ^= 0xFF
	}
	var signer solana.PublicKey
	copy(signer[:], pub)
	var signature solana.Signature
	copy(signature[:], sig)
	return &sigverifySnapshot{
		slot:       9,
		signers:    []solana.PublicKey{signer},
		signatures: []solana.Signature{signature},
		firstKeys:  []solana.PublicKey{signer},
		message:    message,
	}
}

// The pool verifies valid snapshots and releases the block's WaitGroup —
// the join contract ProcessBlock relies on.
func TestSigverifyPoolVerifiesAndJoins(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		enqueueSigverify(signedTestSnapshot(t, false), &wg)
	}
	wg.Wait() // hangs (test timeout) if any worker fails to Done()
}

// An invalid signature still halts — same deliberate panic semantics as the
// per-goroutine version this pool replaced.
func TestVerifySignaturesPanicsOnBadSignature(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on invalid signature")
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, "invalid signature") {
			t.Fatalf("panic = %v, want invalid-signature message", r)
		}
	}()
	var wg sync.WaitGroup
	wg.Add(1)
	verifySignatures(signedTestSnapshot(t, true), &wg)
}

// Failure diagnostics render from raw snapshot data on demand — the base58
// work deliberately deferred off the execution path.
func TestSigverifyDiagContextRendersFromRawData(t *testing.T) {
	s := signedTestSnapshot(t, false)
	diag := s.diagContext()
	if !strings.Contains(diag, s.txSigString()) || !strings.Contains(diag, s.signers[0].String()) {
		t.Fatalf("diag context missing identifiers: %s", diag)
	}
}
