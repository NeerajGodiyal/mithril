package txstatus

import (
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gagliardetto/solana-go"
)

// Config bounds the index. Both bounds are mandatory: an unbounded receipt
// index on a long-running validator is a memory leak with extra steps.
type Config struct {
	// MaxReceipts caps how many submissions are tracked at once. Completed
	// receipts are evicted first, then the oldest receipt when necessary.
	MaxReceipts int
	// Retention drops receipts this long after their last update.
	Retention time.Duration
	// Now defaults to time.Now; tests supply a clock.
	Now func() time.Time
}

// Index is a bounded, fork-aware record of transactions this node submitted.
// It is safe for concurrent use: replay publishes while RPC submits and reads.
type Index struct {
	maxReceipts int
	retention   time.Duration
	now         func() time.Time

	mu sync.RWMutex
	// receipts is keyed by the FULL signature. The persisted TxDigest
	// elsewhere keeps only an eight-byte prefix, which is not collision-safe
	// for an identity a caller acts on.
	receipts map[solana.Signature]*Receipt
	// observedBlockHeight is the highest block height the node has reported.
	// Solana blockhash validity is height-based, not slot-based.
	observedBlockHeight uint64
	// rootedThrough is the highest slot known rooted, so a receipt landing in
	// an already-rooted slot is promoted immediately rather than waiting for
	// the next Rooted call.
	rootedThrough uint64
}

// NewIndex validates the bounds and builds an index.
func NewIndex(cfg Config) (*Index, error) {
	if cfg.MaxReceipts <= 0 {
		return nil, errors.New("txstatus: MaxReceipts must be positive")
	}
	if cfg.Retention <= 0 {
		return nil, errors.New("txstatus: Retention must be positive")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Index{
		maxReceipts: cfg.MaxReceipts,
		retention:   cfg.Retention,
		now:         now,
		receipts:    make(map[solana.Signature]*Receipt, cfg.MaxReceipts),
	}, nil
}

// SubmissionAttempted records the start of forwarding. Re-attempting a
// signature already tracked never resets an observed outcome.
func (i *Index) SubmissionAttempted(
	signature solana.Signature,
	recentBlockhash solana.Hash,
	lastValidBlockHeight *uint64,
) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.evictLocked()

	if _, exists := i.receipts[signature]; exists {
		return nil
	}
	if len(i.receipts) >= i.maxReceipts {
		i.evictOldestLocked()
	}
	now := i.now()
	var deadline *uint64
	if lastValidBlockHeight != nil {
		value := *lastValidBlockHeight
		deadline = &value
	}
	i.receipts[signature] = &Receipt{
		Signature:            signature,
		Status:               StatusSubmissionUnknown,
		SubmittedAt:          now,
		RecentBlockhash:      recentBlockhash,
		LastValidBlockHeight: deadline,
		UpdatedAt:            now,
	}
	return nil
}

// Forwarded records that at least one send completed successfully.
func (i *Index) Forwarded(signature solana.Signature) {
	i.mu.Lock()
	defer i.mu.Unlock()

	receipt, ours := i.receipts[signature]
	if !ours {
		return
	}
	if receipt.Status == StatusExpired && receipt.statusBeforeExpiry == StatusSubmissionUnknown {
		receipt.statusBeforeExpiry = StatusSubmitted
		receipt.UpdatedAt = i.now()
		return
	}
	if receipt.Status != StatusSubmissionUnknown {
		return
	}
	receipt.Status = StatusSubmitted
	receipt.UpdatedAt = i.now()
}

// Landed records replay observing one of our transactions in a block. A
// signature we never submitted is ignored — this index tracks our own
// submissions, not the chain.
func (i *Index) Landed(signature solana.Signature, slot uint64, executionError string) {
	i.mu.Lock()
	defer i.mu.Unlock()

	receipt, ours := i.receipts[signature]
	if !ours {
		return
	}
	if receipt.Status.Terminal() && receipt.Status != StatusExpired {
		return
	}
	receipt.LandedSlot = slot
	receipt.ExecutionError = boundedExecutionError(executionError)
	receipt.statusBeforeExpiry = StatusUnknown
	if receipt.ExecutionError != "" {
		if slot <= i.rootedThrough {
			receipt.Status = StatusFailed
		} else {
			receipt.Status = StatusLandedFailed
		}
	} else if slot <= i.rootedThrough {
		receipt.Status = StatusRooted
	} else {
		receipt.Status = StatusLanded
	}
	receipt.UpdatedAt = i.now()
}

// Rooted promotes every receipt that landed at or below throughSlot.
func (i *Index) Rooted(throughSlot uint64) {
	i.mu.Lock()
	defer i.mu.Unlock()

	if throughSlot > i.rootedThrough {
		i.rootedThrough = throughSlot
	}
	now := i.now()
	for _, receipt := range i.receipts {
		if receipt.LandedSlot > throughSlot {
			continue
		}
		switch receipt.Status {
		case StatusLanded:
			receipt.Status = StatusRooted
			receipt.UpdatedAt = now
		case StatusLandedFailed:
			receipt.Status = StatusFailed
			receipt.UpdatedAt = now
		}
	}
}

// Unwound reverts non-rooted receipts that landed at or above fromSlot. A
// rooted slot cannot be unwound, so rooted receipts are left alone rather than
// being corrupted by a caller that got the fork point wrong.
func (i *Index) Unwound(fromSlot uint64) {
	i.mu.Lock()
	defer i.mu.Unlock()

	now := i.now()
	for _, receipt := range i.receipts {
		if receipt.Status.Terminal() || receipt.LandedSlot < fromSlot {
			continue
		}
		if receipt.Status != StatusLanded && receipt.Status != StatusLandedFailed && receipt.Status != StatusUnwound {
			continue
		}
		receipt.Status = StatusUnwound
		// Clear the slot: a receipt must never point at an abandoned fork.
		receipt.LandedSlot = 0
		receipt.ExecutionError = ""
		receipt.UpdatedAt = now
	}
}

// DurableRewound lowers the durable slot watermark after AccountsDB is
// explicitly rolled back. Outcomes above the new boundary become provisional
// again so replay can unwind or re-root them against the surviving fork.
func (i *Index) DurableRewound(throughSlot uint64) {
	i.mu.Lock()
	defer i.mu.Unlock()

	if throughSlot >= i.rootedThrough {
		return
	}
	i.rootedThrough = throughSlot
	now := i.now()
	for _, receipt := range i.receipts {
		if receipt.LandedSlot <= throughSlot {
			continue
		}
		switch receipt.Status {
		case StatusRooted:
			receipt.Status = StatusLanded
			receipt.UpdatedAt = now
		case StatusFailed:
			receipt.Status = StatusLandedFailed
			receipt.UpdatedAt = now
		}
	}
}

// ObserveBlockHeight advances the node's block height so submissions with a
// known deadline can expire even when none of ours land.
func (i *Index) ObserveBlockHeight(blockHeight uint64) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.observeBlockHeightLocked(blockHeight)
}

// RewindBlockHeight lowers the observed height after replay abandons a fork.
// A receipt that expired only on the abandoned suffix returns to its exact
// pre-expiry state when its deadline becomes valid again.
func (i *Index) RewindBlockHeight(blockHeight uint64) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if blockHeight >= i.observedBlockHeight {
		return
	}
	i.observedBlockHeight = blockHeight
	now := i.now()
	for _, receipt := range i.receipts {
		if receipt.Status != StatusExpired || receipt.LastValidBlockHeight == nil || blockHeight > *receipt.LastValidBlockHeight {
			continue
		}
		restore := receipt.statusBeforeExpiry
		if restore != StatusSubmissionUnknown && restore != StatusSubmitted && restore != StatusUnwound {
			restore = StatusSubmissionUnknown
		}
		receipt.Status = restore
		receipt.statusBeforeExpiry = StatusUnknown
		receipt.UpdatedAt = now
	}
}

func (i *Index) observeBlockHeightLocked(blockHeight uint64) {
	if blockHeight <= i.observedBlockHeight {
		return
	}
	i.observedBlockHeight = blockHeight
	now := i.now()
	for _, receipt := range i.receipts {
		// Only a transaction that never landed can expire. One that landed and
		// unwound may still land again under the same blockhash, so it expires
		// on the same rule rather than being held open forever.
		switch receipt.Status {
		case StatusSubmissionUnknown, StatusSubmitted, StatusUnwound:
			if receipt.LastValidBlockHeight != nil && blockHeight > *receipt.LastValidBlockHeight {
				receipt.statusBeforeExpiry = receipt.Status
				receipt.Status = StatusExpired
				receipt.UpdatedAt = now
			}
		}
	}
}

const maxExecutionErrorBytes = 512

func boundedExecutionError(value string) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, value)
	value = strings.TrimSpace(value)
	if len(value) <= maxExecutionErrorBytes {
		return value
	}
	value = value[:maxExecutionErrorBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return strings.TrimSpace(value)
}

// Lookup returns a copy of what is known about a signature. The second result
// is false when nothing is known, in which case the receipt is the zero value
// with StatusUnknown — which is not a failure.
func (i *Index) Lookup(signature solana.Signature) (Receipt, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.evictLocked()

	receipt, ok := i.receipts[signature]
	if !ok {
		return Receipt{Signature: signature, Status: StatusUnknown, StatusName: StatusUnknown.String()}, false
	}
	out := *receipt
	if receipt.LastValidBlockHeight != nil {
		deadline := *receipt.LastValidBlockHeight
		out.LastValidBlockHeight = &deadline
	}
	out.StatusName = out.Status.String()
	return out, true
}

// Len reports how many receipts are currently tracked.
func (i *Index) Len() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.evictLocked()
	return len(i.receipts)
}

// evictLocked bounds the lifetime of every in-memory receipt. This index is
// operational evidence, not durable history; after eviction callers get the
// explicit unknown result rather than stale or fabricated certainty.
func (i *Index) evictLocked() {
	cutoff := i.now().Add(-i.retention)
	for signature, receipt := range i.receipts {
		if receipt.UpdatedAt.Before(cutoff) {
			delete(i.receipts, signature)
		}
	}
}

// evictOldestLocked makes one slot for a new receipt. It prefers completed
// evidence, then evicts the oldest unresolved record so remote submissions
// cannot permanently disable receipt tracking.
func (i *Index) evictOldestLocked() {
	var oldestSig solana.Signature
	var oldest time.Time
	found := false
	for signature, receipt := range i.receipts {
		if !receipt.Status.Terminal() {
			continue
		}
		if !found || receipt.UpdatedAt.Before(oldest) {
			oldestSig, oldest, found = signature, receipt.UpdatedAt, true
		}
	}
	if found {
		delete(i.receipts, oldestSig)
		return
	}
	for signature, receipt := range i.receipts {
		if !found || receipt.UpdatedAt.Before(oldest) {
			oldestSig, oldest, found = signature, receipt.UpdatedAt, true
		}
	}
	if found {
		delete(i.receipts, oldestSig)
	}
}
