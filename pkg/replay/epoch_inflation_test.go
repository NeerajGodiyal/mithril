package replay

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	a "github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/rewards"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/stretchr/testify/require"
)

func TestDecodeEncodeEpochInflationAccountStateAgaveFixture(t *testing.T) {
	// Observed on Alpenglow RPC after the epoch 114 -> 115 boundary.
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
	require.Equal(t, data, encodeEpochInflationAccountState(decoded))
}

func TestNewEpochInflationStateUsesVotingRewards(t *testing.T) {
	epochSchedule := &sealevel.SysvarEpochSchedule{SlotsPerEpoch: 54000}
	inflation := rewards.Inflation{Initial: 0.08, Terminal: 0.015, Taper: 0.15}
	f := features.NewFeaturesDefault()
	f.EnableFeature(features.FullInflationEnable, 0)
	f.EnableFeature(features.FullInflationVote, 0)

	withoutVotingRewards := newEpochInflationState(
		epochSchedule, &inflation, 10_000_000_000_000, 0, 115, 78_840_000, f,
	)
	withVotingRewards := newEpochInflationState(
		epochSchedule, &inflation, 10_000_000_000_000, 5_000_000_000_000, 115, 78_840_000, f,
	)

	require.Greater(t, withVotingRewards.MaxPossibleValidatorReward, withoutVotingRewards.MaxPossibleValidatorReward)
	require.Equal(t, uint64(115), withVotingRewards.Epoch)
	require.Equal(t, uint64(54000), withVotingRewards.SlotsPerEpoch)
}

func TestStageEpochInflationAccountRollsStateAndCapitalization(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "accounts"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "largest_file_id"), make([]byte, 8), 0o644))

	db, err := accountsdb.OpenDb(dir)
	require.NoError(t, err)
	db.InitCaches()
	t.Cleanup(db.CloseDb)

	previousRent := sealevel.SysvarCache.Rent.Sysvar
	rent := sealevel.NewDefaultRentSysvar()
	sealevel.SysvarCache.Rent.Sysvar = &rent
	t.Cleanup(func() { sealevel.SysvarCache.Rent.Sysvar = previousRent })

	existing := EpochInflationAccountState{Current: EpochInflationState{
		MaxPossibleValidatorReward: 123,
		SlotsPerEpoch:              54000,
		Epoch:                      114,
	}}
	parent := &accounts.Account{
		Key:       VoteRewardAccountAddr(),
		Lamports:  500,
		Data:      encodeEpochInflationAccountState(existing),
		Owner:     a.SystemProgramAddr,
		RentEpoch: 0,
	}
	parentStored := make(chan struct{})
	require.NoError(t, db.StoreAccounts([]*accounts.Account{parent}, 10, func() { close(parentStored) }))
	<-parentStored

	f := features.NewFeaturesDefault()
	f.EnableFeature(features.FullInflationEnable, 0)
	f.EnableFeature(features.FullInflationVote, 0)
	replayCtx := &ReplayCtx{
		Capitalization: 10_000_000_000_000,
		Inflation:      rewards.Inflation{Initial: 0.08, Terminal: 0.015, Taper: 0.15},
		SlotsPerYear:   78_840_000,
	}
	startingCapitalization := replayCtx.Capitalization

	updated, capturedParent, err := stageEpochInflationAccount(
		db, 10, 11, replayCtx, &sealevel.SysvarEpochSchedule{SlotsPerEpoch: 54000}, f,
		115, startingCapitalization, 5_000_000,
	)
	require.NoError(t, err)
	require.Equal(t, parent.Clone(), capturedParent)
	require.Equal(t, uint64(11), updated.Slot)
	require.Equal(t, a.SystemProgramAddr, updated.Owner)
	require.Equal(t, rent.MinimumBalance(uint64(len(updated.Data))), updated.Lamports)
	require.Equal(t, startingCapitalization+updated.Lamports-parent.Lamports, replayCtx.Capitalization)

	state, err := decodeEpochInflationAccountState(updated.Data)
	require.NoError(t, err)
	require.Equal(t, uint64(115), state.Current.Epoch)
	require.NotZero(t, state.Current.MaxPossibleValidatorReward)
	require.NotNil(t, state.Prev)
	require.Equal(t, existing.Current, *state.Prev)

	db.WaitForStoreWorker()
	stored, err := db.GetAccount(11, VoteRewardAccountAddr())
	require.NoError(t, err)
	require.Equal(t, updated.Data, stored.Data)
	require.Equal(t, updated.Lamports, stored.Lamports)
}
