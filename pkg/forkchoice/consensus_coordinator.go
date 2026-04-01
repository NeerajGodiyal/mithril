package forkchoice

import (
	"errors"

	"github.com/gagliardetto/solana-go"
)

var (
	ErrNeedWait        = errors.New("consensus: vote landing window not reached, need to wait")
	ErrNoSupermajority = errors.New("consensus: no hash reached supermajority for target slot")
)

// SlotDecision represents the resolved action for a single slot.
type SlotDecision struct {
	Slot     uint64
	UseBlock bool // true = use the block, false = slot is empty/skipped
}

// ResolvedPath is a confirmed execution path from the current anchor to a
// vote-confirmed leaf.
type ResolvedPath struct {
	LeafSlot      uint64
	LeafBankhash  solana.Hash
	SlotDecisions []SlotDecision
}

// ConsensusCoordinator bridges the ForkChoiceService (vote accumulation) and
// the PoH path resolver. It finds a confirmed leaf ahead of the current anchor,
// then reconstructs which slots should use blocks versus be treated as skipped.
type ConsensusCoordinator struct {
	forkChoice *ForkChoiceService
	maxDepth   int
	policy     string // "halt" = return error on unresolved, "warn" = log and continue
}

// NewConsensusCoordinator creates a coordinator with the given forkchoice service,
// maximum path depth, and unresolved policy.
func NewConsensusCoordinator(fc *ForkChoiceService, maxDepth int, policy string) *ConsensusCoordinator {
	return &ConsensusCoordinator{
		forkChoice: fc,
		maxDepth:   maxDepth,
		policy:     policy,
	}
}

// ResolveFromAnchor finds the highest confirmed leaf reachable within maxDepth
// from the current execution anchor, then reconstructs the block/skip path to it.
func (cc *ConsensusCoordinator) ResolveFromAnchor(anchorSlot uint64) (*ResolvedPath, error) {
	latestObserved := cc.forkChoice.LatestObservedSlot()
	if latestObserved <= anchorSlot {
		return nil, ErrNeedWait
	}

	latestSearchSlot := latestObserved
	sawConfirmedLeafBeyondDepth := false
	if cc.maxDepth > 0 {
		maxLeafSlot := anchorSlot + uint64(cc.maxDepth)
		if latestSearchSlot > maxLeafSlot {
			for slot := latestObserved; slot > maxLeafSlot; slot-- {
				_, status := cc.forkChoice.GetSupermajorityHash(slot)
				if status == BankhashHasSupermajority {
					sawConfirmedLeafBeyondDepth = true
					break
				}
			}
			latestSearchSlot = maxLeafSlot
		}
	}

	sawPathIncomplete := false

	for slot := latestSearchSlot; slot > anchorSlot; slot-- {
		winningHash, status := cc.forkChoice.GetSupermajorityHash(slot)
		if status != BankhashHasSupermajority {
			continue
		}

		result, err := cc.forkChoice.ResolvePathToLeaf(anchorSlot, slot, cc.maxDepth)
		if err != nil {
			switch {
			case errors.Is(err, ErrPathIncomplete):
				sawPathIncomplete = true
				continue
			case errors.Is(err, ErrNoPath):
				// The newest confirmed leaf may belong to a branch that does not
				// connect to the current execution anchor. Keep scanning older
				// confirmed leaves before declaring failure.
				continue
			default:
				return nil, err
			}
		}

		decisions := make([]SlotDecision, len(result.Path))
		for i, useBlock := range result.Path {
			decisions[i] = SlotDecision{
				Slot:     anchorSlot + uint64(i) + 1,
				UseBlock: useBlock,
			}
		}

		return &ResolvedPath{
			LeafSlot:      slot,
			LeafBankhash:  winningHash,
			SlotDecisions: decisions,
		}, nil
	}

	if sawConfirmedLeafBeyondDepth {
		return nil, ErrDepthExceeded
	}
	if sawPathIncomplete {
		return nil, ErrPathIncomplete
	}
	return nil, ErrNeedWait
}

// Policy returns the coordinator's unresolved policy ("halt" or "warn").
func (cc *ConsensusCoordinator) Policy() string {
	return cc.policy
}
