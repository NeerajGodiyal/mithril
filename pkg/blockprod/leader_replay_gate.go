package blockprod

import (
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/alpenglow"
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
		_, _, err := l.resolveProductionParent(leaderSlot)
		if err != nil {
			return false, err
		}
		return true, nil
	}
	return false, fmt.Errorf("%w: ordered replay at %d needs outcome through slot %d", errParentNotReady, replaySlot, requiredFrontier)
}

// resolveProductionParent applies Agave's ParentReady gate at the first slot
// of each four-slot leader window. Other slots retain the dev branch's ordered
// replay parent behavior.
func (l *LeaderLoop) resolveProductionParent(leaderSlot uint64) (alpenglow.BlockID, bool, error) {
	if leaderSlot == 0 || leaderSlot%alpenglow.LeaderWindowSlots != 0 || l.productionParent == nil {
		return alpenglow.BlockID{}, false, nil
	}
	parent := l.productionParent(leaderSlot)
	switch parent.Kind {
	case alpenglow.BlockProductionParentReady:
		if parent.Parent.IsZero() || !parent.Parent.HasHash() || parent.Parent.Slot >= leaderSlot {
			return alpenglow.BlockID{}, true, fmt.Errorf("%w: invalid ParentReady value for leader window %d", errParentNotReady, leaderSlot)
		}
		return parent.Parent, true, nil
	case alpenglow.BlockProductionParentMissedWindow:
		return alpenglow.BlockID{}, true, fmt.Errorf("%w: ParentReady arrived after leader window %d", errParentNotReady, leaderSlot)
	default:
		return alpenglow.BlockID{}, true, fmt.Errorf("%w: no verified ParentReady for leader window %d", errParentNotReady, leaderSlot)
	}
}
