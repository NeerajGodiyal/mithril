package replay

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/stretchr/testify/assert"
)

func TestProductionLimitsFollowDelayedSlotTimeStage(t *testing.T) {
	schedule := &sealevel.SysvarEpochSchedule{SlotsPerEpoch: 100, FirstNormalEpoch: 0}
	f := features.NewFeaturesDefault()
	f.EnableFeature(features.ReduceSlotTimeTo350ms, 50)
	f.EnableFeature(features.ReduceSlotTimeTo300ms, 150)
	f.EnableFeature(features.ReduceSlotTimeTo250ms, 250)
	f.EnableFeature(features.ReduceSlotTimeTo200ms, 350)

	tests := []struct {
		slot           uint64
		blockCost      uint64
		writableCost   uint64
		dataDelta      uint64
		dataShreds     uint64
		maxEntryBytes  uint64
		rewardAccounts uint64
	}{
		{99, 60_000_000, 24_000_000, 100_000_000, 32_768, 20*1024*1024 - 48, 4096},
		{100, 52_500_000, 21_000_000, 87_500_000, 28_672, 17*1024*1024 + 512*1024 - 48, 3584},
		{200, 45_000_000, 18_000_000, 75_000_000, 24_576, 15*1024*1024 - 48, 3072},
		{300, 37_500_000, 15_000_000, 62_500_000, 20_480, 12*1024*1024 + 512*1024 - 48, 2560},
		{400, 30_000_000, 12_000_000, 50_000_000, 16_384, 10*1024*1024 - 48, 2048},
	}
	for _, tt := range tests {
		limits := ProductionLimitsForSlot(f, schedule, tt.slot)
		assert.Equal(t, tt.blockCost, limits.BlockCost, "slot %d block cost", tt.slot)
		assert.Equal(t, tt.writableCost, limits.WritableAccountCost, "slot %d writable cost", tt.slot)
		assert.Equal(t, tt.dataDelta, limits.AllocatedDataSizeDelta, "slot %d data delta", tt.slot)
		assert.Equal(t, tt.dataShreds, limits.MaxDataShreds, "slot %d data shreds", tt.slot)
		assert.Equal(t, tt.maxEntryBytes, limits.MaxEntryBytes, "slot %d entry bytes", tt.slot)
		assert.Equal(t, tt.rewardAccounts, limits.RewardAccountsPerBlock, "slot %d reward accounts", tt.slot)
	}
}

func TestUpdateSlotsPerYearAtDelayedSlotTimeBoundary(t *testing.T) {
	schedule := &sealevel.SysvarEpochSchedule{SlotsPerEpoch: 100, FirstNormalEpoch: 0}
	f := features.NewFeaturesDefault()
	f.EnableFeature(features.ReduceSlotTimeTo350ms, 50)
	f.EnableFeature(features.ReduceSlotTimeTo300ms, 150)
	replayCtx := &ReplayCtx{SlotsPerYear: 90_162_645.696}

	updateSlotsPerYearForSlot(replayCtx, schedule, f, 200)
	assert.Equal(t, 105_189_753.312, replayCtx.SlotsPerYear)
}

func TestShredFeatureEffectiveFollowingEpoch(t *testing.T) {
	schedule := &sealevel.SysvarEpochSchedule{SlotsPerEpoch: 100, FirstNormalEpoch: 0}
	f := features.NewFeaturesDefault()
	f.EnableFeature(features.EnforceFixedFECSet, 50)
	assert.False(t, shredFeatureEffective(f, schedule, features.EnforceFixedFECSet, 99))
	assert.True(t, shredFeatureEffective(f, schedule, features.EnforceFixedFECSet, 100))
}

func TestProductionLimitsComposeRaisedBlockLimits(t *testing.T) {
	schedule := &sealevel.SysvarEpochSchedule{SlotsPerEpoch: 100, FirstNormalEpoch: 0}
	f := features.NewFeaturesDefault()
	f.EnableFeature(features.ReduceSlotTimeTo350ms, 50)
	f.EnableFeature(features.RaiseBlockLimitsTo100m, 75)

	before := ProductionLimitsForSlot(f, schedule, 74)
	assert.Equal(t, uint64(60_000_000), before.BlockCost)
	assert.Equal(t, uint64(24_000_000), before.WritableAccountCost)

	limits := ProductionLimitsForSlot(f, schedule, 100)
	assert.Equal(t, uint64(87_500_000), limits.BlockCost)
	assert.Equal(t, uint64(35_000_000), limits.WritableAccountCost)
	assert.Equal(t, uint64(87_500_000), limits.AllocatedDataSizeDelta)
}
