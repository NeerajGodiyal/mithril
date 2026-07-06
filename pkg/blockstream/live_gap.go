// live_gap: Live-stream gap detection and healing: a missing slot is repair's job on turbine and a reconnect/RPC question on lightbringer.
package blockstream

import (
	"context"
	"errors"
	"fmt"
	b "github.com/Overclock-Validator/mithril/pkg/block"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (bs *BlockSource) clearLightbringerGapWatch() {
	bs.lightbringerGapSlot.Store(0)
	bs.lightbringerGapSinceUnix.Store(0)
	bs.lightbringerGapLastLogUnix.Store(0)
	bs.lightbringerGapReconnectSlot.Store(0)
}

func (bs *BlockSource) invalidateLightbringerResults() uint64 {
	return bs.lightbringerResultGeneration.Add(1)
}

func (bs *BlockSource) setLightbringerCancel(cancel context.CancelFunc) {
	bs.lightbringerCancelMu.Lock()
	bs.lightbringerCancel = cancel
	bs.lightbringerCancelMu.Unlock()
}

func (bs *BlockSource) clearLightbringerCancel() {
	bs.lightbringerCancelMu.Lock()
	bs.lightbringerCancel = nil
	bs.lightbringerCancelMu.Unlock()
}

func (bs *BlockSource) requestLightbringerReconnect(reason string) bool {
	if !bs.lightbringerConnected.Load() {
		return false
	}
	if !bs.lightbringerReconnectRequested.CompareAndSwap(false, true) {
		return false
	}

	bs.reorderMu.Lock()
	waitingSlot := bs.nextSlotToSend
	lastEmitted := bs.lastEmittedBlockSlot
	bs.reorderMu.Unlock()
	latestStreamed := bs.lightbringerLastStreamSlot.Load()

	action := "stream reconnect requested"
	if bs.sourceType == BlockSourceTurbine {
		// Not a remote stream: this restarts our LOCAL UDP receiver.
		action = "local receiver restart requested"
	}
	mlog.Log.Warnf("%s %s: %s | waiting_slot=%d | last_emitted=%d | latest_streamed=%d",
		bs.liveShredStreamName(), action, reason, waitingSlot, lastEmitted, latestStreamed)

	bs.lightbringerCancelMu.Lock()
	cancel := bs.lightbringerCancel
	bs.lightbringerCancelMu.Unlock()
	if cancel == nil {
		bs.lightbringerReconnectRequested.Store(false)
		return false
	}
	cancel()
	return true
}

func isLightbringerReconnectCancel(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	return status.Code(err) == codes.Canceled
}

func (bs *BlockSource) maybeReconnectActiveLightbringerForNoProgress(stallDuration time.Duration) {
	if !bs.usesLiveShredStream() {
		return
	}
	if !bs.lightbringerActive.Load() || !bs.isNearTip.Load() {
		return
	}
	if stallDuration < lightbringerNoEmitReconnect {
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
		if blk == nil || !blk.FromLightbringer || slot <= waitingSlot {
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

	bs.requestLightbringerReconnect(reason)
}

func (bs *BlockSource) isLightbringerRepairSlot(slot uint64) bool {
	return slot != 0 && bs.lightbringerRepairSlot.Load() == slot
}

func (bs *BlockSource) clearLightbringerRepairSlot(slot uint64) {
	if slot == 0 {
		return
	}
	bs.lightbringerRepairSlot.CompareAndSwap(slot, 0)
}

func (bs *BlockSource) resetLightbringerRepairSlot() {
	bs.lightbringerRepairSlot.Store(0)
}

func (bs *BlockSource) lightbringerBlockConnectsLocked(blk *b.Block) bool {
	if blk == nil || !blk.FromLightbringer {
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

func (bs *BlockSource) shouldPreferIncomingLightbringerBlockLocked(existing, incoming *b.Block) bool {
	if existing == nil || incoming == nil {
		return false
	}
	if !existing.FromLightbringer || !incoming.FromLightbringer {
		return false
	}
	if existing.Slot != incoming.Slot {
		return false
	}
	return !bs.lightbringerBlockConnectsLocked(existing) && bs.lightbringerBlockConnectsLocked(incoming)
}

func (bs *BlockSource) waitingLightbringerParentMismatchLocked() (waitingSlot uint64, observedParentSlot uint64, expectedParentSlot uint64, mismatch bool) {
	blk := bs.reorderBuffer[bs.nextSlotToSend]
	if blk == nil || !blk.FromLightbringer {
		return 0, 0, 0, false
	}
	if bs.lightbringerBlockConnectsLocked(blk) {
		return 0, 0, 0, false
	}
	return blk.Slot, blk.SourceParentSlot, bs.lastEmittedBlockSlot, true
}

func (bs *BlockSource) repairLightbringerGap(waitingSlot, firstBufferedSlot, firstBufferedParentSlot uint64, bufferedCount int) {
	if !bs.usesLiveShredStream() {
		return
	}
	if !bs.lightbringerActive.Load() {
		bs.forceRPCForLightbringerGap(waitingSlot, firstBufferedSlot, firstBufferedParentSlot, bufferedCount)
		return
	}

	gapSinceUnix := bs.lightbringerGapSinceUnix.Load()
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
	lastLog := time.Unix(0, bs.lightbringerGapLastLogUnix.Load())
	if lastLog.IsZero() || now.Sub(lastLog) >= reorderGapWarnInterval {
		bs.lightbringerGapLastLogUnix.Store(now.UnixNano())
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
		bs.requestLightbringerReconnect(reconnectReason)
	}
}

func (bs *BlockSource) forceRPCForLightbringerGap(waitingSlot, firstBufferedSlot, firstBufferedParentSlot uint64, bufferedCount int) {
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

	recoveryUntil := waitingSlot + lightbringerRecoverySlots
	if recoveryUntil < waitingSlot {
		recoveryUntil = ^uint64(0)
	}

	bs.lightbringerForceRPCUntil.Store(waitingSlot)
	bs.lightbringerCooldownUntil.Store(recoveryUntil)
	oldHandoff := bs.lightbringerHandoffSlot.Swap(0)
	wasActive := bs.lightbringerActive.Swap(false)
	resultGeneration := bs.invalidateLightbringerResults()
	bs.lightbringerNeedRPCResume.Store(true)
	bs.clearLightbringerGapWatch()
	bs.resetLightbringerRepairSlot()
	clearedPrefetched := bs.clearBufferedLightbringerBlocks()

	bs.reorderMu.Lock()
	removedSlots := make([]uint64, 0)
	for slot, blk := range bs.reorderBuffer {
		if blk != nil && blk.FromLightbringer && slot >= waitingSlot {
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

func (bs *BlockSource) handleDetectedLightbringerGap(waitingSlot, firstBufferedSlot, firstBufferedParentSlot uint64, bufferedCount int) {
	if bs.lightbringerActive.Load() {
		bs.repairLightbringerGap(waitingSlot, firstBufferedSlot, firstBufferedParentSlot, bufferedCount)
		return
	}
	bs.forceRPCForLightbringerGap(waitingSlot, firstBufferedSlot, firstBufferedParentSlot, bufferedCount)
}

func (bs *BlockSource) forceRPCForLightbringerParentMismatch(waitingSlot, observedParentSlot, expectedParentSlot uint64) {
	if !bs.usesLiveShredStream() {
		return
	}

	recoveryUntil := waitingSlot + lightbringerRecoverySlots
	if recoveryUntil < waitingSlot {
		recoveryUntil = ^uint64(0)
	}

	bs.lightbringerForceRPCUntil.Store(waitingSlot)
	bs.lightbringerCooldownUntil.Store(recoveryUntil)
	oldHandoff := bs.lightbringerHandoffSlot.Swap(0)
	wasActive := bs.lightbringerActive.Swap(false)
	resultGeneration := bs.invalidateLightbringerResults()
	bs.lightbringerNeedRPCResume.Store(true)
	bs.clearLightbringerGapWatch()
	bs.resetLightbringerRepairSlot()
	clearedPrefetched := bs.clearBufferedLightbringerBlocks()

	bs.reorderMu.Lock()
	removedSlots := make([]uint64, 0)
	for slot, blk := range bs.reorderBuffer {
		if blk != nil && blk.FromLightbringer && slot >= waitingSlot {
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

func (bs *BlockSource) logLightbringerModeState(mode string, gap uint64) {
	if !bs.usesLiveShredStream() {
		return
	}

	handoffSlot := bs.lightbringerHandoffSlot.Load()
	active := bs.lightbringerActive.Load()
	cooldownUntil := bs.lightbringerCooldownUntil.Load()
	connected := bs.lightbringerConnected.Load()
	lastSlot := bs.lightbringerLastStreamSlot.Load()
	started := bs.lightbringerStarted.Load()

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
	var lightbringerCount int
	first := true
	for slot, blk := range bs.reorderBuffer {
		if first || slot < minSlot {
			minSlot = slot
		}
		if first || slot > maxSlot {
			maxSlot = slot
		}
		if blk != nil && blk.FromLightbringer {
			lightbringerCount++
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

	firstLightbringerSlot, firstLightbringerParentSlot, _, firstConnectedSlot, firstConnectedParentSlot, foundConnected := bs.inspectLaterLightbringerBlocksLocked(waitingSlot)

	firstLightbringerDesc := "none"
	if firstLightbringerSlot != 0 {
		firstLightbringerDesc = fmt.Sprintf("%d(parent=%d)", firstLightbringerSlot, firstLightbringerParentSlot)
	}

	firstConnectedDesc := "none"
	if foundConnected {
		firstConnectedDesc = fmt.Sprintf("%d(parent=%d gap_span=%d)", firstConnectedSlot, firstConnectedParentSlot, firstConnectedSlot-waitingSlot)
	}

	mlog.NamedFilef("catchup", "reorder buffer: waiting on missing slot %d | waiting_state=%s | buffered=%d slots (%d from live stream) | buffered_range=%d-%d | gap_to_first_buffered=%d (buffered blocks are the live edge, not the missing range — repair owns the hole) | first_live=%s | first_connected_to_anchor=%s | mode=%s%s",
		waitingSlot, waitingState, len(bs.reorderBuffer), lightbringerCount, minSlot, maxSlot, gapToFirst, firstLightbringerDesc, firstConnectedDesc, bs.currentModeString(), bs.catchupDiagSuffix())
}

func (bs *BlockSource) detectLightbringerGapLocked() (waitingSlot uint64, firstBufferedSlot uint64, firstBufferedParentSlot uint64, bufferedCount int, shouldFallback bool) {
	if !bs.usesLiveShredStream() {
		return 0, 0, 0, 0, false
	}
	if bs.lightbringerForceRPCUntil.Load() != 0 {
		bs.clearLightbringerGapWatch()
		return 0, 0, 0, 0, false
	}
	handoffSlot := bs.lightbringerHandoffSlot.Load()
	lightbringerActive := bs.lightbringerActive.Load()
	if handoffSlot == 0 && !lightbringerActive {
		bs.clearLightbringerGapWatch()
		return 0, 0, 0, 0, false
	}
	waitingSlot = bs.nextSlotToSend
	if !lightbringerActive && handoffSlot != 0 && waitingSlot < handoffSlot {
		// RPC still owns slots before the pending handoff boundary. Buffered
		// Lightbringer blocks beyond that boundary are expected and must not be
		// treated as evidence that the current RPC-owned waiting slot is missing.
		bs.clearLightbringerGapWatch()
		return 0, 0, 0, 0, false
	}
	if bs.reorderBuffer[waitingSlot] != nil || bs.skippedSlots[waitingSlot] {
		bs.clearLightbringerGapWatch()
		return 0, 0, 0, 0, false
	}
	if lightbringerActive && bs.isLightbringerRepairSlot(waitingSlot) {
		bs.clearLightbringerGapWatch()
		return 0, 0, 0, 0, false
	}

	first := true
	for slot, blk := range bs.reorderBuffer {
		if blk == nil || !blk.FromLightbringer || slot <= waitingSlot {
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
		bs.clearLightbringerGapWatch()
		return 0, 0, 0, 0, false
	}

	now := time.Now()
	gapSlot := bs.lightbringerGapSlot.Load()
	gapSinceUnix := bs.lightbringerGapSinceUnix.Load()
	if gapSlot != waitingSlot || gapSinceUnix == 0 {
		bs.lightbringerGapSlot.Store(waitingSlot)
		bs.lightbringerGapSinceUnix.Store(now.UnixNano())
		bs.lightbringerGapLastLogUnix.Store(0)
		bs.lightbringerGapReconnectSlot.Store(0)
		return waitingSlot, firstBufferedSlot, firstBufferedParentSlot, bufferedCount, false
	}

	if bufferedCount >= lightbringerGapBufferDepth {
		return waitingSlot, firstBufferedSlot, firstBufferedParentSlot, bufferedCount, true
	}
	if now.Sub(time.Unix(0, gapSinceUnix)) < lightbringerGapFallbackWait {
		return waitingSlot, firstBufferedSlot, firstBufferedParentSlot, bufferedCount, false
	}

	return waitingSlot, firstBufferedSlot, firstBufferedParentSlot, bufferedCount, true
}
