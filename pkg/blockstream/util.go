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

func isHistoryUnavailableErr(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	// -32011: transaction history disabled on the node.
	// -32001: "Block X cleaned up, does not exist on node. First available
	// block: Y" — the RPC's ledger retention has pruned the slot. Without
	// this match the error classifies as "other" and catchup retries the
	// same pruned slot forever instead of failing with an actionable stop.
	return strings.Contains(errStr, "Transaction history is not available from this node") ||
		strings.Contains(errStr, "-32011") ||
		strings.Contains(errStr, "-32001") ||
		strings.Contains(errStr, "cleaned up, does not exist") ||
		strings.Contains(errStr, "First available block")
}

// isTransientNetworkErr returns true for common transient network/RPC errors
// that should be retried rather than treated as permanent failures.
// These errors are retried on the SAME endpoint - they don't trigger failover.
func isTransientNetworkErr(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "EOF") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "timeout") ||
		strings.Contains(errStr, "context deadline exceeded") ||
		strings.Contains(errStr, "502") ||
		strings.Contains(errStr, "503") ||
		strings.Contains(errStr, "504") ||
		strings.Contains(errStr, "Bad Gateway") ||
		strings.Contains(errStr, "Service Unavailable") ||
		strings.Contains(errStr, "Gateway Timeout") ||
		strings.Contains(errStr, "i/o timeout") ||
		strings.Contains(errStr, "temporary failure")
}

// isHardConnectivityErr returns true for hard connectivity failures that
// indicate the endpoint is unreachable. These warrant RPC failover.
// This is a subset of transient errors - only the "endpoint is down" cases.
func isHardConnectivityErr(err error) bool {
	if err == nil {
		return false
	}
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "no such host") ||
		strings.Contains(errStr, "dial tcp") ||
		strings.Contains(errStr, "tls") || // covers "tls: handshake failure" etc.
		strings.Contains(errStr, "network is unreachable") ||
		strings.Contains(errStr, "no route to host")
}
