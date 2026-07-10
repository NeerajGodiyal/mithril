package alpenglow

import (
	"testing"

	"github.com/gagliardetto/solana-go"
)

func TestChainTrackerRequiresVerifiedCertificatesByDefault(t *testing.T) {
	tracker := NewChainTracker()

	_, err := tracker.ObserveCertificate(Certificate{Type: CertificateSkip, Slot: 11})
	if err != nil {
		t.Fatalf("observe unverified skip certificate: %v", err)
	}

	if decision, ok := tracker.NextDecision(10); ok {
		t.Fatalf("expected unverified certificate not to drive a decision, got %+v", decision)
	}

	snap := tracker.Snapshot()
	if snap.CertificatesObserved != 1 || snap.CertificatesAccepted != 0 || snap.CertificatesIgnoredUntrusted != 1 {
		t.Fatalf("unexpected snapshot after unverified cert: %+v", snap)
	}

	_, err = tracker.ObserveCertificate(Certificate{Type: CertificateSkip, Slot: 12, SignatureVerified: true})
	if err != nil {
		t.Fatalf("observe verified skip certificate: %v", err)
	}

	decision, ok := tracker.NextDecision(11)
	if !ok {
		t.Fatalf("expected verified skip certificate to drive a decision")
	}
	if decision.Kind != ChainDecisionKindSkip || decision.Slot != 12 || decision.CertificateType != CertificateSkip {
		t.Fatalf("unexpected skip decision: %+v", decision)
	}
}

func TestChainTrackerCanAllowUnverifiedCertificatesForDiagnostics(t *testing.T) {
	tracker := NewChainTrackerWithConfig(ChainConfig{RequireVerifiedCertificates: false})

	_, err := tracker.ObserveCertificate(Certificate{Type: CertificateSkip, Slot: 11})
	if err != nil {
		t.Fatalf("observe skip certificate: %v", err)
	}

	decision, ok := tracker.NextDecision(10)
	if !ok {
		t.Fatalf("expected diagnostic tracker to accept unverified skip certificate")
	}
	if decision.Kind != ChainDecisionKindSkip || decision.Slot != 11 {
		t.Fatalf("unexpected decision: %+v", decision)
	}
}

func TestChainTrackerCanRequireStakeVerifiedCertificates(t *testing.T) {
	tracker := NewChainTrackerWithConfig(ChainConfig{
		RequireVerifiedCertificates:      false,
		RequireStakeVerifiedCertificates: true,
	})

	_, err := tracker.ObserveCertificate(Certificate{Type: CertificateSkip, Slot: 11})
	if err != nil {
		t.Fatalf("observe untrusted skip certificate: %v", err)
	}
	if decision, ok := tracker.NextDecision(10); ok {
		t.Fatalf("expected stake-unverified certificate not to drive a decision, got %+v", decision)
	}

	_, err = tracker.ObserveCertificate(Certificate{Type: CertificateSkip, Slot: 12, StakeVerified: true})
	if err != nil {
		t.Fatalf("observe stake-verified skip certificate: %v", err)
	}
	decision, ok := tracker.NextDecision(11)
	if !ok {
		t.Fatalf("expected stake-verified certificate to drive a decision")
	}
	if decision.Kind != ChainDecisionKindSkip || decision.Slot != 12 {
		t.Fatalf("unexpected decision: %+v", decision)
	}
}

func TestChainTrackerResolvesCertifiedBlock(t *testing.T) {
	tracker := NewChainTracker()
	blockID := BlockID{Slot: 11, Hash: chainTestHash(1)}

	_, err := tracker.ObserveCertificate(Certificate{
		Type:              CertificateNotarize,
		Slot:              blockID.Slot,
		BlockHash:         blockID.Hash,
		SignatureVerified: true,
	})
	if err != nil {
		t.Fatalf("observe notarize certificate: %v", err)
	}

	decision, ok := tracker.NextDecision(10)
	if !ok {
		t.Fatalf("expected certified block decision")
	}
	if decision.Kind != ChainDecisionKindBlock || decision.Block != blockID || decision.Observed {
		t.Fatalf("unexpected decision before replay observation: %+v", decision)
	}

	parentHash := chainTestHash(9)
	tracker.ObserveReplayBlock(ReplayBlockObservation{
		Block:      blockID,
		ParentSlot: 10,
		ParentHash: parentHash,
	})

	decision, ok = tracker.NextDecision(10)
	if !ok {
		t.Fatalf("expected certified block decision after replay observation")
	}
	if decision.Kind != ChainDecisionKindBlock || !decision.Observed || decision.ParentSlot != 10 || decision.ParentHash != parentHash {
		t.Fatalf("unexpected decision after replay observation: %+v", decision)
	}
}

func TestChainTrackerDetectsCertifiedBlockConflict(t *testing.T) {
	tracker := NewChainTracker()

	for _, hash := range []solana.Hash{chainTestHash(1), chainTestHash(2)} {
		_, err := tracker.ObserveCertificate(Certificate{
			Type:              CertificateNotarize,
			Slot:              11,
			BlockHash:         hash,
			SignatureVerified: true,
		})
		if err != nil {
			t.Fatalf("observe notarize certificate: %v", err)
		}
	}

	decision, ok := tracker.NextDecision(10)
	if !ok {
		t.Fatalf("expected conflict decision")
	}
	if decision.Kind != ChainDecisionKindConflict || len(decision.Candidates) != 2 {
		t.Fatalf("unexpected conflict decision: %+v", decision)
	}
	if snap := tracker.Snapshot(); snap.ConflictingSlots != 1 {
		t.Fatalf("expected one conflicting slot, got snapshot %+v", snap)
	}
}

func TestChainTrackerDetectsBlockAndSkipConflict(t *testing.T) {
	tracker := NewChainTracker()

	_, err := tracker.ObserveCertificate(Certificate{
		Type:              CertificateNotarize,
		Slot:              11,
		BlockHash:         chainTestHash(1),
		SignatureVerified: true,
	})
	if err != nil {
		t.Fatalf("observe notarize certificate: %v", err)
	}
	_, err = tracker.ObserveCertificate(Certificate{
		Type:              CertificateSkip,
		Slot:              11,
		SignatureVerified: true,
	})
	if err != nil {
		t.Fatalf("observe skip certificate: %v", err)
	}

	decision, ok := tracker.NextDecision(10)
	if !ok {
		t.Fatalf("expected conflict decision")
	}
	if decision.Kind != ChainDecisionKindConflict || decision.Reason == "" {
		t.Fatalf("unexpected conflict decision: %+v", decision)
	}
}

func TestChainTrackerFastFinalizationDerivesOmittedSkips(t *testing.T) {
	tracker := NewChainTracker()
	blockID := BlockID{Slot: 15, Hash: chainTestHash(15)}

	_, err := tracker.ObserveCertificate(Certificate{
		Type:              CertificateFinalizeFast,
		Slot:              blockID.Slot,
		BlockHash:         blockID.Hash,
		SignatureVerified: true,
	})
	if err != nil {
		t.Fatalf("observe fast finalization certificate: %v", err)
	}
	tracker.ObserveReplayBlock(ReplayBlockObservation{
		Block:      blockID,
		ParentSlot: 12,
		ParentHash: chainTestHash(12),
	})

	path := tracker.ResolvePath(12, 4)
	if len(path.Decisions) != 3 {
		t.Fatalf("expected two skips and one block decision, got %+v", path)
	}
	for i, wantSlot := range []uint64{13, 14} {
		decision := path.Decisions[i]
		if decision.Kind != ChainDecisionKindSkip || decision.Slot != wantSlot || !decision.Indirect || decision.ViaFinalized != blockID {
			t.Fatalf("unexpected indirect skip decision %d: %+v", i, decision)
		}
	}
	blockDecision := path.Decisions[2]
	if blockDecision.Kind != ChainDecisionKindBlock || blockDecision.Block != blockID || !blockDecision.Observed {
		t.Fatalf("unexpected finalized block decision: %+v", blockDecision)
	}
}

func TestChainTrackerSlowFinalizationRequiresNotarizationCertificate(t *testing.T) {
	tracker := NewChainTracker()
	blockID := BlockID{Slot: 15, Hash: chainTestHash(15)}

	_, err := tracker.ObserveCertificate(Certificate{
		Type:              CertificateFinalize,
		Slot:              blockID.Slot,
		SignatureVerified: true,
	})
	if err != nil {
		t.Fatalf("observe finalization certificate: %v", err)
	}
	tracker.ObserveReplayBlock(ReplayBlockObservation{
		Block:      blockID,
		ParentSlot: 12,
		ParentHash: chainTestHash(12),
	})

	if snap := tracker.Snapshot(); snap.DirectFinalizedBlocks != 0 || snap.IndirectSkips != 0 {
		t.Fatalf("finalization without notarization should not identify a block, got snapshot %+v", snap)
	}

	_, err = tracker.ObserveCertificate(Certificate{
		Type:              CertificateNotarize,
		Slot:              blockID.Slot,
		BlockHash:         blockID.Hash,
		SignatureVerified: true,
	})
	if err != nil {
		t.Fatalf("observe notarization certificate: %v", err)
	}

	path := tracker.ResolvePath(12, 4)
	if len(path.Decisions) != 3 {
		t.Fatalf("expected slow-finalized path, got %+v", path)
	}
	if path.Decisions[0].Kind != ChainDecisionKindSkip || path.Decisions[0].Slot != 13 {
		t.Fatalf("unexpected first decision: %+v", path.Decisions[0])
	}
	if path.Decisions[2].Kind != ChainDecisionKindBlock || path.Decisions[2].Block != blockID {
		t.Fatalf("unexpected block decision: %+v", path.Decisions[2])
	}
	if path.Decisions[2].CertificateType != CertificateFinalize {
		t.Fatalf("slow-finalized block decision cert type = %q, want %q", path.Decisions[2].CertificateType, CertificateFinalize)
	}
}

// TestChainTrackerFinalizedAncestryWalkDerivesDeepSkips models the
// bootstrap-catchup stall: slots 13-16 were skipped before the Votor listener
// connected (no certs for them, nor for blocks 17-19), but block 20 is fast-
// finalized live. Observing the ancestor chain 20 -> 19 -> 18 -> 17 (parent
// 12) must derive indirect skips for 13-16 even though only block 20 has a
// certificate.
func TestChainTrackerFinalizedAncestryWalkDerivesDeepSkips(t *testing.T) {
	tracker := NewChainTracker()

	block17 := BlockID{Slot: 17, Hash: chainTestHash(17)}
	block18 := BlockID{Slot: 18, Hash: chainTestHash(18)}
	block19 := BlockID{Slot: 19, Hash: chainTestHash(19)}
	block20 := BlockID{Slot: 20, Hash: chainTestHash(20)}

	if _, err := tracker.ObserveCertificate(Certificate{
		Type:              CertificateFinalizeFast,
		Slot:              block20.Slot,
		BlockHash:         block20.Hash,
		SignatureVerified: true,
	}); err != nil {
		t.Fatalf("observe fast finalization certificate: %v", err)
	}

	tracker.ObserveReplayBlock(ReplayBlockObservation{Block: block20, ParentSlot: 19, ParentHash: block19.Hash})
	tracker.ObserveReplayBlock(ReplayBlockObservation{Block: block19, ParentSlot: 18, ParentHash: block18.Hash})
	tracker.ObserveReplayBlock(ReplayBlockObservation{Block: block18, ParentSlot: 17, ParentHash: block17.Hash})

	// Chain not linked back past 17 yet: no skips derivable below it.
	if decision, ok := tracker.NextDecision(12); ok {
		t.Fatalf("expected no decision before ancestry reaches slot 17, got %+v", decision)
	}

	// Observing 17 (parent 12) closes the chain; 13-16 must become skips.
	tracker.ObserveReplayBlock(ReplayBlockObservation{Block: block17, ParentSlot: 12, ParentHash: chainTestHash(12)})

	for slot := uint64(13); slot <= 16; slot++ {
		decision, ok := tracker.NextDecision(slot - 1)
		if !ok {
			t.Fatalf("expected indirect skip decision for slot %d", slot)
		}
		if decision.Kind != ChainDecisionKindSkip || decision.Slot != slot || !decision.Indirect || decision.ViaFinalized != block17 {
			t.Fatalf("unexpected decision for slot %d: %+v", slot, decision)
		}
	}

	snap := tracker.Snapshot()
	if snap.IndirectSkips != 4 {
		t.Fatalf("expected 4 indirect skips, got snapshot %+v", snap)
	}
	if snap.FinalizedAncestorBlocks != 4 {
		t.Fatalf("expected 4 finalized ancestors (19, 18, 17, 12), got snapshot %+v", snap)
	}
}

// TestChainTrackerFinalizedAncestryWalkHandlesInOrderObservation covers the
// repair path delivering blocks oldest-first: ancestors observed before the
// finalize certificate arrives must still produce the deep skips.
func TestChainTrackerFinalizedAncestryWalkHandlesInOrderObservation(t *testing.T) {
	tracker := NewChainTracker()

	block17 := BlockID{Slot: 17, Hash: chainTestHash(17)}
	block18 := BlockID{Slot: 18, Hash: chainTestHash(18)}
	block20 := BlockID{Slot: 20, Hash: chainTestHash(20)}

	tracker.ObserveReplayBlock(ReplayBlockObservation{Block: block17, ParentSlot: 12, ParentHash: chainTestHash(12)})
	tracker.ObserveReplayBlock(ReplayBlockObservation{Block: block18, ParentSlot: 17, ParentHash: block17.Hash})
	tracker.ObserveReplayBlock(ReplayBlockObservation{Block: block20, ParentSlot: 18, ParentHash: block18.Hash})

	if _, err := tracker.ObserveCertificate(Certificate{
		Type:              CertificateFinalizeFast,
		Slot:              block20.Slot,
		BlockHash:         block20.Hash,
		SignatureVerified: true,
	}); err != nil {
		t.Fatalf("observe fast finalization certificate: %v", err)
	}

	// Gap 13-16 below ancestor 17, plus gap 19 between 18 and 20.
	for _, slot := range []uint64{13, 14, 15, 16, 19} {
		decision, ok := tracker.NextDecision(slot - 1)
		if !ok {
			t.Fatalf("expected indirect skip decision for slot %d", slot)
		}
		if decision.Kind != ChainDecisionKindSkip || decision.Slot != slot || !decision.Indirect {
			t.Fatalf("unexpected decision for slot %d: %+v", slot, decision)
		}
	}
}

func TestChainTrackerRejectsBlockCertificateWithEmptyHash(t *testing.T) {
	tracker := NewChainTracker()

	if _, err := tracker.ObserveCertificate(Certificate{
		Type:              CertificateNotarize,
		Slot:              11,
		SignatureVerified: true,
	}); err == nil {
		t.Fatalf("expected empty block hash to be rejected")
	}
}

func TestChainTrackerFinalizedAncestorUsesFinalizationCertType(t *testing.T) {
	tracker := NewChainTracker()

	type link struct {
		slot       uint64
		parentSlot uint64
		seed       byte
	}
	links := []link{
		{slot: 11, parentSlot: 10, seed: 11},
		{slot: 12, parentSlot: 11, seed: 12},
		{slot: 13, parentSlot: 12, seed: 13},
	}
	for _, entry := range links {
		blockID := BlockID{Slot: entry.slot, Hash: chainTestHash(entry.seed)}
		if _, err := tracker.ObserveCertificate(Certificate{
			Type:              CertificateNotarize,
			Slot:              blockID.Slot,
			BlockHash:         blockID.Hash,
			SignatureVerified: true,
		}); err != nil {
			t.Fatalf("observe notarize certificate for slot %d: %v", entry.slot, err)
		}
		parentHash := chainTestHash(10)
		if entry.parentSlot != 10 {
			parentHash = chainTestHash(byte(entry.parentSlot))
		}
		tracker.ObserveReplayBlock(ReplayBlockObservation{
			Block:      blockID,
			ParentSlot: entry.parentSlot,
			ParentHash: parentHash,
		})
	}

	tip := BlockID{Slot: 13, Hash: chainTestHash(13)}
	if _, err := tracker.ObserveCertificate(Certificate{
		Type:              CertificateFinalizeFast,
		Slot:              tip.Slot,
		BlockHash:         tip.Hash,
		SignatureVerified: true,
	}); err != nil {
		t.Fatalf("observe fast finalization certificate: %v", err)
	}

	for _, slot := range []uint64{11, 12, 13} {
		decision, ok := tracker.NextDecision(slot - 1)
		if !ok {
			t.Fatalf("expected decision for slot %d", slot)
		}
		if decision.Kind != ChainDecisionKindBlock || decision.Block.Slot != slot {
			t.Fatalf("unexpected decision for slot %d: %+v", slot, decision)
		}
		if decision.CertificateType != CertificateFinalizeFast {
			t.Fatalf("slot %d cert type = %q, want %q", slot, decision.CertificateType, CertificateFinalizeFast)
		}
	}
}

func TestChainTrackerFinalizedAncestorWithoutNotarizeCert(t *testing.T) {
	tracker := NewChainTracker()

	block11 := BlockID{Slot: 11, Hash: chainTestHash(11)}
	block12 := BlockID{Slot: 12, Hash: chainTestHash(12)}
	block13 := BlockID{Slot: 13, Hash: chainTestHash(13)}

	tracker.ObserveReplayBlock(ReplayBlockObservation{Block: block11, ParentSlot: 10, ParentHash: chainTestHash(10)})
	tracker.ObserveReplayBlock(ReplayBlockObservation{Block: block12, ParentSlot: 11, ParentHash: block11.Hash})
	tracker.ObserveReplayBlock(ReplayBlockObservation{Block: block13, ParentSlot: 12, ParentHash: block12.Hash})

	if _, err := tracker.ObserveCertificate(Certificate{
		Type:              CertificateFinalizeFast,
		Slot:              block13.Slot,
		BlockHash:         block13.Hash,
		SignatureVerified: true,
	}); err != nil {
		t.Fatalf("observe fast finalization certificate: %v", err)
	}

	for _, slot := range []uint64{11, 12} {
		decision, ok := tracker.NextDecision(slot - 1)
		if !ok {
			t.Fatalf("expected finalized-ancestor decision for slot %d", slot)
		}
		if decision.Kind != ChainDecisionKindBlock || decision.Block.Slot != slot || !decision.Observed {
			t.Fatalf("unexpected decision for slot %d: %+v", slot, decision)
		}
		if decision.CertificateType != CertificateFinalizeFast {
			t.Fatalf("slot %d cert type = %q, want %q", slot, decision.CertificateType, CertificateFinalizeFast)
		}
	}
}

func chainTestHash(seed byte) solana.Hash {
	var hash solana.Hash
	hash[0] = seed
	hash[31] = seed
	return hash
}
