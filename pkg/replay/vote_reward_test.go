package replay

import (
	"math"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanonicalizeAlpenglowVoteAccountMatchesAgaveConstructor(t *testing.T) {
	var key, owner solana.PublicKey
	key[0], owner[0] = 1, 2
	acct := &accounts.Account{
		Key:        key,
		Lamports:   42,
		Data:       []byte{3, 4},
		Owner:      owner,
		Executable: true,
		RentEpoch:  math.MaxUint64,
	}

	canonicalizeAlpenglowVoteAccount(acct)

	assert.Equal(t, key, acct.Key)
	assert.Equal(t, uint64(42), acct.Lamports)
	assert.Equal(t, []byte{3, 4}, acct.Data)
	assert.Equal(t, [32]byte(owner), acct.Owner)
	assert.False(t, acct.Executable)
	assert.Zero(t, acct.RentEpoch)
}

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

func TestCalcSlotTimestampNanosInclusiveRange(t *testing.T) {
	const producer int64 = 1783653042790683871
	schedule := &sealevel.SysvarEpochSchedule{SlotsPerEpoch: 100, FirstNormalEpoch: 0}
	got := calcSlotTimestampNanos(nil, schedule, 965552, 965560, producer)
	want := producer - int64(965560-965552)*int64(legacyNsPerSlot)
	assert.Equal(t, want, got)
	assert.Equal(t, int64(1783653039), got/1_000_000_000)

	assert.Equal(t, producer, calcSlotTimestampNanos(nil, schedule, 965560, 965560, producer))
	assert.Equal(t, int64(math.MinInt64), calcSlotTimestampNanos(nil, schedule, 10, 20, math.MinInt64+1))
	assert.Equal(t, int64(math.MinInt64), calcSlotTimestampNanos(nil, schedule, 0, math.MaxUint64, producer))
}

func TestCalcSlotTimestampNanosCrossesSlotTimeTransition(t *testing.T) {
	schedule := &sealevel.SysvarEpochSchedule{SlotsPerEpoch: 100, FirstNormalEpoch: 0}
	f := features.NewFeaturesDefault()
	// Activation in epoch 1 becomes effective at the first slot of epoch 2.
	f.EnableFeature(features.ReduceSlotTimeTo200ms, 150)

	const producer int64 = 10_000_000_000
	// target+1..=bank is slots 199..202: 400ms + 3*200ms.
	assert.Equal(t, producer-1_000_000_000, calcSlotTimestampNanos(f, schedule, 198, 202, producer))
}

func TestMaybeUpdateVotesV4AdvancesLastTimestamp(t *testing.T) {
	vs := &sealevel.VoteState4{
		LastTimestamp: sealevel.BlockTimestamp{Slot: 100, Timestamp: 1_000},
	}
	maybeUpdateVotesV4(vs, 200, 1_500_000_000_000)
	assert.Equal(t, uint64(200), vs.LastTimestamp.Slot)
	assert.Equal(t, int64(1_500), vs.LastTimestamp.Timestamp)
	require.Equal(t, 1, vs.Votes.Len())
	assert.Equal(t, uint64(200), vs.Votes.At(0).Lockout.Slot)

	// Strictly greater timestamp required; equal/older must not rewrite.
	prev := vs.LastTimestamp
	maybeUpdateVotesV4(vs, 300, 1_500_000_000_000)
	assert.Equal(t, prev, vs.LastTimestamp)
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
