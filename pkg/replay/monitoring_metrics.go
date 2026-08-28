package replay

import (
	"time"

	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/statsd"
)

var monitoringVerificationStates = []VerificationState{
	VerificationComplete,
	VerificationIncomplete,
	VerificationStalled,
	VerificationDiverged,
	VerificationUnavailable,
	VerificationNotApplicable,
}

var monitoringFinalitySources = []string{"certificate", "delegated", "mixed", "classic"}

func publishVerificationState(active VerificationState) {
	values := make(map[string]float64, len(monitoringVerificationStates))
	for _, state := range monitoringVerificationStates {
		if state == active {
			values[string(state)] = 1
		} else {
			values[string(state)] = 0
		}
	}
	_ = statsd.ReplaceGaugeFamily(statsd.VerificationStateMetric, values)
}

func publishVerificationStartupMetrics(required bool, state VerificationState, rootedSlot uint64, finalitySource string) {
	requiredValue := 0.0
	if required {
		requiredValue = 1
	}
	_ = statsd.Gauge(statsd.VerificationRequired, requiredValue, nil)
	_ = statsd.Gauge(statsd.VerifierConfiguredLagSlots, float64(TrailingVerifierCfg.LagSlots), nil)
	_ = statsd.Gauge(statsd.FoldBatchSlotsMetric, float64(FoldBatchSlots), nil)
	_ = statsd.Gauge(statsd.UnrootedTailLimitSlots, float64(unrootedTailHaltCap), nil)
	_ = statsd.Gauge(statsd.MithrilRootedSlot, float64(rootedSlot), nil)
	_ = statsd.Gauge(statsd.VerifiedSlot, float64(rootedSlot), nil)
	_ = statsd.Gauge(statsd.VerificationLagSlots, 0, nil)
	_ = statsd.Gauge(statsd.UnrootedTailSlots, 0, nil)
	publishVerificationState(state)
	publishFinalityMetric(finalitySource, rootedSlot)
}

func publishVerificationProgress(state VerificationState, verified, eligible uint64) {
	lag := uint64(0)
	if eligible > verified {
		lag = eligible - verified
	}
	_ = statsd.Gauge(statsd.VerifiedSlot, float64(verified), nil)
	_ = statsd.Gauge(statsd.VerificationLagSlots, float64(lag), nil)
	publishVerificationState(state)
}

func publishPromotionMetrics(rootedSlot uint64, tail *unrootedTail) {
	_ = statsd.Gauge(statsd.MithrilRootedSlot, float64(rootedSlot), nil)
	publishUnrootedTailMetrics(tail)
}

func publishUnrootedTailMetrics(tail *unrootedTail) {
	_ = statsd.Gauge(statsd.UnrootedTailSlots, float64(unrootedTailLengthSlots(tail)), nil)
}

func publishReplayProgress(slot uint64, at time.Time) {
	_ = statsd.Gauge(statsd.MithrilReplaySlot, float64(slot), nil)
	_ = statsd.Gauge(statsd.ReplayLastSuccessTimestamp, float64(at.Unix()), nil)
}

func publishEffectiveSlotDuration(alpenglow bool, f *features.Features, epochSchedule *sealevel.SysvarEpochSchedule, slot uint64) {
	seconds := float64(alpenglowNsPerSlot) / float64(time.Second)
	if !alpenglow {
		d := classicSlotRangeDuration(f, epochSchedule, slot, slot)
		seconds = float64(d.Secs) + float64(d.Nanos)/float64(time.Second)
	}
	_ = statsd.Gauge(statsd.EffectiveSlotDurationSeconds, seconds, nil)
}

func unrootedTailLengthSlots(tail *unrootedTail) uint64 {
	if tail == nil || tail.overlay == nil {
		return 0
	}
	held := tail.overlay.HeldSlots()
	if held < 0 {
		return 0
	}
	return uint64(held)
}

func publishFinalityMetric(source string, slot uint64) {
	_ = statsd.Gauge(statsd.MithrilFinalitySlot, float64(slot), nil)
	values := make(map[string]float64, len(monitoringFinalitySources))
	for _, candidate := range monitoringFinalitySources {
		if candidate == source {
			values[candidate] = float64(slot)
		} else {
			values[candidate] = 0
		}
	}
	_ = statsd.ReplaceGaugeFamily(statsd.MithrilFinalitySourceSlot, values)
}

func updateFinalityMetric(currentSlot uint64, currentSource string, observedSlot uint64, observedSource string) (uint64, string) {
	switch {
	case observedSlot > currentSlot:
		return observedSlot, observedSource
	case observedSlot == currentSlot && observedSlot != 0 && currentSource != observedSource:
		return currentSlot, "mixed"
	default:
		return currentSlot, currentSource
	}
}
