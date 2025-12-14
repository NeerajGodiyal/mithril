package blockstream

import "strings"

func isSlotNotAvailableErr(err error) bool {
	return strings.Contains(err.Error(), "Block not available for slot")
}

func isRateLimitedErr(err error) bool {
	errStr := err.Error()
	return strings.Contains(errStr, "rate limited") ||
		strings.Contains(errStr, "Too many requests") ||
		strings.Contains(errStr, "429")
}
