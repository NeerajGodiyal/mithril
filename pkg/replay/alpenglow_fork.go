package replay

import (
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/alpenglow"
	"github.com/Overclock-Validator/mithril/pkg/forkchoice"
)

// applyAlpenglowForkDecision drives the multi-branch fork engine from an Alpenglow
// chain decision. Unlike TowerBFT (which picks the heaviest subtree from vote
// weight), Alpenglow's certificate NAMES the winner: a certified block confirms
// that version — evicting any competing block executed for the same slot — while a
// conflict (two certified blocks / block+skip) halts for safety. Skip decisions
// promote nothing. Callers finalize the confirmed chain separately via OnFinalized.
func applyAlpenglowForkDecision(d *forkDriver, dec alpenglow.ChainDecision, competing []forkchoice.SlotHashKey) error {
	switch dec.Kind {
	case alpenglow.ChainDecisionKindBlock:
		var winner forkchoice.SlotHashKey
		winner.Slot = dec.Slot
		copy(winner.Hash[:], dec.Block.Hash[:])
		for _, c := range competing {
			if c != winner {
				d.OnDuplicate(c)
			}
		}
		d.OnDuplicateConfirmed(winner)
		return nil
	case alpenglow.ChainDecisionKindConflict:
		return fmt.Errorf("alpenglow: unresolved conflict at slot %d (competing certified branches)", dec.Slot)
	default:
		// skip / unknown: no branch to promote for this slot.
		return nil
	}
}
