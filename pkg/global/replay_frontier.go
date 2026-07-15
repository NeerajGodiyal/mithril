package global

import "sync/atomic"

// replayFrontier is the highest slot whose ordered replay outcome is known.
// Unlike Slot(), it advances across certified/RPC-confirmed skips and is only
// published after a block has completed replay successfully.
var replayFrontier atomic.Uint64

func SetReplayFrontier(slot uint64) {
	replayFrontier.Store(slot)
}

func ReplayFrontier() uint64 {
	return replayFrontier.Load()
}
