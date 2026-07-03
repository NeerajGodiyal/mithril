package alpenglow

import (
	"bytes"
	"math/big"
	"sort"
	"testing"

	bls12381 "github.com/Overclock-Validator/gnark-crypto/ecc/bls12-381"
	"github.com/Overclock-Validator/mithril/pkg/epochstakes"
	"github.com/gagliardetto/solana-go"
)

// Equal-stake ranks must tie-break by the COMPRESSED pubkey, matching agave's
// BLSPubkeyToRankMap (a_pubkey_compressed.cmp). Wrong ordering desyncs bitmap→rank
// lookups and breaks cert verification. Verified against agave source + a live cluster.
func TestBuildValidatorSetEqualStakeRanksByCompressedPubkey(t *testing.T) {
	const equalStake = uint64(100)
	stakes := map[solana.PublicKey]uint64{}
	voteAccts := map[solana.PublicKey]*epochstakes.VoteAccount{}

	type ref struct {
		vote       solana.PublicKey
		compressed [48]byte
	}
	var refs []ref
	for i := 0; i < 8; i++ {
		key := big.NewInt(int64(i + 3))
		var pub bls12381.G1Affine
		pub.ScalarMultiplicationBase(key)
		compressed := pub.Bytes() // [48]byte

		var vote solana.PublicKey
		vote[0], vote[1] = byte(i+1), 0xAA
		stakes[vote] = equalStake // all equal → tie-break decides order
		c := compressed
		voteAccts[vote] = &epochstakes.VoteAccount{
			NodePubkey:          vote,
			BlsPubkeyCompressed: &c,
		}
		refs = append(refs, ref{vote: vote, compressed: compressed})
	}

	set, err := BuildValidatorSet(4, stakes, voteAccts, equalStake*uint64(len(refs)))
	if err != nil {
		t.Fatalf("BuildValidatorSet: %v", err)
	}

	// Expected order = agave's: equal stake → ascending compressed pubkey bytes.
	sort.Slice(refs, func(i, j int) bool {
		return bytes.Compare(refs[i].compressed[:], refs[j].compressed[:]) < 0
	})

	if len(set.Validators) != len(refs) {
		t.Fatalf("set has %d validators, want %d", len(set.Validators), len(refs))
	}
	for rank, v := range set.Validators {
		if v.Rank != uint16(rank) {
			t.Fatalf("validator at index %d has Rank %d", rank, v.Rank)
		}
		if v.VoteAccount != refs[rank].vote {
			t.Errorf("rank %d = vote %s, want %s (equal-stake ties must follow compressed pubkey order)",
				rank, v.VoteAccount, refs[rank].vote)
		}
		if rank > 0 && bytes.Compare(set.Validators[rank-1].BlsPubkeyCompressed[:], v.BlsPubkeyCompressed[:]) >= 0 {
			t.Errorf("ranks not sorted by compressed pubkey at rank %d", rank)
		}
	}
}
