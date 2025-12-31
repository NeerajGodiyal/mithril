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
	RpcClient  *rpcclient.RpcClient
	SourceType BlockSourceType
	StartSlot  uint64
	EndSlot    uint64
	BlockDir   string

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
	skipped bool // true if SlotSkipped error
}

var errBeyondTip = errors.New("slot beyond confirmed tip")

// BlockSourceStats contains metrics for parallel block fetching
type BlockSourceStats struct {
	// Fetch counts
	FetchAttempts  atomic.Uint64
	FetchSuccesses atomic.Uint64
	FetchRetries   atomic.Uint64
	FetchSkipped   atomic.Uint64

	// Error buckets
	ErrSlotNotAvail atomic.Uint64
	ErrRateLimited  atomic.Uint64
	ErrBeyondTip    atomic.Uint64
	ErrOther        atomic.Uint64

	// Latency tracking (nanoseconds)
	TotalFetchLatencyNs atomic.Uint64
	FetchLatencyCount   atomic.Uint64

	// Buffer stats
	MaxBufferedSlot atomic.Uint64
}

type BlockSource struct {
	rpcClient   *rpcclient.RpcClient
	streamChan  chan *b.Block
	startSlot   uint64
	endSlot     uint64
	currentSlot uint64
	blockDir    string
	sourceType  BlockSourceType

	// Rate limiting
	rateLimiter *rate.Limiter
	maxInflight int

	// Tip tracking
	confirmedTip    atomic.Uint64
	tipSafetyMargin uint64
	tipPollInterval time.Duration

	// Reorder buffer
	reorderMu      sync.Mutex
	reorderBuffer  map[uint64]*b.Block
	skippedSlots   map[uint64]bool
	nextSlotToSend uint64
	maxPending     int

	// Slot state tracking (prevents duplicates)
	slotStateMu sync.Mutex
	slotState   map[uint64]slotStatus

	// Retry queue for slots that need rescheduling
	retryMu    sync.Mutex
	retrySlots []uint64

	// Worker coordination
	workQueue   chan uint64
	resultQueue chan fetchResult
	stopChan    chan struct{}
	stopped     atomic.Bool

	// Stats tracking
	stats BlockSourceStats
}

// Default values
const (
	defaultMaxRPS          = 10
	defaultMaxInflight     = 10
	defaultTipPollMs       = 1000
	defaultTipSafetyMargin = 64
	defaultMaxPending      = 500
	streamChanBuffer       = 100
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

	bs := &BlockSource{
		rpcClient:       opts.RpcClient,
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
		workQueue:       make(chan uint64, maxInflight*2),
		resultQueue:     make(chan fetchResult, maxInflight*2),
		stopChan:        make(chan struct{}),
	}

	return bs
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

// fetchBlockOnce fetches a single block without internal retry loop
func (bs *BlockSource) fetchBlockOnce(slot uint64) (*b.Block, error) {
	// Try file first
	if blk, err := bs.tryGetBlockFromFile(slot); err == nil {
		return blk, nil
	}

	// Single RPC attempt (no internal retry - scheduler handles retries)
	blockResult, err := bs.rpcClient.GetBlockConfirmedOnce(slot)
	if err != nil {
		return nil, err
	}

	return block.FromBlockResult(blockResult, slot, bs.rpcClient), nil
}

// pollTip periodically updates the confirmed tip
func (bs *BlockSource) pollTip() {
	// Get initial tip
	if tip, err := bs.rpcClient.GetSlot(); err == nil {
		bs.confirmedTip.Store(tip)
	}

	ticker := time.NewTicker(bs.tipPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-bs.stopChan:
			return
		case <-ticker.C:
			if tip, err := bs.rpcClient.GetSlot(); err == nil {
				bs.confirmedTip.Store(tip)
			}
		}
	}
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

		// Tip underflow guard
		tip := bs.confirmedTip.Load()
		var maxSlot uint64
		if tip <= bs.tipSafetyMargin {
			maxSlot = 0
		} else {
			maxSlot = tip - bs.tipSafetyMargin
		}

		// Beyond tip - send back for retry
		if maxSlot > 0 && slot > maxSlot {
			bs.stats.ErrBeyondTip.Add(1)
			bs.resultQueue <- fetchResult{slot: slot, err: errBeyondTip}
			continue
		}

		// Fetch block with latency tracking
		bs.stats.FetchAttempts.Add(1)
		fetchStart := time.Now()
		blk, err := bs.fetchBlockOnce(slot)
		fetchLatency := time.Since(fetchStart)

		// Track latency for successful fetches
		if err == nil {
			bs.stats.TotalFetchLatencyNs.Add(uint64(fetchLatency.Nanoseconds()))
			bs.stats.FetchLatencyCount.Add(1)
		}

		skipped := err == rpcclient.SlotSkipped
		bs.resultQueue <- fetchResult{slot: slot, block: blk, err: err, skipped: skipped}
	}
}

// emitOrderedBlocks receives results and emits blocks in order
func (bs *BlockSource) emitOrderedBlocks() {
	for result := range bs.resultQueue {
		bs.reorderMu.Lock()

		// Track error buckets
		if result.err != nil {
			if result.err == errBeyondTip {
				// Already tracked in worker
			} else if isSlotNotAvailableErr(result.err) {
				bs.stats.ErrSlotNotAvail.Add(1)
			} else if isRateLimitedErr(result.err) {
				bs.stats.ErrRateLimited.Add(1)
			} else if result.err != rpcclient.SlotSkipped {
				bs.stats.ErrOther.Add(1)
			}
		}

		// Handle result
		isRetriable := result.err == errBeyondTip || isSlotNotAvailableErr(result.err) || isRateLimitedErr(result.err)

		if result.skipped {
			bs.stats.FetchSkipped.Add(1)
			bs.skippedSlots[result.slot] = true
		} else if isRetriable {
			// Schedule retry for transient errors
			bs.stats.FetchRetries.Add(1)
			bs.scheduleRetry(result.slot)
		} else if result.err != nil {
			// Other error - log and mark as skipped
			mlog.Log.Errorf("block fetch error slot=%d: %v", result.slot, result.err)
			bs.skippedSlots[result.slot] = true
		} else if result.block != nil {
			bs.stats.FetchSuccesses.Add(1)
			bs.reorderBuffer[result.slot] = result.block
			// Track max buffered slot
			if result.slot > bs.stats.MaxBufferedSlot.Load() {
				bs.stats.MaxBufferedSlot.Store(result.slot)
			}
		}

		// Mark slot done (even on error), unless retriable
		bs.slotStateMu.Lock()
		if !isRetriable {
			bs.slotState[result.slot] = slotDone
		} else {
			// Will be retried, reset to pending
			delete(bs.slotState, result.slot)
		}
		bs.slotStateMu.Unlock()

		// Emit consecutive blocks
		for {
			if blk, ok := bs.reorderBuffer[bs.nextSlotToSend]; ok {
				delete(bs.reorderBuffer, bs.nextSlotToSend)
				bs.reorderMu.Unlock()
				bs.streamChan <- blk
				bs.reorderMu.Lock()
				bs.nextSlotToSend++
			} else if bs.skippedSlots[bs.nextSlotToSend] {
				// Slot was skipped, advance without emitting
				delete(bs.skippedSlots, bs.nextSlotToSend)
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

// canScheduleMore returns true if we can schedule more slots
func (bs *BlockSource) canScheduleMore() bool {
	bs.reorderMu.Lock()
	pending := len(bs.reorderBuffer)
	bs.reorderMu.Unlock()
	return pending < bs.maxPending
}

// scheduleSlot schedules a slot if not already scheduled
func (bs *BlockSource) scheduleSlot(slot uint64) bool {
	bs.slotStateMu.Lock()
	if _, exists := bs.slotState[slot]; exists {
		bs.slotStateMu.Unlock()
		return false // Already scheduled
	}
	bs.slotState[slot] = slotInflight
	bs.slotStateMu.Unlock()

	select {
	case bs.workQueue <- slot:
		return true
	case <-bs.stopChan:
		return false
	}
}

// scheduler feeds slots to the work queue
func (bs *BlockSource) scheduler() {
	nextToSchedule := bs.startSlot

	retryTicker := time.NewTicker(200 * time.Millisecond)
	defer retryTicker.Stop()

	for {
		// Check for shutdown
		if bs.stopped.Load() {
			return
		}

		// Check if all slots are done
		bs.reorderMu.Lock()
		allDone := bs.nextSlotToSend >= bs.endSlot
		bs.reorderMu.Unlock()

		if allDone {
			return
		}

		// Process retry slots first
		select {
		case <-retryTicker.C:
			for _, slot := range bs.getRetrySlots() {
				if bs.canScheduleMore() {
					bs.scheduleSlot(slot)
				} else {
					bs.scheduleRetry(slot) // Put back
				}
			}
		default:
		}

		// Schedule new slots if we have capacity
		if bs.canScheduleMore() && nextToSchedule < bs.endSlot {
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
			for {
				blockResult, err = bs.rpcClient.GetBlockConfirmed(uint64(slot))
				if err == nil {
					break
				} else if err == rpcclient.SlotSkipped {
					return nil, err
				} else if isSlotNotAvailableErr(err) {
					time.Sleep(500 * time.Millisecond)
				} else if isRateLimitedErr(err) {
					time.Sleep(2 * time.Second)
				} else {
					return nil, fmt.Errorf("error fetching block: %w", err)
				}
			}
			blk = block.FromBlockResult(blockResult, slot, bs.rpcClient)
		}
	} else if bs.sourceType == BlockSourceOvercast {
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

// FetchStatsSnapshot returns a snapshot of fetch stats for logging
type FetchStatsSnapshot struct {
	Attempts      uint64
	Successes     uint64
	Retries       uint64
	Skipped       uint64
	AvgLatencyMs  float64
	ErrNotAvail   uint64
	ErrRateLimit  uint64
	ErrBeyondTip  uint64
	ErrOther      uint64
	BufferDepth   int
	LeadSlots     int64 // MaxBufferedSlot - NextSlotToSend
	NextSlot      uint64
	MaxBuffered   uint64
	ConfirmedTip  uint64
	WorkQueueLen  int
	ReorderBufLen int
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

	return FetchStatsSnapshot{
		Attempts:      attempts,
		Successes:     successes,
		Retries:       bs.stats.FetchRetries.Load(),
		Skipped:       bs.stats.FetchSkipped.Load(),
		AvgLatencyMs:  avgLatencyMs,
		ErrNotAvail:   bs.stats.ErrSlotNotAvail.Load(),
		ErrRateLimit:  bs.stats.ErrRateLimited.Load(),
		ErrBeyondTip:  bs.stats.ErrBeyondTip.Load(),
		ErrOther:      bs.stats.ErrOther.Load(),
		BufferDepth:   len(bs.streamChan),
		LeadSlots:     leadSlots,
		NextSlot:      nextSlot,
		MaxBuffered:   maxBuffered,
		ConfirmedTip:  bs.confirmedTip.Load(),
		WorkQueueLen:  len(bs.workQueue),
		ReorderBufLen: reorderLen,
	}
}

// ResetStats resets the fetch statistics (useful between 100-slot windows)
func (bs *BlockSource) ResetStats() {
	bs.stats.FetchAttempts.Store(0)
	bs.stats.FetchSuccesses.Store(0)
	bs.stats.FetchRetries.Store(0)
	bs.stats.FetchSkipped.Store(0)
	bs.stats.ErrSlotNotAvail.Store(0)
	bs.stats.ErrRateLimited.Store(0)
	bs.stats.ErrBeyondTip.Store(0)
	bs.stats.ErrOther.Store(0)
	bs.stats.TotalFetchLatencyNs.Store(0)
	bs.stats.FetchLatencyCount.Store(0)
}
