// live_handoff: Live-stream staging and handoff: buffering blocks ahead of the emission frontier, arming the runway, and the intake gates (shared by turbine and lightbringer sources).
package blockstream

import (
	"fmt"
	b "github.com/Overclock-Validator/mithril/pkg/block"
	"math"
	"sort"

	"github.com/Overclock-Validator/mithril/pkg/mlog"
)

func (bs *BlockSource) bufferLightbringerBlock(blk *b.Block) {
	if blk == nil {
		return
	}

	var evicted []uint64
	bs.lightbringerBufferMu.Lock()
	if _, exists := bs.lightbringerBuffer[blk.Slot]; exists {
		bs.lightbringerBufferMu.Unlock()
		return
	}

	bs.lightbringerBuffer[blk.Slot] = blk
	bs.lightbringerBufferOrder = append(bs.lightbringerBufferOrder, blk.Slot)

	catchup := bs.repairCatchupActive()
	for len(bs.lightbringerBufferOrder) > lightbringerBufferSlots {
		victimIdx := 0
		if catchup {
			// During catchup the staging buffer holds BOTH the repair-window
			// blocks emission needs next AND live-edge prefetch streaming in
			// at cluster pace. Insertion-order eviction flushes the WINDOW
			// out under edge pressure — including the head block itself,
			// deadlocking emission behind the assembler's completed-slot
			// marker (observed: four consecutive catchups stuck at exactly
			// the head slot). Evict the HIGHEST slot instead: the edge is
			// cheap to re-fetch near tip; the window is what unblocks replay.
			for i, slot := range bs.lightbringerBufferOrder {
				if slot > bs.lightbringerBufferOrder[victimIdx] {
					victimIdx = i
				}
			}
		}
		victim := bs.lightbringerBufferOrder[victimIdx]
		bs.lightbringerBufferOrder = append(bs.lightbringerBufferOrder[:victimIdx], bs.lightbringerBufferOrder[victimIdx+1:]...)
		delete(bs.lightbringerBuffer, victim)
		evicted = append(evicted, victim)
	}
	bs.lightbringerBufferMu.Unlock()

	// An evicted staged block leaves a completed-slot marker in the
	// assembler, which then silently ignores every future shred AND skips
	// the slot in repair-request generation — the block becomes permanently
	// unfetchable. Clear the marker so eviction means "re-fetch later",
	// never "gone forever".
	for _, slot := range evicted {
		bs.resetTurbineSlotState(slot)
	}
}

// drainStagedCatchupBlocks moves staged blocks inside [from, to] to the
// emitter's queue — the post-handoff companion of the staging intake window:
// far-future live blocks wait in capped staging, and replay pulls them in as
// it approaches.
func (bs *BlockSource) drainStagedCatchupBlocks(from, to uint64) {
	var ready []*b.Block
	bs.lightbringerBufferMu.Lock()
	for slot, blk := range bs.lightbringerBuffer {
		if blk != nil && slot >= from && slot <= to {
			ready = append(ready, blk)
			delete(bs.lightbringerBuffer, slot)
		}
	}
	if len(ready) > 0 {
		order := bs.lightbringerBufferOrder[:0]
		for _, slot := range bs.lightbringerBufferOrder {
			if _, still := bs.lightbringerBuffer[slot]; still {
				order = append(order, slot)
			}
		}
		bs.lightbringerBufferOrder = order
	}
	bs.lightbringerBufferMu.Unlock()
	if len(ready) > 0 {
		bs.enqueueLightbringerBlocks(ready)
	}
}

// stagedLightbringerBlock returns the staged (pre-handoff) block for slot,
// nil when none is held.
func (bs *BlockSource) stagedLightbringerBlock(slot uint64) *b.Block {
	bs.lightbringerBufferMu.Lock()
	defer bs.lightbringerBufferMu.Unlock()
	return bs.lightbringerBuffer[slot]
}

// applyCertifiedSkipToCatchupHead marks the emitter's waiting slot as
// certificate-skipped and nudges the results loop with a synthetic skip
// result so emission ADVANCES. During a pre-handoff repair drive nothing
// else feeds that loop — staged blocks bypass it — so even a skip the chain
// tracker already knows about would never apply, and a genuinely skipped
// head stalls catchup forever: zero shreds exist to repair (observed live:
// slot 6176512, skipped on-chain, four-minute stall at "held 0").
func (bs *BlockSource) applyCertifiedSkipToCatchupHead(slot uint64) bool {
	bs.reorderMu.Lock()
	already := bs.skippedSlots[slot] || slot != bs.nextSlotToSend
	if !already {
		if bs.reorderBuffer[slot] != nil {
			// A buffered candidate contradicted by a skip certificate is
			// noncanonical — the certificate wins; drop the block so the
			// synthetic skip can advance the emitter.
			delete(bs.reorderBuffer, slot)
		}
		bs.alpenglowCertifiedSkips[slot] = true
	}
	bs.reorderMu.Unlock()
	if already {
		return false
	}
	// Stop repairing a block that provably does not exist.
	bs.resetTurbineSlotState(slot)
	select {
	case bs.resultQueue <- fetchResult{slot: slot, skipped: true, rpcIdx: -1, liveStreamGeneration: bs.lightbringerResultGeneration.Load()}:
		return true
	case <-bs.stopChan:
		return false
	}
}

func (bs *BlockSource) clearBufferedLightbringerBlocks() int {
	bs.lightbringerBufferMu.Lock()
	cleared := make([]uint64, 0, len(bs.lightbringerBuffer))
	for slot := range bs.lightbringerBuffer {
		cleared = append(cleared, slot)
	}
	bs.lightbringerBuffer = make(map[uint64]*b.Block)
	bs.lightbringerBufferOrder = nil
	bs.lightbringerBufferMu.Unlock()

	// A cleared staged block's assembler completed-slot marker would make
	// the slot silently unfetchable forever; reset so it can be re-fetched.
	for _, slot := range cleared {
		bs.resetTurbineSlotState(slot)
	}
	return len(cleared)
}

func (bs *BlockSource) purgeRPCStateAtOrBeyondSlot(slot uint64) {
	if slot == 0 {
		return
	}

	bs.reorderMu.Lock()
	for bufferedSlot, blk := range bs.reorderBuffer {
		if bufferedSlot < slot {
			continue
		}
		if blk == nil || !blk.FromLightbringer {
			delete(bs.reorderBuffer, bufferedSlot)
		}
	}
	for skippedSlot := range bs.skippedSlots {
		if skippedSlot >= slot {
			delete(bs.skippedSlots, skippedSlot)
			delete(bs.lightbringerSynthesizedSkips, skippedSlot)
			delete(bs.alpenglowCertifiedSkips, skippedSlot)
		}
	}
	bs.reorderMu.Unlock()

	bs.slotStateMu.Lock()
	for trackedSlot := range bs.slotState {
		if trackedSlot >= slot && !bs.isLightbringerRepairSlot(trackedSlot) {
			delete(bs.slotState, trackedSlot)
			delete(bs.inflightStart, trackedSlot)
		}
	}
	bs.slotStateMu.Unlock()

	bs.retryMu.Lock()
	if len(bs.retrySlots) > 0 {
		filtered := bs.retrySlots[:0]
		for _, retrySlot := range bs.retrySlots {
			if retrySlot < slot || bs.isLightbringerRepairSlot(retrySlot) {
				filtered = append(filtered, retrySlot)
			}
		}
		bs.retrySlots = filtered
	}
	bs.retryMu.Unlock()
}

func (bs *BlockSource) lightbringerHandoffMaxReplayGap() uint64 {
	// Arm Lightbringer only in the lower half of the near-tip window. Once
	// forkchoice buffering starts, replay can wait for vote-confirmed path
	// resolution; keeping this headroom prevents immediate lost-tip fallback.
	maxGap := bs.nearTipThreshold / 2
	if maxGap == 0 && bs.nearTipThreshold > 0 {
		maxGap = 1
	}
	if bs.catchupThreshold > 0 && maxGap >= bs.catchupThreshold {
		maxGap = bs.catchupThreshold - 1
	}
	return maxGap
}

func (bs *BlockSource) lightbringerHandoffTipEstimate() uint64 {
	tip := bs.confirmedTip.Load()
	if bs.lightbringerConnected.Load() {
		if streamed := bs.lightbringerLastStreamSlot.Load(); streamed > tip {
			tip = streamed
		}
	}
	return tip
}

func (bs *BlockSource) lightbringerHandoffReplayGapOK() (bool, uint64, uint64, uint64, uint64) {
	maxGap := bs.lightbringerHandoffMaxReplayGap()
	tip := bs.lightbringerHandoffTipEstimate()
	lastExecuted := bs.lastExecutedSlot.Load()
	if tip == 0 || lastExecuted == 0 {
		return true, 0, maxGap, tip, lastExecuted
	}

	var gap uint64
	if tip > lastExecuted {
		gap = tip - lastExecuted
	}
	return gap <= maxGap, gap, maxGap, tip, lastExecuted
}

func lightbringerDefaultHandoffLastSlot(waitingSlot uint64) uint64 {
	requiredLastSlot := waitingSlot + uint64(lightbringerMinHandoffRun) - 1
	if requiredLastSlot < waitingSlot {
		requiredLastSlot = math.MaxUint64
	}
	return requiredLastSlot
}

func (bs *BlockSource) lightbringerLiveEdgeHandoffMaxLag() uint64 {
	maxLag := bs.nearTipLookahead + 2
	if maxLag < lightbringerLiveEdgeHandoffMaxLag {
		maxLag = lightbringerLiveEdgeHandoffMaxLag
	}
	return maxLag
}

func (bs *BlockSource) allowsLiveEdgeHandoff() bool {
	return bs.sourceType == BlockSourceTurbine
}

func (bs *BlockSource) lightbringerHandoffRequiredLastSlot(waitingSlot uint64) uint64 {
	// Repair catchup: arm the handoff the moment the HEAD slot is assembled
	// and anchor-connected — replay should start executing immediately, not
	// wait for a deep staged runway. The 8-block minimum run below exists to
	// avoid arm/disarm thrash on a raw live edge; during catchup the drive
	// keeps repair pressure on the window, later blocks flow to the emitter
	// as they assemble, and the parent-connect check at emission still gates
	// execution.
	if bs.repairCatchupActive() {
		return waitingSlot
	}
	requiredLastSlot := lightbringerDefaultHandoffLastSlot(waitingSlot)
	if !bs.allowsLiveEdgeHandoff() || !bs.lightbringerConnected.Load() {
		return requiredLastSlot
	}

	latestStreamed := bs.lightbringerLastStreamSlot.Load()
	if latestStreamed < waitingSlot {
		return requiredLastSlot
	}

	tip := bs.lightbringerHandoffTipEstimate()
	if tip > latestStreamed && tip-latestStreamed > bs.lightbringerLiveEdgeHandoffMaxLag() {
		return requiredLastSlot
	}

	if latestStreamed < requiredLastSlot {
		return latestStreamed
	}
	return requiredLastSlot
}

func (bs *BlockSource) prepareLightbringerHandoff(waitingSlot uint64, anchorSlot uint64) ([]*b.Block, uint64, bool) {
	if !bs.isNearTip.Load() && !bs.repairCatchupActive() {
		return nil, 0, false
	}
	if handoffSlot := bs.lightbringerHandoffSlot.Load(); handoffSlot != 0 {
		return nil, handoffSlot, false
	}
	if !bs.repairCatchupActive() {
		if ok, _, _, _, _ := bs.lightbringerHandoffReplayGapOK(); !ok {
			return nil, 0, false
		}
	}

	bs.lightbringerBufferMu.Lock()

	_, coveredUntil, _, _, ok := bs.connectedLightbringerRunwayLocked(waitingSlot, anchorSlot)
	if !ok {
		bs.lightbringerBufferMu.Unlock()
		return nil, 0, false
	}

	requiredLastSlot := bs.lightbringerHandoffRequiredLastSlot(waitingSlot)
	if coveredUntil < requiredLastSlot {
		bs.lightbringerBufferMu.Unlock()
		return nil, 0, false
	}

	handoffSlot := waitingSlot
	if !bs.lightbringerHandoffSlot.CompareAndSwap(0, handoffSlot) {
		current := bs.lightbringerHandoffSlot.Load()
		bs.lightbringerBufferMu.Unlock()
		return nil, current, false
	}

	bs.lightbringerNeedRPCResume.Store(false)
	bs.purgeRPCStateAtOrBeyondSlot(handoffSlot)

	// Hand over EVERYTHING staged at/above the handoff slot, not just the
	// connected runway. The reorder buffer is exactly the structure for
	// out-of-order blocks, and holes between them are the repair drive's
	// job. Handing over only the runway proved catastrophic with the
	// prewarm spool: one missing slot right after the head reduced the
	// runway to a single block and dropped ~190 pre-collected blocks for
	// slow re-repair (observed live: first buffered block 252 slots ahead
	// of replay). The ARMING requirement stays runway-based — the head must
	// parent-link to the anchor; only the payload widens.
	blocks := make([]*b.Block, 0, len(bs.lightbringerBuffer))
	var dropped []uint64
	for slot, blk := range bs.lightbringerBuffer {
		if slot >= handoffSlot && blk != nil {
			blocks = append(blocks, blk)
		} else {
			// Below the handoff slot: stale relative to the resume point.
			// Reset turbine state so the marker never strands the slot.
			dropped = append(dropped, slot)
		}
	}

	bs.lightbringerBuffer = make(map[uint64]*b.Block)
	bs.lightbringerBufferOrder = nil
	bs.lightbringerBufferMu.Unlock()

	for _, slot := range dropped {
		bs.resetTurbineSlotState(slot)
	}

	return blocks, handoffSlot, true
}

func (bs *BlockSource) connectedLightbringerRunwayLocked(waitingSlot uint64, anchorSlot uint64) ([]*b.Block, uint64, uint64, uint64, bool) {
	candidates := make([]*b.Block, 0, len(bs.lightbringerBuffer))
	var firstBufferedSlot uint64
	var maxBufferedSlot uint64
	foundFirstBuffered := false
	for slot, blk := range bs.lightbringerBuffer {
		if blk == nil || slot < waitingSlot {
			continue
		}
		candidates = append(candidates, blk)
		if !foundFirstBuffered || slot < firstBufferedSlot {
			firstBufferedSlot = slot
			foundFirstBuffered = true
		}
		if slot > maxBufferedSlot {
			maxBufferedSlot = slot
		}
	}
	if len(candidates) == 0 {
		return nil, 0, 0, 0, false
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Slot == candidates[j].Slot {
			return candidates[i].SourceParentSlot < candidates[j].SourceParentSlot
		}
		return candidates[i].Slot < candidates[j].Slot
	})

	currentAnchor := anchorSlot
	minSlot := waitingSlot
	coveredUntil := waitingSlot - 1
	if waitingSlot == 0 {
		coveredUntil = 0
	}

	runway := make([]*b.Block, 0, len(candidates))
	used := make(map[uint64]bool, len(candidates))
	for {
		var next *b.Block
		for _, blk := range candidates {
			if blk == nil || used[blk.Slot] || blk.Slot < minSlot {
				continue
			}
			if blk.SourceParentSlot != currentAnchor {
				continue
			}
			next = blk
			break
		}
		if next == nil {
			break
		}

		runway = append(runway, next)
		used[next.Slot] = true
		coveredUntil = next.Slot
		currentAnchor = next.Slot
		minSlot = next.Slot + 1
	}

	return runway, coveredUntil, firstBufferedSlot, maxBufferedSlot, len(runway) != 0
}

func (bs *BlockSource) lightbringerHandoffWaitReason(waitingSlot uint64, anchorSlot uint64) string {
	bs.lightbringerBufferMu.Lock()
	defer bs.lightbringerBufferMu.Unlock()

	runway, coveredUntil, firstBufferedSlot, maxBufferedSlot, ok := bs.connectedLightbringerRunwayLocked(waitingSlot, anchorSlot)
	if !ok {
		if len(bs.lightbringerBuffer) == 0 {
			return fmt.Sprintf("waiting for first buffered %s slot", bs.liveShredStreamName())
		}
		return fmt.Sprintf("no buffered %s slot at or beyond waiting slot %d", bs.liveShredStreamName(), waitingSlot)
	}

	requiredLastSlot := bs.lightbringerHandoffRequiredLastSlot(waitingSlot)
	if coveredUntil >= requiredLastSlot {
		if ok, gap, maxGap, tip, lastExecuted := bs.lightbringerHandoffReplayGapOK(); !ok {
			return fmt.Sprintf("handoff-ready runway buffered through slot %d, but replay gap %d exceeds handoff arm threshold %d (last executed %d, live tip estimate %d)",
				coveredUntil, gap, maxGap, lastExecuted, tip)
		}
		return fmt.Sprintf("handoff-ready runway buffered through slot %d", coveredUntil)
	}

	firstRunwaySlot := runway[0].Slot
	if firstRunwaySlot > waitingSlot {
		return fmt.Sprintf("waiting for slot %d or skipped-slot inference; first buffered %s block is slot %d (parent %d), connected runway covers through slot %d, latest buffered slot %d",
			waitingSlot, bs.liveShredStreamName(), firstRunwaySlot, runway[0].SourceParentSlot, coveredUntil, maxBufferedSlot)
	}

	if firstBufferedSlot != 0 && firstRunwaySlot != firstBufferedSlot {
		return fmt.Sprintf("earliest buffered slot %d is not on the current connected runway; connected runway covers through slot %d, latest buffered slot %d",
			firstBufferedSlot, coveredUntil, maxBufferedSlot)
	}

	return fmt.Sprintf("connected runway only covers through slot %d; need through slot %d before handoff", coveredUntil, requiredLastSlot)
}

func (bs *BlockSource) enqueueLightbringerBlocks(blocks []*b.Block) {
	if len(blocks) == 0 {
		return
	}
	generation := bs.lightbringerResultGeneration.Load()

	sort.Slice(blocks, func(i, j int) bool {
		return blocks[i].Slot < blocks[j].Slot
	})

	for _, blk := range blocks {
		if blk == nil {
			continue
		}

		select {
		case bs.resultQueue <- fetchResult{slot: blk.Slot, block: blk, rpcIdx: -1, liveStreamGeneration: generation}:
		case <-bs.stopChan:
			return
		}
	}
}

func (bs *BlockSource) maybePrepareLightbringerHandoff() {
	if !bs.usesLiveShredStream() || (!bs.isNearTip.Load() && !bs.repairCatchupActive()) {
		return
	}
	if bs.lightbringerForceRPCUntil.Load() != 0 {
		return
	}
	if bs.lightbringerCooldownUntil.Load() != 0 {
		return
	}

	bs.reorderMu.Lock()
	waitingSlot := bs.nextSlotToSend
	anchorSlot := bs.lastEmittedBlockSlot
	bs.reorderMu.Unlock()

	// Resume-time anchor: nothing has emitted this process yet, but the
	// runway's first block must parent-link to the resume block. This also
	// chain-checks the repaired gap against our durable state.
	if anchorSlot == 0 {
		if lastExecuted := bs.lastExecutedSlot.Load(); lastExecuted != 0 {
			anchorSlot = lastExecuted
		}
	}
	// Fresh boot: replay has not executed ANYTHING this process yet, so both
	// anchors above are zero — but startSlot IS the resume frontier, so the
	// anchor is the slot before it. Without this, the runway can never
	// connect (no block has SourceParentSlot == 0), the handoff can never
	// arm, and on a shreds-only node — where the FIRST emission can only
	// come from this very handoff — catchup deadlocks at boot with the head
	// fully assembled and staged. Observed live on four consecutive runs.
	if anchorSlot == 0 && bs.startSlot > 0 {
		anchorSlot = bs.startSlot - 1
	}

	// A large replay gap is the POINT of repair catchup; the gap check guards
	// the ordinary near-tip handoff only.
	if !bs.repairCatchupActive() {
		if ok, _, _, _, _ := bs.lightbringerHandoffReplayGapOK(); !ok {
			return
		}
	}

	blocks, handoffSlot, prepared := bs.prepareLightbringerHandoff(waitingSlot, anchorSlot)
	if !prepared {
		return
	}

	runwayCoveredUntil := handoffSlot - 1
	if len(blocks) > 0 {
		runwayCoveredUntil = blocks[len(blocks)-1].Slot
	}
	catchupNote := "RPC catchup continues until then"
	if !bs.rpcBlockFetchAllowed() {
		catchupNote = "repair catchup continues until then"
	}
	mlog.Log.Infof("%s handoff ready at slot %d (connected runway buffered through slot %d; %s)",
		bs.liveShredStreamName(), handoffSlot, runwayCoveredUntil, catchupNote)
	bs.enqueueLightbringerBlocks(blocks)
}

func (bs *BlockSource) lightbringerStagingMaxReplayGap() uint64 {
	maxGap := bs.catchupThreshold
	minGap := bs.nearTipThreshold + uint64(lightbringerMinHandoffRun)
	if minGap < bs.nearTipThreshold {
		minGap = math.MaxUint64
	}
	if maxGap < minGap {
		maxGap = minGap
	}
	return maxGap
}

func (bs *BlockSource) shouldStageLightbringerSlot(slot uint64) bool {
	if bs.lightbringerForceRPCUntil.Load() != 0 {
		return false
	}
	if bs.lightbringerCooldownUntil.Load() != 0 {
		return false
	}

	lastExecuted := bs.lastExecutedSlot.Load()
	if lastExecuted == 0 || slot <= lastExecuted {
		return false
	}

	bs.reorderMu.Lock()
	nextSlot := bs.nextSlotToSend
	bs.reorderMu.Unlock()
	if nextSlot != 0 && slot < nextSlot {
		return false
	}

	tip := bs.lightbringerHandoffTipEstimate()
	if tip <= lastExecuted {
		return false
	}
	return tip-lastExecuted <= bs.lightbringerStagingMaxReplayGap()
}

func (bs *BlockSource) shouldDecodeLightbringerSlot(slot uint64) bool {
	if bs.lightbringerForceRPCUntil.Load() != 0 {
		return false
	}
	if bs.lightbringerCooldownUntil.Load() != 0 {
		return false
	}
	handoffSlot := bs.lightbringerHandoffSlot.Load()
	if handoffSlot == 0 {
		// While repair catchup is pending or active, buffer everything above
		// the resume frontier: the runway arms the handoff at the gap start,
		// and edge blocks assembled meanwhile must not be dropped (a dropped
		// block's slot is marked completed in the assembler and would need a
		// cert-driven reset to re-fetch).
		if (bs.repairCatchupPending.Load() || bs.repairCatchupActive()) && slot >= bs.repairCatchupGateSlot() {
			return true
		}
		if bs.catchupRescueCovers(slot) {
			return true // catchup stall rescue: this slot is being repair-pulled
		}
		return bs.isNearTip.Load() || bs.shouldStageLightbringerSlot(slot)
	}
	return (bs.isNearTip.Load() || bs.repairCatchupActive()) && slot >= handoffSlot
}

// inspectLaterLightbringerBlocksLocked summarizes later buffered live shred
// traffic for the currently waiting slot so diagnostics can distinguish
// "we have later stream traffic" from "we have a descendant that reconnects
// directly to the current anchor".
func (bs *BlockSource) inspectLaterLightbringerBlocksLocked(waitingSlot uint64) (firstBufferedSlot uint64, firstBufferedParentSlot uint64, bufferedCount int, firstConnectedSlot uint64, firstConnectedParentSlot uint64, foundConnected bool) {
	for slot, blk := range bs.reorderBuffer {
		if blk == nil || !blk.FromLightbringer || slot <= waitingSlot {
			continue
		}
		bufferedCount++
		if firstBufferedSlot == 0 || slot < firstBufferedSlot {
			firstBufferedSlot = slot
			firstBufferedParentSlot = blk.SourceParentSlot
		}
		if blk.SourceParentSlot != bs.lastEmittedBlockSlot {
			continue
		}
		if !foundConnected || slot < firstConnectedSlot {
			firstConnectedSlot = slot
			firstConnectedParentSlot = blk.SourceParentSlot
			foundConnected = true
		}
	}
	return firstBufferedSlot, firstBufferedParentSlot, bufferedCount, firstConnectedSlot, firstConnectedParentSlot, foundConnected
}
