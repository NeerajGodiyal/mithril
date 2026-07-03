package alpenglow

import (
	"testing"

	"github.com/gagliardetto/solana-go"
)

// A long run must not grow the tracker's maps without bound: finalizing a high slot
// prunes settled state well behind the watermark, while recent state is retained.
func TestChainTrackerPrunesBehindFinality(t *testing.T) {
	tracker := NewChainTracker()

	// Finalize an old slot, then finalize a slot far ahead — the old one is settled
	// and should be pruned (> chainTrackerRetentionSlots behind).
	oldSlot := uint64(10)
	newSlot := uint64(oldSlot + chainTrackerRetentionSlots + 100)
	feed := func(c Certificate) {
		c.SignatureVerified, c.StakeVerified = true, true
		if _, err := tracker.ObserveCertificate(c); err != nil {
			t.Fatalf("observe %s slot %d: %v", c.Type, c.Slot, err)
		}
	}
	feed(Certificate{Type: CertificateFinalizeFast, Slot: oldSlot, BlockHash: solana.Hash{0xA}})
	feed(Certificate{Type: CertificateFinalizeFast, Slot: newSlot, BlockHash: solana.Hash{0xB}})

	tracker.mu.RLock()
	defer tracker.mu.RUnlock()
	// The old slot's block/cert must be gone; the recent one retained.
	for id := range tracker.blocks {
		if id.Slot == oldSlot {
			t.Fatalf("old slot %d block was not pruned behind finality", oldSlot)
		}
	}
	if _, ok := tracker.directFinalized[BlockID{Slot: oldSlot, Hash: solana.Hash{0xA}}]; ok {
		t.Fatalf("old finalized slot %d not pruned", oldSlot)
	}
	if _, ok := tracker.directFinalized[BlockID{Slot: newSlot, Hash: solana.Hash{0xB}}]; !ok {
		t.Fatalf("recent finalized slot %d was pruned (retention window too small)", newSlot)
	}
}

// Byzantine conflict evidence must survive pruning: the promotion gate fails closed
// on a conflicted slot even when the executed tip reaches it long after the cert
// watermark (and its pruning) passed.
func TestChainTrackerConflictsSurvivePruning(t *testing.T) {
	tracker := NewChainTracker()
	feed := func(c Certificate) {
		c.SignatureVerified, c.StakeVerified = true, true
		_, _ = tracker.ObserveCertificate(c)
	}
	// Equivocation at slot 5: two notarized blocks -> conflict recorded.
	feed(Certificate{Type: CertificateNotarize, Slot: 5, BlockHash: solana.Hash{0xA}})
	feed(Certificate{Type: CertificateNotarize, Slot: 5, BlockHash: solana.Hash{0xB}})
	if !tracker.FinalityConflictAt(5) {
		t.Fatal("conflict at slot 5 not recorded")
	}
	// Watermark races far ahead -> auto-prune fires well past slot 5.
	feed(Certificate{Type: CertificateFinalizeFast, Slot: 5 + chainTrackerRetentionSlots + 100, BlockHash: solana.Hash{0xC}})

	if !tracker.FinalityConflictAt(5) {
		t.Fatal("pruning erased Byzantine conflict evidence at slot 5")
	}
}

// Write-once evidence: after pruning rebuilt a conflicted slot's state, a re-observed
// cert (honest footer re-ingest or QUIC replay) must NOT erase the conflict flag.
func TestChainTrackerConflictSurvivesCertReobservation(t *testing.T) {
	tracker := NewChainTracker()
	feed := func(c Certificate) {
		c.SignatureVerified, c.StakeVerified = true, true
		_, _ = tracker.ObserveCertificate(c)
	}
	feed(Certificate{Type: CertificateNotarize, Slot: 5, BlockHash: solana.Hash{0xA}})
	feed(Certificate{Type: CertificateNotarize, Slot: 5, BlockHash: solana.Hash{0xB}})
	// Prune wipes slot 5's certs/blocks but keeps the conflict...
	feed(Certificate{Type: CertificateFinalizeFast, Slot: 5 + chainTrackerRetentionSlots + 100, BlockHash: solana.Hash{0xC}})
	// ...then ONE of the original certs is re-observed (dedup key was pruned).
	feed(Certificate{Type: CertificateNotarize, Slot: 5, BlockHash: solana.Hash{0xA}})

	if !tracker.FinalityConflictAt(5) {
		t.Fatal("re-observed cert erased the recorded conflict (write-once violated)")
	}
}

// Explicit PruneBeforeSlot drops everything strictly below the given slot.
func TestChainTrackerPruneBeforeSlotExplicit(t *testing.T) {
	tracker := NewChainTracker()
	feed := func(c Certificate) {
		c.SignatureVerified, c.StakeVerified = true, true
		_, _ = tracker.ObserveCertificate(c)
	}
	for slot := uint64(1); slot <= 20; slot++ {
		feed(Certificate{Type: CertificateNotarize, Slot: slot, BlockHash: solana.Hash{byte(slot)}})
	}
	tracker.PruneBeforeSlot(15)

	tracker.mu.RLock()
	defer tracker.mu.RUnlock()
	for k := range tracker.certificates {
		if k.Slot < 15 {
			t.Fatalf("cert for slot %d survived PruneBeforeSlot(15)", k.Slot)
		}
	}
	if len(tracker.certificates) == 0 {
		t.Fatal("PruneBeforeSlot removed everything — slots 15..20 should remain")
	}
}
