package blockprod

import (
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/alpenglow"
	"github.com/Overclock-Validator/mithril/pkg/global"
)

// leaderSlotReplayReady reports whether replay has advanced far enough to start
// forging leaderSlot.
//
// Block production stays aligned with replay. Intrawindow slots wait until their
// immediate parent has passed authoritative replay. The first slot of a four-slot
// window is different: Votor's verified ParentReady selects the parent, which may
// be older than leaderSlot-1 when the preceding window was skipped. This mirrors
// Agave's ProduceWindow gate instead of waiting for those skipped slot numbers to
// pass through ordered replay before opening the new window.
func (l *LeaderLoop) leaderSlotReplayReady(leaderSlot uint64) (bool, error) {
	if leaderSlot == 0 {
		return true, nil
	}

	replaySlot := global.ReplayFrontier()
	if replaySlot >= leaderSlot {
		return false, fmt.Errorf("%w: ordered replay already resolved leader slot %d", errParentNotReady, leaderSlot)
	}

	selectedParent, parentReadyRequired, err := l.resolveProductionParent(leaderSlot)
	if err != nil {
		return false, err
	}
	if parentReadyRequired {
		if replaySlot < selectedParent.Slot {
			return false, fmt.Errorf("%w: ordered replay at %d needs selected ParentReady parent %d", errParentNotReady, replaySlot, selectedParent.Slot)
		}
		return true, nil
	}

	requiredFrontier := leaderSlot - 1
	if replaySlot == requiredFrontier {
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
