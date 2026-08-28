package replay

import (
	"github.com/Overclock-Validator/mithril/pkg/costmodel"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/rewards"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
)

// ProductionLimitsForSlot returns the SIMD-0525 limits effective at slot.
func ProductionLimitsForSlot(f *features.Features, epochSchedule *sealevel.SysvarEpochSchedule, slot uint64) costmodel.Limits {
	d := classicSlotRangeDuration(f, epochSchedule, slot, slot)
	nanos := d.Secs*1_000_000_000 + uint64(d.Nanos)
	raiseBlockLimits := f.IsActiveAtSlot(features.RaiseBlockLimitsTo100m, slot)
	return costmodel.LimitsForSlotNanos(nanos, raiseBlockLimits)
}

func updateSlotsPerYearForSlot(replayCtx *ReplayCtx, epochSchedule *sealevel.SysvarEpochSchedule, f *features.Features, slot uint64) {
	bankEpoch := epochSchedule.GetEpoch(slot)
	replayCtx.SlotsPerYear = rewards.InflationSlotsPerYearAtSlot(epochSchedule, replayCtx.SlotsPerYear, bankEpoch, slot, f)
}

func shredFeatureEffective(f *features.Features, epochSchedule *sealevel.SysvarEpochSchedule, gate features.FeatureGate, slot uint64) bool {
	if f == nil || epochSchedule == nil {
		return false
	}
	activationSlot, active := f.ActivationSlot(gate)
	return active && epochSchedule.GetEpoch(activationSlot) < epochSchedule.GetEpoch(slot)
}
