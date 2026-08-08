package replay

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/gagliardetto/solana-go"
)

// The trailing verifier is the verifying mode's external execution oracle.
// Full validator mode does not use RPC block data: it executes blocks locally
// and checks the bank hash in the certified block footer before voting/folding.
// For a non-voting verifying node, the
// verifier re-derives each executed slot's per-transaction results from RPC
// getBlock metadata (finalized commitment) a configurable lag behind the tip
// and compares against the slot digests replay recorded. The fold watermark
// is min(finality, verified): nothing reaches durable state unverified.
//
// Failure semantics are deliberately asymmetric:
//   - a same-block digest mismatch is an execution divergence -> halt +
//     persist evidence; deterministic divergence is NOT self-healing, so the
//     node-level recovery loop must not auto-retry it
//   - a different blockhash at the same slot is a SIBLING question (fork
//     identity), owned by the certificate gate/switch — requeue, never halt
//   - RPC outages just stall the watermark; with Required=true the fold
//     stalls with it until the OverCap halt (designed fail-closed backpressure)

// VerifierConfig configures the trailing verifier ([verifier] section).
type VerifierConfig struct {
	Enabled             bool
	LagSlots            uint64        // don't attempt verification until executedTip - LagSlots
	MaxRPS              int           // verifier's own RPC budget (never shares block fetch)
	StallWindow         time.Duration // max time behind without watermark progress
	Required            bool          // gate folds on the verified watermark
	ValidatorFooterHash bool          // validator mode uses footer/local bank-hash parity, never RPC blocks
}

const (
	defaultTrailingVerifierMaxRPS = 8
	maxTrailingVerifierRPS        = 1000
)

// TrailingVerifierDefaults returns the default configuration: enabled,
// required, 32-slot lag, 8 requests/second, and a five-minute stall window.
func TrailingVerifierDefaults() VerifierConfig {
	return VerifierConfig{
		Enabled:     true,
		LagSlots:    32,
		MaxRPS:      defaultTrailingVerifierMaxRPS,
		StallWindow: 5 * time.Minute,
		Required:    true,
	}
}

// ValidateTrailingVerifierMaxRPS rejects values that cannot form a useful
// verifier cadence. The verifier issues requests serially, so a 1 ms cadence
// is already well above its practical throughput.
func ValidateTrailingVerifierMaxRPS(maxRPS int) error {
	if maxRPS < 1 || maxRPS > maxTrailingVerifierRPS {
		return fmt.Errorf("verifier.max_rps must be between 1 and %d", maxTrailingVerifierRPS)
	}
	return nil
}

// TrailingVerifierCfg is the active configuration ([verifier] section), set by
// node startup before ReplayBlocks.
var TrailingVerifierCfg = TrailingVerifierDefaults()

// verifierTx is one transaction's externally-attested record extracted from
// RPC metadata.
type verifierTx struct {
	Sig    solana.Signature
	Fee    uint64
	Failed bool
	Pre    []uint64
	Post   []uint64
}

// verifiedBlock is the extracted finalized view of one slot.
type verifiedBlock struct {
	Blockhash solana.Hash
	Txs       []verifierTx
}

// blockVerificationSource fetches the finalized attested view of a slot.
// Returns rpcclient.SlotSkipped for finalized-skipped slots; any other error
// is transient (backoff + retry). Faked in tests.
type blockVerificationSource interface {
	FetchFinalized(context.Context, uint64) (*verifiedBlock, error)
}

// rpcVerificationSource adapts an RpcClient to blockVerificationSource.
type rpcVerificationSource struct {
	rpcc *rpcclient.RpcClient
}

func (r *rpcVerificationSource) FetchFinalized(ctx context.Context, slot uint64) (*verifiedBlock, error) {
	result, err := r.rpcc.GetBlockFinalizedOnceContext(ctx, slot)
	if err != nil {
		return nil, err
	}
	vb := &verifiedBlock{Blockhash: result.Blockhash, Txs: make([]verifierTx, 0, len(result.Transactions))}
	for i := range result.Transactions {
		rtx := &result.Transactions[i]
		parsed, perr := rtx.GetTransaction()
		if perr != nil || parsed == nil || len(parsed.Signatures) == 0 || rtx.Meta == nil {
			return nil, fmt.Errorf("slot %d tx %d: unparseable RPC transaction/meta", slot, i)
		}
		vb.Txs = append(vb.Txs, verifierTx{
			Sig:    parsed.Signatures[0],
			Fee:    rtx.Meta.Fee,
			Failed: rtx.Meta.Err != nil,
			Pre:    rtx.Meta.PreBalances,
			Post:   rtx.Meta.PostBalances,
		})
	}
	return vb, nil
}

type pendingDigest struct {
	digest      *SlotDigest
	attempts    int
	skipHits    int // cross-checks confirming an RPC skip disagreement
	siblingHits int // consecutive different-blockhash observations
	nextTry     time.Time
}

// TrailingVerifier verifies executed slots against RPC metadata in the
// background and exposes the contiguous verified watermark.
type TrailingVerifier struct {
	cfg VerifierConfig
	src blockVerificationSource

	mu           sync.Mutex
	pending      map[uint64]*pendingDigest
	order        []uint64 // ascending recorded slots not yet verified
	firstSlot    uint64   // first slot ever recorded (watermark floor anchor)
	verified     uint64   // all recorded slots <= verified are verified
	executedTip  uint64
	failure      *ReplayDivergence
	evidenceOK   bool
	behind       bool
	lastProgress time.Time
	now          func() time.Time

	verifiedCount uint64
	requeues      uint64
}

func newTrailingVerifier(src blockVerificationSource, cfg VerifierConfig) *TrailingVerifier {
	if cfg.LagSlots == 0 {
		cfg.LagSlots = 32
	}
	cfg.MaxRPS = safeTrailingVerifierMaxRPS(cfg.MaxRPS)
	if cfg.StallWindow <= 0 {
		cfg.StallWindow = 5 * time.Minute
	}
	return &TrailingVerifier{
		cfg:     cfg,
		src:     src,
		pending: make(map[uint64]*pendingDigest),
		now:     time.Now,
	}
}

func safeTrailingVerifierMaxRPS(maxRPS int) int {
	switch {
	case maxRPS <= 0:
		return defaultTrailingVerifierMaxRPS
	case maxRPS > maxTrailingVerifierRPS:
		return maxTrailingVerifierRPS
	default:
		return maxRPS
	}
}

// Record registers an executed slot's digest for verification.
func (v *TrailingVerifier) Record(d *SlotDigest) {
	if v == nil || d == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.firstSlot == 0 || d.Slot < v.firstSlot {
		v.firstSlot = d.Slot
		floor := slotBefore(d.Slot)
		if v.verified == 0 || v.verified > floor {
			v.verified = floor
		}
	}
	if _, exists := v.pending[d.Slot]; !exists {
		index := sort.Search(len(v.order), func(i int) bool { return v.order[i] >= d.Slot })
		v.order = append(v.order, 0)
		copy(v.order[index+1:], v.order[index:])
		v.order[index] = d.Slot
	}
	v.pending[d.Slot] = &pendingDigest{digest: d}
	if d.Slot > v.executedTip {
		v.executedTip = d.Slot
	}
	v.publishStatusLocked()
}

func slotBefore(slot uint64) uint64 {
	if slot == 0 {
		return 0
	}
	return slot - 1
}

// RecordSkip registers a slot replay treated as skipped.
func (v *TrailingVerifier) RecordSkip(slot uint64) {
	v.Record(&SlotDigest{Slot: slot, Skipped: true})
}

// SetExecutedTip advances the tip used for the verification lag.
func (v *TrailingVerifier) SetExecutedTip(slot uint64) {
	if v == nil {
		return
	}
	v.mu.Lock()
	if slot > v.executedTip {
		v.executedTip = slot
	}
	v.publishStatusLocked()
	v.mu.Unlock()
}

// VerifiedWatermark returns the highest slot V such that every recorded slot
// <= V verified clean. Before anything is recorded it returns 0 (nothing is
// foldable yet anyway). Nil receiver (verifier disabled) = no gating.
func (v *TrailingVerifier) VerifiedWatermark() uint64 {
	if v == nil {
		return ^uint64(0)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.verified
}

// Failure returns the first confirmed divergence (nil if none).
func (v *TrailingVerifier) Failure() *ReplayDivergence {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.failure
}

// PruneThrough drops verified bookkeeping for slots <= slot (post-fold).
func (v *TrailingVerifier) PruneThrough(slot uint64) {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	kept := v.order[:0]
	for _, s := range v.order {
		if s <= slot {
			delete(v.pending, s)
			continue
		}
		kept = append(kept, s)
	}
	v.order = kept
}

// RewindFrom discards verifier state for an abandoned fork suffix.
func (v *TrailingVerifier) RewindFrom(slot uint64) {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()

	kept := v.order[:0]
	for _, recorded := range v.order {
		if recorded >= slot {
			delete(v.pending, recorded)
			continue
		}
		kept = append(kept, recorded)
	}
	v.order = kept
	floor := slotBefore(slot)
	if v.verified > floor {
		v.verified = floor
	}
	if v.executedTip >= slot {
		v.executedTip = floor
	}
	if v.firstSlot == 0 || v.firstSlot >= slot {
		v.firstSlot = slot
	}
	v.publishStatusLocked()
}

// Run drives verification until ctx is done. One slot per permit, oldest
// first, capped at MaxRPS.
func (v *TrailingVerifier) Run(ctx context.Context) {
	interval := time.Second / time.Duration(safeTrailingVerifierMaxRPS(v.cfg.MaxRPS))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			v.verifyNextContext(ctx)
		}
	}
}

func startTrailingVerifier(ctx context.Context, verifier *TrailingVerifier) func() {
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		verifier.Run(runCtx)
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

// verifyNext picks the oldest eligible pending slot and verifies it.
func (v *TrailingVerifier) verifyNext() {
	v.verifyNextContext(context.Background())
}

func (v *TrailingVerifier) verifyNextContext(ctx context.Context) {
	v.mu.Lock()
	if v.failure != nil {
		v.mu.Unlock()
		return
	}
	var slot uint64
	var pd *pendingDigest
	backedOff := false
	now := v.now()
	for _, s := range v.order {
		cand := v.pending[s]
		if cand == nil {
			continue
		}
		if v.executedTip < v.cfg.LagSlots || s > v.executedTip-v.cfg.LagSlots {
			break // too fresh; order is ascending so nothing later is eligible
		}
		if now.Before(cand.nextTry) {
			backedOff = true
			continue
		}
		slot, pd = s, cand
		break
	}
	if pd == nil && backedOff {
		v.publishStatusLocked()
	}
	v.mu.Unlock()
	if pd == nil {
		return
	}

	result, err := v.src.FetchFinalized(ctx, slot)
	v.mu.Lock()
	defer func() {
		v.publishStatusLocked()
		v.mu.Unlock()
	}()
	if v.pending[slot] != pd { // pruned concurrently
		return
	}

	switch {
	case err == rpcclient.SlotSkipped:
		v.evidenceOK = true
		if pd.digest.Skipped {
			v.markVerifiedLocked(slot)
			return
		}
		// We executed a block; RPC (finalized) says skipped. Confirm across
		// retries before declaring divergence — transient RPC views exist.
		pd.skipHits++
		if pd.skipHits >= 3 {
			v.failure = &ReplayDivergence{Slot: slot, TxIndex: -1, Kind: "skip_mismatch",
				Detail: "replay executed a block but RPC finalized view reports the slot skipped"}
			return
		}
		pd.attempts++
		pd.nextTry = v.now().Add(5 * time.Second)
		return
	case err != nil:
		// Transient (not yet finalized on the RPC view, outage, etc.): back off.
		v.evidenceOK = false
		pd.attempts++
		pd.nextTry = v.now().Add(backoffFor(pd.attempts))
		return
	}
	v.evidenceOK = true

	if pd.digest.Skipped {
		// We skipped; RPC has a finalized block. Confirm, then diverge.
		pd.skipHits++
		if pd.skipHits >= 3 {
			v.failure = &ReplayDivergence{Slot: slot, TxIndex: -1, Kind: "skip_mismatch",
				Detail: "replay skipped the slot but RPC finalized view has a block"}
			return
		}
		pd.attempts++
		pd.nextTry = v.now().Add(5 * time.Second)
		return
	}

	// Sibling check: a different blockhash is a fork-identity question owned
	// by the certificate gate, NOT an execution divergence. Requeue.
	if result.Blockhash != pd.digest.Blockhash {
		pd.siblingHits++
		v.requeues++
		if pd.siblingHits%10 == 0 {
			mlog.Log.Warnf("trailing verifier: slot %d blockhash differs from RPC finalized view (%d observations) — executed a non-canonical sibling? holding the fold watermark; the certificate gate owns identity",
				slot, pd.siblingHits)
		}
		pd.attempts++
		pd.nextTry = v.now().Add(backoffFor(pd.attempts))
		return
	}

	if div := compareSlotDigest(pd.digest, result); div != nil {
		v.failure = div
		return
	}
	v.markVerifiedLocked(slot)
}

func (v *TrailingVerifier) publishStatusLocked() {
	eligible := uint64(0)
	if v.executedTip >= v.cfg.LagSlots {
		eligible = v.executedTip - v.cfg.LagSlots
	}
	now := v.now()
	behind := v.cfg.Required && v.evidenceOK && v.verified < eligible
	if behind && !v.behind {
		// Start the clock when usable evidence first shows that coverage is
		// behind. Time spent with an unavailable source is reported as
		// unavailable, not retroactively relabelled as a verifier stall.
		v.lastProgress = now
	}
	v.behind = behind
	sinceProgress := time.Duration(0)
	if behind && !v.lastProgress.IsZero() {
		sinceProgress = now.Sub(v.lastProgress)
		if sinceProgress < 0 {
			sinceProgress = 0
		}
	}
	state := EvaluateCoverage(
		v.cfg.Required,
		v.evidenceOK,
		v.verified,
		eligible,
		sinceProgress,
		v.cfg.StallWindow,
	)
	if v.failure != nil {
		state = VerificationDiverged
	}
	updateVerificationProgress(state, v.verified, eligible)
}

func backoffFor(attempts int) time.Duration {
	d := time.Duration(attempts) * 2 * time.Second
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

// markVerifiedLocked marks slot verified (pending -> nil) and advances the
// watermark over the contiguous verified prefix of recorded slots.
func (v *TrailingVerifier) markVerifiedLocked(slot uint64) {
	if _, ok := v.pending[slot]; !ok {
		return
	}
	before := v.verified
	v.pending[slot] = nil
	v.verifiedCount++
	for len(v.order) > 0 {
		s := v.order[0]
		if pd, ok := v.pending[s]; ok && pd != nil {
			break // oldest recorded slot still unverified
		}
		delete(v.pending, s)
		v.order = v.order[1:]
		if s > v.verified {
			v.verified = s
		}
	}
	if v.verified > before {
		v.lastProgress = v.now()
	}
}

// compareSlotDigest recomputes each transaction's record hash from the
// attested view (using the recorded comparability mask) and compares.
func compareSlotDigest(d *SlotDigest, vb *verifiedBlock) *ReplayDivergence {
	if len(vb.Txs) != len(d.Txs) {
		return &ReplayDivergence{Slot: d.Slot, TxIndex: -1, Kind: "tx_count",
			Detail: fmt.Sprintf("replay executed %d transactions, attested block has %d", len(d.Txs), len(vb.Txs))}
	}
	for i := range vb.Txs {
		vt := &vb.Txs[i]
		td := &d.Txs[i]
		var sigPrefix [8]byte
		copy(sigPrefix[:], vt.Sig[:8])
		if sigPrefix != td.SigPrefix {
			return &ReplayDivergence{Slot: d.Slot, TxIndex: i, TxSignature: vt.Sig.String(), Kind: "tx_record",
				Detail: "transaction order/signature differs from attested block"}
		}
		if td.RecordHash == ([16]byte{}) {
			return &ReplayDivergence{Slot: d.Slot, TxIndex: i, TxSignature: vt.Sig.String(), Kind: "missing_record",
				Detail: "replay captured no execution record for this transaction"}
		}
		got := txRecordHash(vt.Sig, vt.Fee, vt.Failed, td.NumAccts, td.SkipMask, vt.Pre, vt.Post)
		if got != td.RecordHash {
			return &ReplayDivergence{Slot: d.Slot, TxIndex: i, TxSignature: vt.Sig.String(), Kind: "tx_record",
				Detail: fmt.Sprintf("fee/status/balance record differs from attested metadata (fee=%d failed=%v accts=%d)", vt.Fee, vt.Failed, td.NumAccts)}
		}
	}
	return nil
}
