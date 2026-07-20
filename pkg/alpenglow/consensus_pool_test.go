package alpenglow

import (
	"math/big"
	"reflect"
	"testing"

	"github.com/gagliardetto/solana-go"
)

func TestConsensusPoolBuildsAgaveCertificatesFromVerifiedVotes(t *testing.T) {
	set, keys := testBLSValidatorSet(100, 40, 35, 25)
	pool := NewConsensusPool(DefaultConsensusPoolConfig())
	pool.NoteLiveSlot(10)
	blockHash := parentReadyHash(7)

	update, err := pool.AddVerifiedVote(poolVote(t, set, keys, 0, NewNotarizationVote(11, blockHash), false))
	if err != nil {
		t.Fatal(err)
	}
	if len(update.Certificates) != 0 {
		t.Fatalf("40%% unexpectedly formed certificate: %+v", update.Certificates)
	}
	update, err = pool.AddVerifiedVote(poolVote(t, set, keys, 1, NewNotarizationVote(11, blockHash), false))
	if err != nil {
		t.Fatal(err)
	}
	assertCertificateTypes(t, update.Certificates, CertificateNotarize, CertificateNotarizeFallback)
	for _, cert := range update.Certificates {
		if _, _, err := verifyCertificateWithSet(set, cert, true); err != nil {
			t.Fatalf("locally assembled %s certificate failed verification: %v", cert.Type, err)
		}
	}
	update, err = pool.AddVerifiedVote(poolVote(t, set, keys, 2, NewNotarizationVote(11, blockHash), false))
	if err != nil {
		t.Fatal(err)
	}
	assertCertificateTypes(t, update.Certificates, CertificateFinalizeFast)
	if len(update.Events) == 0 || !hasConsensusEvent(update.Events, ConsensusEventFinalized, 11, blockHash) {
		t.Fatalf("missing fast-finalized event: %+v", update.Events)
	}
}

func TestConsensusPoolSlowFinalizationNeedsNotarizeAndFinalize(t *testing.T) {
	pool := NewConsensusPool(DefaultConsensusPoolConfig())
	hash := parentReadyHash(5)
	finalize := Certificate{Type: CertificateFinalize, Slot: 20, StakeVerified: true, SignatureVerified: true}
	update, err := pool.AddVerifiedCertificate(finalize)
	if err != nil {
		t.Fatal(err)
	}
	if hasConsensusEvent(update.Events, ConsensusEventFinalized, 20, hash) {
		t.Fatal("finalize certificate alone selected a block")
	}
	notarize := Certificate{Type: CertificateNotarize, Slot: 20, BlockHash: hash, StakeVerified: true, SignatureVerified: true}
	update, err = pool.AddVerifiedCertificate(notarize)
	if err != nil {
		t.Fatal(err)
	}
	if !hasConsensusEvent(update.Events, ConsensusEventFinalized, 20, hash) {
		t.Fatalf("missing slow-finalized event: %+v", update.Events)
	}
}

func TestConsensusPoolFastFinalizationUpgradesSlowFinalization(t *testing.T) {
	pool := NewConsensusPool(DefaultConsensusPoolConfig())
	hash := parentReadyHash(15)
	for _, cert := range []Certificate{
		{Type: CertificateFinalize, Slot: 20, StakeVerified: true, SignatureVerified: true},
		{Type: CertificateNotarize, Slot: 20, BlockHash: hash, StakeVerified: true, SignatureVerified: true},
	} {
		if _, err := pool.AddVerifiedCertificate(cert); err != nil {
			t.Fatal(err)
		}
	}
	update, err := pool.AddVerifiedCertificate(Certificate{
		Type: CertificateFinalizeFast, Slot: 20, BlockHash: hash,
		StakeVerified: true, SignatureVerified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(update.Events) == 0 || !update.Events[len(update.Events)-1].Fast {
		t.Fatalf("fast certificate did not upgrade slow finalization: %+v", update.Events)
	}
}

func TestConsensusPoolCertificateAssemblyOrderMatchesAgave(t *testing.T) {
	set, keys := testBLSValidatorSet(100, 100)
	pool := NewConsensusPool(DefaultConsensusPoolConfig())
	pool.NoteLiveSlot(10)
	update, err := pool.AddVerifiedVote(poolVote(t, set, keys, 0, NewNotarizationVote(11, parentReadyHash(7)), false))
	if err != nil {
		t.Fatal(err)
	}
	want := []CertificateType{CertificateNotarize, CertificateNotarizeFallback, CertificateFinalizeFast}
	if len(update.Certificates) != len(want) {
		t.Fatalf("certificates = %v, want %v", update.Certificates, want)
	}
	for i, cert := range update.Certificates {
		if cert.Type != want[i] {
			t.Fatalf("certificate %d = %s, want %s", i, cert.Type, want[i])
		}
	}
}

func TestConsensusPoolSlowFinalizationFailsClosedOnTwinNotarizeCertificates(t *testing.T) {
	pool := NewConsensusPool(DefaultConsensusPoolConfig())
	for _, hash := range []solana.Hash{parentReadyHash(5), parentReadyHash(6)} {
		_, err := pool.AddVerifiedCertificate(Certificate{
			Type: CertificateNotarize, Slot: 20, BlockHash: hash,
			StakeVerified: true, SignatureVerified: true,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	update, err := pool.AddVerifiedCertificate(Certificate{
		Type: CertificateFinalize, Slot: 20,
		StakeVerified: true, SignatureVerified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasConsensusEvent(update.Events, ConsensusEventConflict, 20, solana.Hash{}) {
		t.Fatalf("missing conflict event: %+v", update.Events)
	}
	if got := pool.Snapshot().FinalizedBlocks; got != 0 {
		t.Fatalf("finalized blocks = %d, want 0", got)
	}
	if got := pool.Snapshot().ConflictingSlots; got != 1 {
		t.Fatalf("conflicting slots = %d, want 1", got)
	}
}

func TestConsensusPoolDetectsLateTwinAfterSlowFinalization(t *testing.T) {
	pool := NewConsensusPool(DefaultConsensusPoolConfig())
	first := Certificate{
		Type: CertificateNotarize, Slot: 20, BlockHash: parentReadyHash(5),
		StakeVerified: true, SignatureVerified: true,
	}
	finalize := Certificate{Type: CertificateFinalize, Slot: 20, StakeVerified: true, SignatureVerified: true}
	if _, err := pool.AddVerifiedCertificate(finalize); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.AddVerifiedCertificate(first); err != nil {
		t.Fatal(err)
	}
	update, err := pool.AddVerifiedCertificate(Certificate{
		Type: CertificateNotarize, Slot: 20, BlockHash: parentReadyHash(6),
		StakeVerified: true, SignatureVerified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasConsensusEvent(update.Events, ConsensusEventConflict, 20, solana.Hash{}) {
		t.Fatalf("late twin did not produce conflict: %+v", update.Events)
	}
	if got := pool.Snapshot().FinalizedBlocks; got != 0 {
		t.Fatalf("late twin left %d finalized blocks", got)
	}
}

func TestConsensusPoolRejectedFallbackCertificateIsNotRetained(t *testing.T) {
	pool := NewConsensusPool(DefaultConsensusPoolConfig())
	for i := byte(1); i <= maxNotarFallbackBlocks; i++ {
		update, err := pool.AddVerifiedCertificate(Certificate{
			Type: CertificateNotarizeFallback, Slot: 20, BlockHash: parentReadyHash(i),
			StakeVerified: true, SignatureVerified: true,
		})
		if err != nil {
			t.Fatalf("insert fallback %d: %v", i, err)
		}
		if len(update.Certificates) != 1 {
			t.Fatalf("fallback %d accepted certificates = %d", i, len(update.Certificates))
		}
	}
	_, err := pool.AddVerifiedCertificate(Certificate{
		Type: CertificateNotarizeFallback, Slot: 20, BlockHash: parentReadyHash(99),
		StakeVerified: true, SignatureVerified: true,
	})
	if err == nil {
		t.Fatal("eighth fallback certificate was accepted")
	}
	if got := pool.Snapshot().Certificates; got != maxNotarFallbackBlocks {
		t.Fatalf("retained certificates = %d, want %d", got, maxNotarFallbackBlocks)
	}
}

func TestConsensusPoolDoesNotRegrowPrunedCertificates(t *testing.T) {
	pool := NewConsensusPool(DefaultConsensusPoolConfig())
	pool.SetRoot(BlockID{Slot: 20, Hash: parentReadyHash(20)})
	update, err := pool.AddVerifiedCertificate(Certificate{
		Type: CertificateSkip, Slot: 10, StakeVerified: true, SignatureVerified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(update.Events) != 0 || pool.Snapshot().Certificates != 0 {
		t.Fatalf("pruned certificate regrew pool state: update=%+v snapshot=%+v", update, pool.Snapshot())
	}
}

func TestConsensusPoolPrunesBehindFinalityButRetainsRewardWindow(t *testing.T) {
	set, keys := testBLSValidatorSet(100, 10, 90)
	pool := NewConsensusPool(DefaultConsensusPoolConfig())
	pool.NoteLiveSlot(100)
	if _, err := pool.AddVerifiedVote(poolVote(t, set, keys, 0, NewSkipVote(83), false)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.AddVerifiedVote(poolVote(t, set, keys, 0, NewSkipVote(92), false)); err != nil {
		t.Fatal(err)
	}

	pool.PruneBehindFinality(100) // default retention 16 -> floor 84
	snapshot := pool.Snapshot()
	if snapshot.RootSlot != 84 || snapshot.TrackedSlots != 1 {
		t.Fatalf("snapshot after finalized pruning = %+v", snapshot)
	}
	before := snapshot.RejectedVotes
	if _, err := pool.AddVerifiedVote(poolVote(t, set, keys, 1, NewSkipVote(84), false)); err != nil {
		t.Fatal(err)
	}
	if got := pool.Snapshot().RejectedVotes; got != before+1 {
		t.Fatalf("vote at finalized floor was not rejected: before=%d after=%d", before, got)
	}
}

func TestConsensusPoolVoteAdmissionDispositions(t *testing.T) {
	set, keys := testBLSValidatorSet(100, 50, 50)

	t.Run("accepted and exact duplicate", func(t *testing.T) {
		pool := NewConsensusPool(ConsensusPoolConfig{MaxVerifiedVotes: 1})
		pool.NoteLiveSlot(10)
		vote := poolVote(t, set, keys, 0, NewSkipVote(11), false)

		update, err := pool.AddVerifiedVote(vote)
		if err != nil {
			t.Fatal(err)
		}
		if update.VoteAdmission != VoteAdmissionAccepted || !update.VoteAccepted {
			t.Fatalf("accepted vote update = %+v", update)
		}

		// An exact logical duplicate remains distinguishable even when the vote
		// capacity is full. It was already admitted and must not be mistaken for
		// a new bounded-pool rejection.
		update, err = pool.AddVerifiedVote(vote)
		if err != nil {
			t.Fatal(err)
		}
		if update.VoteAdmission != VoteAdmissionExactDuplicate || update.VoteAccepted {
			t.Fatalf("duplicate vote update = %+v", update)
		}
		if snapshot := pool.Snapshot(); snapshot.DuplicateVotes != 1 || snapshot.RejectedVotes != 0 {
			t.Fatalf("duplicate vote counters = %+v", snapshot)
		}
	})

	t.Run("network-first local duplicate restores safe actions at capacity", func(t *testing.T) {
		pool := NewConsensusPool(ConsensusPoolConfig{MaxVerifiedVotes: 2})
		pool.NoteLiveSlot(12)
		ours := poolVote(t, set, keys, 0, NewNotarizationVote(12, parentReadyHash(12)), false)
		peerSkip := poolVote(t, set, keys, 1, NewSkipVote(12), false)

		update, err := pool.AddVerifiedVote(ours)
		if err != nil || len(update.Events) != 0 {
			t.Fatalf("network-first local-rank vote update=%+v err=%v", update, err)
		}
		update, err = pool.AddVerifiedVote(peerSkip)
		if err != nil || len(update.Events) != 0 {
			t.Fatalf("threshold before local provenance update=%+v err=%v", update, err)
		}

		ours.Local = true
		update, err = pool.AddVerifiedVote(ours)
		if err != nil {
			t.Fatal(err)
		}
		if update.VoteAdmission != VoteAdmissionExactDuplicate || update.VoteAccepted {
			t.Fatalf("local duplicate update = %+v", update)
		}
		if len(update.Events) != 1 || update.Events[0].Kind != ConsensusEventSafeToSkip || update.Events[0].Slot != 12 {
			t.Fatalf("local duplicate did not restore safe action: %+v", update.Events)
		}
		snapshot := pool.Snapshot()
		if snapshot.VerifiedVotes != 2 || snapshot.DuplicateVotes != 1 || snapshot.RejectedVotes != 0 {
			t.Fatalf("local duplicate changed tally/capacity counters: %+v", snapshot)
		}

		update, err = pool.AddVerifiedVote(ours)
		if err != nil {
			t.Fatal(err)
		}
		if update.VoteAdmission != VoteAdmissionExactDuplicate || len(update.Events) != 0 {
			t.Fatalf("repeat local duplicate re-emitted safe action: %+v", update)
		}
	})

	t.Run("rooted", func(t *testing.T) {
		pool := NewConsensusPool(ConsensusPoolConfig{RootBlock: BlockID{Slot: 10}})
		update, err := pool.AddVerifiedVote(poolVote(t, set, keys, 0, NewSkipVote(10), false))
		if err != nil {
			t.Fatal(err)
		}
		assertRejectedVoteAdmission(t, pool, update, VoteAdmissionRooted)
	})

	t.Run("too far ahead", func(t *testing.T) {
		pool := NewConsensusPool(ConsensusPoolConfig{MaxSlotsAhead: 1})
		pool.NoteLiveSlot(10)
		update, err := pool.AddVerifiedVote(poolVote(t, set, keys, 0, NewSkipVote(12), false))
		if err != nil {
			t.Fatal(err)
		}
		assertRejectedVoteAdmission(t, pool, update, VoteAdmissionTooFarAhead)
	})

	t.Run("tracked slot capacity", func(t *testing.T) {
		pool := NewConsensusPool(ConsensusPoolConfig{MaxTrackedSlots: 1})
		pool.NoteLiveSlot(10)
		if update, err := pool.AddVerifiedVote(poolVote(t, set, keys, 0, NewSkipVote(11), false)); err != nil || !update.VoteAccepted {
			t.Fatalf("seed vote update=%+v err=%v", update, err)
		}
		update, err := pool.AddVerifiedVote(poolVote(t, set, keys, 1, NewSkipVote(12), false))
		if err != nil {
			t.Fatal(err)
		}
		assertRejectedVoteAdmission(t, pool, update, VoteAdmissionTrackedSlotCapacity)
	})

	t.Run("vote capacity", func(t *testing.T) {
		pool := NewConsensusPool(ConsensusPoolConfig{MaxVerifiedVotes: 1})
		pool.NoteLiveSlot(10)
		if update, err := pool.AddVerifiedVote(poolVote(t, set, keys, 0, NewSkipVote(11), false)); err != nil || !update.VoteAccepted {
			t.Fatalf("seed vote update=%+v err=%v", update, err)
		}
		update, err := pool.AddVerifiedVote(poolVote(t, set, keys, 1, NewSkipVote(12), false))
		if err != nil {
			t.Fatal(err)
		}
		assertRejectedVoteAdmission(t, pool, update, VoteAdmissionVoteCapacity)
		if got := pool.Snapshot().TrackedSlots; got != 1 {
			t.Fatalf("vote-capacity rejection created an empty slot state: tracked=%d", got)
		}
	})

	t.Run("conflict", func(t *testing.T) {
		pool := NewConsensusPool(DefaultConsensusPoolConfig())
		pool.NoteLiveSlot(10)
		if update, err := pool.AddVerifiedVote(poolVote(t, set, keys, 0, NewSkipVote(11), false)); err != nil || !update.VoteAccepted {
			t.Fatalf("seed vote update=%+v err=%v", update, err)
		}
		update, err := pool.AddVerifiedVote(poolVote(t, set, keys, 0, NewNotarizationVote(11, parentReadyHash(1)), false))
		if err != nil {
			t.Fatal(err)
		}
		assertRejectedVoteAdmission(t, pool, update, VoteAdmissionConflict)
		if len(pool.Evidence()) != 1 {
			t.Fatalf("conflicting vote evidence = %+v", pool.Evidence())
		}
	})
}

func TestConsensusPoolConflictEvidencePrecedesFullCapacity(t *testing.T) {
	set, keys := testBLSValidatorSet(100, 50, 50)
	pool := NewConsensusPool(ConsensusPoolConfig{MaxVerifiedVotes: 1})
	pool.NoteLiveSlot(10)
	if update, err := pool.AddVerifiedVote(poolVote(t, set, keys, 0, NewSkipVote(11), false)); err != nil || !update.VoteAccepted {
		t.Fatalf("fill vote update=%+v err=%v", update, err)
	}
	update, err := pool.AddVerifiedVote(poolVote(t, set, keys, 0, NewNotarizationVote(11, parentReadyHash(1)), false))
	if err != nil {
		t.Fatal(err)
	}
	if update.VoteAdmission != VoteAdmissionConflict {
		t.Fatalf("full-capacity conflict admission = %s", update.VoteAdmission)
	}
	if evidence := pool.Evidence(); len(evidence) != 1 || evidence[0].Slot != 11 || evidence[0].Rank != 0 {
		t.Fatalf("full-capacity conflict evidence = %+v", evidence)
	}
}

func TestConsensusPoolRootPruningReleasesActiveVoteCapacity(t *testing.T) {
	set, keys := testBLSValidatorSet(100, 50, 50)
	pool := NewConsensusPool(ConsensusPoolConfig{MaxVerifiedVotes: 1})
	pool.NoteLiveSlot(10)
	if update, err := pool.AddVerifiedVote(poolVote(t, set, keys, 0, NewSkipVote(11), false)); err != nil || !update.VoteAccepted {
		t.Fatalf("first vote update=%+v err=%v", update, err)
	}
	if update, err := pool.AddVerifiedVote(poolVote(t, set, keys, 1, NewSkipVote(12), false)); err != nil || update.VoteAdmission != VoteAdmissionVoteCapacity {
		t.Fatalf("capacity update=%+v err=%v", update, err)
	}
	pool.SetRoot(BlockID{Slot: 11, Hash: parentReadyHash(11)})
	update, err := pool.AddVerifiedVote(poolVote(t, set, keys, 1, NewSkipVote(12), false))
	if err != nil || !update.VoteAccepted {
		t.Fatalf("post-prune vote update=%+v err=%v", update, err)
	}
	if snapshot := pool.Snapshot(); snapshot.TrackedSlots != 1 || snapshot.VerifiedVotes != 2 {
		t.Fatalf("post-prune pool snapshot = %+v", snapshot)
	}
}

func assertRejectedVoteAdmission(t *testing.T, pool *ConsensusPool, update ConsensusUpdate, want VoteAdmissionDisposition) {
	t.Helper()
	if update.VoteAdmission != want || update.VoteAccepted {
		t.Fatalf("vote admission update = %+v, want disposition %q and VoteAccepted=false", update, want)
	}
	if got := pool.Snapshot().RejectedVotes; got != 1 {
		t.Fatalf("rejected vote count = %d, want 1", got)
	}
}

func TestConsensusPoolRejectsVerifiedConflictingVotes(t *testing.T) {
	set, keys := testBLSValidatorSet(100, 100)
	pool := NewConsensusPool(DefaultConsensusPoolConfig())
	pool.NoteLiveSlot(30)
	if _, err := pool.AddVerifiedVote(poolVote(t, set, keys, 0, NewSkipVote(31), false)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.AddVerifiedVote(poolVote(t, set, keys, 0, NewNotarizationVote(31, parentReadyHash(1)), false)); err != nil {
		t.Fatal(err)
	}
	if len(pool.Evidence()) != 1 {
		t.Fatalf("evidence = %+v", pool.Evidence())
	}
	if got := pool.Snapshot().ConflictingVotes; got != 1 {
		t.Fatalf("conflicting votes = %d, want 1", got)
	}
}

func TestConflictingVoteTypesMatchAgave(t *testing.T) {
	tests := map[VoteType][]VoteType{
		VoteTypeFinalize:         {VoteTypeNotarizeFallback, VoteTypeSkip, VoteTypeSkipFallback, VoteTypeGenesis},
		VoteTypeNotarize:         {VoteTypeSkip, VoteTypeNotarizeFallback, VoteTypeGenesis},
		VoteTypeNotarizeFallback: {VoteTypeFinalize, VoteTypeNotarize, VoteTypeGenesis},
		VoteTypeSkip:             {VoteTypeFinalize, VoteTypeNotarize, VoteTypeSkipFallback, VoteTypeGenesis},
		VoteTypeSkipFallback:     {VoteTypeSkip, VoteTypeFinalize, VoteTypeGenesis},
		VoteTypeGenesis:          {VoteTypeFinalize, VoteTypeNotarize, VoteTypeNotarizeFallback, VoteTypeSkip, VoteTypeSkipFallback},
	}
	for voteType, want := range tests {
		if got := conflictingVoteTypes(voteType); !reflect.DeepEqual(got, want) {
			t.Fatalf("conflictingVoteTypes(%s) = %v, want %v", voteType, got, want)
		}
	}
}

func TestConsensusPoolSafeEventsRequireLocalVote(t *testing.T) {
	set, keys := testBLSValidatorSet(100, 10, 40, 50)
	pool := NewConsensusPool(DefaultConsensusPoolConfig())
	pool.NoteLiveSlot(39)
	target := parentReadyHash(8)
	if _, err := pool.AddVerifiedVote(poolVote(t, set, keys, 0, NewSkipVote(40), true)); err != nil {
		t.Fatal(err)
	}
	update, err := pool.AddVerifiedVote(poolVote(t, set, keys, 1, NewNotarizationVote(40, target), false))
	if err != nil {
		t.Fatal(err)
	}
	if !hasConsensusEvent(update.Events, ConsensusEventSafeToNotar, 40, target) {
		t.Fatalf("missing safe-to-notar event: %+v", update.Events)
	}
}

func TestConsensusPoolRestoredLocalFirstVoteBeforeAndAfterTallies(t *testing.T) {
	set, keys := testBLSValidatorSet(100, 10, 40, 50)
	first := NewNotarizationVote(40, parentReadyHash(1))

	t.Run("seed before slot state", func(t *testing.T) {
		pool := NewConsensusPool(DefaultConsensusPoolConfig())
		pool.NoteLiveSlot(39)
		update, err := pool.RestoreLocalFirstVote(first, 0)
		if err != nil || len(update.Events) != 0 {
			t.Fatalf("restore before state update=%+v err=%v", update, err)
		}
		update, err = pool.AddVerifiedVote(poolVote(t, set, keys, 1, NewSkipVote(40), false))
		if err != nil {
			t.Fatal(err)
		}
		if !hasConsensusEvent(update.Events, ConsensusEventSafeToSkip, 40, solana.Hash{}) {
			t.Fatalf("pending local-first provenance did not trigger safe-to-skip: %+v", update.Events)
		}
		if snapshot := pool.Snapshot(); snapshot.VerifiedVotes != 1 {
			t.Fatalf("provenance consumed verified-vote capacity: %+v", snapshot)
		}
	})

	t.Run("seed after threshold", func(t *testing.T) {
		pool := NewConsensusPool(DefaultConsensusPoolConfig())
		pool.NoteLiveSlot(39)
		update, err := pool.AddVerifiedVote(poolVote(t, set, keys, 1, NewSkipVote(40), false))
		if err != nil || len(update.Events) != 0 {
			t.Fatalf("network threshold before provenance update=%+v err=%v", update, err)
		}
		update, err = pool.RestoreLocalFirstVote(first, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !hasConsensusEvent(update.Events, ConsensusEventSafeToSkip, 40, solana.Hash{}) {
			t.Fatalf("late local-first provenance did not reevaluate safety: %+v", update.Events)
		}
		update, err = pool.RestoreLocalFirstVote(first, 0)
		if err != nil || len(update.Events) != 0 {
			t.Fatalf("idempotent provenance update=%+v err=%v", update, err)
		}
	})
}

func TestConsensusPoolFallbackCannotEstablishLocalFirstVote(t *testing.T) {
	set, keys := testBLSValidatorSet(100, 10, 40, 50)
	pool := NewConsensusPool(DefaultConsensusPoolConfig())
	pool.NoteLiveSlot(39)
	fallback := poolVote(t, set, keys, 0, NewNotarizationFallbackVote(40, parentReadyHash(2)), true)
	update, err := pool.AddVerifiedVote(fallback)
	if err != nil || len(update.Events) != 0 {
		t.Fatalf("local fallback update=%+v err=%v", update, err)
	}
	update, err = pool.AddVerifiedVote(poolVote(t, set, keys, 1, NewSkipVote(40), false))
	if err != nil {
		t.Fatal(err)
	}
	if hasConsensusEvent(update.Events, ConsensusEventSafeToSkip, 40, solana.Hash{}) {
		t.Fatalf("fallback vote incorrectly became local first vote: %+v", update.Events)
	}

	first := NewNotarizationVote(40, parentReadyHash(1))
	update, err = pool.RestoreLocalFirstVote(first, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasConsensusEvent(update.Events, ConsensusEventSafeToSkip, 40, solana.Hash{}) {
		t.Fatalf("durable round-one provenance did not replace fallback ordering: %+v", update.Events)
	}
}

func TestOnlyRoundOneVotesEstablishLocalFirstVote(t *testing.T) {
	tests := map[VoteType]bool{
		VoteTypeNotarize:         true,
		VoteTypeSkip:             true,
		VoteTypeFinalize:         false,
		VoteTypeNotarizeFallback: false,
		VoteTypeSkipFallback:     false,
		VoteTypeGenesis:          false,
	}
	for voteType, want := range tests {
		if got := voteEstablishesLocalFirst(voteType); got != want {
			t.Fatalf("voteEstablishesLocalFirst(%s) = %t, want %t", voteType, got, want)
		}
	}
}

func TestConsensusPoolRestoredLocalFirstVoteConflictAndRootPruning(t *testing.T) {
	set, keys := testBLSValidatorSet(100, 10, 40, 50)
	networkFirst := NewConsensusPool(DefaultConsensusPoolConfig())
	networkFirst.NoteLiveSlot(39)
	conflicting := NewNotarizationVote(40, parentReadyHash(2))
	if _, err := networkFirst.AddVerifiedVote(poolVote(t, set, keys, 0, conflicting, false)); err != nil {
		t.Fatal(err)
	}
	if _, err := networkFirst.RestoreLocalFirstVote(NewNotarizationVote(40, parentReadyHash(1)), 0); err == nil {
		t.Fatal("durable local provenance ignored a conflicting verified vote from its own rank")
	}
	if _, retained := networkFirst.localFirstVotes[40]; retained {
		t.Fatal("conflicting own-rank vote installed local provenance")
	}

	pool := NewConsensusPool(DefaultConsensusPoolConfig())
	first := NewNotarizationVote(40, parentReadyHash(1))
	if _, err := pool.RestoreLocalFirstVote(first, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.RestoreLocalFirstVote(NewSkipVote(40), 0); err == nil {
		t.Fatal("conflicting durable first-vote provenance was accepted")
	}
	if _, err := pool.AddVerifiedVote(poolVote(t, set, keys, 0, NewSkipVote(40), true)); err == nil {
		t.Fatal("conflicting local base vote was accepted")
	}
	if snapshot := pool.Snapshot(); snapshot.TrackedSlots != 0 || snapshot.VerifiedVotes != 0 {
		t.Fatalf("conflicting local provenance left an empty tracked slot: %+v", snapshot)
	}
	pool.SetRoot(BlockID{Slot: 40, Hash: parentReadyHash(3)})
	pool.mu.Lock()
	_, retained := pool.localFirstVotes[40]
	pool.mu.Unlock()
	if retained {
		t.Fatal("rooted local first-vote provenance was retained")
	}
}

func TestConsensusPoolRestoreParentReadyPreservesAndAdvancesRootSafely(t *testing.T) {
	set, keys := testBLSValidatorSet(100, 40, 60)
	root := BlockID{Slot: 10, Hash: parentReadyHash(10)}
	pool := NewConsensusPool(ConsensusPoolConfig{RootBlock: root})
	pool.NoteLiveSlot(10)
	if _, err := pool.RestoreLocalFirstVote(NewSkipVote(11), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.AddVerifiedVote(poolVote(t, set, keys, 0, NewSkipVote(11), false)); err != nil {
		t.Fatal(err)
	}

	if pool.RestoreParentReady(12, BlockID{Slot: 9, Hash: parentReadyHash(9)}) {
		t.Fatal("older persisted ParentReady regressed the admission root")
	}
	if got := pool.Snapshot().RootSlot; got != root.Slot {
		t.Fatalf("root after stale restore = %d, want %d", got, root.Slot)
	}
	if pool.RestoreParentReady(11, BlockID{Slot: 10, Hash: parentReadyHash(99)}) {
		t.Fatal("conflicting exact-root ParentReady identity was accepted")
	}
	if pool.RestoreParentReady(12, BlockID{Slot: 12, Hash: parentReadyHash(12)}) {
		t.Fatal("self-parenting persisted ParentReady edge was accepted")
	}

	parent := BlockID{Slot: 12, Hash: parentReadyHash(12)}
	if !pool.RestoreParentReady(13, parent) {
		t.Fatal("newer persisted ParentReady was not restored")
	}
	snapshot := pool.Snapshot()
	if snapshot.RootSlot != parent.Slot || snapshot.TrackedSlots != 0 || snapshot.VerifiedVotes != 1 {
		// VerifiedVotes is a lifetime accepted counter; TrackedSlots is the
		// capacity-relevant retained state that must be pruned.
		t.Fatalf("advanced ParentReady did not run root cleanup: %+v", snapshot)
	}
	pool.mu.Lock()
	_, retained := pool.localFirstVotes[11]
	pool.mu.Unlock()
	if retained {
		t.Fatal("advanced ParentReady retained rooted first-vote provenance")
	}
	highParent := BlockID{Slot: 13, Hash: parentReadyHash(13)}
	if !pool.RestoreParentReady(20, highParent) {
		t.Fatal("new live ParentReady high-water was not restored")
	}
	if pool.RestoreParentReady(15, BlockID{Slot: 14, Hash: parentReadyHash(14)}) {
		t.Fatal("stale persisted target regressed the ParentReady high-water")
	}
	if slot, _, ok := pool.HighestParentReady(); !ok || slot != 20 {
		t.Fatalf("ParentReady high-water after stale restore = %d, ok=%t", slot, ok)
	}
}

func TestConsensusPoolRestoreParentReadyCannotReplaceLiveTrackerState(t *testing.T) {
	root := BlockID{Slot: 10, Hash: parentReadyHash(10)}
	pool := NewConsensusPool(ConsensusPoolConfig{RootBlock: root})
	liveParent := BlockID{Slot: 12, Hash: parentReadyHash(12)}
	update, err := pool.AddVerifiedCertificate(Certificate{
		Type: CertificateNotarizeFallback, Slot: liveParent.Slot, BlockHash: liveParent.Hash,
		StakeVerified: true, SignatureVerified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(update.Certificates) != 1 {
		t.Fatalf("accepted live certificates = %+v", update.Certificates)
	}

	persistedParent := BlockID{Slot: 11, Hash: parentReadyHash(11)}
	if pool.RestoreParentReady(20, persistedParent) {
		t.Fatal("persisted ParentReady replaced live tracker state")
	}
	if slot, parent, ok := pool.HighestParentReady(); !ok || slot != 13 || parent != liveParent {
		t.Fatalf("live ParentReady after rejected restore = slot %d parent %v ok=%t", slot, parent, ok)
	}
	if got := pool.BlockProductionParent(13); got.Kind != BlockProductionParentReady || got.Parent != liveParent {
		t.Fatalf("live production parent after rejected restore = %+v", got)
	}
}

func TestConsensusPoolDefersIntrawindowSafeToNotar(t *testing.T) {
	set, keys := testBLSValidatorSet(100, 10, 40, 50)
	pool := NewConsensusPool(DefaultConsensusPoolConfig())
	pool.NoteLiveSlot(40)
	target := parentReadyHash(9)
	if _, err := pool.AddVerifiedVote(poolVote(t, set, keys, 0, NewSkipVote(41), true)); err != nil {
		t.Fatal(err)
	}
	update, err := pool.AddVerifiedVote(poolVote(t, set, keys, 1, NewNotarizationVote(41, target), false))
	if err != nil {
		t.Fatal(err)
	}
	if hasConsensusEvent(update.Events, ConsensusEventSafeToNotar, 41, target) {
		t.Fatalf("intrawindow SafeToNotar emitted before parent checks: %+v", update.Events)
	}
	pending := pool.TakePendingSafeToNotar()
	block := BlockID{Slot: 41, Hash: target}
	if len(pending) != 1 || pending[0] != block {
		t.Fatalf("pending SafeToNotar = %+v", pending)
	}
	if pending = pool.TakePendingSafeToNotar(); len(pending) != 0 {
		t.Fatalf("pending queue was not drained: %+v", pending)
	}
	pool.RequeuePendingSafeToNotar(block)
	pool.SetRoot(BlockID{Slot: block.Slot, Hash: parentReadyHash(10)})
	if pending = pool.TakePendingSafeToNotar(); len(pending) != 0 {
		t.Fatalf("root-equal pending candidate survived pruning: %+v", pending)
	}
	pool.RequeuePendingSafeToNotar(block)
	if pending = pool.TakePendingSafeToNotar(); len(pending) != 0 {
		t.Fatalf("root-equal candidate was requeued: %+v", pending)
	}
}

func poolVote(t *testing.T, set ValidatorSet, keys []*big.Int, rank uint16, vote Vote, local bool) VerifiedVote {
	t.Helper()
	message := VoteMessage{
		Vote:      vote,
		Signature: testBLSSignature(t, []testBLSVoteSignature{{Vote: vote, Key: keys[int(rank)]}}),
		Rank:      rank,
	}
	result, err := verifyVoteMessageWithSet(set, message)
	if err != nil {
		t.Fatalf("verify test vote: %v", err)
	}
	return VerifiedVote{Message: message, Result: result, Local: local}
}

func assertCertificateTypes(t *testing.T, certs []Certificate, want ...CertificateType) {
	t.Helper()
	got := make(map[CertificateType]bool)
	for _, cert := range certs {
		got[cert.Type] = true
	}
	if len(got) != len(want) {
		t.Fatalf("certificate types = %v, want %v", got, want)
	}
	for _, certType := range want {
		if !got[certType] {
			t.Fatalf("missing certificate type %s in %v", certType, got)
		}
	}
}

func hasConsensusEvent(events []ConsensusEvent, kind ConsensusEventKind, slot uint64, hash solana.Hash) bool {
	for _, event := range events {
		if event.Kind == kind && event.Slot == slot && (hash == (solana.Hash{}) || event.Block.Hash == hash) {
			return true
		}
	}
	return false
}
