package replay

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/prometheus/client_golang/prometheus"
)

func monitoringGaugeSeries(t *testing.T, name, label string) map[string]float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]float64{}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			key := ""
			for _, pair := range metric.GetLabel() {
				if pair.GetName() == label {
					key = pair.GetValue()
				}
			}
			values[key] = metric.GetGauge().GetValue()
		}
	}
	return values
}

func TestVerificationAndFinalityMetricFamiliesAreComplete(t *testing.T) {
	publishVerificationStartupMetrics(true, VerificationUnavailable, 7, "mixed")
	states := monitoringGaugeSeries(t, "mithril_verification_state", "state")
	if len(states) != len(monitoringVerificationStates) || states[string(VerificationUnavailable)] != 1 {
		t.Fatalf("verification state family = %#v", states)
	}
	legacy := monitoringGaugeSeries(t, "mithril_finality_slot", "")
	if len(legacy) != 1 || legacy[""] != 7 {
		t.Fatalf("legacy finality scalar = %#v, want one unlabeled value at 7", legacy)
	}
	finality := monitoringGaugeSeries(t, "mithril_finality_source_slot", "source")
	if len(finality) != len(monitoringFinalitySources) {
		t.Fatalf("finality family = %#v", finality)
	}
	if finality["mixed"] != 7 {
		t.Fatalf("restored finality family = %#v, want mixed=7", finality)
	}

	slot, source := updateFinalityMetric(0, "", 10, "delegated")
	slot, source = updateFinalityMetric(slot, source, 10, "certificate")
	publishFinalityMetric(source, slot)
	legacy = monitoringGaugeSeries(t, "mithril_finality_slot", "")
	if len(legacy) != 1 || legacy[""] != 10 {
		t.Fatalf("updated legacy finality scalar = %#v, want one unlabeled value at 10", legacy)
	}
	finality = monitoringGaugeSeries(t, "mithril_finality_source_slot", "source")
	if finality["mixed"] != 10 {
		t.Fatalf("mixed finality family = %#v", finality)
	}
}

func TestUnrootedTailMetricTracksGrowth(t *testing.T) {
	tail := newUnrootedTail(nil, nil, 512, 2, "")
	tail.Add(7, nil, nil)
	tail.Add(9, nil, nil)
	publishUnrootedTailMetrics(tail)

	families := monitoringGaugeSeries(t, "mithril_unrooted_tail_slots", "")
	if got := families[""]; got != 2 {
		t.Fatalf("unrooted tail metric = %v, want 2", got)
	}
}

func TestEffectiveSlotDurationTracksRuntimeStage(t *testing.T) {
	epochSchedule := &sealevel.SysvarEpochSchedule{
		SlotsPerEpoch:            100,
		LeaderScheduleSlotOffset: 100,
	}
	f := features.NewFeaturesDefault()
	f.EnableFeature(features.ReduceSlotTimeTo350ms, 50)

	publishEffectiveSlotDuration(false, f, epochSchedule, 99)
	if got := monitoringGaugeSeries(t, "mithril_effective_slot_duration_seconds", "")[""]; got != 0.4 {
		t.Fatalf("pre-transition slot duration = %v, want 0.4", got)
	}
	publishEffectiveSlotDuration(false, f, epochSchedule, 100)
	if got := monitoringGaugeSeries(t, "mithril_effective_slot_duration_seconds", "")[""]; got != 0.35 {
		t.Fatalf("effective slot duration = %v, want 0.35", got)
	}
	publishEffectiveSlotDuration(true, f, epochSchedule, 100)
	if got := monitoringGaugeSeries(t, "mithril_effective_slot_duration_seconds", "")[""]; got != 0.2 {
		t.Fatalf("Alpenglow slot duration = %v, want 0.2", got)
	}
}
