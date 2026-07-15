package blockprod

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	b "github.com/Overclock-Validator/mithril/pkg/block"
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
	// AlpenglowSlotDuration is the community cluster's target slot duration.
	// It also bounds the footer nanosecond clock and drives wall-clock leader
	// activation; keeping those clocks identical prevents late/missed slots.
	AlpenglowSlotDuration = 200 * time.Millisecond
)

var errParentNotReady = errors.New("parent not ready")
var errEpochTransitionProductionUnsupported = errors.New("leader production across an epoch transition is not supported")
var errEpochRewardsProductionUnsupported = errors.New("leader production during partitioned epoch rewards is not supported")

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
	controller     *Controller
	identity       solana.PrivateKey
	accountsDb     *accountsdb.AccountsDb
	broadcaster    turbine.PacketBroadcaster
	shredVersion   uint16
	userAgent      []byte
	rewardCerts    RewardCertBuilder
	epochSchedule  *sealevel.SysvarEpochSchedule
	alpenglowClock bool
	parentContext  func(uint64) ParentContext
	onBlock        func(*b.Block)

	currentSlot   func() uint64
	leaderForSlot func(uint64) (solana.PublicKey, bool)
	parentBlockID func(uint64) (solana.Hash, bool)

	pollInterval time.Duration
	slotDuration time.Duration

	mu                  sync.Mutex
	activeSlot          uint64
	activeBank          *WorkingBank
	activeSess          *turbine.BroadcastSession
	activeSink          *ShredSink
	parentCtx           ParentContext
	finishedLeaderSlots map[uint64]struct{}
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
	OnBlock        func(*b.Block)
	CurrentSlot    func() uint64
	LeaderForSlot  func(uint64) (solana.PublicKey, bool)
	ParentBlockID  func(parentSlot uint64) (solana.Hash, bool)
	RewardCerts    RewardCertBuilder
	PollInterval   time.Duration
	SlotDuration   time.Duration
}

func NewLeaderLoop(cfg LeaderLoopConfig) *LeaderLoop {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 50 * time.Millisecond
	}
	if cfg.UserAgent == nil {
		cfg.UserAgent = []byte("mithril")
	}
	if cfg.SlotDuration <= 0 {
		cfg.SlotDuration = AlpenglowSlotDuration
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
		onBlock:             cfg.OnBlock,
		currentSlot:         cfg.CurrentSlot,
		leaderForSlot:       cfg.LeaderForSlot,
		parentBlockID:       cfg.ParentBlockID,
		rewardCerts:         cfg.RewardCerts,
		pollInterval:        cfg.PollInterval,
		slotDuration:        cfg.SlotDuration,
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
			if !l.activeParentStillCanonicalLocked() {
				mlog.Log.Warnf("leader slot %d aborted: replay parent changed before freeze", l.activeSlot)
				l.abortActiveSlotLocked()
				continue
			}
			if wallSlot <= l.activeSlot {
				return
			}
			l.finishActiveSlotLocked()
			continue
		}

		// Never backfill a historical leader slot. A block produced after its wall-
		// clock window can conflict with the already-propagating network outcome.
		// If replay does not make the current slot's parent ready in time, this
		// validator safely misses that slot and tries again at its next live turn.
		targetSlot := wallSlot
		leader, ok := l.leaderForSlot(targetSlot)
		if !ok || leader != l.identity.PublicKey() {
			return
		}
		if l.isLeaderSlotFinishedLocked(targetSlot) {
			return
		}

		if ready, _ := l.leaderSlotReplayReady(targetSlot); !ready {
			return
		}

		if err := l.startSlotLocked(targetSlot); err != nil {
			if errors.Is(err, errEpochTransitionProductionUnsupported) || errors.Is(err, errEpochRewardsProductionUnsupported) {
				mlog.Log.Warnf("leader slot %d intentionally skipped: %v", targetSlot, err)
				l.markLeaderSlotFinished(targetSlot)
				continue
			}
			return
		}

		return
	}
}

func (l *LeaderLoop) activeParentStillCanonicalLocked() bool {
	if l.activeBank == nil || l.parentContext == nil || l.activeSlot == 0 {
		return true
	}
	frontier := global.ReplayFrontier()
	// A certified skip (or any other accepted outcome) may resolve our slot
	// while we are still producing it. Stop before emitting a footer in that
	// case; the ordered replay outcome is authoritative.
	if frontier >= l.activeSlot {
		return false
	}
	if frontier < l.activeSlot-1 {
		return false
	}
	current := l.parentContext(l.activeSlot)
	return current.ParentSlot == l.parentCtx.ParentSlot &&
		current.ParentBankhash == l.parentCtx.ParentBankhash
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
	l.finishedLeaderSlots[slot] = struct{}{}
	for finished := range l.finishedLeaderSlots {
		if finished+256 < slot {
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
	if err := l.activeSink.Err(); err != nil {
		mlog.Log.Warnf("leader slot %d abandoned after entry broadcast failure: %v", slot, err)
		l.markLeaderSlotFinished(slot)
		l.activeBank = nil
		l.activeSess = nil
		l.activeSink = nil
		l.activeSlot = 0
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

	var producedBlock *b.Block
	if l.accountsDb != nil && l.epochSchedule != nil {
		block := BuildLeaderBlock(LeaderBlockInput{
			Bank:             l.activeBank,
			EpochSchedule:    l.epochSchedule,
			ParentSlot:       l.parentCtx.ParentSlot,
			ParentBankhash:   l.parentCtx.ParentBankhash,
			PrevNumSigs:      l.parentCtx.PrevNumSigs,
			PrevFeeGovernor:  l.parentCtx.PrevFeeGovernor,
			EntryBlockhash:   tickHash,
			TxFeeAccumulator: l.activeBank.TxFeeAccumulator(),
		})
		block.SkipRewardCert = append([]byte(nil), footerRewards.Skip...)
		block.NotarRewardCert = append([]byte(nil), footerRewards.Notar...)
		block.FooterProducerTimeNanos = producerTimeNanos
		block.HasAlpenglowFooter = true
		block.AlpenglowShredVersion = l.shredVersion
		if slot > 0 {
			if parentID, ok := global.AlpenglowBlockID(block.ParentSlot); ok {
				block.AlpenglowParentBlockID = parentID
				block.HasAlpenglowParentBlockID = true
			}
		}

		if _, err := replay.CommitLeaderSlot(replay.CommitLeaderInput{
			AcctsDb:                 l.accountsDb,
			SlotCtx:                 l.activeBank.SlotCtx(),
			Block:                   block,
			EpochSchedule:           l.epochSchedule,
			TxFeeAccumulator:        l.activeBank.TxFeeAccumulator(),
			AlpenglowClock:          l.alpenglowClock,
			AlpenglowShredVersion:   l.shredVersion,
			FooterTimestamp:         footerTimestamp,
			FooterProducerTimeNanos: producerTimeNanos,
		}); err != nil {
			mlog.Log.Errorf("leader slot %d freeze failed; leaving the partial slot incomplete: %v", slot, err)
		} else {
			producedBlock = block
		}
	}

	if l.activeSess != nil && producedBlock != nil {
		footerBankhash := solana.HashFromBytes(l.activeBank.SlotCtx().FinalBankhash)
		if err := l.activeSess.BroadcastFooter(footerBankhash, producerTimeNanos, footerRewards.Skip, footerRewards.Notar); err != nil {
			mlog.Log.Warnf("leader slot %d footer broadcast failed: %v", slot, err)
		} else if err := l.activeSess.BroadcastEndingTickLast(tickHash); err != nil {
			mlog.Log.Warnf("leader slot %d ending tick broadcast failed: %v", slot, err)
		} else {
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
		}
	}

	l.markLeaderSlotFinished(slot)

	if l.controller != nil {
		l.controller.ClearWorkingBank()
	}
	l.activeBank = nil
	l.activeSess = nil
	l.activeSink = nil
	l.activeSlot = 0
}

func (l *LeaderLoop) startSlotLocked(slot uint64) error {
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
	if l.controller != nil {
		l.controller.SetWorkingBank(bank)
	}
	return nil
}
