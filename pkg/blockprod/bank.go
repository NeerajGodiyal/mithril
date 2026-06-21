package blockprod

import (
	"sync"

	"github.com/Overclock-Validator/mithril/pkg/costmodel"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/replay"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/turbine"
	"github.com/gagliardetto/solana-go"
)

// BatchSink receives flushed entry batches for shred/broadcast.
type BatchSink interface {
	OnEntryBatch(entries []turbine.Entry, batchBytes int)
}

// NopBatchSink discards flushed entry batches.
type NopBatchSink struct{}

func (NopBatchSink) OnEntryBatch([]turbine.Entry, int) {}

// WorkingBank is the leader's mutable slot state used by the forge sink.
type WorkingBank struct {
	mu sync.Mutex

	slotCtx *sealevel.SlotCtx
	slot    uint64
	leader  solana.PublicKey

	costs   *costmodel.CostTracker
	entries *EntryBuilder
	sink    BatchSink
}

type BankConfig struct {
	SlotCtx   *sealevel.SlotCtx
	Slot      uint64
	Leader    solana.PublicKey
	Limits    costmodel.Limits
	EntryHash solana.Hash
	Sink      BatchSink
}

func NewWorkingBank(cfg BankConfig) *WorkingBank {
	limits := cfg.Limits
	if limits.BlockCost == 0 {
		limits = costmodel.DefaultLimits()
	}
	sink := cfg.Sink
	if sink == nil {
		sink = NopBatchSink{}
	}
	return &WorkingBank{
		slotCtx: cfg.SlotCtx,
		slot:    cfg.Slot,
		leader:  cfg.Leader,
		costs:   costmodel.NewCostTracker(limits),
		entries: NewEntryBuilder(limits, cfg.EntryHash),
		sink:    sink,
	}
}

func (b *WorkingBank) Slot() uint64 {
	return b.slot
}

func (b *WorkingBank) Leader() solana.PublicKey {
	return b.leader
}

func (b *WorkingBank) SlotCtx() *sealevel.SlotCtx {
	return b.slotCtx
}

func (b *WorkingBank) CostTracker() *costmodel.CostTracker {
	return b.costs
}

func (b *WorkingBank) EntryBuilder() *EntryBuilder {
	return b.entries
}

// ForgeResult reports whether a transaction was committed into the working bank.
type ForgeResult int

const (
	ForgeAccepted ForgeResult = iota
	ForgeDroppedNoLeader
	ForgeDroppedVote
	ForgeDroppedParse
	ForgeDroppedCost
	ForgeDroppedExecution
)

func (r ForgeResult) String() string {
	switch r {
	case ForgeAccepted:
		return "accepted"
	case ForgeDroppedNoLeader:
		return "dropped_no_leader"
	case ForgeDroppedVote:
		return "dropped_vote"
	case ForgeDroppedParse:
		return "dropped_parse"
	case ForgeDroppedCost:
		return "dropped_cost"
	case ForgeDroppedExecution:
		return "dropped_execution"
	default:
		return "unknown"
	}
}

// Forge executes and commits a verified transaction wire into the working bank.
func (b *WorkingBank) Forge(wire []byte) (ForgeResult, costmodel.ExceedReason) {
	tx, err := solana.TransactionFromBytes(wire)
	if err != nil {
		return ForgeDroppedParse, costmodel.ExceedNone
	}
	return b.ForgeTransaction(tx, len(wire))
}

// ForgeTransaction executes and commits a parsed transaction.
func (b *WorkingBank) ForgeTransaction(tx *solana.Transaction, wireSize int) (ForgeResult, costmodel.ExceedReason) {
	if tx == nil {
		return ForgeDroppedParse, costmodel.ExceedNone
	}
	if tx.IsVote() {
		return ForgeDroppedVote, costmodel.ExceedNone
	}

	feats := b.slotCtx.Features
	if feats == nil {
		f := features.NewFeaturesDefault()
		feats = f
	}
	cost, err := costmodel.EstimateTransactionCost(tx, feats)
	if err != nil {
		return ForgeDroppedParse, costmodel.ExceedNone
	}
	cost.WireSize = wireSize

	b.mu.Lock()
	defer b.mu.Unlock()

	if reason := b.costs.WouldExceed(cost); reason != costmodel.ExceedNone {
		return ForgeDroppedCost, reason
	}

	output := replay.LoadAndExecuteTransaction(replay.LoadAndExecuteTransactionInput{
		SlotCtx:     b.slotCtx,
		Transaction: tx,
	})
	if output.ProcessingResult.TransactionError != nil {
		return ForgeDroppedExecution, costmodel.ExceedNone
	}
	if err := replay.ApplySuccessfulTransaction(b.slotCtx, output); err != nil {
		return ForgeDroppedExecution, costmodel.ExceedNone
	}

	b.costs.Record(cost)
	if flushed, batchBytes, didFlush := b.entries.Append(*tx, wireSize); didFlush {
		b.sink.OnEntryBatch(flushed, batchBytes)
	}
	return ForgeAccepted, costmodel.ExceedNone
}

// FlushEntries emits any pending transactions as an entry batch.
func (b *WorkingBank) FlushEntries() {
	b.mu.Lock()
	defer b.mu.Unlock()
	entries, batchBytes := b.entries.Flush()
	if len(entries) == 0 {
		return
	}
	b.sink.OnEntryBatch(entries, batchBytes)
}
