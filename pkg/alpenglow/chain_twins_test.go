package alpenglow

import (
	"math/rand"
	"testing"

	"github.com/gagliardetto/solana-go"
)

// Alpenglow fork-choice "Twins"-style adversarial test: feed randomized, chaotic
// certificate streams (competing block-ids, skips, finalizes, conflicts, delivered
// out-of-order and duplicated) into the ChainTracker and assert the decision
// resolver's core safety properties. The crypto is KAT-tested separately
// (verify_test.go); here we set the verified flags directly and stress the
// DECISION logic — which is the fork-choice part.
//
// Properties asserted:
//   - ORDER-INDEPENDENCE: the same set of certs delivered in any order (with
//     duplicates) yields identical per-slot decisions. Non-determinism here would
//     mean two honest observers disagree on the chain — a consensus split.
//   - SAFETY: a slot is never simultaneously decided block AND skip without being
//     surfaced as a conflict; competing block-ids on one slot => conflict.
//   - IDEMPOTENCE: re-observing certs never changes a decision.

type synthCert struct {
	typ   CertificateType
	slot  uint64
	block solana.Hash
}

func (c synthCert) toCertificate() Certificate {
	return Certificate{
		Type:              c.typ,
		Slot:              c.slot,
		BlockHash:         c.block,
		SignatureVerified: true,
		StakeVerified:     true,
	}
}

// feedCerts observes certs in the given order (verified), returns per-slot decision
// kind for every slot in [anchor+1, maxSlot].
func feedCerts(t *testing.T, certs []synthCert, anchor, maxSlot uint64) map[uint64]ChainDecisionKind {
	t.Helper()
	tracker := NewChainTracker()
	for _, c := range certs {
		if _, err := tracker.ObserveCertificate(c.toCertificate()); err != nil {
			// ValidateBasic may reject malformed certs (e.g. block cert w/ empty
			// hash) — that's fine, it just doesn't enter the tracker.
			continue
		}
	}
	out := make(map[uint64]ChainDecisionKind)
	for s := anchor + 1; s <= maxSlot; s++ {
		if d, ok := tracker.NextDecision(s - 1); ok {
			out[d.Slot] = d.Kind
		}
	}
	return out
}

func TestChainTrackerDecisionsAreOrderIndependent(t *testing.T) {
	blockA := solana.Hash{1}
	blockB := solana.Hash{2}
	const anchor, maxSlot = 100, 130
	types := []CertificateType{
		CertificateNotarize, CertificateFinalize, CertificateFinalizeFast,
		CertificateSkip, CertificateNotarizeFallback,
	}

	for seed := int64(1); seed <= 40; seed++ {
		rng := rand.New(rand.NewSource(seed))

		// Build a random cert set across the slot window.
		var certs []synthCert
		for s := uint64(anchor + 1); s <= maxSlot; s++ {
			n := rng.Intn(3) // 0-2 certs per slot (equivocation/conflict when >1)
			for i := 0; i < n; i++ {
				typ := types[rng.Intn(len(types))]
				blk := solana.Hash{}
				switch typ {
				case CertificateNotarize, CertificateFinalizeFast, CertificateNotarizeFallback, CertificateGenesis:
					if rng.Intn(2) == 0 {
						blk = blockA
					} else {
						blk = blockB
					}
				}
				certs = append(certs, synthCert{typ: typ, slot: s, block: blk})
			}
		}

		// Reference: certs in slot order.
		want := feedCerts(t, certs, anchor, maxSlot)

		// Shuffled + duplicated delivery must produce the identical decision map.
		for trial := 0; trial < 3; trial++ {
			shuffled := append([]synthCert(nil), certs...)
			rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
			// inject duplicates
			for i := 0; i < len(certs)/3; i++ {
				shuffled = append(shuffled, certs[rng.Intn(len(certs))])
			}
			rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

			got := feedCerts(t, shuffled, anchor, maxSlot)
			if len(got) != len(want) {
				t.Fatalf("seed %d trial %d: decision count %d != %d (order-dependent!)", seed, trial, len(got), len(want))
			}
			for slot, kind := range want {
				if got[slot] != kind {
					t.Fatalf("seed %d trial %d: slot %d decided %q in-order but %q shuffled (order-dependent!)",
						seed, trial, slot, kind, got[slot])
				}
			}
		}
	}
}

// Competing notarize certs (two distinct block-ids, same slot) must resolve to a
// conflict, never silently pick one — the equivocation safety case.
func TestChainTrackerCompetingBlockIDsConflict(t *testing.T) {
	tracker := NewChainTracker()
	for _, blk := range []solana.Hash{{1}, {2}} {
		if _, err := tracker.ObserveCertificate(Certificate{
			Type: CertificateNotarize, Slot: 201, BlockHash: blk,
			SignatureVerified: true, StakeVerified: true,
		}); err != nil {
			t.Fatalf("observe: %v", err)
		}
	}
	d, ok := tracker.NextDecision(200)
	if !ok || d.Kind != ChainDecisionKindConflict {
		t.Fatalf("competing block-ids: decision=%+v ok=%v, want conflict", d, ok)
	}
}

// A verified skip-only slot must never resolve to block.
func TestChainTrackerSkipNeverResolvesToBlock(t *testing.T) {
	tracker := NewChainTracker()
	if _, err := tracker.ObserveCertificate(Certificate{
		Type: CertificateSkip, Slot: 301, SignatureVerified: true, StakeVerified: true,
	}); err != nil {
		t.Fatalf("observe skip: %v", err)
	}
	d, ok := tracker.NextDecision(300)
	if !ok || d.Kind == ChainDecisionKindBlock {
		t.Fatalf("skip-only slot resolved to %+v, must not be block", d)
	}
}

// A cert seen while untrusted (validator set not yet installed) must be folded in
// when re-observed after it becomes trustable — else early certs stall finality.
func TestChainTrackerReprocessesCertTrustedLater(t *testing.T) {
	tracker := NewChainTrackerWithConfig(ChainConfig{
		RequireVerifiedCertificates:      true,
		RequireStakeVerifiedCertificates: true,
	})
	// Arrives stake-unverified (set not installed) → ignored, no decision.
	untrusted := Certificate{Type: CertificateSkip, Slot: 501, SignatureVerified: true, StakeVerified: false}
	if _, err := tracker.ObserveCertificate(untrusted); err != nil {
		t.Fatalf("observe untrusted: %v", err)
	}
	if _, ok := tracker.NextDecision(500); ok {
		t.Fatalf("untrusted cert should not yield a decision yet")
	}
	// Same cert re-observed once trustable → must now resolve.
	trusted := untrusted
	trusted.StakeVerified = true
	if _, err := tracker.ObserveCertificate(trusted); err != nil {
		t.Fatalf("observe trusted: %v", err)
	}
	d, ok := tracker.NextDecision(500)
	if !ok || d.Kind != ChainDecisionKindSkip || d.Slot != 501 {
		t.Fatalf("reprocessed cert: decision=%+v ok=%v, want skip@501", d, ok)
	}
}

// Re-observing the same cert must not change the decision (idempotence).
func TestChainTrackerObservationIsIdempotent(t *testing.T) {
	mk := func() *ChainTracker {
		tr := NewChainTracker()
		_, _ = tr.ObserveCertificate(Certificate{Type: CertificateNotarize, Slot: 401, BlockHash: solana.Hash{9}, SignatureVerified: true, StakeVerified: true})
		return tr
	}
	once := mk()
	d1, _ := once.NextDecision(400)

	twice := mk()
	for i := 0; i < 5; i++ {
		_, _ = twice.ObserveCertificate(Certificate{Type: CertificateNotarize, Slot: 401, BlockHash: solana.Hash{9}, SignatureVerified: true, StakeVerified: true})
	}
	d2, _ := twice.NextDecision(400)

	if d1.Kind != d2.Kind || d1.Block != d2.Block {
		t.Fatalf("idempotence: %+v != %+v after repeated observation", d1, d2)
	}
}
