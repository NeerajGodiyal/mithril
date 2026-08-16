package blockprod

import (
	"sync"

	"github.com/Overclock-Validator/mithril/pkg/costmodel"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/fees"
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

	costs     *costmodel.CostTracker
	entries   *EntryBuilder
	sink      BatchSink
	forgedTxs []*solana.Transaction
	txFees    fees.TxFeeInfoAccumulator
	numSigs   uint64
	entryHash solana.Hash
	accepting bool
	// ancestorStatuses is the immutable transaction-status lineage pinned when
	// this bank's replay parent was selected.
	ancestorStatuses *replay.TransactionStatusView
	// seenMessages is the bank-local AlreadyProcessed status set. The TPU's
	// signature LRU is only an ingress optimization and is not authoritative.
	seenMessages map[[32]byte]struct{}
}

type BankConfig struct {
	SlotCtx             *sealevel.SlotCtx
	Slot                uint64
	Leader              solana.PublicKey
	Limits              costmodel.Limits
	EntryHash           solana.Hash
	Sink                BatchSink
	TransactionStatuses *replay.TransactionStatusView
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
		slotCtx:          cfg.SlotCtx,
		slot:             cfg.Slot,
		leader:           cfg.Leader,
		costs:            costmodel.NewCostTracker(limits),
		entries:          NewEntryBuilder(limits, cfg.EntryHash),
		sink:             sink,
		entryHash:        cfg.EntryHash,
		accepting:        true,
		ancestorStatuses: cfg.TransactionStatuses,
		seenMessages:     make(map[[32]byte]struct{}),
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

func (b *WorkingBank) EntryHash() solana.Hash {
	return b.entryHash
}

func (b *WorkingBank) ForgedTransactions() []*solana.Transaction {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*solana.Transaction, len(b.forgedTxs))
	copy(out, b.forgedTxs)
	return out
}

func (b *WorkingBank) NumSignatures() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.numSigs
}

func (b *WorkingBank) TxFeeAccumulator() fees.TxFeeInfoAccumulator {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.txFees
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
	ForgeDroppedAlreadyProcessed
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
	case ForgeDroppedAlreadyProcessed:
		return "dropped_already_processed"
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
	messageHash, err := replay.TransactionMessageHash(tx)
	if err != nil {
		return ForgeDroppedParse, costmodel.ExceedNone
	}
	ancestorAlreadyProcessed := false
	var ancestorStatusErr error
	if b.ancestorStatuses == nil {
		ancestorStatusErr = &replay.IncompleteTransactionStatusCoverageError{}
	} else {
		// The view is immutable and internally synchronizes its lazy per-blockhash
		// index. Do that potentially large one-time lookup outside the bank lock.
		ancestorAlreadyProcessed, ancestorStatusErr = b.ancestorStatuses.ContainsMessage(tx.Message.RecentBlockhash, messageHash)
	}

	var cost costmodel.TransactionCost
	if !ancestorAlreadyProcessed && ancestorStatusErr == nil {
		feats := b.slotCtx.Features
		if feats == nil {
			f := features.NewFeaturesDefault()
			feats = f
		}
		cost, err = costmodel.EstimateTransactionCost(tx, feats)
		if err != nil {
			return ForgeDroppedParse, costmodel.ExceedNone
		}
		cost.WireSize = wireSize
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.accepting {
		return ForgeDroppedNoLeader, costmodel.ExceedNone
	}
	if _, alreadyProcessed := b.seenMessages[messageHash]; alreadyProcessed {
		return ForgeDroppedAlreadyProcessed, costmodel.ExceedNone
	}
	if ancestorAlreadyProcessed {
		return ForgeDroppedAlreadyProcessed, costmodel.ExceedNone
	}
	if ancestorStatusErr != nil {
		// A live leader is never constructed with incomplete coverage. Preserve a
		// fail-closed result for isolated callers that bypass LeaderLoop.
		return ForgeDroppedNoLeader, costmodel.ExceedNone
	}

	if reason := b.costs.WouldExceed(cost); reason != costmodel.ExceedNone {
		return ForgeDroppedCost, reason
	}

	output := replay.LoadAndExecuteTransaction(replay.LoadAndExecuteTransactionInput{
		SlotCtx:     b.slotCtx,
		Transaction: tx,
		LeanResult:  true,
	})
	if output.ProcessingResult.TransactionError != nil {
		return ForgeDroppedExecution, costmodel.ExceedNone
	}
	if err := replay.ApplySuccessfulTransaction(b.slotCtx, output); err != nil {
		return ForgeDroppedExecution, costmodel.ExceedNone
	}
	if b.seenMessages == nil {
		b.seenMessages = make(map[[32]byte]struct{})
	}
	b.seenMessages[messageHash] = struct{}{}
	if output.FeeInfo != nil {
		b.txFees.Add(output.FeeInfo)
	}
	b.forgedTxs = append(b.forgedTxs, tx)
	b.numSigs += uint64(tx.Message.Header.NumRequiredSignatures)

	b.costs.Record(cost)
	if flushed, batchBytes, didFlush := b.entries.Append(*tx, wireSize); didFlush {
		b.entryHash = b.entries.CurrentEntryHash()
		b.sink.OnEntryBatch(flushed, batchBytes)
	}
	return ForgeAccepted, costmodel.ExceedNone
}

// BufferedDropReason classifies why a buffered transaction should be discarded.
type BufferedDropReason int

const (
	BufferedKeep BufferedDropReason = iota
	BufferedExpired
	BufferedAlreadyProcessed
)

// ClassifyBuffered reports whether a buffered transaction is expired or already
// processed relative to this bank.
func (b *WorkingBank) ClassifyBuffered(blockhash solana.Hash, messageHash [32]byte) BufferedDropReason {
	if rbh := sealevel.SysvarCache.RecentBlockHashes.Sysvar; rbh == nil || !rbh.IsBlockhashAgeValid(blockhash) {
		return BufferedExpired
	}
	if b.ancestorStatuses != nil {
		if ok, err := b.ancestorStatuses.ContainsMessage(blockhash, messageHash); err == nil && ok {
			return BufferedAlreadyProcessed
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, seen := b.seenMessages[messageHash]; seen {
		return BufferedAlreadyProcessed
	}
	return BufferedKeep
}

// FlushEntries emits any pending transactions as an entry batch.
func (b *WorkingBank) FlushEntries() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.flushEntriesLocked()
}

// Freeze stops transaction admission and emits the final pending entry batch.
// It waits for any forge already holding the bank lock, so every accepted
// transaction is included before the footer bank hash is computed.
func (b *WorkingBank) Freeze() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.accepting = false
	b.flushEntriesLocked()
}

// Close stops transaction admission without emitting pending entries. It is
// used when an active slot is abandoned because its replay parent changed.
func (b *WorkingBank) Close() {
	b.mu.Lock()
	b.accepting = false
	b.mu.Unlock()
}

func (b *WorkingBank) flushEntriesLocked() {
	entries, batchBytes := b.entries.Flush()
	if len(entries) == 0 {
		return
	}
	b.entryHash = b.entries.CurrentEntryHash()
	b.sink.OnEntryBatch(entries, batchBytes)
}
