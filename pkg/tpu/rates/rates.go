package rates

// IntervalRates returns packets-per-second and gigabits-per-second for a window.
func IntervalRates(packetDelta, byteDelta uint64, elapsedSec float64) (pps, gbps float64) {
	if elapsedSec <= 0 {
		return 0, 0
	}
	pps = float64(packetDelta) / elapsedSec
	gbps = float64(byteDelta) * 8 / elapsedSec / 1e9
	return pps, gbps
}

// AverageRates returns average pps and gbps over a completed run.
func AverageRates(packets, bytes uint64, elapsedSec float64) (pps, gbps float64) {
	return IntervalRates(packets, bytes, elapsedSec)
}
