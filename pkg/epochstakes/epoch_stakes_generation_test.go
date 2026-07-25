package epochstakes

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEpochStakesGenerationTracksMaterialChanges(t *testing.T) {
	const epoch = uint64(17)
	cache := NewEpochStakesCache()
	assert.Zero(t, cache.Generation(epoch))

	var vote solana.PublicKey
	vote[0] = 1
	cache.PutEntry(epoch, vote, 100, nil)
	afterEntry := cache.Generation(epoch)
	require.NotZero(t, afterEntry)

	cache.PutTotalEpochStake(epoch, 100)
	afterTotal := cache.Generation(epoch)
	assert.NotEqual(t, afterEntry, afterTotal)

	cache.ClearEpochStakes(epoch)
	afterClear := cache.Generation(epoch)
	assert.NotEqual(t, afterTotal, afterClear)

	data, err := json.Marshal(PersistedEpochStakes{
		Epoch:      epoch,
		TotalStake: 0,
		Stakes:     map[string]uint64{},
		VoteAccts:  map[string]*VoteAccountJSON{},
	})
	require.NoError(t, err)
	loadedEpoch, err := cache.DeserializeAndLoadEpoch(data)
	require.NoError(t, err)
	assert.Equal(t, epoch, loadedEpoch)
	assert.NotEqual(t, afterClear, cache.Generation(epoch))

	beforeInvalidReload := cache.Generation(epoch)
	invalidData, err := json.Marshal(PersistedEpochStakes{
		Epoch:      epoch,
		TotalStake: 1,
		Stakes:     map[string]uint64{"not-a-pubkey": 1},
		VoteAccts:  map[string]*VoteAccountJSON{},
	})
	require.NoError(t, err)
	_, err = cache.DeserializeAndLoadEpoch(invalidData)
	require.Error(t, err)
	assert.Equal(t, beforeInvalidReload, cache.Generation(epoch))
	assert.Empty(t, cache.EpochStakes(epoch))
}

func TestEpochStakesGenerationIsUniqueAcrossCacheReplacement(t *testing.T) {
	const epoch = uint64(29)
	first := NewEpochStakesCache()
	first.PutTotalEpochStake(epoch, 1)

	second := NewEpochStakesCache()
	second.PutTotalEpochStake(epoch, 1)

	assert.NotEqual(t, first.Generation(epoch), second.Generation(epoch))
}

func TestEpochStakesPutEpochAndCompatibilityAccessorsAreDeeplyImmutable(t *testing.T) {
	const epoch = uint64(31)
	cache := NewEpochStakesCache()

	var votePubkey solana.PublicKey
	votePubkey[0] = 1
	var nodePubkey solana.PublicKey
	nodePubkey[0] = 2
	var owner solana.PublicKey
	owner[0] = 3
	var bls [48]byte
	bls[0] = 4
	voteAccount := &VoteAccount{
		Lamports:            100,
		NodePubkey:          nodePubkey,
		BlsPubkeyCompressed: &bls,
		LastTimestampTs:     5,
		LastTimestampSlot:   6,
		Owner:               owner,
		Executable:          1,
		RentEpoch:           7,
	}
	stakes := map[solana.PublicKey]uint64{votePubkey: 100}
	voteAccounts := map[solana.PublicKey]*VoteAccount{votePubkey: voteAccount}

	generation := cache.PutEpoch(epoch, stakes, voteAccounts, 100)
	require.NotZero(t, generation)

	// Mutating every caller-owned layer after PutEpoch must not affect the cache.
	stakes[votePubkey] = 999
	voteAccounts[votePubkey] = nil
	voteAccount.Lamports = 999
	bls[0] = 99

	snapshot, ok := cache.Snapshot(epoch)
	require.True(t, ok)
	assert.Equal(t, generation, snapshot.Generation)
	assert.Equal(t, uint64(100), snapshot.TotalStake)
	assert.Equal(t, uint64(100), snapshot.Stakes[votePubkey])
	require.NotNil(t, snapshot.VoteAccounts[votePubkey])
	assert.Equal(t, uint64(100), snapshot.VoteAccounts[votePubkey].Lamports)
	require.NotNil(t, snapshot.VoteAccounts[votePubkey].BlsPubkeyCompressed)
	assert.Equal(t, byte(4), snapshot.VoteAccounts[votePubkey].BlsPubkeyCompressed[0])

	// Compatibility getters are detached deep copies.
	stakesCopy := cache.EpochStakes(epoch)
	voteAccountsCopy := cache.EpochStakesAccts(epoch)
	stakesCopy[votePubkey] = 777
	voteAccountsCopy[votePubkey].Lamports = 777
	voteAccountsCopy[votePubkey].BlsPubkeyCompressed[0] = 77

	unchanged, ok := cache.Snapshot(epoch)
	require.True(t, ok)
	assert.Equal(t, uint64(100), unchanged.Stakes[votePubkey])
	assert.Equal(t, uint64(100), unchanged.VoteAccounts[votePubkey].Lamports)
	assert.Equal(t, byte(4), unchanged.VoteAccounts[votePubkey].BlsPubkeyCompressed[0])

	// Compatibility mutation is copy-on-write, so the previously published
	// snapshot remains immutable while a new generation receives the update.
	var replacementBLS [48]byte
	replacementBLS[0] = 5
	cache.PutEntry(epoch, votePubkey, 200, &VoteAccount{Lamports: 200, BlsPubkeyCompressed: &replacementBLS})
	assert.Equal(t, uint64(100), snapshot.Stakes[votePubkey])
	assert.Equal(t, uint64(100), snapshot.VoteAccounts[votePubkey].Lamports)
	assert.Equal(t, byte(4), snapshot.VoteAccounts[votePubkey].BlsPubkeyCompressed[0])
	replaced, ok := cache.Snapshot(epoch)
	require.True(t, ok)
	assert.NotEqual(t, snapshot.Generation, replaced.Generation)
	assert.Equal(t, uint64(200), replaced.Stakes[votePubkey])
	assert.Equal(t, uint64(200), replaced.VoteAccounts[votePubkey].Lamports)
	assert.Equal(t, byte(5), replaced.VoteAccounts[votePubkey].BlsPubkeyCompressed[0])
}

func TestEpochStakesCompatibilityPutEntryClonesVoteAccount(t *testing.T) {
	const epoch = uint64(32)
	cache := NewEpochStakesCache()
	var votePubkey solana.PublicKey
	votePubkey[0] = 1
	var bls [48]byte
	bls[0] = 2
	voteAccount := &VoteAccount{Lamports: 3, BlsPubkeyCompressed: &bls}

	cache.PutEntry(epoch, votePubkey, 4, voteAccount)
	voteAccount.Lamports = 30
	bls[0] = 20

	snapshot, ok := cache.Snapshot(epoch)
	require.True(t, ok)
	assert.Equal(t, uint64(4), snapshot.Stakes[votePubkey])
	assert.Equal(t, uint64(3), snapshot.VoteAccounts[votePubkey].Lamports)
	assert.Equal(t, byte(2), snapshot.VoteAccounts[votePubkey].BlsPubkeyCompressed[0])
}

func TestEpochStakesSnapshotPublicationIsAtomic(t *testing.T) {
	const (
		epoch      = uint64(33)
		iterations = 2_000
		readers    = 4
	)
	cache := NewEpochStakesCache()
	var votePubkey solana.PublicKey
	votePubkey[0] = 1

	makeMaterial := func(value uint64) (map[solana.PublicKey]uint64, map[solana.PublicKey]*VoteAccount) {
		var bls [48]byte
		bls[0] = byte(value)
		return map[solana.PublicKey]uint64{votePubkey: value}, map[solana.PublicKey]*VoteAccount{
			votePubkey: {Lamports: value, BlsPubkeyCompressed: &bls},
		}
	}
	stakesA, voteAccountsA := makeMaterial(11)
	stakesB, voteAccountsB := makeMaterial(22)
	cache.PutEpoch(epoch, stakesA, voteAccountsA, 11)

	start := make(chan struct{})
	errs := make(chan error, readers)
	var wg sync.WaitGroup
	wg.Add(readers + 1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < iterations; i++ {
			if i%2 == 0 {
				cache.PutEpoch(epoch, stakesB, voteAccountsB, 22)
			} else {
				cache.PutEpoch(epoch, stakesA, voteAccountsA, 11)
			}
		}
	}()
	for reader := 0; reader < readers; reader++ {
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < iterations; i++ {
				snapshot, ok := cache.Snapshot(epoch)
				if !ok {
					errs <- fmt.Errorf("epoch disappeared")
					return
				}
				stake := snapshot.Stakes[votePubkey]
				voteAccount := snapshot.VoteAccounts[votePubkey]
				if voteAccount == nil || voteAccount.BlsPubkeyCompressed == nil {
					errs <- fmt.Errorf("missing vote account material")
					return
				}
				if snapshot.Generation == 0 || snapshot.TotalStake != stake || voteAccount.Lamports != stake || uint64(voteAccount.BlsPubkeyCompressed[0]) != stake {
					errs <- fmt.Errorf("mixed snapshot: generation=%d stake=%d total=%d lamports=%d bls=%d", snapshot.Generation, stake, snapshot.TotalStake, voteAccount.Lamports, voteAccount.BlsPubkeyCompressed[0])
					return
				}
				if stake != 11 && stake != 22 {
					errs <- fmt.Errorf("unexpected stake %d", stake)
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}

func TestEpochStakesDeserializeNilVoteAccountIsFailureAtomic(t *testing.T) {
	const epoch = uint64(34)
	cache := NewEpochStakesCache()
	var votePubkey solana.PublicKey
	votePubkey[0] = 1
	cache.PutEpoch(epoch, map[solana.PublicKey]uint64{votePubkey: 10}, map[solana.PublicKey]*VoteAccount{votePubkey: {Lamports: 10}}, 10)
	before, ok := cache.Snapshot(epoch)
	require.True(t, ok)

	data, err := json.Marshal(PersistedEpochStakes{
		Epoch:      epoch,
		TotalStake: 20,
		Stakes:     map[string]uint64{votePubkey.String(): 20},
		VoteAccts:  map[string]*VoteAccountJSON{votePubkey.String(): nil},
	})
	require.NoError(t, err)
	_, err = cache.DeserializeAndLoadEpoch(data)
	require.Error(t, err)

	after, ok := cache.Snapshot(epoch)
	require.True(t, ok)
	assert.Equal(t, before, after)
}
