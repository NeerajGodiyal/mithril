package alpenglow

import (
	"bytes"
	"fmt"
	"sort"
	"time"
)

const (
	// Alpenglow leader windows contain four consecutive slots. ParentReady is
	// emitted only for the first slot in a window, matching Agave.
	LeaderWindowSlots = uint64(4)
	// MAX_NOTAR_FALLBACK_BLOCKS in Agave. Reaching this bound is treated as
	// invalid consensus input instead of panicking the process.
	maxNotarFallbackBlocks = 7
	// Persisted ParentReady is a startup hint, not authority to allocate an
	// unbounded skipped-slot graph. Keep reconstruction inside the same window
	// in which Alpenglow votes are admissible.
	maxParentReadyRestoreGap = AgaveVoteVerificationWindow
)

type BlockProductionParentKind uint8

const (
	BlockProductionParentNotReady BlockProductionParentKind = iota
	BlockProductionParentMissedWindow
	BlockProductionParentReady
)

type BlockProductionParent struct {
	Kind   BlockProductionParentKind
	Parent BlockID
	// ReadyAt is the local monotonic instant at which Votor most recently made
	// this window producible. Agave starts every per-slot block deadline from
	// the ParentReady event, not from a separately extrapolated wall slot.
	ReadyAt time.Time
}

type parentReadyStatus struct {
	skip           bool
	notarFallbacks []BlockID
	parentsReady   []BlockID
	readyAt        time.Time
}

// ParentReadyTracker mirrors Agave's parent-ready state machine. A block is a
// valid parent for slot s when it has a notarize-fallback-or-stronger
// certificate and every intervening slot is skip-certified.
type ParentReadyTracker struct {
	root                   uint64
	highestWithParentReady uint64
	// liveUpdates makes persisted restoration bootstrap-only. Restore replaces
	// the tracker graph, so doing it after a live certificate, skip, or
	// invalidation would orphan retained certificate dedupe state.
	liveUpdates          bool
	slots                map[uint64]*parentReadyStatus
	invalidBlocks        map[BlockID]struct{}
	certifiedBlocks      map[BlockID]struct{}
	certifiedBlockCounts map[uint64]int
}

func NewParentReadyTracker(root BlockID) *ParentReadyTracker {
	t := &ParentReadyTracker{
		slots:                make(map[uint64]*parentReadyStatus),
		invalidBlocks:        make(map[BlockID]struct{}),
		certifiedBlocks:      make(map[BlockID]struct{}),
		certifiedBlockCounts: make(map[uint64]int),
	}
	t.Restore(root.Slot+1, root)
	return t
}

// Restore installs a restart/checkpoint parent-ready boundary. parent must be
// earlier than slot; skipped slots between the two are reconstructed.
func (t *ParentReadyTracker) Restore(slot uint64, parent BlockID) bool {
	if parent.Slot >= slot {
		return false
	}
	if slot-parent.Slot > maxParentReadyRestoreGap {
		return false
	}
	if t.liveUpdates {
		return false
	}
	if _, invalid := t.invalidBlocks[parent]; invalid {
		return false
	}
	if t.slots == nil {
		t.slots = make(map[uint64]*parentReadyStatus)
	}
	t.slots = make(map[uint64]*parentReadyStatus)
	t.root = parent.Slot
	t.highestWithParentReady = slot
	t.status(parent.Slot).notarFallbacks = []BlockID{parent}
	for s := parent.Slot + 1; s < slot; s++ {
		st := t.status(s)
		st.skip = true
		st.parentsReady = []BlockID{parent}
	}
	t.status(slot).parentsReady = []BlockID{parent}
	return true
}

func (t *ParentReadyTracker) SetRoot(root uint64) {
	if root < t.root {
		return
	}
	t.root = root
	for slot := range t.slots {
		if slot < root {
			delete(t.slots, slot)
		}
	}
	for block := range t.invalidBlocks {
		if block.Slot < root {
			delete(t.invalidBlocks, block)
		}
	}
	for block := range t.certifiedBlocks {
		if block.Slot < root {
			delete(t.certifiedBlocks, block)
		}
	}
	for slot := range t.certifiedBlockCounts {
		if slot < root {
			delete(t.certifiedBlockCounts, slot)
		}
	}
}

func (t *ParentReadyTracker) Root() uint64 { return t.root }

func (t *ParentReadyTracker) AddNotarFallbackOrStronger(block BlockID) ([]ConsensusEvent, error) {
	if block.Slot <= t.root {
		return nil, nil
	}
	return t.addNotarFallbackOrStronger(block)
}

// AddGenesis permits the migration genesis block at the current root. This is
// the behavior fixed after Agave v4.2.0-beta.0.
func (t *ParentReadyTracker) AddGenesis(block BlockID) ([]ConsensusEvent, error) {
	if block.Slot < t.root {
		return nil, nil
	}
	return t.addNotarFallbackOrStronger(block)
}

func (t *ParentReadyTracker) addNotarFallbackOrStronger(block BlockID) ([]ConsensusEvent, error) {
	t.liveUpdates = true
	if t.certifiedBlocks == nil {
		t.certifiedBlocks = make(map[BlockID]struct{})
	}
	if t.certifiedBlockCounts == nil {
		t.certifiedBlockCounts = make(map[uint64]int)
	}
	if _, certified := t.certifiedBlocks[block]; certified {
		return nil, nil
	}
	if t.certifiedBlockCounts[block.Slot] >= maxNotarFallbackBlocks {
		return nil, fmt.Errorf("alpenglow parent-ready: slot %d exceeds %d notarize-fallback blocks", block.Slot, maxNotarFallbackBlocks)
	}
	t.certifiedBlocks[block] = struct{}{}
	t.certifiedBlockCounts[block.Slot]++
	if _, invalid := t.invalidBlocks[block]; invalid {
		return nil, nil
	}
	status := t.status(block.Slot)
	if containsBlock(status.notarFallbacks, block) {
		return nil, nil
	}
	status.notarFallbacks = append(status.notarFallbacks, block)

	var events []ConsensusEvent
	for slot := block.Slot + 1; ; slot++ {
		status := t.status(slot)
		if !containsBlock(status.parentsReady, block) {
			status.parentsReady = append(status.parentsReady, block)
			if isLeaderWindowStart(slot) {
				status.readyAt = time.Now()
				events = append(events, ConsensusEvent{Kind: ConsensusEventParentReady, Slot: slot, Block: block})
			}
			if slot > t.highestWithParentReady {
				t.highestWithParentReady = slot
			}
		}
		if !status.skip {
			break
		}
	}
	return events, nil
}

// InvalidateBlock permanently removes an objectively invalid identity from
// active parent selection. Previously emitted ParentReady observations remain
// historical facts, but a surviving sibling is re-emitted as the corrected
// active parent for every affected leader window.
func (t *ParentReadyTracker) InvalidateBlock(block BlockID) []ConsensusEvent {
	if t.invalidBlocks == nil {
		t.invalidBlocks = make(map[BlockID]struct{})
	}
	if _, exists := t.invalidBlocks[block]; exists {
		return nil
	}
	t.invalidBlocks[block] = struct{}{}

	var corrected []ConsensusEvent
	_, affected := t.certifiedBlocks[block]
	for slot, status := range t.slots {
		var removedNotar, removedParent bool
		status.notarFallbacks, removedNotar = removeBlock(status.notarFallbacks, block)
		status.parentsReady, removedParent = removeBlock(status.parentsReady, block)
		affected = affected || removedNotar || removedParent
		if !removedParent || !isLeaderWindowStart(slot) {
			continue
		}
		parents := t.Parents(slot)
		if len(parents) == 0 {
			continue
		}
		corrected = append(corrected, ConsensusEvent{
			Kind:  ConsensusEventParentReady,
			Slot:  slot,
			Block: parents[0],
		})
	}
	if affected {
		t.liveUpdates = true
	}
	t.recomputeHighestWithParentReady()
	sort.Slice(corrected, func(i, j int) bool { return corrected[i].Slot < corrected[j].Slot })
	return corrected
}

func (t *ParentReadyTracker) AddSkip(slot uint64) []ConsensusEvent {
	if slot <= t.root {
		return nil
	}
	t.liveUpdates = true
	t.status(slot).skip = true

	future := []uint64{slot + 1}
	for next := slot + 1; t.status(next).skip; next++ {
		future = append(future, next+1)
	}

	if slot == 0 {
		return nil
	}
	previous, ok := t.slots[slot-1]
	if !ok {
		return nil
	}
	potential := append([]BlockID(nil), previous.notarFallbacks...)
	if previous.skip {
		for _, parent := range previous.parentsReady {
			if !containsBlock(potential, parent) {
				potential = append(potential, parent)
			}
		}
	}
	if len(potential) == 0 {
		return nil
	}

	var events []ConsensusEvent
	for _, next := range future {
		status := t.status(next)
		for _, parent := range potential {
			if containsBlock(status.parentsReady, parent) {
				continue
			}
			status.parentsReady = append(status.parentsReady, parent)
			if isLeaderWindowStart(next) {
				status.readyAt = time.Now()
				events = append(events, ConsensusEvent{Kind: ConsensusEventParentReady, Slot: next, Block: parent})
			}
		}
		if next > t.highestWithParentReady {
			t.highestWithParentReady = next
		}
	}
	return events
}

func (t *ParentReadyTracker) HasNotarFallbackOrStronger(block BlockID) bool {
	status := t.slots[block.Slot]
	return status != nil && containsBlock(status.notarFallbacks, block)
}

func (t *ParentReadyTracker) Parents(slot uint64) []BlockID {
	status := t.slots[slot]
	if status == nil {
		return nil
	}
	parents := make([]BlockID, 0, len(status.parentsReady))
	for _, parent := range status.parentsReady {
		if _, invalid := t.invalidBlocks[parent]; !invalid {
			parents = append(parents, parent)
		}
	}
	sort.Slice(parents, func(i, j int) bool { return blockLess(parents[i], parents[j]) })
	return parents
}

func (t *ParentReadyTracker) BlockProductionParent(slot uint64) BlockProductionParent {
	if t.highestWithParentReady > slot {
		return BlockProductionParent{Kind: BlockProductionParentMissedWindow}
	}
	parents := t.Parents(slot)
	if len(parents) == 0 {
		return BlockProductionParent{Kind: BlockProductionParentNotReady}
	}
	return BlockProductionParent{Kind: BlockProductionParentReady, Parent: parents[0], ReadyAt: t.status(slot).readyAt}
}

func (t *ParentReadyTracker) status(slot uint64) *parentReadyStatus {
	status := t.slots[slot]
	if status == nil {
		status = &parentReadyStatus{}
		t.slots[slot] = status
	}
	return status
}

func (t *ParentReadyTracker) recomputeHighestWithParentReady() {
	t.highestWithParentReady = 0
	for slot := range t.slots {
		if len(t.Parents(slot)) > 0 && slot > t.highestWithParentReady {
			t.highestWithParentReady = slot
		}
	}
}

func removeBlock(blocks []BlockID, block BlockID) ([]BlockID, bool) {
	kept := blocks[:0]
	removed := false
	for _, candidate := range blocks {
		if candidate == block {
			removed = true
			continue
		}
		kept = append(kept, candidate)
	}
	return kept, removed
}

func isLeaderWindowStart(slot uint64) bool { return slot%LeaderWindowSlots == 0 }

func containsBlock(blocks []BlockID, block BlockID) bool {
	for _, existing := range blocks {
		if existing == block {
			return true
		}
	}
	return false
}

func blockLess(a, b BlockID) bool {
	if a.Slot != b.Slot {
		return a.Slot < b.Slot
	}
	return bytes.Compare(a.Hash[:], b.Hash[:]) < 0
}
