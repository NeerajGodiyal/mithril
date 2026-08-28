package blockstream

import "github.com/Overclock-Validator/mithril/pkg/statsd"

var monitoringBlockSources = []string{"rpc", "turbine", "lightbringer", "file"}

func publishActiveSource(active string) {
	values := make(map[string]float64, len(monitoringBlockSources))
	for _, source := range monitoringBlockSources {
		if source == active {
			values[source] = 1
		} else {
			values[source] = 0
		}
	}
	_ = statsd.ReplaceGaugeFamily(statsd.BlockSourceActive, values)
}

// InitializeProvenanceGauges publishes the fixed runtime and source schema.
func InitializeProvenanceGauges(protocolMode, consensusMode, configuredSource string) {
	_ = statsd.Gauge(statsd.RuntimeInfo, 1, []string{protocolMode, consensusMode, configuredSource})
	publishActiveSource(configuredSource)
	_ = statsd.Gauge(statsd.MithrilReplaySlot, 0, nil)
	_ = statsd.Gauge(statsd.ReplayLastSuccessTimestamp, 0, nil)
	_ = statsd.Gauge(statsd.ReplayObservationStartedTimestamp, 0, nil)
	_ = statsd.InitializeHistogramSeries(statsd.ReplaySlotDurationSeconds, nil)
}

// PublishSourceMetrics refreshes the active source from the fetcher's current state.
func (bs *BlockSource) PublishSourceMetrics() {
	if bs == nil {
		return
	}
	current, _, _ := bs.currentSourceSnapshot()
	publishActiveSource(current)
}
