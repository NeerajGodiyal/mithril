package alpenglow

import (
	"testing"
)

// Pins the tracker's conflict/decision semantics to the Alpenglow whitepaper's
// exclusivity guarantees (Lemmas 21, 24, 26): fallback certs legally coexist with
// skips and each other pre-finality; only finalized blocks are exclusive.

func specObserve(t *testing.T, tracker *ChainTracker, cert Certificate) {
	t.Helper()
	cert.SignatureVerified = true
	if _, err := tracker.ObserveCertificate(cert); err != nil {
		t.Fatalf("observe %s slot %d: %v", cert.Type, cert.Slot, err)
	}
}

func specFinalize(t *testing.T, tracker *ChainTracker, block BlockID, certType CertificateType) {
	t.Helper()
	if err := tracker.ObserveFinalized(block, certType); err != nil {
		t.Fatalf("finalize %s with %s: %v", block, certType, err)
	}
}

// Lemma 21(iii): a fast-finalized block plus a skip cert is a safety violation.
func TestSpecFastFinalizedPlusSkipConflicts(t *testing.T) {
	tracker := NewChainTracker()
	block := BlockID{Slot: 21, Hash: chainTestHash(1)}
	specObserve(t, tracker, Certificate{Type: CertificateFinalizeFast, Slot: block.Slot, BlockHash: block.Hash})
	specFinalize(t, tracker, block, CertificateFinalizeFast)
	specObserve(t, tracker, Certificate{Type: CertificateSkip, Slot: 21})

	decision, ok := tracker.NextDecision(20)
	if !ok || decision.Kind != ChainDecisionKindConflict {
		t.Fatalf("fast-finalized + skip must conflict, got %+v (ok=%v)", decision, ok)
	}
}

// Lemma 26(iii): a slow-finalized block (Notarize + Finalize) plus a skip conflicts.
func TestSpecSlowFinalizedPlusSkipConflicts(t *testing.T) {
	tracker := NewChainTracker()
	specObserve(t, tracker, Certificate{Type: CertificateNotarize, Slot: 31, BlockHash: chainTestHash(1)})
	specObserve(t, tracker, Certificate{Type: CertificateFinalize, Slot: 31})
	specFinalize(t, tracker, BlockID{Slot: 31, Hash: chainTestHash(1)}, CertificateFinalize)
	specObserve(t, tracker, Certificate{Type: CertificateSkip, Slot: 31})

	decision, ok := tracker.NextDecision(30)
	if !ok || decision.Kind != ChainDecisionKindConflict {
		t.Fatalf("slow-finalized + skip must conflict, got %+v (ok=%v)", decision, ok)
	}
}

// Lemma 21(ii): a fast-finalized block plus a notar-fallback cert on a DIFFERENT
// block conflicts.
func TestSpecFinalizedPlusCompetingFallbackConflicts(t *testing.T) {
	tracker := NewChainTracker()
	block := BlockID{Slot: 41, Hash: chainTestHash(1)}
	specObserve(t, tracker, Certificate{Type: CertificateFinalizeFast, Slot: block.Slot, BlockHash: block.Hash})
	specFinalize(t, tracker, block, CertificateFinalizeFast)
	specObserve(t, tracker, Certificate{Type: CertificateNotarizeFallback, Slot: 41, BlockHash: chainTestHash(2)})

	decision, ok := tracker.NextDecision(40)
	if !ok || decision.Kind != ChainDecisionKindConflict {
		t.Fatalf("finalized + competing fallback must conflict, got %+v (ok=%v)", decision, ok)
	}
}

// Legal state: a notar-fallback cert plus a skip cert (fallback voting round).
// Resolves to skip — never a conflict, never a block.
func TestSpecFallbackPlusSkipResolvesToSkip(t *testing.T) {
	tracker := NewChainTracker()
	specObserve(t, tracker, Certificate{Type: CertificateNotarizeFallback, Slot: 51, BlockHash: chainTestHash(1)})
	specObserve(t, tracker, Certificate{Type: CertificateSkip, Slot: 51})

	decision, ok := tracker.NextDecision(50)
	if !ok || decision.Kind != ChainDecisionKindSkip {
		t.Fatalf("fallback + skip must resolve to skip, got %+v (ok=%v)", decision, ok)
	}
}

// Legal state: several notar-fallback blocks in one slot (up to 7 per the vote
// budget). Ambiguous — no decision, and definitely no conflict.
func TestSpecMultipleFallbackBlocksWaitWithoutConflict(t *testing.T) {
	tracker := NewChainTracker()
	specObserve(t, tracker, Certificate{Type: CertificateNotarizeFallback, Slot: 61, BlockHash: chainTestHash(1)})
	specObserve(t, tracker, Certificate{Type: CertificateNotarizeFallback, Slot: 61, BlockHash: chainTestHash(2)})

	if decision, ok := tracker.NextDecision(60); ok {
		t.Fatalf("multiple fallback blocks are ambiguous — want no decision, got %+v", decision)
	}
}

// A notar-fallback cert alone is not decisive (several blocks may carry one):
// the slot waits for a notarize/finalize cert or finalized ancestry.
func TestSpecFallbackAloneIsNotDecisive(t *testing.T) {
	tracker := NewChainTracker()
	specObserve(t, tracker, Certificate{Type: CertificateNotarizeFallback, Slot: 71, BlockHash: chainTestHash(1)})

	if decision, ok := tracker.NextDecision(70); ok {
		t.Fatalf("fallback-only slot must wait, got %+v", decision)
	}
}

// The Votor vote budget bounds certified blocks at 7 per slot: 7 fallback blocks are
// a legal (waiting) state, an 8th is protocol-impossible — attack evidence, conflict.
func TestSpecCertifiedBlocksPerSlotBound(t *testing.T) {
	tracker := NewChainTracker()
	for i := 0; i < maxCertifiedBlocksPerSlot; i++ {
		specObserve(t, tracker, Certificate{Type: CertificateNotarizeFallback, Slot: 91, BlockHash: chainTestHash(byte(i + 1))})
	}
	if decision, ok := tracker.NextDecision(90); ok {
		t.Fatalf("7 fallback blocks are legal — want no decision, got %+v", decision)
	}
	specObserve(t, tracker, Certificate{Type: CertificateNotarizeFallback, Slot: 91, BlockHash: chainTestHash(99)})
	decision, ok := tracker.NextDecision(90)
	if !ok || decision.Kind != ChainDecisionKindConflict {
		t.Fatalf("8th certified block must be a conflict, got %+v (ok=%v)", decision, ok)
	}
}

// Finalized-by-ancestry: a fallback-only block becomes decisive once a finalized
// descendant chains through it.
func TestSpecFinalizedAncestryMakesFallbackDecisive(t *testing.T) {
	tracker := NewChainTracker()
	parent := BlockID{Slot: 81, Hash: chainTestHash(1)}
	child := BlockID{Slot: 82, Hash: chainTestHash(2)}

	specObserve(t, tracker, Certificate{Type: CertificateNotarizeFallback, Slot: parent.Slot, BlockHash: parent.Hash})
	// Replay observes the child linking to the parent, then the child fast-finalizes.
	tracker.ObserveReplayBlock(ReplayBlockObservation{
		Block:      child,
		ParentSlot: parent.Slot,
		ParentHash: parent.Hash,
	})
	specObserve(t, tracker, Certificate{Type: CertificateFinalizeFast, Slot: child.Slot, BlockHash: child.Hash})
	specFinalize(t, tracker, child, CertificateFinalizeFast)

	decision, ok := tracker.NextDecision(parent.Slot - 1)
	if !ok || decision.Kind != ChainDecisionKindBlock || decision.Block != parent {
		t.Fatalf("ancestor of finalized block must resolve to block %v, got %+v (ok=%v)", parent, decision, ok)
	}
}

// Agave's parent-ready state machine permits a notar-fallback block and a skip
// certificate at the same slot: that block remains a valid parent for a later
// skip-connected child. If the child finalizes, its exact parent link selects
// the block; the same-slot skip is not conflicting finality and must not erase
// the selected ancestor. This is the live fork shape observed at 3425812->3425816.
func TestSpecFinalizedDescendantSelectsFallbackParentDespiteSkip(t *testing.T) {
	tracker := NewChainTracker()
	parent := BlockID{Slot: 3_425_812, Hash: chainTestHash(12)}
	child := BlockID{Slot: 3_425_816, Hash: chainTestHash(16)}

	tracker.ObserveReplayBlock(ReplayBlockObservation{
		Block:      parent,
		ParentSlot: parent.Slot - 5,
		ParentHash: chainTestHash(7),
	})
	specObserve(t, tracker, Certificate{Type: CertificateNotarizeFallback, Slot: parent.Slot, BlockHash: parent.Hash})
	specObserve(t, tracker, Certificate{Type: CertificateSkip, Slot: parent.Slot})
	for slot := parent.Slot + 1; slot < child.Slot; slot++ {
		specObserve(t, tracker, Certificate{Type: CertificateSkip, Slot: slot})
	}
	tracker.ObserveReplayBlock(ReplayBlockObservation{
		Block:      child,
		ParentSlot: parent.Slot,
		ParentHash: parent.Hash,
	})
	specObserve(t, tracker, Certificate{Type: CertificateFinalizeFast, Slot: child.Slot, BlockHash: child.Hash})
	specFinalize(t, tracker, child, CertificateFinalizeFast)

	if tracker.FinalityConflictAt(parent.Slot) {
		reason, _ := tracker.FinalityConflictReasonAt(parent.Slot)
		t.Fatalf("selected fallback parent plus skip is legal, got conflict: %s", reason)
	}
	if tracker.SkipCertifiedAt(parent.Slot) {
		t.Fatal("selected exact parent must override its same-slot skip for replay")
	}
	decision, ok := tracker.NextDecision(parent.Slot - 1)
	if !ok || decision.Kind != ChainDecisionKindBlock || decision.Block != parent {
		t.Fatalf("selected parent decision = %+v (ok=%v), want block %v", decision, ok, parent)
	}
	for slot := parent.Slot + 1; slot < child.Slot; slot++ {
		if !tracker.SkipCertifiedAt(slot) {
			t.Fatalf("omitted slot %d must remain skipped", slot)
		}
	}
}
