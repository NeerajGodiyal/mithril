package replay

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestAlpenglowVATBurnChangesAtEpochAfterSlotTimeActivation(t *testing.T) {
	f := features.NewFeaturesDefault()
	f.EnableFeature(features.Alpenglow, 1)
	f.EnableFeature(features.ReduceSlotTimeTo200ms, 50)
	schedule := &sealevel.SysvarEpochSchedule{SlotsPerEpoch: 100, LeaderScheduleSlotOffset: 100}

	require.Equal(t, legacyVATBurnPerEpoch, alpenglowVATBurnPerEpoch(f, schedule, 99))
	require.Equal(t, vatBurn200ms, alpenglowVATBurnPerEpoch(f, schedule, 100))
}

func TestFilterEpochStakesForVATRejectsUnderfundedAndMissingBLS(t *testing.T) {
	const minimumBalance = uint64(900)
	var funded, underfunded, missingBLS solana.PublicKey
	funded[0], underfunded[0], missingBLS[0] = 1, 2, 3
	bls := [48]byte{1}
	voteCache := map[solana.PublicKey]*sealevel.VoteStateVersions{
		funded:      {Type: sealevel.VoteStateVersionV4, V4: sealevel.VoteState4{BlsPubkeyCompressed: &bls}},
		underfunded: {Type: sealevel.VoteStateVersionV4, V4: sealevel.VoteState4{BlsPubkeyCompressed: &bls}},
		missingBLS:  {Type: sealevel.VoteStateVersionV4},
	}
	metadata := map[solana.PublicKey]rebuiltVoteAccountMeta{
		funded:      {Lamports: minimumBalance},
		underfunded: {Lamports: minimumBalance - 1},
		missingBLS:  {Lamports: minimumBalance},
	}
	stakes := map[solana.PublicKey]uint64{funded: 100, underfunded: 200, missingBLS: 300}

	filtered, total := filterEpochStakesForVAT(stakes, voteCache, metadata, minimumBalance)
	require.Equal(t, map[solana.PublicKey]uint64{funded: 100}, filtered)
	require.Equal(t, uint64(100), total)
}
