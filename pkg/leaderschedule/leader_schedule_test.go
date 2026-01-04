package leaderschedule

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/gagliardetto/solana-go"
	chacha "github.com/nixberg/chacha-rng-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// pubkeyFromU16 creates a deterministic pubkey matching Agave's test helper
// fn pubkey_from_u16(n: u16) -> Pubkey {
//     let mut bytes = [0; 32];
//     bytes[0..2].copy_from_slice(&n.to_le_bytes());
//     Pubkey::new_from_array(bytes)
// }
func pubkeyFromU16(n uint16) solana.PublicKey {
	var bytes [32]byte
	binary.LittleEndian.PutUint16(bytes[:], n)
	return solana.PublicKeyFromBytes(bytes[:])
}

// TestAgaveStakeWeightedScheduleVectors tests exact compatibility with Agave's test vectors.
// These are from agave/ledger/src/leader_schedule.rs test_stake_leader_schedule_exact_order.
//
// CRITICAL: If any of these tests fail, the local leader schedule will NOT match the network.
func TestAgaveStakeWeightedScheduleVectors(t *testing.T) {
	testCases := []struct {
		name     string
		epoch    uint64
		stakes   []uint64
		length   uint64
		repeat   uint64
		expected []int // indices into sorted pubkeys
	}{
		// From Agave leader_schedule.rs lines 176-197
		{"epoch1_stakes10-20-30_len12_rep1", 1, []uint64{10, 20, 30}, 12, 1, []int{1, 1, 2, 1, 1, 0, 0, 1, 2, 1, 0, 1}},
		{"epoch1_stakes10-20-30_len12_rep2", 1, []uint64{10, 20, 30}, 12, 2, []int{1, 1, 1, 1, 2, 2, 1, 1, 1, 1, 0, 0}},
		{"epoch1_stakes30-10-20_len12_rep1", 1, []uint64{30, 10, 20}, 12, 1, []int{2, 2, 0, 2, 2, 1, 1, 2, 0, 2, 1, 2}},
		{"epoch1_stakes30-10-20_len12_rep2", 1, []uint64{30, 10, 20}, 12, 2, []int{2, 2, 2, 2, 0, 0, 2, 2, 2, 2, 1, 1}},
		{"epoch1_stakes10-20-25-30_len12_rep1", 1, []uint64{10, 20, 25, 30}, 12, 1, []int{2, 2, 3, 1, 2, 0, 1, 1, 3, 2, 1, 2}},
		{"epoch1_7stakes_len15_rep1", 1, []uint64{10, 20, 25, 30, 35, 40, 100}, 15, 1, []int{4, 5, 6, 3, 4, 1, 2, 3, 6, 4, 2, 4, 5, 6, 6}},
		{"epoch1_8stakes_len15_rep1", 1, []uint64{10, 20, 25, 30, 35, 40, 100, 1000}, 15, 1, []int{7, 7, 7, 7, 7, 4, 6, 7, 7, 7, 6, 7, 7, 7, 7}},
		{"epoch1_9stakes_len20_rep1", 1, []uint64{10, 20, 25, 30, 35, 40, 100, 1000, 10_000}, 20, 1, []int{8, 8, 8, 8, 8, 7, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 7}},
		{"epoch1_9stakes_len25_rep1", 1, []uint64{10, 20, 25, 30, 35, 40, 100, 1000, 10_000}, 25, 1, []int{8, 8, 8, 8, 8, 7, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 7, 8, 8, 8, 8, 8}},
		{"epoch457468_len12_rep1", 457468, []uint64{10, 20, 30}, 12, 1, []int{2, 2, 0, 1, 0, 2, 1, 2, 1, 2, 2, 2}},
		{"epoch457468_len12_rep2", 457468, []uint64{10, 20, 30}, 12, 2, []int{2, 2, 2, 2, 0, 0, 1, 1, 0, 0, 2, 2}},
		{"epoch457469_len12_rep1", 457469, []uint64{10, 20, 30}, 12, 1, []int{1, 2, 2, 2, 2, 2, 2, 1, 0, 2, 2, 0}},
		{"epoch457470_len12_rep1", 457470, []uint64{10, 20, 30}, 12, 1, []int{2, 1, 1, 1, 1, 1, 1, 1, 1, 2, 0, 2}},
		{"epoch3466545_len12_rep1", 3466545, []uint64{10, 20, 30}, 12, 1, []int{2, 2, 0, 0, 2, 1, 1, 1, 0, 0, 2, 2}},
		{"epoch3466545_len13_rep1", 3466545, []uint64{10, 20, 30}, 13, 1, []int{2, 2, 0, 0, 2, 1, 1, 1, 0, 0, 2, 2, 1}},
		{"epoch3466545_len13_rep2", 3466545, []uint64{10, 20, 30}, 13, 2, []int{2, 2, 2, 2, 0, 0, 0, 0, 2, 2, 1, 1, 1}},
		{"epoch3466545_len14_rep1", 3466545, []uint64{10, 20, 30}, 14, 1, []int{2, 2, 0, 0, 2, 1, 1, 1, 0, 0, 2, 2, 1, 2}},
		{"epoch3466545_len14_rep2", 3466545, []uint64{10, 20, 30}, 14, 2, []int{2, 2, 2, 2, 0, 0, 0, 0, 2, 2, 1, 1, 1, 1}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create pubkeys matching Agave's test: pubkey_from_u16(0), pubkey_from_u16(1), ...
			pubkeys := make([]solana.PublicKey, len(tc.stakes))
			for i := range tc.stakes {
				pubkeys[i] = pubkeyFromU16(uint16(i))
			}

			// Build keyed stakes (same input order as Agave test)
			keyedStakes := make([]pubkeyAndStakePair, len(tc.stakes))
			for i, stake := range tc.stakes {
				keyedStakes[i] = pubkeyAndStakePair{pubkey: pubkeys[i], stake: stake}
			}

			// Generate leaders
			leaders := stakeWeightedSlotLeaders(keyedStakes, tc.epoch, tc.length, tc.repeat)
			require.Len(t, leaders, int(tc.length), "wrong number of leaders")

			// Convert leaders back to indices
			result := make([]int, len(leaders))
			for i, leader := range leaders {
				found := false
				for j, pk := range pubkeys {
					if leader == pk {
						result[i] = j
						found = true
						break
					}
				}
				require.True(t, found, "leader %s at slot %d not found in pubkeys", leader, i)
			}

			assert.Equal(t, tc.expected, result, "leader schedule mismatch")
		})
	}
}

// TestUint64nLemireMethod validates that uint64n produces correct uniform distribution
// using Lemire's method with a known ChaCha20 seed.
func TestUint64nLemireMethod(t *testing.T) {
	// Create RNG with epoch=1 seed (same as Agave test vectors)
	var seedBytes [32]byte
	binary.LittleEndian.PutUint64(seedBytes[:], 1)
	var seed [8]uint32
	for i := 0; i < 8; i++ {
		seed[i] = binary.LittleEndian.Uint32(seedBytes[i*4:])
	}
	rng := chacha.Seeded20(seed, 0)

	t.Run("small range", func(t *testing.T) {
		// Generate several values and verify they're in range
		for i := 0; i < 100; i++ {
			val := uint64n(rng, 100)
			assert.Less(t, val, uint64(100), "value should be < 100")
		}
	})

	t.Run("range of 1 always returns 0", func(t *testing.T) {
		for i := 0; i < 10; i++ {
			val := uint64n(rng, 1)
			assert.Equal(t, uint64(0), val, "range of 1 should always return 0")
		}
	})

	t.Run("large range near MaxUint64", func(t *testing.T) {
		// Test with a very large range to exercise the rejection logic
		largeN := uint64(math.MaxUint64 - 1000)
		for i := 0; i < 10; i++ {
			val := uint64n(rng, largeN)
			assert.Less(t, val, largeN, "value should be < largeN")
		}
	})

	t.Run("panic on zero range", func(t *testing.T) {
		assert.Panics(t, func() {
			uint64n(rng, 0)
		}, "uint64n(0) should panic")
	})
}

// TestChaChaRawOutput prints raw ChaCha20 output for comparison with Agave.
// Run with: go test -v -run TestChaChaRawOutput ./pkg/leaderschedule
func TestChaChaRawOutput(t *testing.T) {
	epoch := uint64(905)

	// Seed ChaCha the same way as stakeWeightedSlotLeaders
	var seedBytes [32]byte
	binary.LittleEndian.PutUint64(seedBytes[:], epoch)
	var seed [8]uint32
	for i := 0; i < 8; i++ {
		seed[i] = binary.LittleEndian.Uint32(seedBytes[i*4:])
	}
	rng := chacha.Seeded20(seed, 0)

	t.Logf("ChaCha20 raw Uint64 output for epoch %d:", epoch)
	t.Logf("Seed bytes: %x", seedBytes)

	// Print first 20 raw uint64 values
	for i := 0; i < 20; i++ {
		val := rng.Uint64()
		t.Logf("  [%2d] %20d (0x%016x)", i, val, val)
	}

	// Now test with Lemire's method using a realistic total stake
	// Mainnet epoch 905 has ~400M SOL staked = ~400 trillion lamports
	totalStake := uint64(400_000_000_000_000_000) // 400M SOL in lamports

	// Reset RNG
	rng = chacha.Seeded20(seed, 0)

	t.Logf("\nLemire uint64n output for total_stake=%d:", totalStake)
	for i := 0; i < 20; i++ {
		val := uint64n(rng, totalStake)
		t.Logf("  [%2d] %20d", i, val)
	}
}

// TestStakeWeightedSlotLeadersPanics verifies edge case panics.
func TestStakeWeightedSlotLeadersPanics(t *testing.T) {
	pk1 := pubkeyFromU16(1)
	pk2 := pubkeyFromU16(2)

	t.Run("repeat zero panics", func(t *testing.T) {
		stakes := []pubkeyAndStakePair{
			{pubkey: pk1, stake: 100},
			{pubkey: pk2, stake: 200},
		}
		assert.PanicsWithValue(t, "stakeWeightedSlotLeaders: repeat cannot be 0", func() {
			stakeWeightedSlotLeaders(stakes, 1, 10, 0)
		})
	})

	t.Run("zero total stake panics", func(t *testing.T) {
		stakes := []pubkeyAndStakePair{
			{pubkey: pk1, stake: 0},
			{pubkey: pk2, stake: 0},
		}
		assert.PanicsWithValue(t, "stakeWeightedSlotLeaders: total stake is zero", func() {
			stakeWeightedSlotLeaders(stakes, 1, 10, 1)
		})
	})

	t.Run("cumulative stake overflow panics", func(t *testing.T) {
		// Two stakes that sum to more than MaxUint64
		stakes := []pubkeyAndStakePair{
			{pubkey: pk1, stake: math.MaxUint64},
			{pubkey: pk2, stake: 1},
		}
		assert.Panics(t, func() {
			stakeWeightedSlotLeaders(stakes, 1, 10, 1)
		}, "should panic on cumulative overflow")
	})
}
