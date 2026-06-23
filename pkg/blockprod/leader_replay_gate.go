package blockprod

import (
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/global"
)

// pocLeaderSlotReplayReady reports whether replay has advanced far enough to start
// forging leaderSlot.
//
// POC block-production policy: blockprod must stay aligned with replay. We only
// attempt a leader slot once its immediate parent (leaderSlot-1) is available as
// chain tip input. When that parent was led by another validator, replay must have
// executed slot leaderSlot-1 before we try. When the parent was our own leader
// slot, we may proceed once that slot is finished locally (consecutive leader
// windows) even if replay is still catching up via local-leader skip synthesis.
func (l *LeaderLoop) pocLeaderSlotReplayReady(leaderSlot uint64) (bool, error) {
	if leaderSlot == 0 {
		return true, nil
	}

	parentSlot := leaderSlot - 1
	replaySlot := global.Slot()
	self := l.identity.PublicKey()

	parentLeader, ok := l.leaderForSlot(parentSlot)
	parentIsUs := ok && parentLeader == self

	if parentIsUs {
		if replaySlot >= parentSlot || l.isLeaderSlotFinished(parentSlot) {
			return l.parentAlpenglowInputsReady(parentSlot)
		}
		return false, fmt.Errorf("%w: POC replay at %d waiting for own parent slot %d (local finish pending replay)",
			errParentNotReady, replaySlot, parentSlot)
	}

	// POC: parent belonged to another validator — require their block in replay first.
	if replaySlot >= parentSlot {
		return l.parentAlpenglowInputsReady(parentSlot)
	}
	return false, fmt.Errorf("%w: POC replay at %d need >= parent slot %d (other leader %s)",
		errParentNotReady, replaySlot, parentSlot, parentLeader)
}

func (l *LeaderLoop) parentAlpenglowInputsReady(parentSlot uint64) (bool, error) {
	if _, ok := global.AlpenglowBlockID(parentSlot); !ok {
		return false, fmt.Errorf("%w: alpenglow block id missing for parent slot %d",
			errParentNotReady, parentSlot)
	}
	if _, ok := global.AlpenglowChainedMerkleRoot(parentSlot); !ok {
		return false, fmt.Errorf("%w: chained merkle root missing for parent slot %d",
			errParentNotReady, parentSlot)
	}
	return true, nil
}
