package blockstream

import "strings"

func isSlotNotAvailableErr(err error) bool {
	return strings.Contains(err.Error(), "Block not available for slot")
}

func isRateLimitedErr(err error) bool {
	return strings.Contains(err.Error(), "rate limited")
}
