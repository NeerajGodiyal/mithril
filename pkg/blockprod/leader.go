package blockprod

import (
	"sync"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/costmodel"
	"github.com/Overclock-Validator/mithril/pkg/turbine"
	"github.com/gagliardetto/solana-go"
)

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
	_ = s.session.BroadcastEntryBatch(entries)
}

// LeaderLoop activates forge when this validator is the scheduled leader.
type LeaderLoop struct {
	controller   *Controller
	identity     solana.PrivateKey
	accountsDb   *accountsdb.AccountsDb
	broadcaster  turbine.PacketBroadcaster
	shredVersion uint16
	userAgent    []byte

	currentSlot   func() uint64
	leaderForSlot func(uint64) (solana.PublicKey, bool)
	parentBlockID func(uint64) solana.Hash
	bankHash      func(*WorkingBank) solana.Hash

	pollInterval time.Duration

	mu          sync.Mutex
	activeSlot  uint64
	activeBank  *WorkingBank
	activeSess  *turbine.BroadcastSession
}

type LeaderLoopConfig struct {
	Controller    *Controller
	Identity      solana.PrivateKey
	AccountsDb    *accountsdb.AccountsDb
	Broadcaster   turbine.PacketBroadcaster
	ShredVersion  uint16
	UserAgent     []byte
	CurrentSlot   func() uint64
	LeaderForSlot func(uint64) (solana.PublicKey, bool)
	ParentBlockID func(slot uint64) solana.Hash
	BankHash      func(bank *WorkingBank) solana.Hash
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
		controller:    cfg.Controller,
		identity:      cfg.Identity,
		accountsDb:    cfg.AccountsDb,
		broadcaster:   cfg.Broadcaster,
		shredVersion:  cfg.ShredVersion,
		userAgent:     cfg.UserAgent,
		currentSlot:   cfg.CurrentSlot,
		leaderForSlot: cfg.LeaderForSlot,
		parentBlockID: cfg.ParentBlockID,
		bankHash:      cfg.BankHash,
		pollInterval:  cfg.PollInterval,
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
	slot := l.currentSlot()
	leader, ok := l.leaderForSlot(slot)
	if !ok || leader != l.identity.PublicKey() {
		l.finishActiveSlot()
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.activeBank != nil && l.activeSlot == slot {
		return
	}
	if l.activeBank != nil {
		l.finishActiveSlotLocked()
	}
	l.startSlotLocked(slot)
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
	l.activeBank.FlushEntries()
	if l.activeSess != nil {
		tickHash := solana.Hash{0xfe}
		_ = l.activeSess.BroadcastEndingTick(tickHash)
		var bankHash solana.Hash
		if l.bankHash != nil {
			bankHash = l.bankHash(l.activeBank)
		}
		_ = l.activeSess.BroadcastFooter(bankHash, uint64(time.Now().UnixNano()))
	}
	l.controller.ClearWorkingBank()
	l.activeBank = nil
	l.activeSess = nil
	l.activeSlot = 0
}

func (l *LeaderLoop) startSlotLocked(slot uint64) {
	parentSlot := slot
	if slot > 0 {
		parentSlot = slot - 1
	}
	parentID := solana.Hash{}
	if l.parentBlockID != nil {
		parentID = l.parentBlockID(slot)
	}

	session := turbine.NewBroadcastSession(turbine.BroadcastSessionConfig{
		Leader:      l.identity,
		Slot:        slot,
		ParentSlot:  parentSlot,
		Version:     l.shredVersion,
		Broadcaster: l.broadcaster,
		UserAgent:   l.userAgent,
	})
	_ = session.BroadcastHeader(parentID)

	slotCtx, err := NewLeaderSlotCtx(slot, parentSlot, l.accountsDb)
	if err != nil {
		return
	}
	bank := NewWorkingBank(BankConfig{
		SlotCtx:   slotCtx,
		Slot:      slot,
		Leader:    l.identity.PublicKey(),
		Limits:    costmodel.DefaultLimits(),
		EntryHash: solana.Hash{0xab},
		Sink:      NewShredSink(session),
	})
	l.activeSlot = slot
	l.activeBank = bank
	l.activeSess = session
	l.controller.SetWorkingBank(bank)
}
