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

	replayCall := "publishReplayProgress(block.Slot, time.Now())"
	require.Equal(t, 2, strings.Count(source, replayCall), "resolved skips and executed blocks both advance replay progress")
	successPath := sourceBetween(t, source,
		"// Stop before per-slot logging, summary generation, and metric export",
		"// Track last executed slot for accurate tip distance calculation")
	require.Contains(t, successPath, replayCall)

	rootedCall := "publishPromotionMetrics(promotedThrough, unrootedTailState)"
	require.Equal(t, 1, strings.Count(source, rootedCall), "rooted slot may advance only in promotion bookkeeping")
	require.Contains(t, source, "lastRootedWatermark := mithrilState.DurableHighWater()")
	require.Contains(t, source, "statsd.Gauge(statsd.MithrilRootedSlot, float64(lastRootedWatermark), nil)")
	require.Contains(t, source, "monitoringFinalitySlot := lastRootedWatermark")
	require.Contains(t, source, "monitoringFinalitySource := \"classic\"")
	require.Contains(t, source, "monitoringFinalitySource = \"mixed\"")
	promotion := sourceBetween(t, source,
		"applyPromotionBookkeeping := func(promotedThrough uint64, rootedCtx *state.ResumeContext) {",
		"// applyFoldOutcome applies a completed async fold")
	require.Contains(t, promotion, rootedCall)
	require.Less(t,
		strings.Index(promotion, "mithrilState.LastRootedSlot = promotedThrough"),
		strings.Index(promotion, rootedCall))

	finalityCall := "publishFinalityMetric(monitoringFinalitySource, monitoringFinalitySlot)"
	require.Equal(t, 2, strings.Count(source, finalityCall), "only Alpenglow and Classic finality decisions publish the watermark")

	tailGrowth := sourceBetween(t, source,
		"unrootedTailState.SetContext(block.Slot, resumeCtx, lastSlotCtx.BankSysvars())",
		"// Clear ManifestEpochStakes after first replayed slot past snapshot")
	require.Contains(t, tailGrowth, "publishUnrootedTailMetrics(unrootedTailState)",
		"the tail gauge must advance when a speculative slot is retained, even if no fold succeeds")

	tailUnwind := sourceBetween(t, source,
		"rs, parentBankSysvars, fallbackReason := tryInLoopUnwind",
		"if statusErr := transactionStatuses.Unwind(sw.Slot); statusErr != nil")
	require.Contains(t, tailUnwind, "publishUnrootedTailMetrics(unrootedTailState)",
		"the tail gauge must shrink as soon as an in-memory fork suffix is discarded")
}

func sourceBetween(t *testing.T, source, start, end string) string {
	t.Helper()
	startAt := strings.Index(source, start)
	require.NotEqual(t, -1, startAt, "source marker missing: %s", start)
	endAt := strings.Index(source[startAt:], end)
	require.NotEqual(t, -1, endAt, "source marker missing: %s", end)
	return source[startAt : startAt+endAt]
}
