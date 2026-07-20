package alpenglow

import "testing"

func TestConsensusPoolInvalidatesPreferredFallbackParent(t *testing.T) {
	pool := NewConsensusPool(DefaultConsensusPoolConfig())
	low := BlockID{Slot: 3, Hash: parentReadyHash(1)}
	high := BlockID{Slot: 3, Hash: parentReadyHash(2)}
	for _, block := range []BlockID{low, high} {
		update, err := pool.AddVerifiedCertificate(Certificate{
			Type: CertificateNotarizeFallback, Slot: block.Slot, BlockHash: block.Hash,
			StakeVerified: true, SignatureVerified: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(update.Certificates) != 1 {
			t.Fatalf("accepted fallback certificates = %+v", update.Certificates)
		}
	}
	if got := pool.BlockProductionParent(4); got.Kind != BlockProductionParentReady || got.Parent != low {
		t.Fatalf("pre-invalidation pool parent = %+v, want %v", got, low)
	}
	events := pool.InvalidateBlock(low)
	if !hasConsensusEvent(events, ConsensusEventParentReady, 4, high.Hash) {
		t.Fatalf("pool did not emit corrected parent-ready: %+v", events)
	}
	if got := pool.BlockProductionParent(4); got.Kind != BlockProductionParentReady || got.Parent != high {
		t.Fatalf("post-invalidation pool parent = %+v, want %v", got, high)
	}
	if pool.RestoreParentReady(4, low) {
		t.Fatal("pool restore reactivated invalid parent")
	}
}

func TestConsensusPoolRetainsInvalidTargetVoteAndCertificateEvidence(t *testing.T) {
	set, keys := testBLSValidatorSet(100, 100)
	pool := NewConsensusPool(DefaultConsensusPoolConfig())
	pool.NoteLiveSlot(2)
	invalid := BlockID{Slot: 3, Hash: parentReadyHash(1)}
	pool.InvalidateBlock(invalid)

	update, err := pool.AddVerifiedVote(poolVote(t, set, keys, 0, NewNotarizationFallbackVote(invalid.Slot, invalid.Hash), false))
	if err != nil {
		t.Fatal(err)
	}
	if !update.VoteAccepted {
		t.Fatal("verified invalid-target vote was erased instead of retained as evidence")
	}
	if len(update.Certificates) != 0 {
		t.Fatalf("invalid-target votes assembled an active certificate: %+v", update.Certificates)
	}
	if len(update.Events) != 0 {
		t.Fatalf("invalid-target evidence produced active events: %+v", update.Events)
	}
	if got := pool.BlockProductionParent(4); got.Kind == BlockProductionParentReady && got.Parent == invalid {
		t.Fatalf("invalid evidence reactivated production parent: %+v", got)
	}
}

func TestConsensusPoolCertificateAfterInvalidationRemainsEvidenceWithoutActions(t *testing.T) {
	pool := NewConsensusPool(DefaultConsensusPoolConfig())
	invalid := BlockID{Slot: 3, Hash: parentReadyHash(1)}
	pool.InvalidateBlock(invalid)
	update, err := pool.AddVerifiedCertificate(Certificate{
		Type: CertificateNotarizeFallback, Slot: invalid.Slot, BlockHash: invalid.Hash,
		StakeVerified: true, SignatureVerified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(update.Certificates) != 1 || update.Certificates[0].Key() != (CertificateKey{Type: CertificateNotarizeFallback, Slot: invalid.Slot, BlockHash: invalid.Hash}) {
		t.Fatalf("invalid-target certificate evidence = %+v", update.Certificates)
	}
	if len(update.Events) != 0 {
		t.Fatalf("invalid-target certificate produced active events: %+v", update.Events)
	}
}

func TestConsensusPoolInvalidParentsStillConsumeCertifiedBound(t *testing.T) {
	pool := NewConsensusPool(DefaultConsensusPoolConfig())
	const slot = uint64(3)
	for i := byte(1); i <= maxNotarFallbackBlocks; i++ {
		block := BlockID{Slot: slot, Hash: parentReadyHash(i)}
		if _, err := pool.AddVerifiedCertificate(Certificate{
			Type: CertificateNotarizeFallback, Slot: slot, BlockHash: block.Hash,
			StakeVerified: true, SignatureVerified: true,
		}); err != nil {
			t.Fatalf("insert fallback %d: %v", i, err)
		}
		pool.InvalidateBlock(block)
	}
	_, err := pool.AddVerifiedCertificate(Certificate{
		Type: CertificateNotarizeFallback, Slot: slot, BlockHash: parentReadyHash(99),
		StakeVerified: true, SignatureVerified: true,
	})
	if err == nil {
		t.Fatal("invalidating seven active parents reset the certified-block bound")
	}
	if got := pool.Snapshot().Certificates; got != maxNotarFallbackBlocks {
		t.Fatalf("retained certificates = %d, want %d", got, maxNotarFallbackBlocks)
	}
}
