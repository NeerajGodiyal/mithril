package replay

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPercentileHelpers(t *testing.T) {
	vals := []float64{5, 1, 3, 2, 4}
	assert.Equal(t, 3.0, medianF(vals))
	assert.Equal(t, 1.0, percentileF(vals, 0))
	assert.Equal(t, 5.0, percentileF(vals, 100))
	assert.Equal(t, 4.0, percentileF(vals, 90)) // index (5-1)*90/100 = 3 of sorted
	assert.Equal(t, 5.0, maxF(vals))
	// Input order untouched (helpers sort a copy).
	assert.Equal(t, []float64{5, 1, 3, 2, 4}, vals)
	// Empty inputs are zeros, never a panic.
	assert.Equal(t, 0.0, medianF(nil))
	assert.Equal(t, 0.0, maxF(nil))
}

// Shred deltas are self-anchored: the fastest cadence-adjusted arrival reads
// +0ms, everything else is lateness relative to it at the nominal 400ms slot
// cadence.
func TestComputeShredDeltas(t *testing.T) {
	base := int64(1_000_000_000_000) // arbitrary wall origin
	samples := []shredSample{
		{slot: 100, firstNanos: base + 0, fullNanos: base + 74_000_000},
		// One slot later: on-cadence would be base+400ms; arrives 18ms late.
		{slot: 101, firstNanos: base + 400_000_000 + 18_000_000, fullNanos: base + 400_000_000 + 90_000_000},
		// Two slots later, 310ms late.
		{slot: 102, firstNanos: base + 800_000_000 + 310_000_000, fullNanos: 0}, // full unknown -> excluded from fullMs
	}
	firstMs, fullMs, anchor := computeShredDeltas(samples)
	require.Len(t, firstMs, 3)
	assert.InDelta(t, 0, firstMs[0], 0.01, "fastest slot anchors at +0")
	assert.InDelta(t, 18, firstMs[1], 0.01)
	assert.InDelta(t, 310, firstMs[2], 0.01)
	require.Len(t, fullMs, 2, "slots without a full timestamp are never fabricated")
	assert.InDelta(t, 74, fullMs[0], 0.01)
	assert.InDelta(t, 90, fullMs[1], 0.01)
	assert.Equal(t, base-int64(100)*nominalSlotNanos, anchor, "anchor = slot-0-equivalent of the fastest arrival")

	f, fl, a := computeShredDeltas(nil)
	assert.Nil(t, f)
	assert.Nil(t, fl)
	assert.Zero(t, a)
}

func TestFormatHelpers(t *testing.T) {
	assert.Equal(t, "31.0M", fmtMcu(31_000_000))
	assert.Equal(t, "0.5M", fmtMcu(480_000))
	assert.Equal(t, "38k", fmtK(38_000))
	assert.Equal(t, "850", fmtK(850))
	assert.Equal(t, "7abc...Q9x", shortPubkey("7abcDEFGHIJKLMNOPQ9x"))
	assert.Equal(t, "short", shortPubkey("short"))
}

// Per-slot lines match the handoff spec shape; the shreds segment is omitted
// (not dashed, not fabricated) for non-shred-sourced blocks.
func TestBuildSlotStatsLines(t *testing.T) {
	line := buildSlotStatsLine(123456789, "7abcDEFGHIJKLMNOPQ9x", 862, 31_000_000, 170, true, 18, 74, 0)
	assert.Equal(t, "slot 123456789 | leader 7abc...Q9x | txns 862 | cu 31.0M | exec 170ms | eff 5.5ms/Mcu | shreds(first +18ms, full +74ms, repair 0)", line)

	rpcLine := buildSlotStatsLine(42, "7abcDEFGHIJKLMNOPQ9x", 10, 2_000_000, 30, false, 0, 0, 0)
	assert.NotContains(t, rpcLine, "shreds(", "RPC-sourced blocks must not fabricate shred stats")

	zeroCU := buildSlotStatsLine(43, "7abcDEFGHIJKLMNOPQ9x", 0, 0, 5, false, 0, 0, 0)
	assert.Contains(t, zeroCU, "eff --", "no efficiency without compute units")

	// Skipped with nothing observed: dashes throughout, nothing fabricated.
	skipped := buildSkippedStatsLine(123456790, "4defGHIJKLMNOPQRK2p", 0, 0, false, 0)
	assert.Equal(t, "slot 123456790 | leader 4def...K2p | skipped | txns -- | cu -- | exec -- | eff -- | shreds(first --, full --, repair --)", skipped)

	// Skipped but the leader DID send shreds before dying: report the partial
	// arrivals — the signal separating "leader started then stopped" from
	// "leader never transmitted".
	partial := buildSkippedStatsLine(123456791, "4defGHIJKLMNOPQRK2p", 12, 3, true, 18)
	assert.Equal(t, "slot 123456791 | leader 4def...K2p | skipped | txns -- | cu -- | exec -- | eff -- | shreds(first +18ms, full --, repair 3, partial 12)", partial)

	// Partial shreds seen but no usable timing anchor yet: count still reported.
	noAnchor := buildSkippedStatsLine(123456792, "4defGHIJKLMNOPQRK2p", 5, 0, false, 0)
	assert.Equal(t, "slot 123456792 | leader 4def...K2p | skipped | txns -- | cu -- | exec -- | eff -- | shreds(first --, full --, repair 0, partial 5)", noAnchor)
}
