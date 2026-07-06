package replay

import (
	"encoding/hex"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/rewards"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/stretchr/testify/require"
)

func TestDecodeEncodeEpochInflationAccountStateAgaveFixture(t *testing.T) {
	// Observed on Alpenglow RPC for vote_reward_account after epoch 114->115 boundary.
	const fixtureHex = "2446a24a89170000f0d20000000000007300000000000000018fe1d3b589170000f0d20000000000007200000000000000"
	data, err := hex.DecodeString(fixtureHex)
	require.NoError(t, err)

	decoded, err := decodeEpochInflationAccountState(data)
	require.NoError(t, err)
	require.Equal(t, uint64(25878430107172), decoded.Current.MaxPossibleValidatorReward)
	require.Equal(t, uint64(54000), decoded.Current.SlotsPerEpoch)
	require.Equal(t, uint64(115), decoded.Current.Epoch)
	require.NotNil(t, decoded.Prev)
	require.Equal(t, uint64(114), decoded.Prev.Epoch)

	reencoded := encodeEpochInflationAccountState(decoded)
	require.Equal(t, data, reencoded)
}

func TestNewEpochInflationStateUsesAdditionalValidatorRewards(t *testing.T) {
	epochSchedule := &sealevel.SysvarEpochSchedule{
		SlotsPerEpoch: 54000,
	}
	inflation := rewards.Inflation{
		Initial:  0.08,
		Terminal: 0.015,
		Taper:    0.15,
	}
	f := features.NewFeaturesDefault()
	f.EnableFeature(features.FullInflationEnable, 0)
	f.EnableFeature(features.FullInflationVote, 0)

	capitalization := uint64(10_000_000_000_000)
	additional := uint64(5_000_000_000_000)
	slotsPerYear := 78840000.0

	withoutAdditional := newEpochInflationState(
		epochSchedule, &inflation, capitalization, 0, 115, slotsPerYear, f,
	)
	withAdditional := newEpochInflationState(
		epochSchedule, &inflation, capitalization, additional, 115, slotsPerYear, f,
	)
	require.Greater(t, withAdditional.MaxPossibleValidatorReward, withoutAdditional.MaxPossibleValidatorReward)
	require.Equal(t, uint64(115), withAdditional.Epoch)
	require.Equal(t, uint64(54000), withAdditional.SlotsPerEpoch)
}

func TestNewEpochInflationStateRollsPrevFromExistingCurrent(t *testing.T) {
	existing := EpochInflationAccountState{
		Current: EpochInflationState{
			MaxPossibleValidatorReward: 123,
			SlotsPerEpoch:              54000,
			Epoch:                      114,
		},
	}
	prev := existing.Current
	current := EpochInflationState{
		MaxPossibleValidatorReward: 456,
		SlotsPerEpoch:              54000,
		Epoch:                      115,
	}
	state := EpochInflationAccountState{Current: current, Prev: &prev}

	data := encodeEpochInflationAccountState(state)
	decoded, err := decodeEpochInflationAccountState(data)
	require.NoError(t, err)
	require.Equal(t, current, decoded.Current)
	require.NotNil(t, decoded.Prev)
	require.Equal(t, existing.Current, *decoded.Prev)
}
