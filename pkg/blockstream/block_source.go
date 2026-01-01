package blockstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/block"
	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/gagliardetto/solana-go/rpc"
	"golang.org/x/time/rate"
)

type BlockSourceType int

const (
	BlockSourceRpc = iota
	BlockSourceFile
	BlockSourceOvercast
)

type BlockSourceOpts struct {
	RpcClient    *rpcclient.RpcClient // Primary RPC for block fetching (getBlock)
	AuxRpcClient *rpcclient.RpcClient // For tip polling (getSlot) - falls back to RpcClient if nil
	SourceType   BlockSourceType
	StartSlot    uint64
	EndSlot      uint64
	BlockDir     string

	// Backup RPC endpoints for failover (optional)
	// These are tried in order if the primary fails with timeout errors.
	// After 100 slots, the primary is retried and restored if working.
	BackupRpcEndpoints []string

	// Parallel fetch settings
	MaxRPS          int    // Rate limit (requests per second), 0 = use default
	MaxInflight     int    // Max concurrent workers, 0 = use default
	TipPollMs       int    // Tip poll interval ms, 0 = use default
	TipSafetyMargin uint64 // Don't fetch within N slots of tip, 0 = use default
}

// slotStatus tracks the state of each slot being fetched
type slotStatus int

const (
	slotPending slotStatus = iota
	slotInflight
	slotDone
)

// fetchResult is sent from workers to the emitter
type fetchResult struct {
	slot    uint64
	block   *b.Block
	err     error
	skipped bool  // true if SlotSkipped error
	rpcIdx  int32 // which RPC endpoint produced this result (for error attribution)
}

var errBeyondTip = errors.New("slot beyond confirmed tip")

// BlockSourceStats contains metrics for parallel block fetching
type BlockSourceStats struct {
	// Fetch counts
	FetchAttempts  atomic.Uint64
	FetchSuccesses atomic.Uint64
	FetchRetries   atomic.Uint64
	FetchSkipped   atomic.Uint64

	// Speculative/backup requests
	SpeculativeRetries atomic.Uint64 // Backup requests sent for slow slots

	// Error buckets
	ErrSlotNotAvail atomic.Uint64
	ErrRateLimited  atomic.Uint64
	ErrBeyondTip    atomic.Uint64
	ErrTransient    atomic.Uint64 // EOF, timeout, 502/503, connection reset, etc.
	ErrOther        atomic.Uint64

	// Latency tracking (nanoseconds)
	TotalFetchLatencyNs atomic.Uint64
	FetchLatencyCount   atomic.Uint64

	// Buffer stats
	MaxBufferedSlot atomic.Uint64
}

type BlockSource struct {
	rpcClients   []*rpcclient.RpcClient // All RPC clients for block fetching (index 0 = primary)
	auxRpcClient *rpcclient.RpcClient   // For tip polling (getSlot)
	streamChan   chan *b.Block
	startSlot    uint64
	endSlot      uint64
	currentSlot  uint64
	blockDir     string
	sourceType   BlockSourceType

	// RPC failover tracking
	activeRpcIdx         atomic.Int32  // Currently active RPC index (0 = primary)
	slotsSinceFailover   atomic.Uint64 // Slots emitted since failover (for retry timing)
	failoverCount        atomic.Uint64 // Total failovers (for stats)
	hardErrCount         atomic.Uint64 // Consecutive hard connectivity errors (reset on success)
	lastHardErrTime      atomic.Int64  // Unix timestamp of last hard error (for time-windowing)

	// Rate limiting
	rateLimiter *rate.Limiter
	maxInflight int

	// Tip tracking
	confirmedTip        atomic.Uint64
	processedTip        atomic.Uint64 // Processed commitment tip (super tip)
	tipAtSlot           atomic.Uint64 // What slot we had executed when tip was measured
	lastExecutedSlot    atomic.Uint64 // Last slot fully executed by replay (set by SetLastExecutedSlot)
	tipSafetyMargin     uint64
	tipPollInterval     time.Duration
	lastTipUpdate       atomic.Int64  // Unix timestamp of last successful tip poll
	tipPollFailures     atomic.Uint64 // Consecutive tip poll failures
	totalTipPollFails   atomic.Uint64 // Total tip poll failures (for stats)

	// Reorder buffer
	reorderMu      sync.Mutex
	reorderBuffer  map[uint64]*b.Block
	skippedSlots   map[uint64]bool
	nextSlotToSend uint64
	maxPending     int

	// Slot state tracking (prevents duplicates)
	slotStateMu   sync.Mutex
	slotState     map[uint64]slotStatus
	inflightStart map[uint64]time.Time // When each slot started fetching

	// Retry queue for slots that need rescheduling
	retryMu    sync.Mutex
	retrySlots []uint64

	// Worker coordination
	workQueue   chan uint64
	resultQueue chan fetchResult
	stopChan    chan struct{}
	stopped     atomic.Bool

	// Stall detection
	lastProgress atomic.Int64 // Unix timestamp of last successful block emit
	stallTimeout time.Duration
	stallError   atomic.Bool // Set when stall timeout triggers

	// Near-tip mode tracking
	isNearTip        atomic.Bool // True when close to confirmed tip
	catchupTipSafety uint64      // Original tip safety margin for catchup mode

	// Stats tracking
	stats BlockSourceStats
}

// Default values
const (
	defaultMaxRPS          = 10
	defaultMaxInflight     = 10
	defaultTipPollMs       = 5000
	defaultTipSafetyMargin = 64
	defaultMaxPending      = 100
	streamChanBuffer       = 100
	defaultStallTimeout    = 5 * time.Minute // Trigger graceful shutdown if no progress

	// RPC failover settings
	primaryRetryInterval  = 100             // Retry primary RPC every N slots after failover (progress-based)
	primaryProbeInterval  = 1 * time.Minute // Probe primary RPC every minute when on backup (time-based)
	failoverErrThreshold  = 10              // Consecutive hard errors before failover (conservative)
	failoverErrWindowSecs = 5               // Reset error count if no errors for this long

	// Near-tip mode settings
	// When within nearTipThreshold slots of confirmed tip, switch to low-latency mode
	nearTipThreshold    = 32                      // Switch to near-tip mode when gap <= this
	catchupThreshold    = 64                      // Switch back to catchup mode when gap >= this (hysteresis)
	nearTipSafetyMargin = 0                       // No margin in near-tip - rely on retries for "not available"
	nearTipPollInterval = 500 * time.Millisecond // Faster tip polling in near-tip mode
)

func NewBlockSource(opts *BlockSourceOpts) *BlockSource {
	// Apply defaults
	maxRPS := opts.MaxRPS
	if maxRPS <= 0 {
		maxRPS = defaultMaxRPS
	}

	maxInflight := opts.MaxInflight
	if maxInflight <= 0 {
		maxInflight = defaultMaxInflight
	}

	tipPollMs := opts.TipPollMs
	if tipPollMs <= 0 {
		tipPollMs = defaultTipPollMs
	}

	tipSafetyMargin := opts.TipSafetyMargin
	if tipSafetyMargin == 0 {
		tipSafetyMargin = defaultTipSafetyMargin
	}

	// Aux RPC client for tip polling - falls back to block RPC if not set
	auxRpcClient := opts.AuxRpcClient
	if auxRpcClient == nil {
		auxRpcClient = opts.RpcClient
	}

	// Build list of RPC clients: primary + backups
	rpcClients := make([]*rpcclient.RpcClient, 0, 1+len(opts.BackupRpcEndpoints))
	rpcClients = append(rpcClients, opts.RpcClient)
	for _, endpoint := range opts.BackupRpcEndpoints {
		rpcClients = append(rpcClients, rpcclient.NewRpcClient(endpoint))
	}

	bs := &BlockSource{
		rpcClients:      rpcClients,
		auxRpcClient:    auxRpcClient,
		streamChan:      make(chan *b.Block, streamChanBuffer),
		startSlot:       opts.StartSlot,
		endSlot:         opts.EndSlot,
		currentSlot:     opts.StartSlot,
		blockDir:        opts.BlockDir,
		sourceType:      opts.SourceType,
		rateLimiter:     rate.NewLimiter(rate.Limit(maxRPS), maxRPS),
		maxInflight:     maxInflight,
		tipSafetyMargin: tipSafetyMargin,
		tipPollInterval: time.Duration(tipPollMs) * time.Millisecond,
		reorderBuffer:   make(map[uint64]*b.Block),
		skippedSlots:    make(map[uint64]bool),
		nextSlotToSend:  opts.StartSlot,
		maxPending:      defaultMaxPending,
		slotState:       make(map[uint64]slotStatus),
		inflightStart:   make(map[uint64]time.Time),
		workQueue:        make(chan uint64, maxInflight*2),
		resultQueue:      make(chan fetchResult, maxInflight*2),
		stopChan:         make(chan struct{}),
		stallTimeout:     defaultStallTimeout,
		catchupTipSafety: tipSafetyMargin, // Store original for switching back to catchup
	}

	// Initialize lastProgress to now (first block hasn't been fetched yet)
	bs.lastProgress.Store(time.Now().Unix())

	// Log RPC configuration
	if len(rpcClients) > 1 {
		mlog.Log.Infof("Block fetching configured with %d RPC endpoints (primary + %d backups)",
			len(rpcClients), len(rpcClients)-1)
	}

	return bs
}

// updateMode checks the gap to tip and switches between catchup and near-tip mode.
// Uses hysteresis to avoid flapping: enter near-tip at <=128 slots, exit at >=256 slots.
func (bs *BlockSource) updateMode() {
	tip := bs.confirmedTip.Load()
	lastExecuted := bs.lastExecutedSlot.Load()

	// Can't determine mode without tip
	if tip == 0 || lastExecuted == 0 {
		return
	}

	// Calculate gap (tip should always be >= lastExecuted, but handle wrap)
	var gap uint64
	if tip > lastExecuted {
		gap = tip - lastExecuted
	} else {
		gap = 0
	}

	wasNearTip := bs.isNearTip.Load()

	if wasNearTip {
		// Currently in near-tip mode - switch to catchup if gap exceeds threshold
		if gap >= catchupThreshold {
			bs.isNearTip.Store(false)
			mlog.Log.Infof("Switching to CATCHUP mode (gap=%d slots, threshold=%d)", gap, catchupThreshold)
		}
	} else {
		// Currently in catchup mode - switch to near-tip if gap is small
		if gap <= nearTipThreshold {
			bs.isNearTip.Store(true)
			mlog.Log.Infof("Switching to NEAR-TIP mode (gap=%d slots, threshold=%d)", gap, nearTipThreshold)
		}
	}
}

// effectiveTipSafetyMargin returns the tip safety margin for the current mode.
// In near-tip mode, we use a much smaller margin to stay close to the chain.
func (bs *BlockSource) effectiveTipSafetyMargin() uint64 {
	if bs.isNearTip.Load() {
		return nearTipSafetyMargin
	}
	return bs.catchupTipSafety
}


func (bs *BlockSource) tryGetBlockFromFile(slot uint64) (*block.Block, error) {
	if bs.blockDir == "" {
		return nil, fmt.Errorf("no block directory specified")
	}
	blockFilename := filepath.Join(filepath.Clean(bs.blockDir), fmt.Sprintf("%d.json", slot))
	file, err := os.Open(blockFilename)
	if err != nil {
		return nil, fmt.Errorf("error opening blockFilename=%s: %w", blockFilename, err)
	}

	decoder := json.NewDecoder(file)
	out := &block.Block{}

	err = decoder.Decode(out)
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("block decode error: %w", err)
	}
	out.FixupTxVersions()

	file.Close()
	os.Remove(blockFilename)

	return out, nil
}

// getActiveRpc returns the currently active RPC client for block fetching
func (bs *BlockSource) getActiveRpc() *rpcclient.RpcClient {
	idx := bs.activeRpcIdx.Load()
	if idx < 0 || int(idx) >= len(bs.rpcClients) {
		return bs.rpcClients[0]
	}
	return bs.rpcClients[idx]
}

// getPrimaryRpc returns the primary (first) RPC client
func (bs *BlockSource) getPrimaryRpc() *rpcclient.RpcClient {
	return bs.rpcClients[0]
}

// isOnPrimary returns true if we're using the primary RPC
func (bs *BlockSource) isOnPrimary() bool {
	return bs.activeRpcIdx.Load() == 0
}

// failoverToNext switches to the next RPC endpoint on timeout errors.
// Only fails over if there are backup endpoints available.
// Returns true if failover occurred.
func (bs *BlockSource) failoverToNext() bool {
	if len(bs.rpcClients) <= 1 {
		return false // No backups available
	}

	currentIdx := bs.activeRpcIdx.Load()
	nextIdx := (currentIdx + 1) % int32(len(bs.rpcClients))

	// Use CompareAndSwap to avoid race conditions
	if bs.activeRpcIdx.CompareAndSwap(currentIdx, nextIdx) {
		bs.failoverCount.Add(1)
		bs.slotsSinceFailover.Store(0)

		currentEndpoint := bs.rpcClients[currentIdx].Endpoint()
		nextEndpoint := bs.rpcClients[nextIdx].Endpoint()

		if nextIdx == 0 {
			mlog.Log.Infof("RPC failover: restored to primary endpoint %s", nextEndpoint)
		} else {
			mlog.Log.Errorf("RPC failover: switching from %s to backup %s (failover #%d)",
				currentEndpoint, nextEndpoint, bs.failoverCount.Load())
		}
		return true
	}
	return false
}

// probePrimary does a quick health check on the primary RPC endpoint.
// Returns true if the primary responds successfully to a getSlot call.
// Uses a 5-second timeout to prevent blocking the scheduler on hanging RPCs.
func (bs *BlockSource) probePrimary() bool {
	primary := bs.getPrimaryRpc()
	_, err := primary.GetSlotWithTimeout(5 * time.Second)
	return err == nil
}

// restoreToPrimary attempts to switch back to the primary RPC after probing it.
// Returns true if we were on a backup and successfully switched back.
// The probe prevents predictable error bursts from switching to a still-down primary.
func (bs *BlockSource) restoreToPrimary() bool {
	if bs.isOnPrimary() {
		return false // Already on primary
	}

	// Probe primary before switching - avoids predictable error bursts
	if !bs.probePrimary() {
		// Primary still down, stay on backup
		return false
	}

	currentIdx := bs.activeRpcIdx.Load()
	if bs.activeRpcIdx.CompareAndSwap(currentIdx, 0) {
		bs.slotsSinceFailover.Store(0)
		bs.hardErrCount.Store(0) // Reset error count when switching back
		mlog.Log.Infof("RPC restored to primary endpoint %s (health probe succeeded)",
			bs.rpcClients[0].Endpoint())
		return true
	}
	return false
}

// fetchBlockOnce fetches a single block without internal retry loop.
// rpcIdx specifies which RPC client to use (for consistent error attribution).
func (bs *BlockSource) fetchBlockOnce(slot uint64, rpcIdx int32) (*b.Block, error) {
	// Try file first
	if blk, err := bs.tryGetBlockFromFile(slot); err == nil {
		return blk, nil
	}

	// Use the specified RPC client (not getActiveRpc - that could race with failover)
	if rpcIdx < 0 || int(rpcIdx) >= len(bs.rpcClients) {
		rpcIdx = 0
	}
	rpc := bs.rpcClients[rpcIdx]

	// Single RPC attempt (no internal retry - scheduler handles retries)
	blockResult, err := rpc.GetBlockConfirmedOnce(slot)
	if err != nil {
		return nil, err
	}

	return block.FromBlockResult(blockResult, slot, rpc), nil
}

// pollTip periodically updates the confirmed tip by querying all configured RPCs
// and using the maximum slot value. This handles RPCs that fall behind.
// Uses GetSlotWithTimeout to prevent hung RPCs from blocking the poll loop.
// Polls faster in near-tip mode for lower latency.
func (bs *BlockSource) pollTip() {
	const tipPollTimeout = 5 * time.Second

	// Get initial tip and update mode
	if tip := bs.getMaxTipFromAllRpcs(tipPollTimeout); tip > 0 {
		bs.updateTipSnapshot(tip)
		bs.tipPollFailures.Store(0)
		bs.updateMode()
	} else {
		bs.tipPollFailures.Add(1)
		bs.totalTipPollFails.Add(1)
	}

	// Start with catchup interval, will adjust based on mode
	currentInterval := bs.tipPollInterval
	ticker := time.NewTicker(currentInterval)
	defer ticker.Stop()

	for {
		select {
		case <-bs.stopChan:
			return
		case <-ticker.C:
			if tip := bs.getMaxTipFromAllRpcs(tipPollTimeout); tip > 0 {
				bs.updateTipSnapshot(tip)
				bs.tipPollFailures.Store(0)
				bs.updateMode() // Check if we should switch modes
			} else {
				bs.tipPollFailures.Add(1)
				bs.totalTipPollFails.Add(1)
			}

			// Adjust poll interval based on mode
			var targetInterval time.Duration
			if bs.isNearTip.Load() {
				targetInterval = nearTipPollInterval
			} else {
				targetInterval = bs.tipPollInterval
			}
			if targetInterval != currentInterval {
				currentInterval = targetInterval
				ticker.Reset(currentInterval)
			}
		}
	}
}

// SetLastExecutedSlot is called by the replay loop after each block is fully executed.
// This allows accurate tip distance calculation without blocking on replay progress.
// Also triggers mode switching based on replay progress (not just tip polling).
func (bs *BlockSource) SetLastExecutedSlot(slot uint64) {
	bs.lastExecutedSlot.Store(slot)
	bs.updateMode() // React to replay progress immediately
}

// NotifyBlockStart is called at the START of block execution.
// In near-tip mode, this triggers fetching N+1 so the RPC latency (~200ms)
// overlaps with execution time, hiding the wait from the user.
func (bs *BlockSource) NotifyBlockStart(slot uint64) {
	if bs.isNearTip.Load() {
		nextSlot := slot + 1
		if bs.canScheduleMore(nextSlot) {
			bs.scheduleSlot(nextSlot)
			mlog.Log.Debugf("near-tip: triggered prefetch of slot %d at start of %d", nextSlot, slot)
		}
	}
}

// updateTipSnapshot stores the confirmed tip along with what slot was last executed.
// This allows accurate distance calculation: tip - tipAtSlot is precise at measurement time.
func (bs *BlockSource) updateTipSnapshot(confirmedTip uint64) {
	slotAtTip := bs.lastExecutedSlot.Load()

	bs.confirmedTip.Store(confirmedTip)
	bs.tipAtSlot.Store(slotAtTip)
	bs.lastTipUpdate.Store(time.Now().Unix())
}

// RefreshTipsForSummary triggers an async refresh of both confirmed and processed tips.
// Call this near summary time (e.g., at slot 95) so fresh tips are ready at slot 100.
// This is non-blocking - it spawns a goroutine to do the work.
func (bs *BlockSource) RefreshTipsForSummary() {
	go func() {
		const timeout = 5 * time.Second

		// Capture last executed slot before RPC calls (set by replay loop)
		slotAtTip := bs.lastExecutedSlot.Load()

		// Query all RPCs for both confirmed and processed tips concurrently
		type result struct {
			confirmed uint64
			processed uint64
		}
		results := make(chan result, len(bs.rpcClients))

		for _, rpc := range bs.rpcClients {
			go func(client *rpcclient.RpcClient) {
				var r result
				if tip, err := client.GetSlotWithTimeout(timeout); err == nil {
					r.confirmed = tip
				}
				if tip, err := client.GetSlotProcessedWithTimeout(timeout); err == nil {
					r.processed = tip
				}
				results <- r
			}(rpc)
		}

		// Collect results and find max for each
		var maxConfirmed, maxProcessed uint64
		for i := 0; i < len(bs.rpcClients); i++ {
			r := <-results
			if r.confirmed > maxConfirmed {
				maxConfirmed = r.confirmed
			}
			if r.processed > maxProcessed {
				maxProcessed = r.processed
			}
		}

		// Store results
		if maxConfirmed > 0 {
			bs.confirmedTip.Store(maxConfirmed)
			bs.tipAtSlot.Store(slotAtTip)
			bs.lastTipUpdate.Store(time.Now().Unix())
			bs.tipPollFailures.Store(0)
		}
		if maxProcessed > 0 {
			bs.processedTip.Store(maxProcessed)
		}
	}()
}

// getMaxTipFromAllRpcs queries all configured RPCs for the current slot (confirmed)
// and returns the maximum value. This handles RPCs that fall behind.
func (bs *BlockSource) getMaxTipFromAllRpcs(timeout time.Duration) uint64 {
	if len(bs.rpcClients) == 0 {
		return 0
	}

	// For single RPC, no need for goroutines
	if len(bs.rpcClients) == 1 {
		if tip, err := bs.rpcClients[0].GetSlotWithTimeout(timeout); err == nil {
			return tip
		}
		return 0
	}

	// Query all RPCs concurrently
	type result struct {
		tip uint64
		err error
	}
	results := make(chan result, len(bs.rpcClients))

	for _, rpc := range bs.rpcClients {
		go func(client *rpcclient.RpcClient) {
			tip, err := client.GetSlotWithTimeout(timeout)
			results <- result{tip: tip, err: err}
		}(rpc)
	}

	// Collect results and find max
	var maxTip uint64
	for i := 0; i < len(bs.rpcClients); i++ {
		r := <-results
		if r.err == nil && r.tip > maxTip {
			maxTip = r.tip
		}
	}

	return maxTip
}

// worker fetches blocks from the work queue
func (bs *BlockSource) worker(wg *sync.WaitGroup, id int) {
	defer wg.Done()

	for slot := range bs.workQueue {
		// Check for shutdown
		if bs.stopped.Load() {
			return
		}

		// Wait for rate limiter
		bs.rateLimiter.Wait(context.Background())

		// Tip safety gate - only applies in catchup mode.
		// In near-tip mode, we bypass this entirely and rely on RPC "slot not available"
		// errors + 200ms retries. This allows true JIT fetching right at the tip.
		if !bs.isNearTip.Load() {
			tip := bs.confirmedTip.Load()
			margin := bs.effectiveTipSafetyMargin()
			var maxSlot uint64
			if tip <= margin {
				maxSlot = 0
			} else {
				maxSlot = tip - margin
			}

			// Beyond tip - send back for retry
			if maxSlot > 0 && slot > maxSlot {
				bs.stats.ErrBeyondTip.Add(1)
				bs.resultQueue <- fetchResult{slot: slot, err: errBeyondTip, rpcIdx: -1}
				continue
			}
		}

		// Capture which RPC we're using BEFORE the fetch (for error attribution)
		// Pass this to fetchBlockOnce so it uses the same client we're attributing to
		rpcIdx := bs.activeRpcIdx.Load()

		// Fetch block with latency tracking
		bs.stats.FetchAttempts.Add(1)
		fetchStart := time.Now()
		blk, err := bs.fetchBlockOnce(slot, rpcIdx)
		fetchLatency := time.Since(fetchStart)

		// Track latency for successful fetches
		if err == nil {
			bs.stats.TotalFetchLatencyNs.Add(uint64(fetchLatency.Nanoseconds()))
			bs.stats.FetchLatencyCount.Add(1)
		}

		skipped := err == rpcclient.SlotSkipped
		bs.resultQueue <- fetchResult{slot: slot, block: blk, err: err, skipped: skipped, rpcIdx: rpcIdx}
	}
}

// emitOrderedBlocks receives results and emits blocks in order
func (bs *BlockSource) emitOrderedBlocks() {
	for result := range bs.resultQueue {
		bs.reorderMu.Lock()

		// Check if this is a duplicate result (from backup request)
		// If slot is already done or we already have the block, discard this result
		bs.slotStateMu.Lock()
		isDuplicate := bs.slotState[result.slot] == slotDone
		bs.slotStateMu.Unlock()
		if isDuplicate || bs.reorderBuffer[result.slot] != nil || bs.skippedSlots[result.slot] {
			bs.reorderMu.Unlock()
			continue
		}

		// Track error buckets
		// Note: isHardConnectivityErr is checked independently because hard errors
		// (connection refused, no such host) are NOT in isTransientNetworkErr anymore
		isHardErr := false
		if result.err != nil {
			if result.err == errBeyondTip {
				// Already tracked in worker
			} else if isSlotNotAvailableErr(result.err) {
				bs.stats.ErrSlotNotAvail.Add(1)
			} else if isRateLimitedErr(result.err) {
				bs.stats.ErrRateLimited.Add(1)
			} else if isHardConnectivityErr(result.err) {
				// Hard connectivity errors: endpoint is down (connection refused, no such host, etc.)
				// These count as transient for stats but also trigger failover logic
				bs.stats.ErrTransient.Add(1)
				isHardErr = true
			} else if isTransientNetworkErr(result.err) {
				// Soft transient errors: retry same endpoint (502/503, timeouts, EOF)
				bs.stats.ErrTransient.Add(1)
			} else if result.err != rpcclient.SlotSkipped {
				bs.stats.ErrOther.Add(1)
			}
		}

		// RPC failover: only after repeated HARD connectivity errors
		// - Only count errors from the currently active RPC (ignore late errors from old endpoint)
		// - Time-windowed: reset count if no errors for 5 seconds
		// - Threshold-based: require 10 consecutive errors before failover
		if isHardErr && len(bs.rpcClients) > 1 && result.rpcIdx == bs.activeRpcIdx.Load() {
			now := time.Now().Unix()
			lastErr := bs.lastHardErrTime.Load()

			// Time-window: reset if no errors for failoverErrWindowSecs
			if lastErr > 0 && (now-lastErr) > failoverErrWindowSecs {
				bs.hardErrCount.Store(0)
			}
			bs.lastHardErrTime.Store(now)

			errCount := bs.hardErrCount.Add(1)
			if errCount >= failoverErrThreshold {
				bs.failoverToNext()
				bs.hardErrCount.Store(0) // Reset after failover
			}
		}

		// Handle result
		// CRITICAL: All errors except SlotSkipped are retriable.
		// Never skip a non-skipped slot - that causes silent state divergence.
		// If we truly can't fetch a block, we'll stall and eventually timeout.
		isRetriable := result.err != nil && !result.skipped

		if result.skipped {
			bs.stats.FetchSkipped.Add(1)
			bs.skippedSlots[result.slot] = true
			bs.hardErrCount.Store(0) // Reset on progress
		} else if result.block != nil {
			// Success! This takes priority over any pending error results.
			bs.stats.FetchSuccesses.Add(1)
			bs.reorderBuffer[result.slot] = result.block
			bs.hardErrCount.Store(0) // Reset error count on success
			// Track max buffered slot
			if result.slot > bs.stats.MaxBufferedSlot.Load() {
				bs.stats.MaxBufferedSlot.Store(result.slot)
			}
			// Local tip proxy: confirmed tip >= any successfully fetched slot (monotonic)
			// This lets us advance without waiting for the next pollTip()
			if result.slot > bs.confirmedTip.Load() {
				bs.confirmedTip.Store(result.slot)
				bs.tipAtSlot.Store(bs.lastExecutedSlot.Load())
				bs.lastTipUpdate.Store(time.Now().Unix())
			}
		} else if isRetriable {
			// All non-skip errors are retriable - schedule retry
			bs.stats.FetchRetries.Add(1)
			bs.scheduleRetry(result.slot)
		}
		// Note: if result.block == nil && result.err == nil && !result.skipped,
		// that's a bug in the worker, but we don't skip - it will stall and be detected.

		// Mark slot done (even on error), unless retriable
		bs.slotStateMu.Lock()
		if !isRetriable {
			bs.slotState[result.slot] = slotDone
			delete(bs.inflightStart, result.slot) // Clean up timing data
		} else {
			// Will be retried, reset to pending
			delete(bs.slotState, result.slot)
			delete(bs.inflightStart, result.slot)
		}
		bs.slotStateMu.Unlock()

		// Emit consecutive blocks
		for {
			if blk, ok := bs.reorderBuffer[bs.nextSlotToSend]; ok {
				delete(bs.reorderBuffer, bs.nextSlotToSend)
				bs.reorderMu.Unlock()
				bs.streamChan <- blk
				// Update progress timestamp for stall detection
				bs.lastProgress.Store(time.Now().Unix())

				// Track slots since failover for primary retry
				if !bs.isOnPrimary() {
					slots := bs.slotsSinceFailover.Add(1)
					// Every primaryRetryInterval slots, try the primary again
					if slots%primaryRetryInterval == 0 {
						bs.restoreToPrimary()
						// If primary fails, failover will trigger again on next transient error
					}
				}

				bs.reorderMu.Lock()
				bs.nextSlotToSend++
			} else if bs.skippedSlots[bs.nextSlotToSend] {
				// Slot was skipped (SlotSkipped from RPC), advance without emitting
				delete(bs.skippedSlots, bs.nextSlotToSend)
				// Also update progress - skipped slots count as progress
				bs.lastProgress.Store(time.Now().Unix())

				// Track slots since failover for primary retry (skipped slots count too)
				if !bs.isOnPrimary() {
					slots := bs.slotsSinceFailover.Add(1)
					if slots%primaryRetryInterval == 0 {
						bs.restoreToPrimary()
					}
				}

				bs.nextSlotToSend++
			} else {
				break
			}
		}

		bs.reorderMu.Unlock()
	}
}

// scheduleRetry adds a slot to the retry queue
func (bs *BlockSource) scheduleRetry(slot uint64) {
	bs.retryMu.Lock()
	bs.retrySlots = append(bs.retrySlots, slot)
	bs.retryMu.Unlock()
}

// getRetrySlots returns and clears the retry queue
func (bs *BlockSource) getRetrySlots() []uint64 {
	bs.retryMu.Lock()
	slots := bs.retrySlots
	bs.retrySlots = nil
	bs.retryMu.Unlock()
	return slots
}

// canScheduleMore returns true if we can schedule the given slot.
// In catchup mode: gate on buffer size (up to defaultMaxPending).
// In near-tip mode: true JIT - only schedule the exact next slot replay needs.
func (bs *BlockSource) canScheduleMore(slot uint64) bool {
	if bs.isNearTip.Load() {
		// True JIT: only schedule the exact next slot replay needs
		lastExecuted := bs.lastExecutedSlot.Load()

		// Only allow N+1, nothing more
		if lastExecuted > 0 && slot > lastExecuted+1 {
			return false
		}

		// Check slotState - don't schedule if already inflight/done
		bs.slotStateMu.Lock()
		state, exists := bs.slotState[slot]
		bs.slotStateMu.Unlock()
		if exists && (state == slotInflight || state == slotDone) {
			return false
		}

		// Also check if we already have it
		bs.reorderMu.Lock()
		alreadyHave := bs.reorderBuffer[slot] != nil || bs.skippedSlots[slot]
		bs.reorderMu.Unlock()

		return !alreadyHave
	}

	// Catchup mode: gate on buffer capacity
	bs.reorderMu.Lock()
	pending := len(bs.reorderBuffer)
	bs.reorderMu.Unlock()
	return pending < defaultMaxPending
}

// scheduleSlot schedules a slot if not already scheduled
func (bs *BlockSource) scheduleSlot(slot uint64) bool {
	bs.slotStateMu.Lock()
	if _, exists := bs.slotState[slot]; exists {
		bs.slotStateMu.Unlock()
		return false // Already scheduled
	}
	bs.slotState[slot] = slotInflight
	bs.inflightStart[slot] = time.Now()
	bs.slotStateMu.Unlock()

	select {
	case bs.workQueue <- slot:
		return true
	case <-bs.stopChan:
		return false
	}
}

// scheduleBackupRequest sends a backup request for a slow slot (bypasses slotState check)
// Returns true only if the request was actually queued.
func (bs *BlockSource) scheduleBackupRequest(slot uint64) bool {
	select {
	case bs.workQueue <- slot:
		bs.stats.SpeculativeRetries.Add(1) // Only count if actually queued
		return true
	case <-bs.stopChan:
		return false
	default:
		// Work queue full, skip backup request
		return false
	}
}

// getStaleSlots returns slots that have been inflight for longer than threshold
func (bs *BlockSource) getStaleSlots(threshold time.Duration) []uint64 {
	bs.slotStateMu.Lock()
	defer bs.slotStateMu.Unlock()

	now := time.Now()
	var stale []uint64
	for slot, status := range bs.slotState {
		if status == slotInflight {
			if start, ok := bs.inflightStart[slot]; ok {
				if now.Sub(start) > threshold {
					stale = append(stale, slot)
				}
			}
		}
	}
	return stale
}

// scheduler feeds slots to the work queue
func (bs *BlockSource) scheduler() {
	nextToSchedule := bs.startSlot

	retryTicker := time.NewTicker(200 * time.Millisecond)
	defer retryTicker.Stop()

	// Time-based primary probe ticker (independent of progress)
	primaryProbeTicker := time.NewTicker(primaryProbeInterval)
	defer primaryProbeTicker.Stop()

	// Track which slots we've already sent backup requests for
	backupSent := make(map[uint64]bool)

	for {
		// Check for shutdown
		if bs.stopped.Load() {
			return
		}

		// Check for stall timeout - if no progress for too long, trigger graceful shutdown
		lastProgressTime := time.Unix(bs.lastProgress.Load(), 0)
		if time.Since(lastProgressTime) > bs.stallTimeout {
			bs.reorderMu.Lock()
			waitingSlot := bs.nextSlotToSend
			bs.reorderMu.Unlock()

			// Last-chance probe: if we're on a backup, try primary before giving up
			// This handles the case where backup can't serve getBlock but primary recovered
			if !bs.isOnPrimary() && len(bs.rpcClients) > 1 {
				mlog.Log.Infof("Block fetch stalled on backup endpoint - probing primary before shutdown...")
				if bs.restoreToPrimary() {
					mlog.Log.Infof("Primary RPC restored, resetting stall timer and continuing")
					bs.lastProgress.Store(time.Now().Unix())
					continue // Give primary a chance
				}
				mlog.Log.Infof("Primary RPC still unavailable, proceeding with shutdown")
			}

			mlog.Log.Errorf("FATAL: Block fetch stalled for %v - no progress since slot %d",
				bs.stallTimeout, waitingSlot)
			mlog.Log.Errorf("This indicates persistent network issues or RPC unavailability.")
			mlog.Log.Errorf("Triggering graceful shutdown to preserve AccountsDB state.")

			bs.stallError.Store(true)
			return // Exit scheduler, which triggers shutdown
		}

		// Check if all slots are done
		bs.reorderMu.Lock()
		allDone := bs.nextSlotToSend >= bs.endSlot
		bs.reorderMu.Unlock()

		if allDone {
			return
		}

		// Process retry slots, backup requests, and primary probes on tickers
		select {
		case <-primaryProbeTicker.C:
			// Time-based primary probe - try to restore to primary every minute when on backup
			// This runs independently of block progress, so we'll try primary even if stalled
			if !bs.isOnPrimary() && len(bs.rpcClients) > 1 {
				if bs.restoreToPrimary() {
					// Primary restored! Reset stall timer since we have a fresh endpoint to try
					bs.lastProgress.Store(time.Now().Unix())
				}
			}
		case <-retryTicker.C:
			// Handle normal retries
			for _, slot := range bs.getRetrySlots() {
				if bs.canScheduleMore(slot) {
					bs.scheduleSlot(slot)
				} else {
					bs.scheduleRetry(slot) // Put back
				}
			}

			// Send backup requests for stale slots (>1 second old)
			for _, slot := range bs.getStaleSlots(1 * time.Second) {
				if !backupSent[slot] {
					if bs.scheduleBackupRequest(slot) {
						backupSent[slot] = true // Only mark if actually queued
					}
				}
			}

			// Clean up backupSent for completed or retried slots
			// Clear when slot is done OR no longer inflight (was retried/reset)
			bs.slotStateMu.Lock()
			for slot := range backupSent {
				state, exists := bs.slotState[slot]
				if state == slotDone || !exists || state != slotInflight {
					delete(backupSent, slot)
				}
			}
			bs.slotStateMu.Unlock()
		default:
		}

		// Schedule new slots if we have capacity
		if bs.canScheduleMore(nextToSchedule) && nextToSchedule < bs.endSlot {
			if bs.scheduleSlot(nextToSchedule) {
				nextToSchedule++
			} else {
				// Channel full or stopped, wait a bit
				time.Sleep(10 * time.Millisecond)
			}
		} else {
			// At capacity, wait a bit
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// DownloadInitialBlocks is kept for backward compatibility but is a no-op
// The parallel scheduler handles initial prefetch naturally
func (bs *BlockSource) DownloadInitialBlocks() {
	// No-op - parallel scheduler handles this
}

// Start begins parallel block fetching
func (bs *BlockSource) Start() {
	// For non-RPC sources, use the old sequential approach
	if bs.sourceType != BlockSourceRpc {
		bs.startSequential()
		return
	}

	mlog.Log.Infof("starting parallel block fetch: rps=%d workers=%d safety_margin=%d",
		int(bs.rateLimiter.Limit()), bs.maxInflight, bs.tipSafetyMargin)

	// Start tip poller
	go bs.pollTip()

	// Wait for initial tip
	time.Sleep(100 * time.Millisecond)

	// Start workers
	var wg sync.WaitGroup
	for i := 0; i < bs.maxInflight; i++ {
		wg.Add(1)
		go bs.worker(&wg, i)
	}

	// Start emitter
	emitterDone := make(chan struct{})
	go func() {
		bs.emitOrderedBlocks()
		close(emitterDone)
	}()

	// Start scheduler (runs until all slots done)
	bs.scheduler()

	// Shutdown
	bs.stopped.Store(true)
	close(bs.stopChan)
	close(bs.workQueue)
	wg.Wait()
	close(bs.resultQueue)
	<-emitterDone
	close(bs.streamChan)
}

// startSequential is the old sequential approach for non-RPC sources
func (bs *BlockSource) startSequential() {
	for ; bs.currentSlot < bs.endSlot; bs.currentSlot++ {
		newBlock, _ := bs.fetchAndParseBlockSequential(bs.currentSlot)
		if newBlock != nil {
			bs.streamChan <- newBlock
		}
	}
	close(bs.streamChan)
}

// fetchAndParseBlockSequential is the old sequential fetch with retry
func (bs *BlockSource) fetchAndParseBlockSequential(slot uint64) (*b.Block, error) {
	var err error
	var blockResult *rpc.GetBlockResult
	var blk *b.Block

	if bs.sourceType == BlockSourceFile {
		blk, err = bs.tryGetBlockFromFile(slot)
		if err != nil {
			rpc := bs.getActiveRpc()
			for {
				// Use single-attempt fetch to avoid inner retry loop bypassing rate limits
				blockResult, err = rpc.GetBlockConfirmedOnce(uint64(slot))
				if err == nil {
					break
				} else if err == rpcclient.SlotSkipped {
					return nil, err
				} else if isSlotNotAvailableErr(err) {
					time.Sleep(500 * time.Millisecond)
				} else if isRateLimitedErr(err) {
					time.Sleep(2 * time.Second)
				} else if isHardConnectivityErr(err) || isTransientNetworkErr(err) {
					time.Sleep(1 * time.Second)
				} else {
					return nil, fmt.Errorf("error fetching block: %w", err)
				}
			}
			blk = block.FromBlockResult(blockResult, slot, rpc)
		}
	} else if bs.sourceType == BlockSourceOvercast {
		// NOTE: BlockSourceOvercast is TEMPORARILY NON-FUNCTIONAL.
		// The background block downloader that populated files for this path was removed.
		// This code path will return SlotSkipped for every slot until Overcast streaming
		// is re-implemented as a push-based source feeding directly into the reorder buffer.
		// TODO: Implement Overcast as a streaming source (gRPC stream -> reorder buffer)
		blk, err = bs.tryGetBlockFromFile(slot)
		if err != nil {
			return nil, rpcclient.SlotSkipped
		}
	}

	return blk, nil
}

func (bs *BlockSource) NextBlock() *b.Block {
	block := <-bs.streamChan
	return block
}

func (bs *BlockSource) BufferDepth() int {
	return len(bs.streamChan)
}

// Stalled returns true if the block source stalled due to persistent fetch failures.
// When true, the caller should trigger a graceful shutdown to preserve AccountsDB state.
func (bs *BlockSource) Stalled() bool {
	return bs.stallError.Load()
}

// StallTimeout returns the configured stall timeout duration.
func (bs *BlockSource) StallTimeout() time.Duration {
	return bs.stallTimeout
}

// FetchStatsSnapshot returns a snapshot of fetch stats for logging
type FetchStatsSnapshot struct {
	Attempts           uint64
	Successes          uint64
	Retries            uint64
	Skipped            uint64
	SpeculativeRetries uint64 // Backup requests sent for slow slots
	AvgLatencyMs       float64
	ErrNotAvail        uint64
	ErrRateLimit       uint64
	ErrBeyondTip       uint64
	ErrTransient       uint64 // EOF, timeout, 502/503, connection reset, etc.
	ErrOther           uint64
	BufferDepth        int
	LeadSlots          int64 // MaxBufferedSlot - NextSlotToSend
	NextSlot           uint64
	MaxBuffered        uint64
	ConfirmedTip       uint64
	ProcessedTip       uint64 // Processed commitment tip (super tip)
	TipAtSlot          uint64 // What slot we were emitting when tip was measured
	WorkQueueLen       int
	ReorderBufLen      int
	// Tip poll health
	TipStaleSecs      int64  // Seconds since last successful tip update (0 = healthy)
	TipPollFailures   uint64 // Consecutive tip poll failures
	TotalTipPollFails uint64 // Total tip poll failures this window
	// Mode tracking
	IsNearTip bool // True when in near-tip mode (low-latency, just-in-time scheduling)
}

// GetFetchStats returns a snapshot of current fetch statistics
func (bs *BlockSource) GetFetchStats() FetchStatsSnapshot {
	attempts := bs.stats.FetchAttempts.Load()
	successes := bs.stats.FetchSuccesses.Load()
	latencyCount := bs.stats.FetchLatencyCount.Load()
	totalLatencyNs := bs.stats.TotalFetchLatencyNs.Load()

	var avgLatencyMs float64
	if latencyCount > 0 {
		avgLatencyMs = float64(totalLatencyNs) / float64(latencyCount) / 1e6
	}

	bs.reorderMu.Lock()
	nextSlot := bs.nextSlotToSend
	reorderLen := len(bs.reorderBuffer)
	bs.reorderMu.Unlock()

	maxBuffered := bs.stats.MaxBufferedSlot.Load()
	var leadSlots int64
	if maxBuffered >= nextSlot {
		leadSlots = int64(maxBuffered - nextSlot)
	}

	// Calculate tip staleness
	var tipStaleSecs int64
	lastTipUpdate := bs.lastTipUpdate.Load()
	if lastTipUpdate > 0 {
		tipStaleSecs = time.Now().Unix() - lastTipUpdate
	}

	return FetchStatsSnapshot{
		Attempts:           attempts,
		Successes:          successes,
		Retries:            bs.stats.FetchRetries.Load(),
		Skipped:            bs.stats.FetchSkipped.Load(),
		SpeculativeRetries: bs.stats.SpeculativeRetries.Load(),
		AvgLatencyMs:       avgLatencyMs,
		ErrNotAvail:        bs.stats.ErrSlotNotAvail.Load(),
		ErrRateLimit:       bs.stats.ErrRateLimited.Load(),
		ErrBeyondTip:       bs.stats.ErrBeyondTip.Load(),
		ErrTransient:       bs.stats.ErrTransient.Load(),
		ErrOther:           bs.stats.ErrOther.Load(),
		BufferDepth:        len(bs.streamChan),
		LeadSlots:          leadSlots,
		NextSlot:           nextSlot,
		MaxBuffered:        maxBuffered,
		ConfirmedTip:       bs.confirmedTip.Load(),
		ProcessedTip:       bs.processedTip.Load(),
		TipAtSlot:          bs.tipAtSlot.Load(),
		WorkQueueLen:       len(bs.workQueue),
		ReorderBufLen:      reorderLen,
		TipStaleSecs:       tipStaleSecs,
		TipPollFailures:    bs.tipPollFailures.Load(),
		TotalTipPollFails:  bs.totalTipPollFails.Load(),
		IsNearTip:          bs.isNearTip.Load(),
	}
}

// ResetStats resets the fetch statistics (useful between 100-slot windows)
func (bs *BlockSource) ResetStats() {
	bs.stats.FetchAttempts.Store(0)
	bs.stats.FetchSuccesses.Store(0)
	bs.stats.FetchRetries.Store(0)
	bs.stats.FetchSkipped.Store(0)
	bs.stats.SpeculativeRetries.Store(0)
	bs.stats.ErrSlotNotAvail.Store(0)
	bs.stats.ErrRateLimited.Store(0)
	bs.stats.ErrBeyondTip.Store(0)
	bs.stats.ErrTransient.Store(0)
	bs.stats.ErrOther.Store(0)
	bs.stats.TotalFetchLatencyNs.Store(0)
	bs.stats.FetchLatencyCount.Store(0)
	// Reset tip poll failures for this window (but keep consecutive count for alerting)
	bs.totalTipPollFails.Store(0)
}
