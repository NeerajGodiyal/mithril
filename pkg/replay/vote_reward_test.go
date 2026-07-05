package replay

import (
	"math"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIncrementAlpenglowCreditsNewEpoch(t *testing.T) {
	var credits []sealevel.EpochCredits
	incrementAlpenglowCredits(&credits, 99, 5, 10)
	require.Len(t, credits, 1)
	assert.Equal(t, uint64(5), credits[0].Epoch)
	assert.Equal(t, uint64(10), credits[0].Credits)
}

func TestIncrementAlpenglowCreditsSameEpoch(t *testing.T) {
	credits := []sealevel.EpochCredits{{Epoch: 5, Credits: 10, PrevCredits: 0}}
	incrementAlpenglowCredits(&credits, 99, 5, 3)
	assert.Equal(t, uint64(13), credits[0].Credits)
}

func TestIncrementAlpenglowCreditsAfterMigrationMarker(t *testing.T) {
	credits := []sealevel.EpochCredits{
		{Epoch: 4, Credits: 20, PrevCredits: 10},
		agMigrationEpochCredit,
	}
	incrementAlpenglowCredits(&credits, 4, 5, 7)
	require.Len(t, credits, 3)
	last := credits[2]
	assert.Equal(t, uint64(5), last.Epoch)
	assert.Equal(t, uint64(27), last.Credits)
	assert.Equal(t, uint64(20), last.PrevCredits)
}

func TestCalculateAlpenglowRewardSplit(t *testing.T) {
	inflation := EpochInflationState{
		MaxPossibleValidatorReward: 1_000_000,
		SlotsPerEpoch:              100,
	}
	validator, leader := calculateAlpenglowReward(inflation, 1_000, 500)
	assert.Equal(t, uint64(5000), validator+leader)
	assert.Equal(t, validator, leader)
}

func TestEnsureMigrationMarker(t *testing.T) {
	var credits []sealevel.EpochCredits
	ensureMigrationMarker(&credits)
	require.Len(t, credits, 1)
	assert.Equal(t, uint64(math.MaxUint64), credits[0].Epoch)
}

func TestWriteVersionedVoteStateInPlacePreservesAccountDataLen(t *testing.T) {
	var votePubkey, withdrawer solana.PublicKey
	votePubkey[0] = 1
	withdrawer[1] = 2

	var authVoters sealevel.AuthorizedVoters
	authVoters.AuthorizedVoters.Set(1, votePubkey)

	versioned := &sealevel.VoteStateVersions{
		Type: sealevel.VoteStateVersionV4,
		V4: sealevel.VoteState4{
			NodePubkey:                    votePubkey,
			AuthorizedWithdrawer:          withdrawer,
			AuthorizedVoters:              authVoters,
			InflationRewardsCollector:     votePubkey,
			BlockRevenueCollector:         votePubkey,
			InflationRewardsCommissionBps: 0,
			BlockRevenueCommissionBps:     10000,
		},
	}

	marshaled, err := sealevel.MarshalVersionedVoteState(versioned)
	require.NoError(t, err)
	require.Less(t, len(marshaled), sealevel.VoteStateV3Size)

	data := make([]byte, sealevel.VoteStateV3Size)
	copy(data, marshaled)
	data[len(data)-1] = 0x42

	require.NoError(t, sealevel.WriteVersionedVoteStateInPlace(data, versioned))
	require.Equal(t, sealevel.VoteStateV3Size, len(data))
	require.Equal(t, marshaled, data[:len(marshaled)])
	for _, b := range data[len(marshaled):] {
		require.Equal(t, byte(0), b)
	}
}
