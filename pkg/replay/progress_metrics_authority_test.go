package replay

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProgressMetricUpdateAuthority(t *testing.T) {
	raw, err := os.ReadFile("block.go")
	require.NoError(t, err)
	source := string(raw)

	replayCall := "statsd.Gauge(statsd.MithrilReplaySlot, float64(block.Slot), nil)"
	require.Equal(t, 1, strings.Count(source, replayCall))
	successPath := sourceBetween(t, source,
		"// Stop before per-slot logging, summary generation, and metric export",
		"// Track last executed slot for accurate tip distance calculation")
	require.Contains(t, successPath, replayCall)

	rootedCall := "statsd.Gauge(statsd.MithrilRootedSlot"
	require.Equal(t, 2, strings.Count(source, rootedCall), "rooted slot may only initialize once and advance in promotion bookkeeping")
	require.Contains(t, source, "lastRootedWatermark := mithrilState.DurableHighWater()")
	require.Contains(t, source, "statsd.Gauge(statsd.MithrilRootedSlot, float64(lastRootedWatermark), nil)")
	promotion := sourceBetween(t, source,
		"applyPromotionBookkeeping := func(promotedThrough uint64, rootedCtx *state.ResumeContext) {",
		"// applyFoldOutcome applies a completed async fold")
	require.Equal(t, 1, strings.Count(promotion, rootedCall))
	require.Less(t,
		strings.Index(promotion, "mithrilState.LastRootedSlot = promotedThrough"),
		strings.Index(promotion, "statsd.Gauge(statsd.MithrilRootedSlot, float64(promotedThrough), nil)"))

	finalityCall := "statsd.Gauge(statsd.MithrilFinalitySlot"
	require.Equal(t, 3, strings.Count(source, finalityCall), "finality slot may only initialize once and publish its two watermark advances")
	require.Contains(t, source, "statsd.Gauge(statsd.MithrilFinalitySlot, float64(lastRootedWatermark), nil)")
	require.Contains(t, source, "lastRootedWatermark = rooted\n\t\t\t\t\tstatsd.Gauge(statsd.MithrilFinalitySlot, float64(rooted), nil)")
	require.Contains(t, source, "lastRootedWatermark = block.Slot\n\t\t\t\tstatsd.Gauge(statsd.MithrilFinalitySlot, float64(block.Slot), nil)")
}

func sourceBetween(t *testing.T, source, start, end string) string {
	t.Helper()
	startAt := strings.Index(source, start)
	require.NotEqual(t, -1, startAt, "source marker missing: %s", start)
	endAt := strings.Index(source[startAt:], end)
	require.NotEqual(t, -1, endAt, "source marker missing: %s", end)
	return source[startAt : startAt+endAt]
}
