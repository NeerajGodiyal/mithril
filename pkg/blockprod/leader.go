package blockprod

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/alpenglow"
	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/costmodel"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/replay"
	"github.com/Overclock-Validator/mithril/pkg/rewardcerts"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/statsd"
	"github.com/Overclock-Validator/mithril/pkg/turbine"
	"github.com/gagliardetto/solana-go"
)

const (
	// AlpenglowSlotDuration is the community cluster's target slot duration.
	// It also bounds the footer nanosecond clock and ParentReady-derived block
	// deadlines; keeping those clocks identical prevents late/missed slots.
	AlpenglowSlotDuration = 200 * time.Millisecond
	// Leave enough time for the Go finalizer, footer shredding, and UDP fanout
	// to complete before Votor's per-slot deadline. Agave currently reserves
	// 6ms; Mithril's finalizer benefits from a wider margin.
	leaderBlockCompletionReserve = 25 * time.Millisecond
)

var errParentNotReady = errors.New("parent not ready")
var errEpochTransitionProductionUnsupported = errors.New("leader production across an epoch transition is not supported")
var errEpochRewardsProductionUnsupported = errors.New("leader production during partitioned epoch rewards is not supported")

const (
	leaderOutcomeBroadcast = "broadcast"
	leaderOutcomeMissed    = "missed"

	leaderReasonComplete                  = "complete"
	leaderReasonLiveClockAdvanced         = "live_clock_advanced"
	leaderReasonAlreadyResolved           = "already_resolved"
	leaderReasonReplayNotReady            = "replay_not_ready"
	leaderReasonParentReadyNotReady       = "parent_ready_not_ready"
	leaderReasonParentReadyMissedWindow   = "parent_ready_missed_window"
	leaderReasonParentReadyInvalid        = "parent_ready_invalid"
	leaderReasonParentBlockIDMismatch     = "parent_block_id_mismatch"
	leaderReasonParentBlockIDMissing      = "parent_block_id_missing"
	leaderReasonParentChainedRootMissing  = "parent_chained_root_missing"
	leaderReasonParentContextNotReady     = "parent_context_not_ready"
	leaderReasonParentChangedBeforeFreeze = "parent_changed_before_freeze"
	leaderReasonEpochTransition           = "epoch_transition"
	leaderReasonEpochRewards              = "epoch_rewards"
	leaderReasonSlotContextFailed         = "slot_context_failed"
	leaderReasonSysvarPrepareFailed       = "sysvar_prepare_failed"
	leaderReasonHeaderBroadcastFailed     = "header_broadcast_failed"
	leaderReasonEntryBroadcastFailed      = "entry_broadcast_failed"
	leaderReasonFreezeFailed              = "freeze_failed"
	leaderReasonFinalizationUnavailable   = "finalization_unavailable"
	leaderReasonFooterBroadcastFailed     = "footer_broadcast_failed"
	leaderReasonEndingTickBroadcastFailed = "ending_tick_broadcast_failed"
	leaderReasonWindowDeadlineElapsed     = "window_deadline_elapsed"
	leaderReasonPreviousSlotMissed        = "previous_slot_missed"
)

type leaderSlotFailure struct {
	reason string
	detail string
}

// ShredSink shreds forged entry batches and broadcasts them via turbine.
type ShredSink struct {
	session *turbine.BroadcastSession
	mu      sync.Mutex
	err     error
}

func NewShredSink(session *turbine.BroadcastSession) *ShredSink {
	return &ShredSink{session: session}
}

func (s *ShredSink) OnEntryBatch(entries []turbine.Entry, _ int) {
	if s.session == nil || len(entries) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return
	}
	if err := s.session.BroadcastEntryBatch(entries); err != nil {
		s.err = err
		mlog.Log.Warnf("blockprod shred broadcast entry batch failed: %v", err)
	}
}

func (s *ShredSink) Err() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// LeaderLoop activates forge when this validator is the scheduled leader.
// RewardCertBuilder produces skip/notar reward certificates for block footers.
type RewardCertBuilder interface {
	BuildForLeaderSlot(slot uint64) rewardcerts.RewardCertificates
}

type LeaderLoop struct {
	controller       *Controller
	identity         solana.PrivateKey
	accountsDb       *accountsdb.AccountsDb
	broadcaster      turbine.PacketBroadcaster
	shredVersion     uint16
	userAgent        []byte
	rewardCerts      RewardCertBuilder
	epochSchedule    *sealevel.SysvarEpochSchedule
	alpenglowClock   bool
	parentContext    func(uint64) ParentContext
	productionParent func(uint64) alpenglow.BlockProductionParent
	onBlock          func(*b.Block)

	currentSlot   func() uint64
	leaderForSlot func(uint64) (solana.PublicKey, bool)
	parentBlockID func(uint64) (solana.Hash, bool)

	pollInterval time.Duration
	slotDuration time.Duration
	now          func() time.Time

	mu                  sync.Mutex
	activeSlot          uint64
	activeBank          *WorkingBank
	activeSess          *turbine.BroadcastSession
	activeSink          *ShredSink
	parentCtx           ParentContext
	activeParentID      solana.Hash
	finishedLeaderSlots map[uint64]struct{}
	pendingFailures     map[uint64]leaderSlotFailure
	missedScanNext      uint64
	missedScanStarted   bool
	productionWindow    leaderProductionWindow
}

type leaderProductionWindow struct {
	active    bool
	startSlot uint64
	endSlot   uint64
	nextSlot  uint64
	readyAt   time.Time
}

type LeaderLoopConfig struct {
	Controller       *Controller
	Identity         solana.PrivateKey
	AccountsDb       *accountsdb.AccountsDb
	Broadcaster      turbine.PacketBroadcaster
	ShredVersion     uint16
	UserAgent        []byte
	EpochSchedule    *sealevel.SysvarEpochSchedule
	AlpenglowClock   bool
	ParentContext    func(uint64) ParentContext
	ProductionParent func(slot uint64) alpenglow.BlockProductionParent
	OnBlock          func(*b.Block)
	CurrentSlot      func() uint64
	LeaderForSlot    func(uint64) (solana.PublicKey, bool)
	ParentBlockID    func(parentSlot uint64) (solana.Hash, bool)
	RewardCerts      RewardCertBuilder
	PollInterval     time.Duration
	SlotDuration     time.Duration
	Now              func() time.Time
}

func NewLeaderLoop(cfg LeaderLoopConfig) *LeaderLoop {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Millisecond
	}
	if cfg.UserAgent == nil {
		cfg.UserAgent = []byte("mithril")
	}
	if cfg.SlotDuration <= 0 {
		cfg.SlotDuration = AlpenglowSlotDuration
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &LeaderLoop{
		controller:          cfg.Controller,
		identity:            cfg.Identity,
		accountsDb:          cfg.AccountsDb,
		broadcaster:         cfg.Broadcaster,
		shredVersion:        cfg.ShredVersion,
		userAgent:           cfg.UserAgent,
		epochSchedule:       cfg.EpochSchedule,
		alpenglowClock:      cfg.AlpenglowClock,
		parentContext:       cfg.ParentContext,
		productionParent:    cfg.ProductionParent,
		onBlock:             cfg.OnBlock,
		currentSlot:         cfg.CurrentSlot,
		leaderForSlot:       cfg.LeaderForSlot,
		parentBlockID:       cfg.ParentBlockID,
		rewardCerts:         cfg.RewardCerts,
		pollInterval:        cfg.PollInterval,
		slotDuration:        cfg.SlotDuration,
		now:                 cfg.Now,
		finishedLeaderSlots: make(map[uint64]struct{}),
		pendingFailures:     make(map[uint64]leaderSlotFailure),
	}
}

func (l *LeaderLoop) Run(stop <-chan struct{}) {
	ticker := time.NewTicker(l.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			l.finishActiveSlot()
			return
		case <-ticker.C:
			l.tick()
		}
	}
}

func (l *LeaderLoop) tick() {
	if l.currentSlot == nil || l.leaderForSlot == nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	for {
		wallSlot := l.currentSlot()
		now := l.now()

		if l.activeBank != nil {
			if canonical, reason := l.activeParentStillCanonicalLocked(); !canonical {
				l.rememberLeaderSlotFailureLocked(l.activeSlot, reason, activeLeaderFailureDetail(reason))
				l.abortActiveSlotLocked()
				if reason == leaderReasonAlreadyResolved || reason == leaderReasonParentReadyMissedWindow {
					l.failProductionWindowLocked(l.productionWindow.nextSlot, reason, activeLeaderFailureDetail(reason))
				} else if reason == leaderReasonParentChangedBeforeFreeze && l.productionWindow.active {
					// Agave resets the window timer on a sad leader handover to a
					// newly selected ParentReady parent.
					parent := l.productionParent(l.productionWindow.startSlot)
					if parent.Kind == alpenglow.BlockProductionParentReady && !parent.ReadyAt.IsZero() {
						l.productionWindow.readyAt = parent.ReadyAt
					}
				}
				continue
			}
			if l.productionWindow.active {
				if now.Before(l.productionWindowDeadlineLocked(l.activeSlot)) {
					return
				}
			} else if wallSlot <= l.activeSlot {
				return
			}
			l.finishActiveSlotLocked()
			continue
		}

		l.reportExpiredLeaderSlotsLocked(wallSlot)

		// The Votor path below may continue an observed four-slot window even when
		// the coarse live-slot estimate jumps; its exact ParentReady deadline is the
		// authority. The legacy path still targets only the current wall slot and
		// never backfills historical production.
		targetSlot := wallSlot
		if l.productionParent != nil {
			var ok bool
			targetSlot, ok = l.productionWindowTargetLocked(wallSlot, now)
			if !ok {
				return
			}
		}
		leader, ok := l.leaderForSlot(targetSlot)
		if !ok || leader != l.identity.PublicKey() {
			return
		}
		if l.isLeaderSlotFinishedLocked(targetSlot) {
			return
		}

		if ready, err := l.leaderSlotReplayReady(targetSlot); !ready {
			detail := "leader replay gate was not ready"
			if err != nil {
				detail = err.Error()
			}
			l.rememberLeaderSlotFailureLocked(targetSlot, leaderFailureReason(err), detail)
			return
		}

		if err := l.startSlotLocked(targetSlot); err != nil {
			if errors.Is(err, errEpochTransitionProductionUnsupported) || errors.Is(err, errEpochRewardsProductionUnsupported) {
				l.failProductionWindowLocked(targetSlot, leaderFailureReason(err), err.Error())
				continue
			}
			l.rememberLeaderSlotFailureLocked(targetSlot, leaderFailureReason(err), err.Error())
			return
		}
		delete(l.pendingFailures, targetSlot)

		return
	}
}

func (l *LeaderLoop) productionWindowTargetLocked(wallSlot uint64, now time.Time) (uint64, bool) {
	if l.productionWindow.active {
		target := l.productionWindow.nextSlot
		if target > l.productionWindow.endSlot {
			l.productionWindow = leaderProductionWindow{}
			return 0, false
		}
		if !now.Before(l.productionWindowDeadlineLocked(target)) {
			failure, ok := l.pendingFailures[target]
			if !ok {
				failure = leaderSlotFailure{
					reason: leaderReasonWindowDeadlineElapsed,
					detail: "ParentReady-based block deadline elapsed before production started",
				}
			} else {
				failure.detail = fmt.Sprintf("%s; ParentReady-based block deadline elapsed", failure.detail)
			}
			l.failProductionWindowLocked(target, failure.reason, failure.detail)
			return 0, false
		}
		return target, true
	}

	startSlot := wallSlot - wallSlot%alpenglow.LeaderWindowSlots
	leader, ok := l.leaderForSlot(startSlot)
	if !ok || leader != l.identity.PublicKey() || l.isLeaderSlotFinishedLocked(startSlot) {
		return 0, false
	}
	parent := l.productionParent(startSlot)
	switch parent.Kind {
	case alpenglow.BlockProductionParentReady:
		readyAt := parent.ReadyAt
		if readyAt.IsZero() {
			l.rememberLeaderSlotFailureLocked(startSlot, leaderReasonParentReadyNotReady, "verified ParentReady has no live timer origin")
			return 0, false
		}
		l.productionWindow = leaderProductionWindow{
			active:    true,
			startSlot: startSlot,
			endSlot:   startSlot + alpenglow.LeaderWindowSlots - 1,
			nextSlot:  startSlot,
			readyAt:   readyAt,
		}
		if !now.Before(l.productionWindowDeadlineLocked(startSlot)) {
			l.failProductionWindowLocked(startSlot, leaderReasonWindowDeadlineElapsed, "ParentReady arrived too late to complete the first block")
			return 0, false
		}
		return startSlot, true
	case alpenglow.BlockProductionParentMissedWindow:
		l.productionWindow = leaderProductionWindow{
			active:    true,
			startSlot: startSlot,
			endSlot:   startSlot + alpenglow.LeaderWindowSlots - 1,
			nextSlot:  startSlot,
		}
		l.failProductionWindowLocked(startSlot, leaderReasonParentReadyMissedWindow, "Votor certified a later window before local production started")
		return 0, false
	default:
		l.rememberLeaderSlotFailureLocked(startSlot, leaderReasonParentReadyNotReady, "waiting for verified ParentReady")
		return 0, false
	}
}

func (l *LeaderLoop) productionWindowDeadlineLocked(slot uint64) time.Time {
	if !l.productionWindow.active || slot < l.productionWindow.startSlot {
		return time.Time{}
	}
	index := slot - l.productionWindow.startSlot + 1
	deadline := l.productionWindow.readyAt.Add(time.Duration(index) * l.slotDuration)
	reserve := leaderBlockCompletionReserve
	if reserve >= l.slotDuration {
		reserve = l.slotDuration / 4
	}
	return deadline.Add(-reserve)
}

func (l *LeaderLoop) failProductionWindowLocked(slot uint64, reason, detail string) {
	if !l.productionWindow.active {
		l.recordLeaderSlotOutcomeLocked(slot, leaderOutcomeMissed, reason, detail)
		return
	}
	end := l.productionWindow.endSlot
	l.recordLeaderSlotOutcomeLocked(slot, leaderOutcomeMissed, reason, detail)
	for remaining := slot + 1; remaining <= end; remaining++ {
		l.recordLeaderSlotOutcomeLocked(remaining, leaderOutcomeMissed, leaderReasonPreviousSlotMissed,
			fmt.Sprintf("cannot chain local slot %d after missed slot %d", remaining, slot))
	}
	l.productionWindow = leaderProductionWindow{}
}

func activeLeaderFailureDetail(reason string) string {
	switch reason {
	case leaderReasonAlreadyResolved:
		return "ordered replay resolved the local leader slot before its block froze"
	case leaderReasonReplayNotReady:
		return "ordered replay no longer covers the selected production parent"
	case leaderReasonParentReadyNotReady:
		return "verified ParentReady became unavailable before the block froze"
	case leaderReasonParentReadyMissedWindow:
		return "Votor moved ParentReady beyond the local leader window before the block froze"
	default:
		return "selected replay parent changed before the block froze"
	}
}

func (l *LeaderLoop) activeParentStillCanonicalLocked() (bool, string) {
	if l.activeBank == nil || l.parentContext == nil || l.activeSlot == 0 {
		return true, ""
	}
	frontier := global.ReplayFrontier()
	// A certified skip (or any other accepted outcome) may resolve our slot
	// while we are still producing it. Stop before emitting a footer in that
	// case; the ordered replay outcome is authoritative.
	if frontier >= l.activeSlot {
		return false, leaderReasonAlreadyResolved
	}

	requiredFrontier := l.activeSlot - 1
	if l.activeSlot%alpenglow.LeaderWindowSlots == 0 && l.productionParent != nil {
		parent := l.productionParent(l.activeSlot)
		switch parent.Kind {
		case alpenglow.BlockProductionParentReady:
			if parent.Parent.Slot != l.parentCtx.ParentSlot || parent.Parent.Hash != l.activeParentID {
				return false, leaderReasonParentChangedBeforeFreeze
			}
			requiredFrontier = parent.Parent.Slot
		case alpenglow.BlockProductionParentMissedWindow:
			// Agave abandons the working bank once highest ParentReady moves beyond
			// its window: the cluster has already selected a later outcome.
			return false, leaderReasonParentReadyMissedWindow
		default:
			return false, leaderReasonParentReadyNotReady
		}
	}
	if frontier < requiredFrontier {
		return false, leaderReasonReplayNotReady
	}
	current := l.parentContext(l.activeSlot)
	if current.ParentSlot != l.parentCtx.ParentSlot || current.ParentBankhash != l.parentCtx.ParentBankhash {
		return false, leaderReasonParentChangedBeforeFreeze
	}
	return true, ""
}

func (l *LeaderLoop) abortActiveSlotLocked() {
	if l.controller != nil {
		l.controller.ClearWorkingBank()
	}
	if l.activeBank != nil {
		l.activeBank.Close()
	}
	l.activeBank = nil
	l.activeSess = nil
	l.activeSink = nil
	l.activeSlot = 0
	l.activeParentID = solana.Hash{}
}

func leaderFailureReason(err error) string {
	if err == nil {
		return leaderReasonLiveClockAdvanced
	}
	if errors.Is(err, errEpochTransitionProductionUnsupported) {
		return leaderReasonEpochTransition
	}
	if errors.Is(err, errEpochRewardsProductionUnsupported) {
		return leaderReasonEpochRewards
	}
	detail := err.Error()
	switch {
	case strings.Contains(detail, "already resolved leader slot"):
		return leaderReasonAlreadyResolved
	case strings.Contains(detail, "ParentReady arrived after"):
		return leaderReasonParentReadyMissedWindow
	case strings.Contains(detail, "no verified ParentReady"):
		return leaderReasonParentReadyNotReady
	case strings.Contains(detail, "invalid ParentReady"):
		return leaderReasonParentReadyInvalid
	case strings.Contains(detail, "does not match ParentReady block id"):
		return leaderReasonParentBlockIDMismatch
	case strings.Contains(detail, "block id missing"), strings.Contains(detail, "no parent block id lookup"):
		return leaderReasonParentBlockIDMissing
	case strings.Contains(detail, "chained merkle root missing"):
		return leaderReasonParentChainedRootMissing
	case strings.Contains(detail, "parent bankhash missing"), strings.Contains(detail, "replay parent slot"):
		return leaderReasonParentContextNotReady
	case strings.Contains(detail, "ordered replay"):
		return leaderReasonReplayNotReady
	case strings.Contains(detail, "new leader slot ctx"):
		return leaderReasonSlotContextFailed
	case strings.Contains(detail, "prepare leader sysvars"):
		return leaderReasonSysvarPrepareFailed
	case strings.Contains(detail, "broadcast header"):
		return leaderReasonHeaderBroadcastFailed
	default:
		return leaderReasonParentContextNotReady
	}
}

func (l *LeaderLoop) rememberLeaderSlotFailureLocked(slot uint64, reason, detail string) {
	if slot == 0 || l.isLeaderSlotFinishedLocked(slot) {
		return
	}
	if l.pendingFailures == nil {
		l.pendingFailures = make(map[uint64]leaderSlotFailure)
	}
	l.pendingFailures[slot] = leaderSlotFailure{reason: reason, detail: detail}
}

// reportExpiredLeaderSlotsLocked records one terminal outcome for every local
// leader slot observed while the loop was running. The first sample establishes
// the live watermark so a process restart does not report historical slots it
// never had an opportunity to produce.
func (l *LeaderLoop) reportExpiredLeaderSlotsLocked(wallSlot uint64) {
	if !l.missedScanStarted {
		l.missedScanStarted = true
		l.missedScanNext = wallSlot
		return
	}
	if wallSlot < l.missedScanNext {
		l.missedScanNext = wallSlot
		return
	}
	for l.missedScanNext < wallSlot {
		slot := l.missedScanNext
		// A ParentReady-timed window owns all four of its outcomes even when the
		// network live-slot estimate jumps ahead. Leave the scanner parked at the
		// window until production either completes it or the next window proves it
		// expired.
		if l.productionParent != nil {
			currentWindowStart := wallSlot - wallSlot%alpenglow.LeaderWindowSlots
			if slot >= currentWindowStart && slot < currentWindowStart+alpenglow.LeaderWindowSlots {
				return
			}
			if l.productionWindow.active && slot >= l.productionWindow.startSlot && slot <= l.productionWindow.endSlot {
				return
			}
		}
		l.missedScanNext++
		if l.isLeaderSlotFinishedLocked(slot) {
			delete(l.pendingFailures, slot)
			continue
		}
		failure, attempted := l.pendingFailures[slot]
		if !attempted {
			leader, ok := l.leaderForSlot(slot)
			if !ok || leader != l.identity.PublicKey() {
				continue
			}
			failure = leaderSlotFailure{
				reason: leaderReasonLiveClockAdvanced,
				detail: "live clock advanced before block production started",
			}
		}
		l.recordLeaderSlotOutcomeLocked(slot, leaderOutcomeMissed, failure.reason, failure.detail)
	}
}

func (l *LeaderLoop) recordLeaderSlotOutcomeLocked(slot uint64, outcome, reason, detail string) {
	if slot == 0 || l.isLeaderSlotFinishedLocked(slot) {
		return
	}
	if reason == "" {
		reason = leaderReasonParentContextNotReady
	}
	_ = statsd.Count(statsd.BlockProductionLeaderSlots, 1, []string{outcome, reason})
	liveSlot := uint64(0)
	if l.currentSlot != nil {
		liveSlot = l.currentSlot()
	}
	if outcome == leaderOutcomeBroadcast {
		mlog.Log.Infof("ALPENGLOW block production: broadcast local leader slot=%d reason=%s replay_frontier=%d live_slot=%d %s", slot, reason, global.ReplayFrontier(), liveSlot, detail)
	} else {
		mlog.Log.Warnf("ALPENGLOW block production: missed local leader slot=%d reason=%s replay_frontier=%d live_slot=%d detail=%s", slot, reason, global.ReplayFrontier(), liveSlot, detail)
	}
	l.markLeaderSlotFinished(slot)
	delete(l.pendingFailures, slot)
}

func (l *LeaderLoop) isLeaderSlotFinishedLocked(slot uint64) bool {
	_, ok := l.finishedLeaderSlots[slot]
	return ok
}

func (l *LeaderLoop) isLeaderSlotFinished(slot uint64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.isLeaderSlotFinishedLocked(slot)
}

func (l *LeaderLoop) markLeaderSlotFinished(slot uint64) {
	if slot == 0 {
		return
	}
	if l.finishedLeaderSlots == nil {
		l.finishedLeaderSlots = make(map[uint64]struct{})
	}
	l.finishedLeaderSlots[slot] = struct{}{}
	for finished := range l.finishedLeaderSlots {
		if finished < slot && slot-finished > 256 {
			delete(l.finishedLeaderSlots, finished)
		}
	}
}

func (l *LeaderLoop) finishActiveSlot() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.finishActiveSlotLocked()
}

// clampProducerTimeNanos clamps the leader's wall-clock footer timestamp into the
// Alpenglow nanosecond-clock bounds derived from the parent bank, matching Agave's
// block_creation_loop::skew_block_producer_time_nanos. Without this, a leader that
// just caught up stamps real "now", which is typically >2*slot_duration past the
// parent clock and is rejected by the network as NanosecondClockOutOfBounds.
func (l *LeaderLoop) clampProducerTimeNanos(slot uint64, nowNanos int64) uint64 {
	if !l.alpenglowClock || l.accountsDb == nil || l.activeBank == nil {
		return uint64(nowNanos)
	}
	slotCtx := l.activeBank.SlotCtx()
	if slotCtx == nil {
		return uint64(nowNanos)
	}
	parentSlot := slotCtx.ParentSlot
	parentNanos, ok := replay.ReadNanosecondClockFromSlotCtx(slotCtx)
	if !ok {
		return uint64(nowNanos)
	}
	var elapsed uint64
	if slot > parentSlot {
		elapsed = (slot - parentSlot) * uint64(l.slotDuration.Nanoseconds())
	}
	clamped := replay.SkewBlockProducerTimeNanos(parentNanos, nowNanos, elapsed)
	if clamped < 0 {
		clamped = 0
	}
	return uint64(clamped)
}

func (l *LeaderLoop) finishActiveSlotLocked() {
	if l.activeBank == nil {
		return
	}
	slot := l.activeSlot

	// Detach first so no new TPU packet can obtain this bank. Freeze then waits
	// for any packet that already did, closing the footer/entry race.
	if l.controller != nil {
		l.controller.ClearWorkingBank()
	}
	l.activeBank.Freeze()
	if l.activeSink == nil {
		l.failProductionWindowLocked(slot, leaderReasonFinalizationUnavailable, "entry broadcast sink is unavailable")
		l.abortActiveSlotLocked()
		return
	}
	if err := l.activeSink.Err(); err != nil {
		l.failProductionWindowLocked(slot, leaderReasonEntryBroadcastFailed, err.Error())
		l.abortActiveSlotLocked()
		return
	}

	pohTip := l.activeBank.EntryHash()
	tickHash := turbine.AlpentickHash(pohTip)
	producerTimeNanos := l.clampProducerTimeNanos(slot, time.Now().UnixNano())
	footerTimestamp := int64(producerTimeNanos / 1_000_000_000)

	var footerRewards rewardcerts.RewardCertificates
	if l.rewardCerts != nil {
		footerRewards = l.rewardCerts.BuildForLeaderSlot(slot)
	}

	if l.accountsDb == nil || l.epochSchedule == nil || l.activeSess == nil {
		l.failProductionWindowLocked(slot, leaderReasonFinalizationUnavailable, "accounts DB, epoch schedule, or broadcast session is unavailable")
		l.abortActiveSlotLocked()
		return
	}

	producedBlock := BuildLeaderBlock(LeaderBlockInput{
		Bank:             l.activeBank,
		EpochSchedule:    l.epochSchedule,
		ParentSlot:       l.parentCtx.ParentSlot,
		ParentBankhash:   l.parentCtx.ParentBankhash,
		PrevNumSigs:      l.parentCtx.PrevNumSigs,
		PrevFeeGovernor:  l.parentCtx.PrevFeeGovernor,
		EntryBlockhash:   tickHash,
		TxFeeAccumulator: l.activeBank.TxFeeAccumulator(),
	})
	producedBlock.SkipRewardCert = append([]byte(nil), footerRewards.Skip...)
	producedBlock.NotarRewardCert = append([]byte(nil), footerRewards.Notar...)
	producedBlock.FooterProducerTimeNanos = producerTimeNanos
	producedBlock.HasAlpenglowFooter = true
	producedBlock.AlpenglowShredVersion = l.shredVersion
	if slot > 0 {
		if parentID, ok := global.AlpenglowBlockID(producedBlock.ParentSlot); ok {
			producedBlock.AlpenglowParentBlockID = parentID
			producedBlock.HasAlpenglowParentBlockID = true
		}
	}

	if _, err := replay.CommitLeaderSlot(replay.CommitLeaderInput{
		AcctsDb:                 l.accountsDb,
		SlotCtx:                 l.activeBank.SlotCtx(),
		Block:                   producedBlock,
		EpochSchedule:           l.epochSchedule,
		TxFeeAccumulator:        l.activeBank.TxFeeAccumulator(),
		AlpenglowClock:          l.alpenglowClock,
		AlpenglowShredVersion:   l.shredVersion,
		FooterTimestamp:         footerTimestamp,
		FooterProducerTimeNanos: producerTimeNanos,
	}); err != nil {
		l.failProductionWindowLocked(slot, leaderReasonFreezeFailed, err.Error())
		l.abortActiveSlotLocked()
		return
	}

	footerBankhash := solana.HashFromBytes(l.activeBank.SlotCtx().FinalBankhash)
	if err := l.activeSess.BroadcastFooter(footerBankhash, producerTimeNanos, footerRewards.Skip, footerRewards.Notar); err != nil {
		l.failProductionWindowLocked(slot, leaderReasonFooterBroadcastFailed, err.Error())
		l.abortActiveSlotLocked()
		return
	}
	if err := l.activeSess.BroadcastEndingTickLast(tickHash); err != nil {
		l.failProductionWindowLocked(slot, leaderReasonEndingTickBroadcastFailed, err.Error())
		l.abortActiveSlotLocked()
		return
	}
	if slot > 0 {
		parentSlot := producedBlock.ParentSlot
		if parentID, ok := global.AlpenglowBlockID(parentSlot); ok {
			blockID := l.activeSess.BlockID(parentSlot, parentID)
			producedBlock.AlpenglowBlockID = blockID
			producedBlock.HasAlpenglowBlockID = true
			global.SetAlpenglowBlockID(slot, blockID)
		}
	}
	if chained := l.activeSess.ChainedMerkleRoot(); chained != (solana.Hash{}) {
		producedBlock.AlpenglowLastChainedRoot = chained
		producedBlock.HasAlpenglowLastChainedRoot = true
		global.SetAlpenglowChainedMerkleRoot(slot, chained)
	}
	if l.onBlock != nil {
		l.onBlock(producedBlock)
	}
	timingDetail := ""
	if l.productionWindow.active && slot >= l.productionWindow.startSlot {
		index := slot - l.productionWindow.startSlot + 1
		protocolDeadline := l.productionWindow.readyAt.Add(time.Duration(index) * l.slotDuration)
		completedAt := l.now()
		timingDetail = fmt.Sprintf(" window_elapsed_ms=%d deadline_margin_ms=%d",
			completedAt.Sub(l.productionWindow.readyAt).Milliseconds(), protocolDeadline.Sub(completedAt).Milliseconds())
	}
	l.recordLeaderSlotOutcomeLocked(slot, leaderOutcomeBroadcast, leaderReasonComplete,
		fmt.Sprintf("block=%s parent_slot=%d txns=%d%s", solana.Hash(producedBlock.AlpenglowBlockID).String(), producedBlock.ParentSlot, len(producedBlock.Transactions), timingDetail))
	if l.productionWindow.active && l.productionWindow.nextSlot == slot {
		l.productionWindow.nextSlot++
		if l.productionWindow.nextSlot > l.productionWindow.endSlot {
			l.productionWindow = leaderProductionWindow{}
		}
	}
	l.abortActiveSlotLocked()
}

func (l *LeaderLoop) startSlotLocked(slot uint64) error {
	selectedParent, parentReadyRequired, err := l.resolveProductionParent(slot)
	if err != nil {
		return err
	}
	parentCtx := ParentContext{}
	if l.parentContext != nil {
		parentCtx = l.parentContext(slot)
	}
	parentSlot := parentCtx.ParentSlot
	if slot == 0 {
		parentSlot = 0
	}
	if ready, err := l.leaderSlotReplayReady(slot); !ready {
		return err
	}
	if parentReadyRequired && parentCtx.ParentSlot != selectedParent.Slot {
		return fmt.Errorf("%w: replay parent slot %d does not match ParentReady slot %d", errParentNotReady, parentCtx.ParentSlot, selectedParent.Slot)
	}
	if slot > 0 && parentCtx.ParentBankhash == (solana.Hash{}) {
		return fmt.Errorf("%w: parent bankhash missing for slot %d", errParentNotReady, parentSlot)
	}
	if slot > 0 && l.epochSchedule != nil && l.epochSchedule.GetEpoch(parentSlot) != l.epochSchedule.GetEpoch(slot) {
		return fmt.Errorf("%w: parent block is slot %d", errEpochTransitionProductionUnsupported, parentSlot)
	}
	if parentCtx.EpochRewardsActive {
		return errEpochRewardsProductionUnsupported
	}
	l.parentCtx = parentCtx

	parentID := solana.Hash{}
	if l.parentBlockID != nil {
		var ok bool
		parentID, ok = l.parentBlockID(parentSlot)
		if !ok {
			return fmt.Errorf("%w: alpenglow block id missing for parent slot %d", errParentNotReady, parentSlot)
		}
	} else if slot > 0 {
		return fmt.Errorf("%w: no parent block id lookup configured", errParentNotReady)
	}
	if parentReadyRequired && parentID != selectedParent.Hash {
		return fmt.Errorf("%w: replay parent block id %s does not match ParentReady block id %s", errParentNotReady, parentID, selectedParent.Hash)
	}

	parentChainedRoot := solana.Hash{}
	if slot > 0 {
		var ok bool
		parentChainedRoot, ok = global.AlpenglowChainedMerkleRoot(parentSlot)
		if !ok {
			return fmt.Errorf("%w: chained merkle root missing for parent slot %d", errParentNotReady, parentSlot)
		}
	}

	session := turbine.NewBroadcastSession(turbine.BroadcastSessionConfig{
		Leader:                  l.identity,
		Slot:                    slot,
		ParentSlot:              parentSlot,
		ParentBlockID:           parentID,
		ParentChainedMerkleRoot: parentChainedRoot,
		Version:                 l.shredVersion,
		Broadcaster:             l.broadcaster,
		UserAgent:               l.userAgent,
	})
	slotCtx, err := NewLeaderSlotCtx(slot, parentSlot, l.accountsDb, parentCtx, l.epochSchedule)
	if err != nil {
		return fmt.Errorf("new leader slot ctx: %w", err)
	}
	if l.accountsDb != nil && l.epochSchedule != nil {
		prepBlock := &b.Block{
			Slot:           slot,
			ParentSlot:     parentSlot,
			Epoch:          l.epochSchedule.GetEpoch(slot),
			ParentBankhash: parentCtx.ParentBankhash,
		}
		if err := replay.PrepareLeaderSlotSysvars(slotCtx, prepBlock, l.epochSchedule, l.alpenglowClock); err != nil {
			return fmt.Errorf("prepare leader sysvars: %w", err)
		}
	}
	startEntryHash := parentCtx.ParentLastEntryHash
	if startEntryHash == (solana.Hash{}) {
		startEntryHash = parentCtx.ParentBankhash
	}
	sink := NewShredSink(session)
	bank := NewWorkingBank(BankConfig{
		SlotCtx:   slotCtx,
		Slot:      slot,
		Leader:    l.identity.PublicKey(),
		Limits:    costmodel.DefaultLimits(),
		EntryHash: startEntryHash,
		Sink:      sink,
	})
	// Publish the working bank only after all local preparation and the header
	// broadcast succeed; failures before this point cannot admit TPU traffic.
	if err := session.BroadcastHeader(parentID); err != nil {
		bank.Close()
		return fmt.Errorf("broadcast header parent_block_id=%s: %w", parentID, err)
	}
	l.activeSlot = slot
	l.activeBank = bank
	l.activeSess = session
	l.activeSink = sink
	l.activeParentID = parentID
	if l.controller != nil {
		l.controller.SetWorkingBank(bank)
	}
	return nil
}
