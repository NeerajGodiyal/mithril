package statsd

import (
	"testing"

	dto "github.com/prometheus/client_model/go"
)

func TestNodeMonitoringMetricsAreRegistered(t *testing.T) {
	want := []string{
		"mithril_monitoring_schema_ready",
		"mithril_runtime_info",
		"mithril_block_source_active",
		"mithril_replay_slot",
		"mithril_replay_last_success_timestamp_seconds",
		"mithril_replay_observation_started_timestamp_seconds",
		"mithril_effective_slot_duration_seconds",
		"mithril_replay_slot_duration_seconds",
		"mithril_rooted_slot",
		"mithril_finality_slot",
		"mithril_finality_source_slot",
		"mithril_verification_required",
		"mithril_verification_state",
		"mithril_verified_slot",
		"mithril_verification_lag_slots",
		"mithril_verifier_configured_lag_slots",
		"mithril_fold_batch_slots",
		"mithril_unrooted_tail_slots",
		"mithril_unrooted_tail_limit_slots",
	}

	registered := make(map[string]bool, len(MetricToType))
	for metric := range MetricToType {
		registered[metric.String()] = true
	}
	for _, name := range want {
		if !registered[name] {
			t.Errorf("monitoring rule metric %q is not registered by the node", name)
		}
	}
}

func TestMonitoringLifecyclePublishesNoBootstrapInventory(t *testing.T) {
	InitializeMonitoringLifecycle()
	for _, metric := range []Metric{MonitoringSchemaReady, SnapshotBootstrapActive, SnapshotBootstrapStartedAt} {
		value := &dto.Metric{}
		if err := metricsCollection.gauges[metric].WithLabelValues().Write(value); err != nil {
			t.Fatal(err)
		}
		if got := value.GetGauge().GetValue(); got != 0 {
			t.Fatalf("%s startup value = %v, want 0", metric, got)
		}
	}
}
