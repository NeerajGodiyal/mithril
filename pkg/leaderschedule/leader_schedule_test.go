package leaderschedule

import (
	"math"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
)

func TestSortStakesOrdering(t *testing.T) {
	// Create deterministic pubkeys for testing using byte arrays
	// pk1 < pk2 < pk3 lexicographically
	var pk1Bytes, pk2Bytes, pk3Bytes [32]byte
	pk1Bytes[0] = 1
	pk2Bytes[0] = 2
	pk3Bytes[0] = 3
	pk1 := solana.PublicKeyFromBytes(pk1Bytes[:])
	pk2 := solana.PublicKeyFromBytes(pk2Bytes[:])
	pk3 := solana.PublicKeyFromBytes(pk3Bytes[:])

	t.Run("descending stake order", func(t *testing.T) {
		stakes := []pubkeyAndStakePair{
			{pubkey: pk1, stake: 100},
			{pubkey: pk2, stake: 300},
			{pubkey: pk3, stake: 200},
		}
		sorted := sortStakes(stakes)

		assert.Equal(t, pk2, sorted[0].pubkey, "highest stake (300) should be first")
		assert.Equal(t, pk3, sorted[1].pubkey, "middle stake (200) should be second")
		assert.Equal(t, pk1, sorted[2].pubkey, "lowest stake (100) should be last")
	})

	t.Run("tiebreak by pubkey descending", func(t *testing.T) {
		// When stakes are equal, higher pubkey should come first (descending order)
		stakes := []pubkeyAndStakePair{
			{pubkey: pk1, stake: 100},
			{pubkey: pk3, stake: 100}, // pk3 > pk1 lexicographically
		}
		sorted := sortStakes(stakes)

		assert.Equal(t, pk3, sorted[0].pubkey, "higher pubkey should come first when stakes equal")
		assert.Equal(t, pk1, sorted[1].pubkey)
	})

	t.Run("large u64 stakes no overflow", func(t *testing.T) {
		// This test catches the overflow bug: int(r.stake - l.stake) would overflow
		largePk1 := solana.NewWallet().PublicKey()
		largePk2 := solana.NewWallet().PublicKey()

		stakes := []pubkeyAndStakePair{
			{pubkey: largePk1, stake: math.MaxUint64 - 1},
			{pubkey: largePk2, stake: 1},
		}
		sorted := sortStakes(stakes)

		assert.Equal(t, uint64(math.MaxUint64-1), sorted[0].stake, "largest stake should be first")
		assert.Equal(t, uint64(1), sorted[1].stake, "smallest stake should be last")
	})

	t.Run("dedup consecutive duplicates", func(t *testing.T) {
		stakes := []pubkeyAndStakePair{
			{pubkey: pk1, stake: 100},
			{pubkey: pk2, stake: 300},
			{pubkey: pk1, stake: 100}, // duplicate
		}
		sorted := sortStakes(stakes)

		// After sort: [pk2(300), pk1(100), pk1(100)]
		// After compact: duplicates removed
		assert.Len(t, sorted, 2, "duplicates should be removed")
		assert.Equal(t, pk2, sorted[0].pubkey)
		assert.Equal(t, pk1, sorted[1].pubkey)
	})
}
