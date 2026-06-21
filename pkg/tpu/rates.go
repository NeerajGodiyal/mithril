package tpu

import "github.com/Overclock-Validator/mithril/pkg/tpu/rates"

func IntervalRates(packetDelta, byteDelta uint64, elapsedSec float64) (pps, gbps float64) {
	return rates.IntervalRates(packetDelta, byteDelta, elapsedSec)
}

func AverageRates(packets, bytes uint64, elapsedSec float64) (pps, gbps float64) {
	return rates.AverageRates(packets, bytes, elapsedSec)
}
