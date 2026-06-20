package alpenglow

import "testing"

func TestObserverTracksCertificatesAndDeduplicates(t *testing.T) {
	observer := NewObserver()
	cert := Certificate{
		Type:          CertificateNotarize,
		Slot:          100,
		BlockHash:     testHash(3),
		IncludedStake: 61,
		TotalStake:    100,
	}

	obs, err := observer.ObserveCertificate(cert)
	if err != nil {
		t.Fatalf("ObserveCertificate returned error: %v", err)
	}
	if !obs.New {
		t.Fatalf("expected first certificate to be new")
	}
	if obs.Snapshot.CertificatesObserved != 1 || obs.Snapshot.CertificatesRetained != 1 {
		t.Fatalf("certificate counts = %+v, want observed=1 retained=1", obs.Snapshot)
	}
	if obs.Snapshot.LatestNotarizedBlock.Slot != cert.Slot || obs.Snapshot.LatestNotarizedBlock.Hash != cert.BlockHash {
		t.Fatalf("latest notarized block = %+v, want cert block", obs.Snapshot.LatestNotarizedBlock)
	}

	obs, err = observer.ObserveCertificate(cert)
	if err != nil {
		t.Fatalf("ObserveCertificate duplicate returned error: %v", err)
	}
	if obs.New {
		t.Fatalf("expected duplicate certificate to be marked old")
	}
	if obs.Snapshot.CertificatesObserved != 1 || obs.Snapshot.CertificatesRetained != 1 {
		t.Fatalf("duplicate changed certificate counts: %+v", obs.Snapshot)
	}
}

func TestObserverTracksFastFinalization(t *testing.T) {
	observer := NewObserver()
	cert := Certificate{
		Type:          CertificateFinalizeFast,
		Slot:          123,
		BlockHash:     testHash(4),
		IncludedStake: 80,
		TotalStake:    100,
	}

	obs, err := observer.ObserveCertificate(cert)
	if err != nil {
		t.Fatalf("ObserveCertificate returned error: %v", err)
	}
	if obs.Snapshot.LatestFinalizedSlot != cert.Slot {
		t.Fatalf("latest finalized slot = %d, want %d", obs.Snapshot.LatestFinalizedSlot, cert.Slot)
	}
	if obs.Snapshot.LatestFastFinalizedBlock.Slot != cert.Slot || obs.Snapshot.LatestFastFinalizedBlock.Hash != cert.BlockHash {
		t.Fatalf("latest fast finalized block = %+v, want cert block", obs.Snapshot.LatestFastFinalizedBlock)
	}
}

func TestObserverTracksVotesAndReplay(t *testing.T) {
	observer := NewObserver()
	vote := VoteMessage{
		Vote: NewSkipVote(55),
		Rank: 7,
	}

	obs, err := observer.ObserveVote(vote)
	if err != nil {
		t.Fatalf("ObserveVote returned error: %v", err)
	}
	if obs.Snapshot.VotesObserved != 1 || obs.Snapshot.VotesRetained != 1 || obs.Snapshot.LatestVoteSlot != 55 {
		t.Fatalf("vote snapshot = %+v", obs.Snapshot)
	}

	blockObs := observer.ObserveReplayBlock(ReplayBlockObservation{
		Block: BlockID{Slot: 56, Hash: testHash(5)},
	})
	if !blockObs.New {
		t.Fatalf("expected replay block to be new")
	}
	if blockObs.Snapshot.ReplayBlocksObserved != 1 ||
		blockObs.Snapshot.OldestReplayBlockSlot != 56 ||
		blockObs.Snapshot.LatestReplayBlockSlot != 56 {
		t.Fatalf("replay snapshot = %+v", blockObs.Snapshot)
	}
	if blockObs.Snapshot.LatestReplayBlock != (BlockID{Slot: 56, Hash: testHash(5)}) {
		t.Fatalf("latest replay block = %+v", blockObs.Snapshot.LatestReplayBlock)
	}
}

func TestObserverMatchesCertificateAgainstReplayBlock(t *testing.T) {
	observer := NewObserver()
	block := BlockID{Slot: 100, Hash: testHash(9)}

	blockObs := observer.ObserveReplayBlock(ReplayBlockObservation{Block: block})
	if blockObs.Snapshot.ReplayBlocksRetained != 1 {
		t.Fatalf("replay block retention = %+v", blockObs.Snapshot)
	}

	certObs, err := observer.ObserveCertificate(Certificate{
		Type:      CertificateNotarize,
		Slot:      block.Slot,
		BlockHash: block.Hash,
	})
	if err != nil {
		t.Fatalf("ObserveCertificate returned error: %v", err)
	}
	if certObs.Snapshot.CertificateReplayMatches != 1 || certObs.Snapshot.CertificateReplayMismatches != 0 || certObs.Snapshot.CertificateReplayPending != 0 {
		t.Fatalf("certificate replay snapshot = %+v", certObs.Snapshot)
	}
	if certObs.Snapshot.LatestCertificateReplayMatch != block {
		t.Fatalf("latest replay match = %+v, want %+v", certObs.Snapshot.LatestCertificateReplayMatch, block)
	}
}

func TestObserverMatchesCertificateWhenReplayArrivesLater(t *testing.T) {
	observer := NewObserver()
	block := BlockID{Slot: 101, Hash: testHash(10)}

	certObs, err := observer.ObserveCertificate(Certificate{
		Type:      CertificateNotarize,
		Slot:      block.Slot,
		BlockHash: block.Hash,
	})
	if err != nil {
		t.Fatalf("ObserveCertificate returned error: %v", err)
	}
	if certObs.Snapshot.CertificateReplayPending != 1 {
		t.Fatalf("pending replay snapshot = %+v", certObs.Snapshot)
	}
	if certObs.Snapshot.CertificateReplayMaturePending != 0 ||
		certObs.Snapshot.CertificateReplayPendingOldestSlot != block.Slot ||
		certObs.Snapshot.CertificateReplayPendingNewestSlot != block.Slot {
		t.Fatalf("pending replay detail snapshot = %+v", certObs.Snapshot)
	}

	replayObs := observer.ObserveReplayBlock(ReplayBlockObservation{Block: block})
	if replayObs.Snapshot.CertificateReplayMatches != 1 || replayObs.Snapshot.CertificateReplayPending != 0 {
		t.Fatalf("certificate replay snapshot after replay = %+v", replayObs.Snapshot)
	}
}

func TestObserverReportsMaturePendingCertificates(t *testing.T) {
	observer := NewObserver()
	observer.ObserveReplayBlock(ReplayBlockObservation{Block: BlockID{Slot: 105, Hash: testHash(1)}})
	observer.ObserveReplayBlock(ReplayBlockObservation{Block: BlockID{Slot: 107, Hash: testHash(2)}})

	if _, err := observer.ObserveCertificate(Certificate{
		Type:      CertificateNotarize,
		Slot:      104,
		BlockHash: testHash(3),
	}); err != nil {
		t.Fatalf("ObserveCertificate pre-window pending returned error: %v", err)
	}
	if _, err := observer.ObserveCertificate(Certificate{
		Type:      CertificateNotarize,
		Slot:      106,
		BlockHash: testHash(4),
	}); err != nil {
		t.Fatalf("ObserveCertificate mature pending returned error: %v", err)
	}
	obs, err := observer.ObserveCertificate(Certificate{
		Type:      CertificateNotarize,
		Slot:      108,
		BlockHash: testHash(5),
	})
	if err != nil {
		t.Fatalf("ObserveCertificate future pending returned error: %v", err)
	}

	if obs.Snapshot.CertificateReplayPending != 3 ||
		obs.Snapshot.CertificateReplayMaturePending != 1 ||
		obs.Snapshot.CertificateReplayPreWindowPending != 1 ||
		obs.Snapshot.CertificateReplayPendingOldestSlot != 104 ||
		obs.Snapshot.CertificateReplayPendingNewestSlot != 108 ||
		obs.Snapshot.CertificateReplayMatureOldestSlot != 106 ||
		obs.Snapshot.OldestReplayBlockSlot != 105 ||
		obs.Snapshot.LatestReplayBlockSlot != 107 {
		t.Fatalf("pending replay detail snapshot = %+v", obs.Snapshot)
	}
}

func TestObserverDetectsCertificateReplayMismatch(t *testing.T) {
	observer := NewObserver()
	replayBlock := BlockID{Slot: 102, Hash: testHash(11)}
	certBlock := BlockID{Slot: 102, Hash: testHash(12)}

	observer.ObserveReplayBlock(ReplayBlockObservation{Block: replayBlock})
	certObs, err := observer.ObserveCertificate(Certificate{
		Type:      CertificateNotarize,
		Slot:      certBlock.Slot,
		BlockHash: certBlock.Hash,
	})
	if err != nil {
		t.Fatalf("ObserveCertificate returned error: %v", err)
	}
	if certObs.Snapshot.CertificateReplayMatches != 0 || certObs.Snapshot.CertificateReplayMismatches != 1 || certObs.Snapshot.CertificateReplayPending != 0 {
		t.Fatalf("certificate replay snapshot = %+v", certObs.Snapshot)
	}
	if certObs.Snapshot.LatestCertificateReplayMiss != certBlock || certObs.Snapshot.LatestReplayMiss != replayBlock {
		t.Fatalf("latest replay miss snapshot = %+v", certObs.Snapshot)
	}
}

func TestObserverBoundsVoteAndCertificateRetention(t *testing.T) {
	observer := NewObserverWithConfig(ObserverConfig{
		MaxTrackedVotes:        1,
		MaxTrackedCertificates: 1,
	})

	if _, err := observer.ObserveVote(VoteMessage{Vote: NewSkipVote(1), Rank: 1}); err != nil {
		t.Fatalf("ObserveVote 1 returned error: %v", err)
	}
	if _, err := observer.ObserveVote(VoteMessage{Vote: NewSkipVote(2), Rank: 2}); err != nil {
		t.Fatalf("ObserveVote 2 returned error: %v", err)
	}

	if _, err := observer.ObserveCertificate(Certificate{
		Type:          CertificateSkip,
		Slot:          1,
		TotalStake:    100,
		IncludedStake: 60,
	}); err != nil {
		t.Fatalf("ObserveCertificate 1 returned error: %v", err)
	}
	if _, err := observer.ObserveCertificate(Certificate{
		Type:          CertificateSkip,
		Slot:          2,
		TotalStake:    100,
		IncludedStake: 60,
	}); err != nil {
		t.Fatalf("ObserveCertificate 2 returned error: %v", err)
	}

	snapshot := observer.Snapshot()
	if snapshot.VotesObserved != 2 || snapshot.VotesRetained != 1 {
		t.Fatalf("vote retention snapshot = %+v", snapshot)
	}
	if snapshot.CertificatesObserved != 2 || snapshot.CertificatesRetained != 1 {
		t.Fatalf("certificate retention snapshot = %+v", snapshot)
	}
}

func TestObserverObservesWireMessage(t *testing.T) {
	observer := NewObserver()
	wire, err := EncodeMessage(NewVoteMessage(NewFinalizationVote(88), testSignatureSeq(2), 7))
	if err != nil {
		t.Fatalf("EncodeMessage returned error: %v", err)
	}

	obs, err := observer.ObserveWireMessage(wire)
	if err != nil {
		t.Fatalf("ObserveWireMessage returned error: %v", err)
	}
	if !obs.New {
		t.Fatalf("expected first wire vote to be new")
	}
	if obs.Snapshot.VotesObserved != 1 || obs.Snapshot.LatestVoteSlot != 88 {
		t.Fatalf("wire vote snapshot = %+v", obs.Snapshot)
	}

	if _, err := observer.ObserveWireMessage(append(wire, 0xff)); err == nil {
		t.Fatalf("expected malformed wire message error")
	}
}
