package snapshot

import (
	"encoding/json"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/base58"
	"github.com/Overclock-Validator/mithril/pkg/epochstakes"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/state"
	"github.com/stretchr/testify/require"
)

func TestPopulateManifestSeedKeepsManifestEpochFrame(t *testing.T) {
	manifest := &SnapshotManifest{
		Bank: &DeserializableVersionedBank{
			Slot:  463538376,
			Epoch: 1073,
			EpochSchedule: sealevel.SysvarEpochSchedule{
				SlotsPerEpoch:            432000,
				LeaderScheduleSlotOffset: 432000,
			},
		},
		VersionedEpochStakes: []VersionedEpochStakesPair{
			{
				Epoch: 1073,
				Val: VersionedEpochStakes{
					TotalStake: 42,
					Stakes:     Stake{},
				},
			},
		},
	}
	mithrilState := state.NewReadyState(manifest.Bank.Slot, 1073, "", "", 0, 0)

	PopulateManifestSeed(mithrilState, manifest)

	require.Equal(t, uint64(432000), mithrilState.ManifestEpochSchedule.SlotsPerEpoch)
	require.Equal(t, uint64(432000), mithrilState.ManifestEpochSchedule.LeaderScheduleSlotOffset)

	data, exists := mithrilState.ManifestEpochStakes[1073]
	if !exists {
		t.Fatalf("expected manifest epoch stakes for epoch 1073, got keys %#v", mithrilState.ManifestEpochStakes)
	}
	var persisted epochstakes.PersistedEpochStakes
	if err := json.Unmarshal([]byte(data), &persisted); err != nil {
		t.Fatalf("failed to decode persisted epoch stakes: %v", err)
	}
	if persisted.Epoch != 1073 {
		t.Fatalf("persisted epoch = %d, want 1073", persisted.Epoch)
	}
}

func TestPopulateManifestSeedReplacesSnapshotBlockID(t *testing.T) {
	blockID := [32]byte{1, 2, 3}
	mithrilState := &state.MithrilState{ManifestParentAlpenglowBlockID: "stale"}
	manifest := &SnapshotManifest{Bank: &DeserializableVersionedBank{}, BlockID: &blockID}

	PopulateManifestSeed(mithrilState, manifest)
	require.Equal(t, base58.Encode(blockID[:]), mithrilState.ManifestParentAlpenglowBlockID)

	manifest.BlockID = nil
	PopulateManifestSeed(mithrilState, manifest)
	require.Empty(t, mithrilState.ManifestParentAlpenglowBlockID)
}
