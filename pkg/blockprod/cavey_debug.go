package blockprod

// TODO(cavey-debug): DELETE this entire file after debugging block production on Alpenglow.
// Search the repo for "TODO(cavey-debug)" to find all related wiring and call sites.
// Also remove: global.FormatNextLeaderSuffix and nextLeaderSuffix (replay/block.go).

import (
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/mlog"
)

const caveyDebugPrefix = "cavey debug:"

func caveyDebugf(format string, args ...any) {
	mlog.Log.Infof(caveyDebugPrefix+" "+format, args...)
}

// ForgeCounters mirrors forge.Sink stats without importing pkg/forge (cycle).
type ForgeCounters struct {
	InPackets           uint64
	InBytes             uint64
	Accepted            uint64
	DroppedNoBank         uint64
	DroppedVote           uint64
	DroppedParse          uint64
	DroppedCost           uint64
	DroppedExecution      uint64
	DroppedBlockCost      uint64
	DroppedAccountCost    uint64
	DroppedAllocCost      uint64
	DroppedBatchBytes     uint64
}

func FormatForgeCounters(c ForgeCounters) string {
	return formatForgeCounters(c)
}

func formatForgeCounters(c ForgeCounters) string {
	return fmt.Sprintf(
		"forge_in=%d forge_bytes=%d forge_accepted=%d forge_dropped_no_bank=%d forge_dropped_vote=%d forge_dropped_parse=%d forge_dropped_cost=%d forge_dropped_exec=%d forge_dropped_block_cost=%d forge_dropped_acct_cost=%d forge_dropped_alloc_cost=%d forge_dropped_batch_bytes=%d",
		c.InPackets, c.InBytes, c.Accepted, c.DroppedNoBank, c.DroppedVote, c.DroppedParse,
		c.DroppedCost, c.DroppedExecution, c.DroppedBlockCost, c.DroppedAccountCost,
		c.DroppedAllocCost, c.DroppedBatchBytes,
	)
}

func formatTVUPeersSuffix(tvuPeerCount func() int) string {
	if tvuPeerCount == nil {
		return " tvu_peers=unknown"
	}
	return fmt.Sprintf(" tvu_peers=%d", tvuPeerCount())
}
