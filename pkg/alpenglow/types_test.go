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

func TestCertificateValidateBasicRequiresCanonicalBlockHash(t *testing.T) {
	hash := testHash(4)
	tests := []struct {
		name string
		cert Certificate
		ok   bool
	}{
		{name: "notarize", cert: Certificate{Type: CertificateNotarize, Slot: 42, BlockHash: hash}, ok: true},
		{name: "notarize missing block", cert: Certificate{Type: CertificateNotarize, Slot: 42}},
		{name: "finalize", cert: Certificate{Type: CertificateFinalize, Slot: 42}, ok: true},
		{name: "finalize with block", cert: Certificate{Type: CertificateFinalize, Slot: 42, BlockHash: hash}},
		{name: "skip", cert: Certificate{Type: CertificateSkip, Slot: 42}, ok: true},
		{name: "skip with block", cert: Certificate{Type: CertificateSkip, Slot: 42, BlockHash: hash}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cert.ValidateBasic()
			if tt.ok && err != nil {
				t.Fatalf("ValidateBasic() error = %v", err)
			}
			if !tt.ok && err == nil {
				t.Fatal("ValidateBasic() accepted a non-canonical certificate")
			}
		})
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

func TestVoteValidateBasicRequiresCanonicalBlockHash(t *testing.T) {
	hash := testHash(3)
	tests := []struct {
		name string
		vote Vote
		ok   bool
	}{
		{name: "notarize", vote: NewNotarizationVote(42, hash), ok: true},
		{name: "notarize missing block", vote: NewNotarizationVote(42, solana.Hash{})},
		{name: "notarize fallback", vote: NewNotarizationFallbackVote(42, hash), ok: true},
		{name: "notarize fallback missing block", vote: NewNotarizationFallbackVote(42, solana.Hash{})},
		{name: "genesis", vote: NewGenesisVote(42, hash), ok: true},
		{name: "genesis missing block", vote: NewGenesisVote(42, solana.Hash{})},
		{name: "finalize", vote: NewFinalizationVote(42), ok: true},
		{name: "finalize with block", vote: Vote{Type: VoteTypeFinalize, Slot: 42, BlockHash: hash}},
		{name: "skip", vote: NewSkipVote(42), ok: true},
		{name: "skip with block", vote: Vote{Type: VoteTypeSkip, Slot: 42, BlockHash: hash}},
		{name: "skip fallback", vote: NewSkipFallbackVote(42), ok: true},
		{name: "skip fallback with block", vote: Vote{Type: VoteTypeSkipFallback, Slot: 42, BlockHash: hash}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.vote.ValidateBasic()
			if tt.ok && err != nil {
				t.Fatalf("ValidateBasic() error = %v", err)
			}
			if !tt.ok && err == nil {
				t.Fatal("ValidateBasic() accepted a non-canonical vote")
			}
		})
	}
}
