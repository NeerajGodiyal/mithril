package replay

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Terminal replay stats for the Alpenglow/native-shred path. Terminology
// follows Agave: a slot is "full" when all its data shreds are present and the
// block is reconstructable (SlotMeta/is_full) — NOT finalized/consensus-safe.
//
// Shred timings are self-anchored: the cluster's slot start times are not
// directly observable (no PoH clock), so deltas are measured against an anchor
// chosen so the fastest-arriving slot in the window reads +0ms, assuming the
// nominal slot cadence. That makes "first +18ms" mean "18ms later than the
// fastest slot, cadence-adjusted" — a jitter/lateness signal, not an absolute
// leader-to-us latency.

// nominalSlotNanos is Alpenglow's nominal slot duration used for cadence
// adjustment in shred-timing anchors.
const nominalSlotNanos = int64(400_000_000) // 400ms

// shredSample is one executed slot's shred timing record.
type shredSample struct {
	slot       uint64
	firstNanos int64
	fullNanos  int64
}

// shredAnchorCandidate is the anchor a sample implies: the wall time slot 0
// "would have started" if this sample's first shred arrived exactly on time.
func shredAnchorCandidate(s shredSample) int64 {
	return s.firstNanos - int64(s.slot)*nominalSlotNanos
}

// computeShredDeltas converts samples to per-slot first/full lateness (ms)
// against the window's fastest arrival, and returns that window anchor so the
// live per-slot display can rebase (correcting slow cadence drift). Returns
// nil slices and a zero anchor when no samples.
func computeShredDeltas(samples []shredSample) (firstMs, fullMs []float64, anchor int64) {
	if len(samples) == 0 {
		return nil, nil, 0
	}
	anchor = shredAnchorCandidate(samples[0])
	for _, s := range samples[1:] {
		if c := shredAnchorCandidate(s); c < anchor {
			anchor = c
		}
	}
	firstMs = make([]float64, 0, len(samples))
	fullMs = make([]float64, 0, len(samples))
	for _, s := range samples {
		expected := anchor + int64(s.slot)*nominalSlotNanos
		f := float64(s.firstNanos-expected) / 1e6
		if f < 0 {
			f = 0
		}
		firstMs = append(firstMs, f)
		if s.fullNanos > 0 {
			fl := float64(s.fullNanos-expected) / 1e6
			if fl < 0 {
				fl = 0
			}
			fullMs = append(fullMs, fl)
		}
	}
	return firstMs, fullMs, anchor
}

// medianF / percentileF / maxF operate on a copy; empty input returns 0.
func medianF(vals []float64) float64 { return percentileF(vals, 50) }

func percentileF(vals []float64, pct int) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	if pct <= 0 {
		return sorted[0]
	}
	if pct >= 100 {
		return sorted[len(sorted)-1]
	}
	idx := (len(sorted) - 1) * pct / 100
	return sorted[idx]
}

func maxF(vals []float64) float64 {
	m := 0.0
	for _, v := range vals {
		if v > m {
			m = v
		}
	}
	return m
}

// fmtMcu renders compute units as millions: 31_000_000 -> "31.0M".
func fmtMcu(cu uint64) string {
	return fmt.Sprintf("%.1fM", float64(cu)/1e6)
}

// fmtK renders a count in thousands: 38_000 -> "38k"; below 1000 verbatim.
func fmtK(v float64) string {
	if v >= 1000 {
		return fmt.Sprintf("%.0fk", v/1000)
	}
	return fmt.Sprintf("%.0f", v)
}

// shortPubkey renders "7abc...Q9x" (first 4 + last 3) for terminal alignment.
func shortPubkey(s string) string {
	if len(s) <= 10 {
		return s
	}
	return s[:4] + "..." + s[len(s)-3:]
}

// buildSlotStatsLine renders the per-slot terminal line. The shreds segment is
// omitted for blocks that did not come from shreds (never fabricated).
func buildSlotStatsLine(slot uint64, leader string, txns int, cu uint64, execMs float64, hasShreds bool, firstMs, fullMs float64, repaired int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "slot %d | leader %s | txns %d | cu %s | exec %.0fms", slot, shortPubkey(leader), txns, fmtMcu(cu), execMs)
	if cu > 0 {
		fmt.Fprintf(&b, " | eff %.1fms/Mcu", execMs/(float64(cu)/1e6))
	} else {
		b.WriteString(" | eff --")
	}
	if hasShreds {
		fmt.Fprintf(&b, " | shreds(first +%.0fms, full +%.0fms, repair %d)", firstMs, fullMs, repaired)
	}
	return b.String()
}

// buildSkippedStatsLine renders the skipped-slot terminal line, visually
// aligned with the executed line. When PARTIAL shreds arrived for the slot
// before it was skipped, they are reported — "the leader sent 12 shreds then
// stopped" is a different operator story from "the leader never transmitted".
// A slot with no observed shreds shows dashes; nothing is ever fabricated.
func buildSkippedStatsLine(slot uint64, leader string, partialShreds, repairedShreds int, haveFirst bool, firstMs float64) string {
	prefix := fmt.Sprintf("slot %d | leader %s | skipped | txns -- | cu -- | exec -- | eff --", slot, shortPubkey(leader))
	if partialShreds <= 0 {
		return prefix + " | shreds(first --, full --, repair --)"
	}
	first := "--"
	if haveFirst {
		first = fmt.Sprintf("+%.0fms", firstMs)
	}
	return prefix + fmt.Sprintf(" | shreds(first %s, full --, repair %d, partial %d)", first, repairedShreds, partialShreds)
}

// processRSSBytes reads the resident set size on Linux (/proc/self/statm,
// second field, pages). Returns 0 (omit from output) where unavailable —
// stats must not fabricate on other platforms.
func processRSSBytes() uint64 {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * uint64(os.Getpagesize())
}
