package blockstream

import "strings"

func isSlotNotAvailableErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "Block not available for slot")
}

func isRateLimitedErr(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "rate limited") ||
		strings.Contains(errStr, "Too many requests") ||
		strings.Contains(errStr, "429")
}

// isTransientNetworkErr returns true for common transient network/RPC errors
// that should be retried rather than treated as permanent failures.
func isTransientNetworkErr(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "EOF") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "timeout") ||
		strings.Contains(errStr, "context deadline exceeded") ||
		strings.Contains(errStr, "502") ||
		strings.Contains(errStr, "503") ||
		strings.Contains(errStr, "504") ||
		strings.Contains(errStr, "Bad Gateway") ||
		strings.Contains(errStr, "Service Unavailable") ||
		strings.Contains(errStr, "Gateway Timeout") ||
		strings.Contains(errStr, "i/o timeout") ||
		strings.Contains(errStr, "temporary failure") ||
		strings.Contains(errStr, "no such host")
}
