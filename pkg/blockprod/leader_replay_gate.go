package blockprod

import (
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/global"
)

// leaderSlotReplayReady reports whether replay has advanced far enough to start
// forging leaderSlot.
//
// Block production stays aligned with replay: every leader slot waits until its
// immediate parent has passed authoritative replay, including consecutive local
// leader slots. A locally frozen bank is not itself a committed chain tip.
func (l *LeaderLoop) leaderSlotReplayReady(leaderSlot uint64) (bool, error) {
	if leaderSlot == 0 {
		return true, nil
	}

	requiredFrontier := leaderSlot - 1
	replaySlot := global.ReplayFrontier()
	if replaySlot >= leaderSlot {
		return false, fmt.Errorf("%w: ordered replay already resolved leader slot %d", errParentNotReady, leaderSlot)
	}
	if replaySlot == requiredFrontier {
		return true, nil
	}
	return false, fmt.Errorf("%w: ordered replay at %d needs outcome through slot %d", errParentNotReady, replaySlot, requiredFrontier)
}
