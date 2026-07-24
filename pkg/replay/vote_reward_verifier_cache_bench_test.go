package replay

import (
	"encoding/binary"
	"math/big"
	"testing"

	bls12381 "github.com/Overclock-Validator/gnark-crypto/ecc/bls12-381"
	"github.com/Overclock-Validator/mithril/pkg/alpenglow"
	"github.com/Overclock-Validator/mithril/pkg/epochstakes"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/gagliardetto/solana-go"
)

const benchmarkVoteRewardValidatorCount = 1_000

var (
	benchmarkVoteRewardMaterial *voteRewardVerifierMaterial
	benchmarkVoteRewardVerifier *alpenglow.CertificateVerifier
)

func BenchmarkVoteRewardVerifierCache(b *testing.B) {
	const (
		epoch        = uint64(9_100_000_001)
		shredVersion = uint16(88)
	)

	stakes, voteAccounts, totalStake := benchmarkVoteRewardEpochMaterial(benchmarkVoteRewardValidatorCount)
	global.ClearEpochStakes(epoch)
	clearVoteRewardVerifierCacheForTest()
	global.PutEpochStakes(epoch, stakes, voteAccounts, totalStake)
	b.Cleanup(func() {
		global.ClearEpochStakes(epoch)
		clearVoteRewardVerifierCacheForTest()
	})

	primed, hit, err := cachedVoteRewardVerifiers.get(epoch, shredVersion)
	if err != nil {
		b.Fatalf("prime verifier cache: %v", err)
	}
	if hit {
		b.Fatal("first verifier-cache lookup unexpectedly hit")
	}
	benchmarkVoteRewardMaterial = primed

	b.Run("HotHit_1000Validators", func(b *testing.B) {
		b.ReportAllocs()
		var (
			material *voteRewardVerifierMaterial
			hit      bool
			err      error
		)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			material, hit, err = cachedVoteRewardVerifiers.get(epoch, shredVersion)
		}
		b.StopTimer()
		if err != nil {
			b.Fatalf("cached lookup: %v", err)
		}
		if !hit || material != primed {
			b.Fatal("cached lookup did not return the primed material")
		}
		benchmarkVoteRewardMaterial = material
	})

	b.Run("Rebuild_1000Validators", func(b *testing.B) {
		b.ReportAllocs()
		var verifier *alpenglow.CertificateVerifier
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			snapshot, ok := global.EpochStakesSnapshot(epoch)
			if !ok {
				b.Fatal("benchmark epoch disappeared")
			}
			validatorSet, err := buildValidatorSetForSnapshot(snapshot)
			if err != nil {
				b.Fatalf("build validator set: %v", err)
			}
			verifier = alpenglow.NewCertificateVerifier()
			if err := verifier.SetValidatorSet(validatorSet); err != nil {
				b.Fatalf("configure verifier: %v", err)
			}
			verifier.SetShredVersion(shredVersion)
		}
		b.StopTimer()
		benchmarkVoteRewardVerifier = verifier
	})
}

func benchmarkVoteRewardEpochMaterial(
	count int,
) (
	map[solana.PublicKey]uint64,
	map[solana.PublicKey]*epochstakes.VoteAccount,
	uint64,
) {
	stakes := make(map[solana.PublicKey]uint64, count)
	voteAccounts := make(map[solana.PublicKey]*epochstakes.VoteAccount, count)
	var totalStake uint64
	for i := 0; i < count; i++ {
		var voteAccount solana.PublicKey
		binary.LittleEndian.PutUint64(voteAccount[:8], uint64(i+1))
		var nodePubkey solana.PublicKey
		nodePubkey[0] = 1
		binary.LittleEndian.PutUint64(nodePubkey[1:9], uint64(i+1))

		var blsPubkey bls12381.G1Affine
		blsPubkey.ScalarMultiplicationBase(big.NewInt(int64(i + 1)))
		compressed := blsPubkey.Bytes()
		stake := uint64(count - i)
		stakes[voteAccount] = stake
		voteAccounts[voteAccount] = &epochstakes.VoteAccount{
			NodePubkey:          nodePubkey,
			BlsPubkeyCompressed: &compressed,
		}
		totalStake += stake
	}
	return stakes, voteAccounts, totalStake
}
