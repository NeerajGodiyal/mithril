package blockprod

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/costmodel"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/replay"
	"github.com/Overclock-Validator/mithril/pkg/rewardcerts"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/turbine"
	"github.com/gagliardetto/solana-go"
)

const (
	slotDuration = 400 * time.Millisecond
	// pocLeaderLookahead caps how far ahead of replay we scan the leader schedule.
	// POC policy still requires replay to finish leaderSlot-1 (see pocLeaderSlotReplayReady)
	// before start; this only bounds schedule scanning cost.
	pocLeaderLookahead = 128
)

var errParentNotReady = errors.New("parent not ready")

// ShredSink shreds forged entry batches and broadcasts them via turbine.
type ShredSink struct {
	session *turbine.BroadcastSession
}

func NewShredSink(session *turbine.BroadcastSession) *ShredSink {
	return &ShredSink{session: session}
}

func (s *ShredSink) OnEntryBatch(entries []turbine.Entry, _ int) {
	if s.session == nil || len(entries) == 0 {
		return
	}
	if err := s.session.BroadcastEntryBatch(entries); err != nil {
		// TODO(cavey-debug): remove after debugging — revert to silent ignore or use mlog.Log.Warnf.
		caveyDebugf("blockprod shred broadcast entry batch failed: %v", err)
	}
}

// LeaderLoop activates forge when this validator is the scheduled leader.
// RewardCertBuilder produces skip/notar reward certificates for block footers.
type RewardCertBuilder interface {
	BuildForLeaderSlot(slot uint64) rewardcerts.RewardCertificates
}

type LeaderLoop struct {
	controller   *Controller
	identity     solana.PrivateKey
	accountsDb   *accountsdb.AccountsDb
	broadcaster  turbine.PacketBroadcaster
	shredVersion uint16
	userAgent    []byte
	rewardCerts  RewardCertBuilder
	epochSchedule *sealevel.SysvarEpochSchedule
	alpenglowClock bool
	parentContext  func(uint64) ParentContext

	currentSlot    func() uint64
	leaderForSlot  func(uint64) (solana.PublicKey, bool)
	// TODO(cavey-debug): remove NextLeaderSlot field after debugging.
	nextLeaderSlot func(fromSlot uint64) (uint64, bool)
	parentBlockID  func(uint64) (solana.Hash, bool)
	bankHash       func(*WorkingBank) solana.Hash
	// TODO(cavey-debug): remove ForgeCounters field after debugging.
	forgeCounters func() ForgeCounters
	// TODO(cavey-debug): remove TVUPeerCount field after debugging.
	tvuPeerCount func() int

	pollInterval time.Duration

	mu         sync.Mutex
	activeSlot uint64
	activeBank *WorkingBank
	activeSess *turbine.BroadcastSession
	parentCtx  ParentContext
	// TODO(cavey-debug): remove lastNotLeaderLogSlot and lastActiveLogAt after debugging.
	lastNotLeaderLogSlot uint64
	lastActiveLogAt      time.Time
	lastWaitingLogSlot   uint64
	finishedLeaderSlots  map[uint64]struct{}
}

type LeaderLoopConfig struct {
	Controller     *Controller
	Identity       solana.PrivateKey
	AccountsDb     *accountsdb.AccountsDb
	Broadcaster    turbine.PacketBroadcaster
	ShredVersion   uint16
	UserAgent      []byte
	EpochSchedule  *sealevel.SysvarEpochSchedule
	AlpenglowClock bool
	ParentContext  func(uint64) ParentContext
	CurrentSlot    func() uint64
	LeaderForSlot func(uint64) (solana.PublicKey, bool)
	// TODO(cavey-debug): remove NextLeaderSlot config field after debugging.
	NextLeaderSlot func(fromSlot uint64) (uint64, bool)
	ParentBlockID  func(slot uint64) (solana.Hash, bool)
	BankHash       func(*WorkingBank) solana.Hash
	RewardCerts    RewardCertBuilder
	// TODO(cavey-debug): remove ForgeCounters config field after debugging.
	ForgeCounters func() ForgeCounters
	// TODO(cavey-debug): remove TVUPeerCount config field after debugging.
	TVUPeerCount func() int
	PollInterval  time.Duration
}

func NewLeaderLoop(cfg LeaderLoopConfig) *LeaderLoop {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 50 * time.Millisecond
	}
	if cfg.UserAgent == nil {
		cfg.UserAgent = []byte("mithril")
	}
	return &LeaderLoop{
		controller:     cfg.Controller,
		identity:       cfg.Identity,
		accountsDb:     cfg.AccountsDb,
		broadcaster:    cfg.Broadcaster,
		shredVersion:   cfg.ShredVersion,
		userAgent:      cfg.UserAgent,
		epochSchedule:  cfg.EpochSchedule,
		alpenglowClock: cfg.AlpenglowClock,
		parentContext:  cfg.ParentContext,
		currentSlot:    cfg.CurrentSlot,
		leaderForSlot:  cfg.LeaderForSlot,
		nextLeaderSlot: cfg.NextLeaderSlot,
		parentBlockID:  cfg.ParentBlockID,
		bankHash:       cfg.BankHash,
		rewardCerts:    cfg.RewardCerts,
		forgeCounters:  cfg.ForgeCounters,
		tvuPeerCount:   cfg.TVUPeerCount,
		pollInterval:        cfg.PollInterval,
		finishedLeaderSlots: make(map[uint64]struct{}),
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

		if l.activeBank != nil {
			if wallSlot <= l.activeSlot {
				l.maybeLogActiveLeaderLocked(l.activeSlot)
				return
			}
			l.finishActiveSlotLocked()
			continue
		}

		targetSlot, hasPending := l.nextPendingLeaderSlotLocked()
		if !hasPending {
			leader, ok := l.leaderForSlot(wallSlot)
			if !ok || leader != l.identity.PublicKey() {
				return
			}
			if l.isLeaderSlotFinished(wallSlot) {
				return
			}
			targetSlot = wallSlot
		}
		if l.isLeaderSlotFinished(targetSlot) {
			return
		}

		if ready, err := l.pocLeaderSlotReplayReady(targetSlot); !ready {
			l.maybeLogWaitingForParentLocked(targetSlot, err)
			return
		}

		if err := l.startSlotLocked(targetSlot); err != nil {
			caveyDebugf("blockprod leader start failed slot=%d replay_slot=%d:%s %v",
				targetSlot, global.Slot(), formatTVUPeersSuffix(l.tvuPeerCount), err)
			return
		}

		if wallSlot <= l.activeSlot {
			return
		}
	}
}

func (l *LeaderLoop) isLeaderSlotFinished(slot uint64) bool {
	_, ok := l.finishedLeaderSlots[slot]
	return ok
}

func (l *LeaderLoop) markLeaderSlotFinished(slot uint64) {
	if slot == 0 {
		return
	}
	l.finishedLeaderSlots[slot] = struct{}{}
	for finished := range l.finishedLeaderSlots {
		if finished+256 < slot {
			delete(l.finishedLeaderSlots, finished)
		}
	}
}

// nextPendingLeaderSlotLocked returns the earliest own-leader slot at or after
// replay+1 that passes the POC replay gate (see pocLeaderSlotReplayReady).
func (l *LeaderLoop) nextPendingLeaderSlotLocked() (uint64, bool) {
	replaySlot := global.Slot()
	wallSlot := l.currentSlot()
	start := replaySlot + 1
	maxSlot := wallSlot
	if maxSlot > replaySlot+pocLeaderLookahead {
		maxSlot = replaySlot + pocLeaderLookahead
	}
	for slot := start; slot <= maxSlot; slot++ {
		if l.isLeaderSlotFinished(slot) {
			continue
		}
		leader, ok := l.leaderForSlot(slot)
		if !ok || leader != l.identity.PublicKey() {
			continue
		}
		if ready, _ := l.pocLeaderSlotReplayReady(slot); !ready {
			continue
		}
		return slot, true
	}
	return 0, false
}

func (l *LeaderLoop) maybeLogWaitingForParentLocked(slot uint64, err error) {
	if l.lastWaitingLogSlot == slot {
		return
	}
	l.lastWaitingLogSlot = slot
	// TODO(cavey-debug): remove after debugging.
	caveyDebugf("blockprod leader waiting slot=%d replay_slot=%d:%s %v",
		slot, global.Slot(), formatTVUPeersSuffix(l.tvuPeerCount), err)
}

// TODO(cavey-debug): remove maybeLogActiveLeaderLocked after debugging.
func (l *LeaderLoop) maybeLogActiveLeaderLocked(slot uint64) {
	if time.Since(l.lastActiveLogAt) < time.Second {
		return
	}
	l.lastActiveLogAt = time.Now()
	forgeInfo := ""
	if l.forgeCounters != nil {
		forgeInfo = " " + formatForgeCounters(l.forgeCounters())
	}
	caveyDebugf("blockprod leader in progress slot=%d replay_slot=%d txs=%d sigs=%d%s",
		slot, global.Slot(), len(l.activeBank.ForgedTransactions()), l.activeBank.NumSignatures(), forgeInfo)
}

// TODO(cavey-debug): remove forgeCountersSnapshot after debugging.
func (l *LeaderLoop) forgeCountersSnapshot() ForgeCounters {
	if l.forgeCounters != nil {
		return l.forgeCounters()
	}
	return ForgeCounters{}
}

func (l *LeaderLoop) finishActiveSlot() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.finishActiveSlotLocked()
}

func (l *LeaderLoop) finishActiveSlotLocked() {
	if l.activeBank == nil {
		return
	}
	slot := l.activeSlot
	txCount := len(l.activeBank.ForgedTransactions())
	sigCount := l.activeBank.NumSignatures()
	// TODO(cavey-debug): remove forgeSnap and leader-finished log after debugging.
	forgeSnap := l.forgeCountersSnapshot()

	l.activeBank.FlushEntries()

	pohTip := l.activeBank.EntryHash()
	tickHash := turbine.AlpentickHash(pohTip)
	producerTimeNanos := uint64(time.Now().UnixNano())
	footerTimestamp := int64(producerTimeNanos / 1_000_000_000)

	var footerRewards rewardcerts.RewardCertificates
	if l.rewardCerts != nil {
		footerRewards = l.rewardCerts.BuildForLeaderSlot(slot)
	}

	commitStatus := "skipped_no_accountsdb_or_epoch_schedule"
	if l.accountsDb != nil && l.epochSchedule != nil {
		block := BuildLeaderBlock(LeaderBlockInput{
			Bank:             l.activeBank,
			EpochSchedule:    l.epochSchedule,
			ParentBankhash:   l.parentCtx.ParentBankhash,
			PrevNumSigs:      l.parentCtx.PrevNumSigs,
			PrevFeeGovernor:  l.parentCtx.PrevFeeGovernor,
			EntryBlockhash:   tickHash,
			TxFeeAccumulator: l.activeBank.TxFeeAccumulator(),
		})
		block.SkipRewardCert = append([]byte(nil), footerRewards.Skip...)
		block.NotarRewardCert = append([]byte(nil), footerRewards.Notar...)
		block.FooterProducerTimeNanos = producerTimeNanos

		if _, err := replay.CommitLeaderSlot(replay.CommitLeaderInput{
			AcctsDb:                 l.accountsDb,
			SlotCtx:                 l.activeBank.SlotCtx(),
			Block:                   block,
			EpochSchedule:           l.epochSchedule,
			TxFeeAccumulator:        l.activeBank.TxFeeAccumulator(),
			AlpenglowClock:          l.alpenglowClock,
			FooterTimestamp:         footerTimestamp,
			FooterProducerTimeNanos: producerTimeNanos,
		}); err != nil {
			commitStatus = fmt.Sprintf("failed:%v", err)
			mlog.Log.Errorf("leader slot %d commit failed: %v", slot, err)
		} else {
			commitStatus = "ok"
		}
	} else if l.accountsDb == nil {
		commitStatus = "skipped_accountsdb_nil"
	} else if l.epochSchedule == nil {
		commitStatus = "skipped_epoch_schedule_nil"
	}

	tickStatus := "ok"
	footerStatus := "ok"
	bankHash := l.bankHash(l.activeBank)
	var skipCertLen, notarCertLen int
	rewardSlot := uint64(0)
	if rs, ok := rewardcerts.RewardSlotForLeader(slot); ok {
		rewardSlot = rs
	}
	if l.activeSess != nil {
		skipCertLen = len(footerRewards.Skip)
		notarCertLen = len(footerRewards.Notar)
		if err := l.activeSess.BroadcastFooter(bankHash, producerTimeNanos, footerRewards.Skip, footerRewards.Notar); err != nil {
			footerStatus = fmt.Sprintf("failed:%v", err)
			// TODO(cavey-debug): remove after debugging — keep footerStatus in leader-finished log or revert.
			caveyDebugf("blockprod footer broadcast failed slot=%d: %v", slot, err)
		}
		if err := l.activeSess.BroadcastEndingTickLast(tickHash); err != nil {
			tickStatus = fmt.Sprintf("failed:%v", err)
			// TODO(cavey-debug): remove after debugging — keep tickStatus in leader-finished log or revert.
			caveyDebugf("blockprod ending tick broadcast failed slot=%d: %v", slot, err)
		} else if slot > 0 {
			parentSlot := slot - 1
			if parentID, ok := global.AlpenglowBlockID(parentSlot); ok {
				global.SetAlpenglowBlockID(slot, l.activeSess.BlockID(parentSlot, parentID))
			}
			if chained := l.activeSess.ChainedMerkleRoot(); chained != (solana.Hash{}) {
				global.SetAlpenglowChainedMerkleRoot(slot, chained)
			}
		}
	} else {
		tickStatus = "skipped_no_session"
		footerStatus = "skipped_no_session"
	}

	// TODO(cavey-debug): remove leader-finished log after debugging.
	caveyDebugf("blockprod leader finished slot=%d txs=%d sigs=%d commit=%s tick=%s footer=%s bank_hash=%s footer_reward_slot=%d skip_cert_bytes=%d notar_cert_bytes=%d %s",
		slot, txCount, sigCount, commitStatus, tickStatus, footerStatus, bankHash, rewardSlot, skipCertLen, notarCertLen, FormatForgeCounters(forgeSnap))

	l.markLeaderSlotFinished(slot)

	l.controller.ClearWorkingBank()
	l.activeBank = nil
	l.activeSess = nil
	l.activeSlot = 0
}

func (l *LeaderLoop) startSlotLocked(slot uint64) error {
	parentSlot := slot
	if slot > 0 {
		parentSlot = slot - 1
	}

	parentCtx := ParentContext{}
	if l.parentContext != nil {
		parentCtx = l.parentContext(slot)
	}
	if ready, err := l.pocLeaderSlotReplayReady(slot); !ready {
		return err
	}
	if slot > 0 && parentCtx.ParentBankhash == (solana.Hash{}) {
		return fmt.Errorf("%w: parent bankhash missing for slot %d", errParentNotReady, parentSlot)
	}
	l.parentCtx = parentCtx

	parentID := solana.Hash{}
	if l.parentBlockID != nil {
		var ok bool
		parentID, ok = l.parentBlockID(slot)
		if !ok {
			return fmt.Errorf("%w: alpenglow block id missing for parent slot %d", errParentNotReady, parentSlot)
		}
	} else if slot > 0 {
		return fmt.Errorf("%w: no parent block id lookup configured", errParentNotReady)
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
	if err := session.BroadcastHeader(parentID); err != nil {
		return fmt.Errorf("broadcast header parent_block_id=%s: %w", parentID, err)
	}

	slotCtx, err := NewLeaderSlotCtx(slot, parentSlot, l.accountsDb, parentCtx)
	if err != nil {
		return fmt.Errorf("new leader slot ctx: %w", err)
	}
	startEntryHash := parentCtx.ParentLastEntryHash
	if startEntryHash == (solana.Hash{}) {
		startEntryHash = parentCtx.ParentBankhash
	}
	bank := NewWorkingBank(BankConfig{
		SlotCtx:   slotCtx,
		Slot:      slot,
		Leader:    l.identity.PublicKey(),
		Limits:    costmodel.DefaultLimits(),
		EntryHash: startEntryHash,
		Sink:      NewShredSink(session),
	})
	l.activeSlot = slot
	l.activeBank = bank
	l.activeSess = session
	// TODO(cavey-debug): remove lastActiveLogAt reset after debugging.
	l.lastActiveLogAt = time.Time{}
	l.lastWaitingLogSlot = 0
	l.controller.SetWorkingBank(bank)

	parentBH := "missing"
	if parentCtx.ParentBankhash != (solana.Hash{}) {
		parentBH = parentCtx.ParentBankhash.String()
	}
	// TODO(cavey-debug): remove leader-active log after debugging.
	caveyDebugf("blockprod leader active slot=%d replay_slot=%d identity=%s parent_slot=%d parent_bankhash=%s parent_block_id=%s parent_chained_root=%s shred_version=%d accountsdb=%t epoch_schedule=%t",
		slot, global.Slot(), l.identity.PublicKey(), parentSlot, parentBH, parentID, parentChainedRoot, l.shredVersion,
		l.accountsDb != nil, l.epochSchedule != nil)
	return nil
}

// TODO(cavey-debug): remove formatNextLeaderInfo and formatLeaderETA after debugging.
func formatNextLeaderInfo(nextLeaderSlot func(fromSlot uint64) (uint64, bool), currentSlot uint64) string {
	if nextLeaderSlot == nil {
		return ""
	}
	next, ok := nextLeaderSlot(currentSlot)
	if !ok {
		return " next_leader_slot=none"
	}
	slotsUntil := next - currentSlot
	return fmt.Sprintf(" next_leader_slot=%d slots_until=%d eta=%s", next, slotsUntil, formatLeaderETA(slotsUntil))
}

func formatLeaderETA(slotsUntil uint64) string {
	d := time.Duration(slotsUntil) * slotDuration
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}
