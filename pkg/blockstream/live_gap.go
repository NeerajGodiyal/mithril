// live_gap: Live-stream gap detection and healing: a missing slot is repair's job on turbine and a reconnect/RPC question on lightbringer.
package blockstream

import (
	"context"
	"errors"
	"fmt"
	"time"

	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/gagliardetto/solana-go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	alpenglowEmittedBlockIDHistory  = 65_536
	alpenglowMaxParentLinkedSkipRun = 65_536
)

func (bs *BlockSource) clearLiveGapWatch() {
	bs.lightbringerGapSlot.Store(0)
	bs.liveGapSinceUnix.Store(0)
	bs.liveGapLastLogUnix.Store(0)
	bs.lightbringerGapReconnectSlot.Store(0)
}

func (bs *BlockSource) invalidateLiveStreamResults() uint64 {
	return bs.liveResultGeneration.Add(1)
}

func (bs *BlockSource) setLiveStreamCancel(cancel context.CancelFunc) {
	bs.liveCancelMu.Lock()
	bs.liveCancel = cancel
	bs.liveCancelMu.Unlock()
}

func (bs *BlockSource) clearLiveStreamCancel() {
	bs.liveCancelMu.Lock()
	bs.liveCancel = nil
	bs.liveCancelMu.Unlock()
}

func (bs *BlockSource) requestLiveStreamReconnect(reason string) bool {
	if !bs.liveStreamConnected.Load() {
		return false
	}
	if !bs.liveReconnectRequested.CompareAndSwap(false, true) {
		return false
	}

	bs.reorderMu.Lock()
	waitingSlot := bs.nextSlotToSend
	lastEmitted := bs.lastEmittedBlockSlot
	bs.reorderMu.Unlock()
	latestStreamed := bs.liveLastStreamSlot.Load()

	action := "stream reconnect requested"
	if bs.sourceType == BlockSourceTurbine {
		// Not a remote stream: this restarts our LOCAL UDP receiver.
		action = "local receiver restart requested"
	}
	mlog.Log.Warnf("%s %s: %s | waiting_slot=%d | last_emitted=%d | latest_streamed=%d",
		bs.liveShredStreamName(), action, reason, waitingSlot, lastEmitted, latestStreamed)

	bs.liveCancelMu.Lock()
	cancel := bs.liveCancel
	bs.liveCancelMu.Unlock()
	if cancel == nil {
		bs.liveReconnectRequested.Store(false)
		return false
	}
	cancel()
	return true
}

func isLiveStreamReconnectCancel(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	return status.Code(err) == codes.Canceled
}

func (bs *BlockSource) maybeReconnectActiveLiveStreamForNoProgress(stallDuration time.Duration) {
	if !bs.usesLiveShredStream() {
		return
	}
	if !bs.liveStreamActive.Load() || !bs.isNearTip.Load() {
		return
	}
	if stallDuration < liveNoEmitReconnect {
		return
	}

	bs.reorderMu.Lock()
	waitingSlot := bs.nextSlotToSend
	waitingReady := bs.reorderBuffer[waitingSlot] != nil || bs.skippedSlots[waitingSlot]
	lastEmitted := bs.lastEmittedBlockSlot
	firstBufferedSlot := uint64(0)
	bufferedLightbringer := 0
	foundBuffered := false
	for slot, blk := range bs.reorderBuffer {
		if blk == nil || !blk.FromLiveStream || slot <= waitingSlot {
			continue
		}
		bufferedLightbringer++
		if !foundBuffered || slot < firstBufferedSlot {
			firstBufferedSlot = slot
			foundBuffered = true
		}
	}
	bs.reorderMu.Unlock()

	if waitingReady || len(bs.streamChan) > 0 {
		return
	}

	reason := fmt.Sprintf("no block emitted for %s while %s is active and replay is waiting on slot %d",
		stallDuration.Round(time.Second), bs.liveShredStreamName(), waitingSlot)
	if foundBuffered {
		reason = fmt.Sprintf("no block emitted for %s while waiting on slot %d (first_buffered=%d buffered_live_stream=%d last_emitted=%d)",
			stallDuration.Round(time.Second), waitingSlot, firstBufferedSlot, bufferedLightbringer, lastEmitted)
	}

	bs.requestLiveStreamReconnect(reason)
}

func (bs *BlockSource) isLiveRepairSlot(slot uint64) bool {
	return slot != 0 && bs.liveRepairSlot.Load() == slot
}

func (bs *BlockSource) clearLiveRepairSlot(slot uint64) {
	if slot == 0 {
		return
	}
	bs.liveRepairSlot.CompareAndSwap(slot, 0)
}

func (bs *BlockSource) resetLiveRepairSlot() {
	bs.liveRepairSlot.Store(0)
}

func (bs *BlockSource) liveBlockConnectsLocked(blk *b.Block) bool {
	if blk == nil || !blk.FromLiveStream {
		return true
	}
	if bs.lastEmittedBlockSlot == 0 {
		return true
	}
	if blk.SourceParentSlot == 0 {
		return false
	}
	return blk.SourceParentSlot == bs.lastEmittedBlockSlot
}

func (bs *BlockSource) recordEmittedAlpenglowBlockIDLocked(blk *b.Block) {
	if blk == nil || !blk.HasAlpenglowBlockID {
		bs.lastEmittedAlpenglowBlockID = solana.Hash{}
		bs.hasLastEmittedAlpenglowBlockID = false
		return
	}
	id := solana.Hash(blk.AlpenglowBlockID)
	bs.lastEmittedAlpenglowBlockID = id
	bs.hasLastEmittedAlpenglowBlockID = true
	if _, exists := bs.emittedAlpenglowBlockIDs[blk.Slot]; !exists {
		bs.emittedAlpenglowBlockIDOrder = append(bs.emittedAlpenglowBlockIDOrder, blk.Slot)
	}
	bs.emittedAlpenglowBlockIDs[blk.Slot] = id
	if len(bs.emittedAlpenglowBlockIDOrder) > alpenglowEmittedBlockIDHistory {
		oldest := bs.emittedAlpenglowBlockIDOrder[0]
		bs.emittedAlpenglowBlockIDOrder = bs.emittedAlpenglowBlockIDOrder[1:]
		delete(bs.emittedAlpenglowBlockIDs, oldest)
	}
}

// synthesizeAlpenglowParentLinkedSkipsLocked advances over a missing slot run
// only when a later turbine block carries an exact Alpenglow parent block ID
// matching the last emitted block. The inference is deliberately provisional:
// replay records the skipped outcome and the certificate switch sweep unwinds
// it if Votor later names a real block in the range.
func (bs *BlockSource) synthesizeAlpenglowParentLinkedSkipsLocked() bool {
	if bs.sourceType != BlockSourceTurbine || !bs.turbineAlpenglowBlockIDHints || !bs.hasLastEmittedAlpenglowBlockID {
		return false
	}
	waitingSlot := bs.nextSlotToSend
	if waitingSlot == 0 || bs.reorderBuffer[waitingSlot] != nil || bs.skippedSlots[waitingSlot] {
		return false
	}

	childSlot := uint64(0)
	var child *b.Block
	for slot, candidate := range bs.reorderBuffer {
		if slot <= waitingSlot || candidate == nil || !candidate.HasAlpenglowBlockID || !candidate.HasAlpenglowParentBlockID {
			continue
		}
		if candidate.SourceParentSlot != bs.lastEmittedBlockSlot || solana.Hash(candidate.AlpenglowParentBlockID) != bs.lastEmittedAlpenglowBlockID {
			continue
		}
		if child == nil || slot < childSlot {
			childSlot = slot
			child = candidate
		}
	}
	if child == nil {
		return false
	}
	if childSlot-waitingSlot > alpenglowMaxParentLinkedSkipRun {
		return false
	}
	// Never skip over an observed candidate, even if a later branch reconnects
	// to the anchor. A certificate must adjudicate that fork explicitly.
	for slot, candidate := range bs.reorderBuffer {
		if candidate != nil && slot >= waitingSlot && slot < childSlot {
			return false
		}
	}

	newSkips := uint64(0)
	for slot := waitingSlot; slot < childSlot; slot++ {
		if !bs.skippedSlots[slot] {
			newSkips++
			bs.stats.FetchSkipped.Add(1)
		}
		bs.skippedSlots[slot] = true
		bs.liveSynthesizedSkips[slot] = true
		delete(bs.alpenglowCertifiedSkips, slot)
		bs.slotStateMu.Lock()
		bs.slotState[slot] = slotDone
		delete(bs.inflightStart, slot)
		bs.slotStateMu.Unlock()
		bs.clearSlotErrors(slot)
	}
	if newSkips == 0 {
		return false
	}
	mlog.Log.Infof("ALPENGLOW speculative skip: slots %d-%d are absent on parent-linked branch %s -> %s at slot %d; certificate decisions remain authoritative",
		waitingSlot, childSlot-1, bs.lastEmittedAlpenglowBlockID, solana.Hash(child.AlpenglowBlockID), childSlot)
	return true
}

func (bs *BlockSource) shouldPreferIncomingLiveBlockLocked(existing, incoming *b.Block) bool {
	if existing == nil || incoming == nil {
		return false
	}
	if !existing.FromLiveStream || !incoming.FromLiveStream {
		return false
	}
	if existing.Slot != incoming.Slot {
		return false
	}
	return !bs.liveBlockConnectsLocked(existing) && bs.liveBlockConnectsLocked(incoming)
}

func (bs *BlockSource) waitingLiveParentMismatchLocked() (waitingSlot uint64, observedParentSlot uint64, expectedParentSlot uint64, mismatch bool) {
	blk := bs.reorderBuffer[bs.nextSlotToSend]
	if blk == nil || !blk.FromLiveStream {
		return 0, 0, 0, false
	}
	if bs.liveBlockConnectsLocked(blk) {
		return 0, 0, 0, false
	}
	return blk.Slot, blk.SourceParentSlot, bs.lastEmittedBlockSlot, true
}

func (bs *BlockSource) repairLiveStreamGap(waitingSlot, firstBufferedSlot, firstBufferedParentSlot uint64, bufferedCount int) {
	if !bs.usesLiveShredStream() {
		return
	}
	if !bs.liveStreamActive.Load() {
		bs.forceRPCForLiveGap(waitingSlot, firstBufferedSlot, firstBufferedParentSlot, bufferedCount)
		return
	}

	gapSinceUnix := bs.liveGapSinceUnix.Load()
	gapAge := time.Duration(0)
	if gapSinceUnix != 0 {
		gapAge = time.Since(time.Unix(0, gapSinceUnix))
	}

	streamName := bs.liveShredStreamName()
	waitReason := fmt.Sprintf("first buffered %s block %d still depends on parent slot %d", streamName, firstBufferedSlot, firstBufferedParentSlot)
	switch {
	case firstBufferedParentSlot == waitingSlot:
		waitReason = fmt.Sprintf("waiting on live %s slot %d; later buffered block %d still depends on it", streamName, waitingSlot, firstBufferedSlot)
	case firstBufferedParentSlot > waitingSlot:
		waitReason = fmt.Sprintf("waiting on missing %s ancestry range %d-%d; later buffered block %d still depends on slot %d",
			streamName, waitingSlot, firstBufferedParentSlot, firstBufferedSlot, firstBufferedParentSlot)
	case firstBufferedParentSlot == bs.lastEmittedBlockSlot:
		waitReason = fmt.Sprintf("later buffered block %d points back to anchor %d, but that only proves an observed alternate branch and is not treated as a canonical skipped run", firstBufferedSlot, bs.lastEmittedBlockSlot)
	}

	now := time.Now()
	lastLog := time.Unix(0, bs.liveGapLastLogUnix.Load())
	if lastLog.IsZero() || now.Sub(lastLog) >= reorderGapWarnInterval {
		bs.liveGapLastLogUnix.Store(now.UnixNano())
		// Expected catchup rhythm, not an operator problem: repair owns the
		// hole and the per-slot lines show progress. The full picture —
		// including the repair/hydration counters that explain WHY the head
		// is waiting — goes to catchup.log in the run directory.
		mlog.NamedFilef("catchup", "waiting for missing %s slot %d (keeping %s active) | first_buffered=%d | first_parent_slot=%d | buffered_live_stream=%d | reason=%s | mode=%s%s",
			streamName, waitingSlot, streamName, firstBufferedSlot, firstBufferedParentSlot, bufferedCount, waitReason, bs.currentModeString(), bs.catchupDiagSuffix())
	}

	// Native turbine is NOT a connection: a missing live slot is repair's
	// job, so pin repair on the hole and return — no reconnect reasoning at
	// all. "Reconnecting" would tear down our LOCAL receiver/repair/gossip
	// state (minutes of warmup) and re-stream nothing. Observed live: one
	// missing slot ended the first shred-native replay 15s in. The idle
	// watchdog still restarts a genuinely dead socket.
	if bs.sourceType == BlockSourceTurbine {
		bs.prioritizeTurbineRepairForLiveGap(waitingSlot, firstBufferedParentSlot)
		return
	}

	shouldReconnect := false
	reconnectReason := ""
	switch {
	case firstBufferedParentSlot != waitingSlot:
		if firstBufferedParentSlot <= waitingSlot {
			// A reconnect only helps when later buffered live-stream blocks still
			// depend on an unseen ancestor beyond the current anchor.
			break
		}
		switch {
		case bufferedCount >= reorderGapWarnThreshold && gapAge >= lightbringerDeepGapReconnect:
			shouldReconnect = true
			reconnectReason = fmt.Sprintf("waiting %s for %s ancestry range %d-%d while later buffered block %d still depends on slot %d",
				gapAge.Round(time.Second), streamName, waitingSlot, firstBufferedParentSlot, firstBufferedSlot, firstBufferedParentSlot)
		case gapAge >= lightbringerGapReconnectAfter:
			shouldReconnect = true
			reconnectReason = fmt.Sprintf("waiting %s for %s ancestry range %d-%d while later buffered block %d still depends on slot %d",
				gapAge.Round(time.Second), streamName, waitingSlot, firstBufferedParentSlot, firstBufferedSlot, firstBufferedParentSlot)
		}
	case bufferedCount >= reorderGapWarnThreshold && gapAge >= lightbringerDeepGapReconnect:
		shouldReconnect = true
		reconnectReason = fmt.Sprintf("waiting %s for live %s slot %d while %d later buffered blocks still depend on it",
			gapAge.Round(time.Second), streamName, waitingSlot, bufferedCount)
	case gapAge >= lightbringerGapReconnectAfter:
		shouldReconnect = true
		reconnectReason = fmt.Sprintf("waiting %s for live %s slot %d while later buffered blocks still depend on it",
			gapAge.Round(time.Second), streamName, waitingSlot)
	}

	if shouldReconnect && bs.lightbringerGapReconnectSlot.CompareAndSwap(0, waitingSlot) {
		bs.requestLiveStreamReconnect(reconnectReason)
	}
}

func (bs *BlockSource) forceRPCForLiveGap(waitingSlot, firstBufferedSlot, firstBufferedParentSlot uint64, bufferedCount int) {
	if !bs.usesLiveShredStream() {
		return
	}
	// Shreds-only mode: there is no RPC resume, so setting the force flags
	// would wedge the decode gates waiting for blocks that never come. Push
	// turbine repair at the missing range instead and leave the live path
	// fully open.
	if !bs.rpcFallbackEnabled {
		bs.prioritizeTurbineRepairForLiveGap(waitingSlot, firstBufferedParentSlot)
		now := time.Now()
		if last := time.Unix(bs.noRPCFallbackLogUnix.Load(), 0); now.Sub(last) >= 30*time.Second {
			bs.noRPCFallbackLogUnix.Store(now.Unix())
			mlog.Log.Warnf("live stream gap at slot %d: RPC block fetch is disabled (block.rpc_fallback=false); pushing turbine repair instead | first_buffered=%d | buffered_live_stream=%d",
				waitingSlot, firstBufferedSlot, bufferedCount)
		}
		return
	}

	recoveryUntil := waitingSlot + liveRecoverySlots
	if recoveryUntil < waitingSlot {
		recoveryUntil = ^uint64(0)
	}

	bs.liveForceRPCUntil.Store(waitingSlot)
	bs.liveCooldownUntil.Store(recoveryUntil)
	oldHandoff := bs.liveHandoffSlot.Swap(0)
	wasActive := bs.liveStreamActive.Swap(false)
	resultGeneration := bs.invalidateLiveStreamResults()
	bs.liveNeedRPCResume.Store(true)
	bs.clearLiveGapWatch()
	bs.resetLiveRepairSlot()
	clearedPrefetched := bs.clearBufferedLiveStreamBlocks()

	bs.reorderMu.Lock()
	removedSlots := make([]uint64, 0)
	for slot, blk := range bs.reorderBuffer {
		if blk != nil && blk.FromLiveStream && slot >= waitingSlot {
			delete(bs.reorderBuffer, slot)
			removedSlots = append(removedSlots, slot)
		}
	}
	bs.reorderMu.Unlock()

	if len(removedSlots) > 0 {
		bs.slotStateMu.Lock()
		for _, slot := range removedSlots {
			delete(bs.slotState, slot)
			delete(bs.inflightStart, slot)
		}
		bs.slotStateMu.Unlock()
	}

	if wasActive || oldHandoff != 0 {
		mlog.Log.Warnf("BLOCK SOURCE SWITCH: %s -> RPC at slot %d | reason=missing_streamed_slot | first_buffered=%d | first_parent_slot=%d | buffered_live_stream=%d | cleared_prefetched_live_stream=%d | rpc_forced_until=%d | rpc_cooldown_until=%d | live_generation=%d | mode=%s",
			bs.liveShredStreamName(), waitingSlot, firstBufferedSlot, firstBufferedParentSlot, bufferedCount, clearedPrefetched, waitingSlot, recoveryUntil, resultGeneration, bs.currentModeString())
		return
	}

	mlog.Log.Warnf("BLOCK SOURCE STATUS: forcing RPC because %s skipped waiting slot %d | first_buffered=%d | first_parent_slot=%d | buffered_live_stream=%d | cleared_prefetched_live_stream=%d | rpc_forced_until=%d | rpc_cooldown_until=%d | live_generation=%d | mode=%s",
		bs.liveShredStreamName(), waitingSlot, firstBufferedSlot, firstBufferedParentSlot, bufferedCount, clearedPrefetched, waitingSlot, recoveryUntil, resultGeneration, bs.currentModeString())
}

func (bs *BlockSource) handleDetectedLiveGap(waitingSlot, firstBufferedSlot, firstBufferedParentSlot uint64, bufferedCount int) {
	if bs.liveStreamActive.Load() {
		bs.repairLiveStreamGap(waitingSlot, firstBufferedSlot, firstBufferedParentSlot, bufferedCount)
		return
	}
	bs.forceRPCForLiveGap(waitingSlot, firstBufferedSlot, firstBufferedParentSlot, bufferedCount)
}

func (bs *BlockSource) forceRPCForLiveParentMismatch(waitingSlot, observedParentSlot, expectedParentSlot uint64) {
	if !bs.usesLiveShredStream() {
		return
	}

	recoveryUntil := waitingSlot + liveRecoverySlots
	if recoveryUntil < waitingSlot {
		recoveryUntil = ^uint64(0)
	}

	bs.liveForceRPCUntil.Store(waitingSlot)
	bs.liveCooldownUntil.Store(recoveryUntil)
	oldHandoff := bs.liveHandoffSlot.Swap(0)
	wasActive := bs.liveStreamActive.Swap(false)
	resultGeneration := bs.invalidateLiveStreamResults()
	bs.liveNeedRPCResume.Store(true)
	bs.clearLiveGapWatch()
	bs.resetLiveRepairSlot()
	clearedPrefetched := bs.clearBufferedLiveStreamBlocks()

	bs.reorderMu.Lock()
	removedSlots := make([]uint64, 0)
	for slot, blk := range bs.reorderBuffer {
		if blk != nil && blk.FromLiveStream && slot >= waitingSlot {
			delete(bs.reorderBuffer, slot)
			removedSlots = append(removedSlots, slot)
		}
	}
	bs.reorderMu.Unlock()

	if len(removedSlots) > 0 {
		bs.slotStateMu.Lock()
		for _, slot := range removedSlots {
			delete(bs.slotState, slot)
			delete(bs.inflightStart, slot)
		}
		bs.slotStateMu.Unlock()
	}

	reason := "parent_slot_mismatch"
	if observedParentSlot == 0 {
		reason = "missing_parent_slot"
	}

	if wasActive || oldHandoff != 0 {
		mlog.Log.Warnf("BLOCK SOURCE SWITCH: %s -> RPC at slot %d | reason=%s | observed_parent_slot=%d | expected_parent_slot=%d | cleared_prefetched_live_stream=%d | rpc_forced_until=%d | rpc_cooldown_until=%d | live_generation=%d | mode=%s",
			bs.liveShredStreamName(), waitingSlot, reason, observedParentSlot, expectedParentSlot, clearedPrefetched, waitingSlot, recoveryUntil, resultGeneration, bs.currentModeString())
		return
	}

	mlog.Log.Warnf("BLOCK SOURCE STATUS: rejecting %s handoff at slot %d | reason=%s | observed_parent_slot=%d | expected_parent_slot=%d | cleared_prefetched_live_stream=%d | rpc_forced_until=%d | rpc_cooldown_until=%d | live_generation=%d | mode=%s",
		bs.liveShredStreamName(), waitingSlot, reason, observedParentSlot, expectedParentSlot, clearedPrefetched, waitingSlot, recoveryUntil, resultGeneration, bs.currentModeString())
}

func (bs *BlockSource) logLiveStreamModeState(mode string, gap uint64) {
	if !bs.usesLiveShredStream() {
		return
	}

	handoffSlot := bs.liveHandoffSlot.Load()
	active := bs.liveStreamActive.Load()
	cooldownUntil := bs.liveCooldownUntil.Load()
	connected := bs.liveStreamConnected.Load()
	lastSlot := bs.liveLastStreamSlot.Load()
	started := bs.liveStreamStarted.Load()

	bs.reorderMu.Lock()
	waitingSlot := bs.nextSlotToSend
	bs.reorderMu.Unlock()

	switch mode {
	case "near-tip":
		if active {
			mlog.Log.Infof("BLOCK SOURCE STATUS: near-tip regained and blocks are already arriving from %s | waiting_slot=%d | handoff_slot=%d | gap=%d",
				bs.liveShredStreamName(), waitingSlot, handoffSlot, gap)
			return
		}
		if cooldownUntil != 0 {
			if connected && lastSlot != 0 {
				mlog.Log.Infof("BLOCK SOURCE STATUS: near-tip regained; staying on RPC until slot %d after a %s gap (latest streamed slot %d) | waiting_slot=%d | gap=%d",
					cooldownUntil, bs.liveShredStreamName(), lastSlot, waitingSlot, gap)
				return
			}
			mlog.Log.Infof("BLOCK SOURCE STATUS: near-tip regained; staying on RPC until slot %d after a %s gap | waiting_slot=%d | gap=%d",
				cooldownUntil, bs.liveShredStreamName(), waitingSlot, gap)
			return
		}
		if handoffSlot != 0 {
			mlog.Log.Infof("BLOCK SOURCE STATUS: near-tip regained; waiting to switch block receipt from RPC to %s at handoff slot %d | waiting_slot=%d | gap=%d",
				bs.liveShredStreamName(), handoffSlot, waitingSlot, gap)
			return
		}
		if connected {
			if lastSlot != 0 {
				mlog.Log.Infof("BLOCK SOURCE STATUS: near-tip regained; %s stream is connected (latest streamed slot %d) and waiting to arm handoff | waiting_slot=%d | gap=%d",
					bs.liveShredStreamName(), lastSlot, waitingSlot, gap)
				return
			}
			mlog.Log.Infof("BLOCK SOURCE STATUS: near-tip regained; %s stream is connected and waiting for its first streamed slot | waiting_slot=%d | gap=%d",
				bs.liveShredStreamName(), waitingSlot, gap)
			return
		}
		if started {
			mlog.Log.Infof("BLOCK SOURCE STATUS: near-tip regained; waiting for %s stream connection before handoff | waiting_slot=%d | gap=%d",
				bs.liveShredStreamName(), waitingSlot, gap)
			return
		}
		mlog.Log.Infof("BLOCK SOURCE STATUS: near-tip regained; preparing to switch block receipt from RPC to %s | waiting_slot=%d | gap=%d",
			bs.liveShredStreamName(), waitingSlot, gap)
	}
}

func (bs *BlockSource) maybeLogReorderGapLocked() {
	waitingSlot := bs.nextSlotToSend
	if len(bs.reorderBuffer) < reorderGapWarnThreshold {
		return
	}
	if bs.reorderBuffer[waitingSlot] != nil || bs.skippedSlots[waitingSlot] {
		return
	}

	now := time.Now()
	lastLogUnix := bs.lastReorderGapLog.Load()
	interval := reorderGapWarnInterval
	if bs.lastReorderGapSlot.Load() == waitingSlot {
		// Same head-of-line slot as the previous warn: the situation is known
		// (and the rescue may already be pulling it) — back off 3x instead of
		// repainting the same message every few seconds.
		interval *= 3
	}
	if lastLogUnix != 0 && now.Sub(time.Unix(lastLogUnix, 0)) < interval {
		return
	}
	bs.lastReorderGapLog.Store(now.Unix())
	bs.lastReorderGapSlot.Store(waitingSlot)

	var minSlot uint64
	var maxSlot uint64
	var liveStreamCount int
	first := true
	for slot, blk := range bs.reorderBuffer {
		if first || slot < minSlot {
			minSlot = slot
		}
		if first || slot > maxSlot {
			maxSlot = slot
		}
		if blk != nil && blk.FromLiveStream {
			liveStreamCount++
		}
		first = false
	}
	if first {
		return
	}

	waitingState := "missing"
	bs.slotStateMu.Lock()
	if state, exists := bs.slotState[waitingSlot]; exists {
		switch state {
		case slotInflight:
			waitingState = "inflight"
		case slotDone:
			waitingState = "done"
		case slotPending:
			waitingState = "pending"
		}
	}
	bs.slotStateMu.Unlock()

	var gapToFirst uint64
	if minSlot > waitingSlot {
		gapToFirst = minSlot - waitingSlot
	}

	firstLightbringerSlot, firstLightbringerParentSlot, _, firstConnectedSlot, firstConnectedParentSlot, foundConnected := bs.inspectLaterLiveBlocksLocked(waitingSlot)

	firstLightbringerDesc := "none"
	if firstLightbringerSlot != 0 {
		firstLightbringerDesc = fmt.Sprintf("%d(parent=%d)", firstLightbringerSlot, firstLightbringerParentSlot)
	}

	firstConnectedDesc := "none"
	if foundConnected {
		firstConnectedDesc = fmt.Sprintf("%d(parent=%d gap_span=%d)", firstConnectedSlot, firstConnectedParentSlot, firstConnectedSlot-waitingSlot)
	}

	mlog.NamedFilef("catchup", "reorder buffer: waiting on missing slot %d | waiting_state=%s | buffered=%d slots (%d from live stream) | buffered_range=%d-%d | gap_to_first_buffered=%d (buffered blocks are the live edge, not the missing range — repair owns the hole) | first_live=%s | first_connected_to_anchor=%s | mode=%s%s",
		waitingSlot, waitingState, len(bs.reorderBuffer), liveStreamCount, minSlot, maxSlot, gapToFirst, firstLightbringerDesc, firstConnectedDesc, bs.currentModeString(), bs.catchupDiagSuffix())
}

func (bs *BlockSource) detectLiveGapLocked() (waitingSlot uint64, firstBufferedSlot uint64, firstBufferedParentSlot uint64, bufferedCount int, shouldFallback bool) {
	if !bs.usesLiveShredStream() {
		return 0, 0, 0, 0, false
	}
	if bs.liveForceRPCUntil.Load() != 0 {
		bs.clearLiveGapWatch()
		return 0, 0, 0, 0, false
	}
	handoffSlot := bs.liveHandoffSlot.Load()
	liveStreamActive := bs.liveStreamActive.Load()
	if handoffSlot == 0 && !liveStreamActive {
		bs.clearLiveGapWatch()
		return 0, 0, 0, 0, false
	}
	waitingSlot = bs.nextSlotToSend
	if !liveStreamActive && handoffSlot != 0 && waitingSlot < handoffSlot {
		// RPC still owns slots before the pending handoff boundary. Buffered
		// Lightbringer blocks beyond that boundary are expected and must not be
		// treated as evidence that the current RPC-owned waiting slot is missing.
		bs.clearLiveGapWatch()
		return 0, 0, 0, 0, false
	}
	if bs.reorderBuffer[waitingSlot] != nil || bs.skippedSlots[waitingSlot] {
		bs.clearLiveGapWatch()
		return 0, 0, 0, 0, false
	}
	if liveStreamActive && bs.isLiveRepairSlot(waitingSlot) {
		bs.clearLiveGapWatch()
		return 0, 0, 0, 0, false
	}

	first := true
	for slot, blk := range bs.reorderBuffer {
		if blk == nil || !blk.FromLiveStream || slot <= waitingSlot {
			continue
		}
		bufferedCount++
		if first || slot < firstBufferedSlot {
			firstBufferedSlot = slot
			firstBufferedParentSlot = blk.SourceParentSlot
			first = false
		}
	}

	if bufferedCount == 0 || first {
		bs.clearLiveGapWatch()
		return 0, 0, 0, 0, false
	}

	now := time.Now()
	gapSlot := bs.lightbringerGapSlot.Load()
	gapSinceUnix := bs.liveGapSinceUnix.Load()
	if gapSlot != waitingSlot || gapSinceUnix == 0 {
		bs.lightbringerGapSlot.Store(waitingSlot)
		bs.liveGapSinceUnix.Store(now.UnixNano())
		bs.liveGapLastLogUnix.Store(0)
		bs.lightbringerGapReconnectSlot.Store(0)
		return waitingSlot, firstBufferedSlot, firstBufferedParentSlot, bufferedCount, false
	}

	if bufferedCount >= liveGapBufferDepth {
		return waitingSlot, firstBufferedSlot, firstBufferedParentSlot, bufferedCount, true
	}
	if now.Sub(time.Unix(0, gapSinceUnix)) < liveGapFallbackWait {
		return waitingSlot, firstBufferedSlot, firstBufferedParentSlot, bufferedCount, false
	}

	return waitingSlot, firstBufferedSlot, firstBufferedParentSlot, bufferedCount, true
}
