package blockstream

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func gatheredGaugeSeries(t *testing.T, name, label string) map[string]float64 {
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

func TestProvenanceGaugesPublishCompleteSourceFamily(t *testing.T) {
	InitializeProvenanceGauges("alpenglow", "validator", "turbine")
	sources := gatheredGaugeSeries(t, "mithril_block_source_active", "source")
	if len(sources) != len(monitoringBlockSources) || sources["turbine"] != 1 {
		t.Fatalf("initial source family = %#v", sources)
	}

	(&BlockSource{sourceType: BlockSourceTurbine, rpcFallbackEnabled: true}).PublishSourceMetrics()
	sources = gatheredGaugeSeries(t, "mithril_block_source_active", "source")
	if sources["rpc"] != 1 || sources["turbine"] != 0 {
		t.Fatalf("catch-up source family = %#v", sources)
	}

	(&BlockSource{sourceType: BlockSourceTurbine}).PublishSourceMetrics()
	sources = gatheredGaugeSeries(t, "mithril_block_source_active", "source")
	if sources["turbine"] != 1 || sources["rpc"] != 0 {
		t.Fatalf("shreds-only source family = %#v", sources)
	}
}
