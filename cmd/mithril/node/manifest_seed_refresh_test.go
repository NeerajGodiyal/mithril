package node

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/snapshot"
	"github.com/Overclock-Validator/mithril/pkg/state"
	"github.com/stretchr/testify/require"
)

func TestRefreshManifestSeedRestoresClearedEpochStakes(t *testing.T) {
	schedule := sealevel.SysvarEpochSchedule{
		SlotsPerEpoch:            432000,
		LeaderScheduleSlotOffset: 432000,
	}
	manifest := &snapshot.SnapshotManifest{
		Bank: &snapshot.DeserializableVersionedBank{
			Slot:          100,
			Epoch:         7,
			EpochSchedule: schedule,
		},
		VersionedEpochStakes: []snapshot.VersionedEpochStakesPair{
			{Epoch: 7, Val: snapshot.VersionedEpochStakes{TotalStake: 42}},
		},
	}
	s := state.NewReadyState(manifest.Bank.Slot, manifest.Bank.Epoch, "", "", 0, 0)
	s.ManifestEpochSchedule = &state.ManifestEpochScheduleSeed{
		SlotsPerEpoch:            schedule.SlotsPerEpoch,
		LeaderScheduleSlotOffset: schedule.LeaderScheduleSlotOffset,
	}
	s.ManifestEpochStakes = nil

	refreshManifestSeedFromManifest(t.TempDir(), s, manifest)

	require.Contains(t, s.ManifestEpochStakes, uint64(7))
}
