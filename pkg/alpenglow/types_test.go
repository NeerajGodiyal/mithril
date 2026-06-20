package alpenglow

import (
	"testing"

	"github.com/gagliardetto/solana-go"
)

func testHash(seed byte) solana.Hash {
	var out solana.Hash
	for i := range out {
		out[i] = seed
	}
	return out
}

func TestCertificateThresholdsMatchVotor(t *testing.T) {
	tests := []struct {
		certType CertificateType
		want     Fraction
		votes    []VoteType
	}{
		{CertificateNotarize, CertificateThreshold, []VoteType{VoteTypeNotarize}},
		{CertificateNotarizeFallback, CertificateThreshold, []VoteType{VoteTypeNotarize, VoteTypeNotarizeFallback}},
		{CertificateFinalizeFast, FastFinalizeThreshold, []VoteType{VoteTypeNotarize}},
		{CertificateFinalize, CertificateThreshold, []VoteType{VoteTypeFinalize}},
		{CertificateSkip, CertificateThreshold, []VoteType{VoteTypeSkip, VoteTypeSkipFallback}},
		{CertificateGenesis, GenesisCertificateThreshold, []VoteType{VoteTypeGenesis}},
	}

	for _, tt := range tests {
		t.Run(string(tt.certType), func(t *testing.T) {
			if got := tt.certType.RequiredThreshold(); got != tt.want {
				t.Fatalf("threshold = %+v, want %+v", got, tt.want)
			}
			gotVotes := tt.certType.SourceVoteTypes()
			if len(gotVotes) != len(tt.votes) {
				t.Fatalf("source vote len = %d, want %d", len(gotVotes), len(tt.votes))
			}
			for i := range gotVotes {
				if gotVotes[i] != tt.votes[i] {
					t.Fatalf("source votes = %v, want %v", gotVotes, tt.votes)
				}
			}
		})
	}
}

func TestFractionCompareStakeAvoidsFloatPrecisionLoss(t *testing.T) {
	totalStake := uint64(100_000_000_000_000_000)
	stake := uint64(60_000_000_000_000_001)

	cmp, err := CertificateThreshold.CompareStake(stake, totalStake)
	if err != nil {
		t.Fatalf("CompareStake returned error: %v", err)
	}
	if cmp <= 0 {
		t.Fatalf("expected stake to exceed 60%% threshold, cmp=%d", cmp)
	}
}

func TestCertificateValidateBasicChecksStakeWhenPresent(t *testing.T) {
	cert := Certificate{
		Type:          CertificateNotarize,
		Slot:          10,
		BlockHash:     testHash(1),
		IncludedStake: 59,
		TotalStake:    100,
	}

	if err := cert.ValidateBasic(); err == nil {
		t.Fatalf("expected insufficient stake error")
	}

	cert.IncludedStake = 60
	if err := cert.ValidateBasic(); err != nil {
		t.Fatalf("expected sufficient stake, got %v", err)
	}
}

func TestVoteBlockOnlyForBlockVotes(t *testing.T) {
	blockHash := testHash(2)
	vote := NewNotarizationVote(42, blockHash)
	block, ok := vote.Block()
	if !ok {
		t.Fatalf("notarization vote should have block")
	}
	if block.Slot != 42 || block.Hash != blockHash {
		t.Fatalf("block = %+v, want slot/hash", block)
	}

	if _, ok := NewSkipVote(42).Block(); ok {
		t.Fatalf("skip vote should not have block")
	}
}
