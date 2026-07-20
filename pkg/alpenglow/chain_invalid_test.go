package alpenglow

import "testing"

func TestChainTrackerInvalidAncestorCascadesInBothArrivalOrders(t *testing.T) {
	for _, tc := range []struct {
		name       string
		childFirst bool
	}{
		{name: "parent-invalid-first"},
		{name: "child-observed-first", childFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracker := NewChainTracker()
			parent := BlockID{Slot: 20, Hash: chainTestHash(20)}
			child := BlockID{Slot: 24, Hash: chainTestHash(24)}
			observeChild := func() {
				update := tracker.ObserveReplayBlock(ReplayBlockObservation{
					Block:      child,
					ParentSlot: parent.Slot,
					ParentHash: parent.Hash,
				})
				if update.Conflict {
					t.Fatalf("fallback-free invalid ancestry is not safety evidence: %+v", update)
				}
			}

			if tc.childFirst {
				observeChild()
			}
			if err := tracker.ObserveObjectivelyInvalidBlock(parent, "duplicate transaction messages"); err != nil {
				t.Fatalf("invalidate parent: %v", err)
			}
			if !tc.childFirst {
				observeChild()
			}
			if !tracker.IsObjectivelyInvalidBlock(parent) || !tracker.IsObjectivelyInvalidBlock(child) {
				t.Fatalf("invalid ancestry did not cascade: parent=%v child=%v", tracker.IsObjectivelyInvalidBlock(parent), tracker.IsObjectivelyInvalidBlock(child))
			}
			if tracker.FinalityConflictAt(parent.Slot) || tracker.FinalityConflictAt(child.Slot) {
				t.Fatal("uncertified invalid ancestry incorrectly became a safety conflict")
			}
		})
	}
}

func TestChainTrackerInvalidFallbackIsNonfatalInBothArrivalOrders(t *testing.T) {
	for _, invalidateFirst := range []bool{false, true} {
		name := "certificate-before-invalidation"
		if invalidateFirst {
			name = "invalidation-before-certificate"
		}
		t.Run(name, func(t *testing.T) {
			tracker := NewChainTracker()
			block := BlockID{Slot: 30, Hash: chainTestHash(30)}
			if invalidateFirst {
				if err := tracker.ObserveObjectivelyInvalidBlock(block, "invalid body"); err != nil {
					t.Fatal(err)
				}
			}
			specObserve(t, tracker, Certificate{Type: CertificateNotarizeFallback, Slot: block.Slot, BlockHash: block.Hash})
			if !invalidateFirst {
				if err := tracker.ObserveObjectivelyInvalidBlock(block, "invalid body"); err != nil {
					t.Fatal(err)
				}
			}
			if tracker.FinalityConflictAt(block.Slot) {
				t.Fatal("fallback-only certificate on invalid block must remain nonfatal")
			}
			if _, _, ok := tracker.CertifiedBlockAt(block.Slot); ok {
				t.Fatal("invalid fallback block became a certified selection")
			}
			if wanted := tracker.WantedBlocks(block.Slot-1, 1); len(wanted) != 0 {
				t.Fatalf("invalid fallback block became a repair target: %+v", wanted)
			}
		})
	}
}

func TestChainTrackerInvalidDecisiveEvidenceFailsClosedInBothArrivalOrders(t *testing.T) {
	for _, invalidateFirst := range []bool{false, true} {
		name := "certificate-before-invalidation"
		if invalidateFirst {
			name = "invalidation-before-certificate"
		}
		t.Run(name, func(t *testing.T) {
			tracker := NewChainTracker()
			block := BlockID{Slot: 40, Hash: chainTestHash(40)}
			if invalidateFirst {
				if err := tracker.ObserveObjectivelyInvalidBlock(block, "invalid body"); err != nil {
					t.Fatal(err)
				}
			}
			specObserve(t, tracker, Certificate{Type: CertificateNotarize, Slot: block.Slot, BlockHash: block.Hash})
			if !invalidateFirst {
				if err := tracker.ObserveObjectivelyInvalidBlock(block, "invalid body"); err == nil {
					t.Fatal("decisive certificate followed by invalidation did not fail closed")
				}
			}
			if !tracker.FinalityConflictAt(block.Slot) {
				t.Fatal("invalid block with decisive certificate did not latch conflict")
			}
			if decision, ok := tracker.NextDecision(block.Slot - 1); !ok || decision.Kind != ChainDecisionKindConflict {
				t.Fatalf("invalid decisive block decision = %+v ok=%v", decision, ok)
			}
		})
	}
}

func TestChainTrackerInvalidFinalizedEvidenceFailsClosedInBothArrivalOrders(t *testing.T) {
	for _, invalidateFirst := range []bool{false, true} {
		name := "finalization-before-invalidation"
		if invalidateFirst {
			name = "invalidation-before-finalization"
		}
		t.Run(name, func(t *testing.T) {
			tracker := NewChainTracker()
			block := BlockID{Slot: 50, Hash: chainTestHash(50)}
			if invalidateFirst {
				if err := tracker.ObserveObjectivelyInvalidBlock(block, "invalid body"); err != nil {
					t.Fatal(err)
				}
			}
			specObserve(t, tracker, Certificate{Type: CertificateFinalizeFast, Slot: block.Slot, BlockHash: block.Hash})
			finalizeErr := tracker.ObserveFinalized(block, CertificateFinalizeFast)
			if !invalidateFirst {
				if finalizeErr != nil {
					t.Fatalf("pre-invalidation finalization: %v", finalizeErr)
				}
				if err := tracker.ObserveObjectivelyInvalidBlock(block, "invalid body"); err == nil {
					t.Fatal("finalized block followed by invalidation did not fail closed")
				}
			} else if finalizeErr == nil {
				t.Fatal("invalidation followed by finalization did not fail closed")
			}
			if !tracker.FinalityConflictAt(block.Slot) {
				t.Fatal("invalid finalized block did not latch conflict")
			}
		})
	}
}

func TestChainTrackerInvalidCertifiedCandidatesStillCountForSafety(t *testing.T) {
	for _, invalidateFirst := range []bool{false, true} {
		name := "fallback-then-invalid"
		if invalidateFirst {
			name = "invalid-then-fallback"
		}
		t.Run(name, func(t *testing.T) {
			tracker := NewChainTracker()
			fallback := BlockID{Slot: 60, Hash: chainTestHash(1)}
			finalized := BlockID{Slot: 60, Hash: chainTestHash(2)}
			if invalidateFirst {
				if err := tracker.ObserveObjectivelyInvalidBlock(fallback, "invalid body"); err != nil {
					t.Fatal(err)
				}
			}
			specObserve(t, tracker, Certificate{Type: CertificateNotarizeFallback, Slot: fallback.Slot, BlockHash: fallback.Hash})
			if !invalidateFirst {
				if err := tracker.ObserveObjectivelyInvalidBlock(fallback, "invalid body"); err != nil {
					t.Fatal(err)
				}
			}
			specObserve(t, tracker, Certificate{Type: CertificateFinalizeFast, Slot: finalized.Slot, BlockHash: finalized.Hash})
			if err := tracker.ObserveFinalized(finalized, CertificateFinalizeFast); err == nil {
				t.Fatal("finalized sibling ignored invalid certified competitor")
			}
			if !tracker.FinalityConflictAt(finalized.Slot) {
				t.Fatal("invalid certified competitor did not count in finality conflict")
			}
		})
	}
}

func TestChainTrackerInvalidCandidatesStillCountTowardCertifiedBound(t *testing.T) {
	tracker := NewChainTracker()
	const slot = uint64(70)
	for i := 0; i < maxCertifiedBlocksPerSlot; i++ {
		block := BlockID{Slot: slot, Hash: chainTestHash(byte(i + 1))}
		specObserve(t, tracker, Certificate{Type: CertificateNotarizeFallback, Slot: slot, BlockHash: block.Hash})
		if err := tracker.ObserveObjectivelyInvalidBlock(block, "invalid body"); err != nil {
			t.Fatalf("invalidate fallback %d: %v", i, err)
		}
	}
	eighth := BlockID{Slot: slot, Hash: chainTestHash(99)}
	specObserve(t, tracker, Certificate{Type: CertificateNotarizeFallback, Slot: slot, BlockHash: eighth.Hash})
	if !tracker.FinalityConflictAt(slot) {
		t.Fatal("eight certified identities escaped the protocol bound after seven were invalidated")
	}
}

func TestChainTrackerPrunesInvalidTombstones(t *testing.T) {
	tracker := NewChainTracker()
	parent := BlockID{Slot: 80, Hash: chainTestHash(80)}
	child := BlockID{Slot: 81, Hash: chainTestHash(81)}
	tracker.ObserveReplayBlock(ReplayBlockObservation{Block: child, ParentSlot: parent.Slot, ParentHash: parent.Hash})
	if err := tracker.ObserveObjectivelyInvalidBlock(parent, "invalid body"); err != nil {
		t.Fatal(err)
	}
	tracker.PruneBeforeSlot(child.Slot)
	if tracker.IsObjectivelyInvalidBlock(parent) {
		t.Fatal("invalid parent tombstone survived pruning below durable root")
	}
	if !tracker.IsObjectivelyInvalidBlock(child) {
		t.Fatal("invalid child at durable root was pruned too early")
	}
	tracker.PruneBeforeSlot(child.Slot + 1)
	if tracker.IsObjectivelyInvalidBlock(child) {
		t.Fatal("invalid child tombstone survived pruning below durable root")
	}
}
