package replay

import (
	"crypto/ed25519"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/alpenglow"
	"github.com/Overclock-Validator/mithril/pkg/epochstakes"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestEpochTransitionTargetEpochs(t *testing.T) {
	const (
		slotsPerEpoch = uint64(54_000)
		boundarySlot  = uint64(110 * slotsPerEpoch)
	)

	tests := []struct {
		name   string
		offset uint64
		want   []uint64
	}{
		{
			name:   "standard one epoch offset",
			offset: slotsPerEpoch,
			want:   []uint64{110, 111},
		},
		{
			name:   "same epoch is deduplicated",
			offset: slotsPerEpoch / 2,
			want:   []uint64{110},
		},
		{
			name:   "nonstandard offset is not hardcoded",
			offset: 2 * slotsPerEpoch,
			want:   []uint64{110, 112},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			epochSchedule := &sealevel.SysvarEpochSchedule{
				SlotsPerEpoch:            slotsPerEpoch,
				LeaderScheduleSlotOffset: tt.offset,
			}
			newEpoch := epochSchedule.GetEpoch(boundarySlot)
			leaderScheduleEpoch := epochSchedule.LeaderScheduleEpoch(boundarySlot)

			require.Equal(t, tt.want, epochTransitionTargetEpochs(newEpoch, leaderScheduleEpoch))
		})
	}
}

func TestPrepareEpochTransitionLeaderSchedulesPreparesFutureEpoch(t *testing.T) {
	const slotsPerEpoch = uint64(54_000)
	epochSchedule := &sealevel.SysvarEpochSchedule{
		SlotsPerEpoch:            slotsPerEpoch,
		LeaderScheduleSlotOffset: slotsPerEpoch,
	}
	_, nodePubkey := seedEpochTransitionValidator(t, 110, 111)

	err := prepareEpochTransitionLeaderSchedules(110, 111, epochSchedule, t.TempDir())
	require.NoError(t, err)

	for _, epoch := range []uint64{110, 111} {
		leader, ok := global.LeaderForSlot(epochSchedule.FirstSlotInEpoch(epoch))
		require.Truef(t, ok, "leader schedule for epoch %d was not installed", epoch)
		require.Equal(t, nodePubkey, leader)
	}
}

type epochTransitionValidatorSetRecorder struct {
	fakeEngine
	installed []alpenglow.ValidatorSet
}

func (r *epochTransitionValidatorSetRecorder) SetAlpenglowValidatorSet(set alpenglow.ValidatorSet) error {
	r.installed = append(r.installed, set)
	return nil
}

func TestInstallEpochTransitionAlpenglowValidatorSetsIncludesFuture(t *testing.T) {
	seedEpochTransitionValidator(t, 110, 111)
	recorder := new(epochTransitionValidatorSetRecorder)

	installEpochTransitionAlpenglowValidatorSets(recorder, 110, 111)

	require.Len(t, recorder.installed, 2)
	require.Equal(t, uint64(110), recorder.installed[0].Epoch)
	require.Equal(t, uint64(111), recorder.installed[1].Epoch)
}

func TestInstallEpochTransitionAlpenglowValidatorSetsDeduplicatesEqualEpoch(t *testing.T) {
	seedEpochTransitionValidator(t, 110)
	recorder := new(epochTransitionValidatorSetRecorder)

	installEpochTransitionAlpenglowValidatorSets(recorder, 110, 110)

	require.Len(t, recorder.installed, 1)
	require.Equal(t, uint64(110), recorder.installed[0].Epoch)
}

func seedEpochTransitionValidator(t *testing.T, epochs ...uint64) (solana.PublicKey, solana.PublicKey) {
	t.Helper()

	var voteAccount, nodePubkey solana.PublicKey
	voteAccount[0] = 0xa1
	nodePubkey[0] = 0xb1

	privateKey := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	signer, err := alpenglow.DeriveBLSSigner(privateKey)
	require.NoError(t, err)
	compressed := signer.PublicKeyCompressed()

	for _, epoch := range epochs {
		global.ClearEpochStakes(epoch)
		global.SetLeaderScheduleForEpoch(epoch, nil)

		blsPubkey := compressed
		global.PutEpochStakesEntry(epoch, voteAccount, 100, &epochstakes.VoteAccount{
			NodePubkey:          nodePubkey,
			BlsPubkeyCompressed: &blsPubkey,
		})
		global.PutEpochTotalStake(epoch, 100)
	}

	t.Cleanup(func() {
		for _, epoch := range epochs {
			global.ClearEpochStakes(epoch)
			global.SetLeaderScheduleForEpoch(epoch, nil)
		}
	})

	return voteAccount, nodePubkey
}
