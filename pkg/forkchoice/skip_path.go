package forkchoice

import (
	"errors"
	"fmt"

	"github.com/gagliardetto/solana-go"
)

var (
	ErrDepthExceeded  = errors.New("skip path: depth exceeded max allowed")
	ErrNoPath         = errors.New("skip path: no valid path found to target slot")
	ErrPathIncomplete = errors.New("skip path: path is incomplete and needs more observations")
	ErrEquivocation   = errors.New("skip path: observed conflicting blocks for the same slot")
)

// ObservedBlockMeta is the pre-execution PoH metadata we know about a block.
type ObservedBlockMeta struct {
	Slot            uint64
	Blockhash       solana.Hash
	ParentSlot      uint64
	ParentSlotKnown bool
	ParentBlockhash solana.Hash
}

// SolveResult contains the resolved block/skip path.
// Path[i] corresponds to slot (anchorSlot + 1 + i):
//
//	true  -> use the block at that slot
//	false -> slot is empty/skipped
type SolveResult struct {
	Path        []bool
	MatchedSlot uint64
}

// ResolvePohPath walks backwards from a confirmed leaf slot to a known anchor
// slot using observed PoH parent links. Slots not present on the leaf's ancestry
// are treated as skipped.
func ResolvePohPath(
	anchorSlot uint64,
	leafSlot uint64,
	observed map[uint64]*ObservedBlockMeta,
	equivocatedSlots map[uint64]struct{},
	maxDepth int,
) (*SolveResult, error) {
	if leafSlot < anchorSlot {
		return nil, fmt.Errorf("skip path: leafSlot %d < anchorSlot %d", leafSlot, anchorSlot)
	}

	depth := leafSlot - anchorSlot
	if int(depth) > maxDepth {
		return nil, ErrDepthExceeded
	}
	if depth == 0 {
		return &SolveResult{MatchedSlot: anchorSlot}, nil
	}

	path := make([]bool, depth)
	current := leafSlot
	visited := make(map[uint64]struct{})

	for current > anchorSlot {
		if _, exists := equivocatedSlots[current]; exists {
			return nil, ErrEquivocation
		}
		if _, seen := visited[current]; seen {
			return nil, ErrNoPath
		}
		visited[current] = struct{}{}

		meta, exists := observed[current]
		if !exists {
			return nil, ErrPathIncomplete
		}
		if !meta.ParentSlotKnown {
			return nil, ErrPathIncomplete
		}
		if meta.ParentSlot >= current {
			return nil, ErrNoPath
		}
		if meta.ParentSlot < anchorSlot {
			return nil, ErrNoPath
		}

		path[current-anchorSlot-1] = true
		current = meta.ParentSlot
	}

	return &SolveResult{
		Path:        path,
		MatchedSlot: leafSlot,
	}, nil
}
