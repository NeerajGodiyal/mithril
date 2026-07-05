package blockstream

// TODO(cavey-debug): DELETE this file once turbine near-tip ingest is stable.
// cavey TODO: remove once we are done debugging.

import (
	"fmt"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/turbine"
)

const (
	turbineStuckIngestThreshold   = 2 * time.Second
	turbineStuckIngestLogInterval = 5 * time.Second
)

func (bs *BlockSource) setTurbineReceiverStatsSnapshot(stats turbine.ReceiverStats) {
	bs.turbineStatsSnapMu.Lock()
	bs.turbineStatsSnap = stats
	bs.turbineStatsSnapValid = true
	bs.turbineStatsSnapMu.Unlock()
}

func (bs *BlockSource) turbineReceiverStatsSnapshot() (turbine.ReceiverStats, bool) {
	bs.turbineStatsSnapMu.Lock()
	defer bs.turbineStatsSnapMu.Unlock()
	return bs.turbineStatsSnap, bs.turbineStatsSnapValid
}

func (bs *BlockSource) resetTurbineStuckIngestWatch() {
	bs.turbineStuckSinceUnix.Store(0)
}

func (bs *BlockSource) countBufferedTurbineSlotsAtOrBeyond(waitingSlot uint64) int {
	bs.lightbringerBufferMu.Lock()
	defer bs.lightbringerBufferMu.Unlock()
	count := 0
	for slot := range bs.lightbringerBuffer {
		if slot >= waitingSlot {
			count++
		}
	}
	return count
}

// maybeLogStuckTurbineIngest emits INFO-level turbine/repair ingest diagnostics when
// near-tip replay is connected to the turbine receiver but no blocks have been assembled
// for longer than turbineStuckIngestThreshold. Phase 0 instrumentation only.
func (bs *BlockSource) maybeLogStuckTurbineIngest() {
	if bs.sourceType != BlockSourceTurbine {
		return
	}
	if !bs.lightbringerConnected.Load() || !bs.isNearTip.Load() {
		bs.resetTurbineStuckIngestWatch()
		return
	}
	if bs.lightbringerActive.Load() && !bs.turbineRepairOnlyWaitingForFirstBlock() {
		bs.resetTurbineStuckIngestWatch()
		return
	}

	lastStreamed := bs.lightbringerLastStreamSlot.Load()
	if lastStreamed != 0 {
		bs.resetTurbineStuckIngestWatch()
		return
	}

	now := time.Now()
	stuckSinceUnix := bs.turbineStuckSinceUnix.Load()
	if stuckSinceUnix == 0 {
		bs.turbineStuckSinceUnix.Store(now.Unix())
		return
	}
	stuckFor := now.Sub(time.Unix(stuckSinceUnix, 0))
	if stuckFor < turbineStuckIngestThreshold {
		return
	}

	lastLogUnix := bs.turbineStuckLogAt.Load()
	if lastLogUnix != 0 && now.Sub(time.Unix(lastLogUnix, 0)) < turbineStuckIngestLogInterval {
		return
	}
	bs.turbineStuckLogAt.Store(now.Unix())

	bs.reorderMu.Lock()
	waitingSlot := bs.nextSlotToSend
	bs.reorderMu.Unlock()

	lastExecuted := bs.lastExecutedSlot.Load()
	confirmedTip := bs.confirmedTip.Load()
	var gap uint64
	if confirmedTip > lastExecuted {
		gap = confirmedTip - lastExecuted
	}

	buffered := bs.countBufferedTurbineSlotsAtOrBeyond(waitingSlot)
	handoffSlot := bs.lightbringerHandoffSlot.Load()

	stats, haveStats := bs.turbineReceiverStatsSnapshot()
	statsLine := "receiver_stats=unavailable"
	if haveStats {
		lastPacketAge := "never"
		if stats.LastPacketUnix != 0 {
			lastPacketAge = time.Since(time.Unix(stats.LastPacketUnix, 0)).Round(time.Millisecond).String()
		}
		statsLine = fmt.Sprintf(
			"packets=%d data_shreds=%d coding_shreds=%d recovered=%d blocks_emitted=%d active_slots=%d priority_repair_slots=%d ignored_old_shreds=%d parse_errors=%d sig_errors=%d missing_leaders=%d assembly_errors=%d non_canonical_block_ids=%d last_packet_age=%s last_data_slot=%d last_block_slot=%d repair_peers=%d repair_requests=%d repair_responses=%d repair_timeouts=%d repair_outstanding=%d repair_pings=%d repair_pongs=%d repair_errors=%d",
			stats.Packets, stats.DataShreds, stats.CodingShreds, stats.RecoveredData, stats.BlocksEmitted,
			stats.ActiveSlots, stats.PriorityRepairSlots, stats.IgnoredOldShreds,
			stats.ParseErrors, stats.SignatureErrors, stats.MissingLeaders, stats.AssemblyErrors,
			stats.NonCanonicalBlockIDs, lastPacketAge, stats.LastDataSlot, stats.LastBlockSlot,
			stats.Repair.Peers, stats.Repair.Requests, stats.Repair.Responses, stats.Repair.Timeouts,
			stats.Repair.Outstanding, stats.Repair.Pings, stats.Repair.Pongs, stats.Repair.Errors,
		)
	}

	sharedGossip := bs.gossipClient != nil
	mlog.Log.Infof(
		"cavey debug: turbine ingest STUCK near-tip for %s | waiting_slot=%d exec=%d confirmed_tip=%d gap=%d handoff_slot=%d buffered_turbine_slots=%d latest_streamed=%d shared_gossip=%t alpenglow_hints=%t stuck_for=%s | %s",
		bs.liveShredStreamName(), waitingSlot, lastExecuted, confirmedTip, gap, handoffSlot, buffered,
		lastStreamed, sharedGossip, bs.turbineAlpenglowBlockIDHints, stuckFor.Round(time.Millisecond), statsLine,
	)
}
