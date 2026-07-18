package accountsdb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/cockroachdb/pebble"
	"github.com/gagliardetto/solana-go"
)

var systemProgramAddr [32]byte

// appendVecReadChunkSize amortizes open/close calls without serializing every
// read from a large fold segment behind one worker. Chunks are offset-sorted,
// which also gives the kernel a chance to coalesce nearby appendvec reads.
const appendVecReadChunkSize = 64

type batchAccountLocation struct {
	outputIdx int
	pubkey    solana.PublicKey
	entry     AccountIndexEntry
}

type appendVecID struct {
	slot   uint64
	fileID uint64
}

type appendVecReadChunk struct {
	id        appendVecID
	locations []batchAccountLocation
}

// BatchReadStats is one durable batch read's wall-time and work breakdown.
// Allocation counters describe logical objects created directly by this path;
// they deliberately exclude Pebble/OS/runtime internals.
type BatchReadStats struct {
	RequestedKeys uint64
	DurableKeys   uint64

	WorkingSetHits    uint64
	InProgressHits    uint64
	CacheHits         uint64
	IndexHits         uint64
	IndexMisses       uint64
	UniqueAppendVecs  uint64
	AppendVecChunks   uint64
	AppendVecAccounts uint64
	OpenFailures      uint64
	ReadFailures      uint64
	RetryAccounts     uint64

	CommonCacheAdmissions        uint64
	CommonCacheAdmissionsSkipped uint64
	VoteCacheAdmissions          uint64

	DecodedAccountObjects uint64
	DecodedAccountBytes   uint64
	PlaceholderObjects    uint64

	WorkingSetLookupNanoseconds uint64
	InProgressNanoseconds       uint64
	CacheLookupNanoseconds      uint64
	IndexLookupNanoseconds      uint64
	ReadPlanningNanoseconds     uint64
	AppendVecReadNanoseconds    uint64
}

type batchChunkReadStats struct {
	appendVecAccounts            uint64
	openFailures                 uint64
	readFailures                 uint64
	retryAccounts                uint64
	commonCacheAdmissions        uint64
	commonCacheAdmissionsSkipped uint64
	voteCacheAdmissions          uint64
	decodedAccountObjects        uint64
	decodedAccountBytes          uint64
	placeholderObjects           uint64
}

func (db *AccountsDb) GetAccountsBatch(ctx context.Context, slot uint64, pks []solana.PublicKey) ([]*accounts.Account, error) {
	out, _, err := db.getAccountsBatchWithStats(ctx, slot, pks)
	return out, err
}

// GetAccountsBatchShared is the immutable-parent fast path consumed by replay.
// AccountsDb read-cache values have always been shared; the distinct method
// makes that ownership explicit and lets an unrooted overlay avoid cloning
// those values before transaction execution copy-on-writes them.
func (db *AccountsDb) GetAccountsBatchShared(ctx context.Context, slot uint64, pks []solana.PublicKey) ([]*accounts.Account, error) {
	out, _, err := db.getAccountsBatchWithStats(ctx, slot, pks)
	return out, err
}

// GetAccountsBatchSharedWithStats is replay's instrumented immutable-parent
// path. The non-instrumented APIs above share the same implementation.
func (db *AccountsDb) GetAccountsBatchSharedWithStats(ctx context.Context, slot uint64, pks []solana.PublicKey) ([]*accounts.Account, BatchReadStats, error) {
	return db.getAccountsBatchWithStats(ctx, slot, pks)
}

func (db *AccountsDb) getAccountsBatchWithStats(ctx context.Context, slot uint64, pks []solana.PublicKey) ([]*accounts.Account, BatchReadStats, error) {
	stats := BatchReadStats{RequestedKeys: uint64(len(pks)), DurableKeys: uint64(len(pks))}
	if len(pks) == 0 {
		return nil, stats, nil
	}

	phaseStart := time.Now()
	out := db.getStoreInProgressAccounts(pks)
	stats.InProgressNanoseconds = uint64(time.Since(phaseStart).Nanoseconds())
	phaseStart = time.Now()
	cold := make([]int, 0, len(pks))
	for i, pk := range pks {
		if out[i] != nil {
			stats.InProgressHits++
			continue
		}
		if acct, ok := db.getCachedAccount(pk); ok {
			stats.CacheHits++
			if acct == nil || acct.Lamports == 0 {
				stats.PlaceholderObjects++
			}
			out[i] = batchAccountOrPlaceholder(pk, acct)
			continue
		}
		cold = append(cold, i)
	}
	stats.CacheLookupNanoseconds = uint64(time.Since(phaseStart).Nanoseconds())
	if len(cold) == 0 {
		return out, stats, nil
	}
	if db.Index == nil {
		for _, idx := range cold {
			out[idx] = missingAccount(pks[idx])
		}
		stats.IndexMisses = uint64(len(cold))
		stats.PlaceholderObjects += uint64(len(cold))
		return out, stats, nil
	}

	// Resolve every cold key's index location with a fixed-size worker pool.
	// The old implementation launched one goroutine per miss and merely gated
	// them with a semaphore, creating tens of thousands of goroutine stacks and
	// scheduler operations on a busy block.
	locations := make([]batchAccountLocation, len(cold))
	found := make([]bool, len(cold))
	phaseStart = time.Now()
	err := runBatchWorkers(ctx, len(cold), func(job int) error {
		idx := cold[job]
		pk := pks[idx]
		entryBytes, closer, err := db.Index.Get(pk[:])
		if err != nil {
			if errors.Is(err, pebble.ErrNotFound) {
				out[idx] = missingAccount(pk)
				return nil
			}
			return fmt.Errorf("index get %s: %w", pk, err)
		}
		entry, decodeErr := UnmarshalAcctIdxEntry(entryBytes)
		closer.Close()
		if decodeErr != nil {
			return fmt.Errorf("unmarshal index entry for %s: %w", pk, decodeErr)
		}
		locations[job] = batchAccountLocation{outputIdx: idx, pubkey: pk, entry: *entry}
		found[job] = true
		return nil
	})
	stats.IndexLookupNanoseconds = uint64(time.Since(phaseStart).Nanoseconds())
	if err != nil {
		return nil, stats, err
	}

	phaseStart = time.Now()
	groups := make(map[appendVecID][]batchAccountLocation)
	for i, location := range locations {
		if !found[i] {
			stats.IndexMisses++
			stats.PlaceholderObjects++
			continue
		}
		stats.IndexHits++
		id := appendVecID{slot: location.entry.Slot, fileID: location.entry.FileId}
		groups[id] = append(groups[id], location)
	}
	stats.UniqueAppendVecs = uint64(len(groups))

	chunks := make([]appendVecReadChunk, 0, len(groups))
	for id, group := range groups {
		sort.Slice(group, func(i, j int) bool {
			return group[i].entry.Offset < group[j].entry.Offset
		})
		for start := 0; start < len(group); start += appendVecReadChunkSize {
			end := min(start+appendVecReadChunkSize, len(group))
			chunks = append(chunks, appendVecReadChunk{id: id, locations: group[start:end]})
		}
	}
	stats.AppendVecChunks = uint64(len(chunks))
	stats.ReadPlanningNanoseconds = uint64(time.Since(phaseStart).Nanoseconds())
	admitCommon := len(cold) <= commonAccountCacheCapacity
	chunkStats := make([]batchChunkReadStats, len(chunks))

	// Each worker opens an appendvec once per small chunk, instead of once per
	// account. A stale/unlinked compaction location falls back to the existing
	// one-account read, which re-fetches the index and retries once.
	phaseStart = time.Now()
	err = runBatchWorkers(ctx, len(chunks), func(job int) error {
		chunk := chunks[job]
		jobStats := &chunkStats[job]
		path := filepath.Join(db.AcctsDir, fmt.Sprintf("%d.%d", chunk.id.slot, chunk.id.fileID))
		file, openErr := os.Open(path)
		if openErr != nil {
			jobStats.openFailures++
			jobStats.retryAccounts += uint64(len(chunk.locations))
			return db.retryBatchLocations(ctx, slot, out, chunk.locations, jobStats)
		}
		defer file.Close()

		for _, location := range chunk.locations {
			if err := ctx.Err(); err != nil {
				return err
			}
			acct, readErr := readBatchAccountAt(file, path, location)
			if readErr != nil {
				jobStats.readFailures++
				jobStats.retryAccounts++
				placeholder, err := db.retryBatchLocation(slot, out, location)
				if err != nil {
					return err
				}
				if placeholder {
					jobStats.placeholderObjects++
				}
				continue
			}
			jobStats.appendVecAccounts++
			jobStats.decodedAccountObjects++
			jobStats.decodedAccountBytes += uint64(len(acct.Data))
			voteAdmitted, commonAdmitted := db.cacheBatchReadAccount(location.pubkey, acct, admitCommon)
			switch {
			case voteAdmitted:
				jobStats.voteCacheAdmissions++
			case commonAdmitted:
				jobStats.commonCacheAdmissions++
			default:
				jobStats.commonCacheAdmissionsSkipped++
			}
			if acct.Lamports == 0 {
				jobStats.placeholderObjects++
			}
			out[location.outputIdx] = batchAccountOrPlaceholder(location.pubkey, acct)
		}
		return nil
	})
	stats.AppendVecReadNanoseconds = uint64(time.Since(phaseStart).Nanoseconds())
	for _, chunkStat := range chunkStats {
		stats.AppendVecAccounts += chunkStat.appendVecAccounts
		stats.OpenFailures += chunkStat.openFailures
		stats.ReadFailures += chunkStat.readFailures
		stats.RetryAccounts += chunkStat.retryAccounts
		stats.CommonCacheAdmissions += chunkStat.commonCacheAdmissions
		stats.CommonCacheAdmissionsSkipped += chunkStat.commonCacheAdmissionsSkipped
		stats.VoteCacheAdmissions += chunkStat.voteCacheAdmissions
		stats.DecodedAccountObjects += chunkStat.decodedAccountObjects
		stats.DecodedAccountBytes += chunkStat.decodedAccountBytes
		stats.PlaceholderObjects += chunkStat.placeholderObjects
	}
	if err != nil {
		return nil, stats, err
	}
	return out, stats, nil
}

func readBatchAccountAt(file *os.File, path string, location batchAccountLocation) (*accounts.Account, error) {
	if location.entry.Offset > math.MaxInt64 {
		return nil, fmt.Errorf("account offset %d overflows int64", location.entry.Offset)
	}
	offset := int64(location.entry.Offset)
	acct, err := unmarshalAcctFromAppendVecAcctHeader(io.NewSectionReader(file, offset, math.MaxInt64-offset))
	if err != nil {
		return nil, fmt.Errorf("unmarshal account at %s@%d: %w", path, location.entry.Offset, err)
	}
	if acct.Key != location.pubkey {
		return nil, fmt.Errorf("record at %s@%d holds %s (stale index entry)", path, location.entry.Offset, acct.Key)
	}
	acct.Slot = location.entry.Slot
	return acct, nil
}

func (db *AccountsDb) retryBatchLocations(ctx context.Context, slot uint64, out []*accounts.Account, locations []batchAccountLocation, stats *batchChunkReadStats) error {
	for _, location := range locations {
		if err := ctx.Err(); err != nil {
			return err
		}
		placeholder, err := db.retryBatchLocation(slot, out, location)
		if err != nil {
			return err
		}
		if placeholder {
			stats.placeholderObjects++
		}
	}
	return nil
}

func (db *AccountsDb) retryBatchLocation(slot uint64, out []*accounts.Account, location batchAccountLocation) (bool, error) {
	acct, err := db.getStoredAccount(slot, location.pubkey)
	if err != nil && err != ErrNoAccount {
		return false, err
	}
	placeholder := acct == nil || acct.Lamports == 0
	out[location.outputIdx] = batchAccountOrPlaceholder(location.pubkey, acct)
	return placeholder, nil
}

func batchAccountOrPlaceholder(pubkey solana.PublicKey, acct *accounts.Account) *accounts.Account {
	if acct == nil || acct.Lamports == 0 {
		return missingAccount(pubkey)
	}
	return acct
}

func missingAccount(pubkey solana.PublicKey) *accounts.Account {
	return &accounts.Account{Key: pubkey, Owner: systemProgramAddr, RentEpoch: math.MaxUint64}
}

// runBatchWorkers executes count indexed jobs using at most 2*GOMAXPROCS
// goroutines. The first error stops new work and is returned after active jobs
// drain. This bounds both goroutine count and scheduler traffic independently
// of block size.
func runBatchWorkers(ctx context.Context, count int, work func(int) error) error {
	if count == 0 {
		return nil
	}
	workerCount := min(count, max(1, runtime.GOMAXPROCS(0)*2))
	var next atomic.Uint64
	var stopped atomic.Bool
	var firstErr error
	var errOnce sync.Once
	setError := func(err error) {
		errOnce.Do(func() {
			firstErr = err
			stopped.Store(true)
		})
	}

	var wg sync.WaitGroup
	wg.Add(workerCount)
	for range workerCount {
		go func() {
			defer wg.Done()
			for !stopped.Load() {
				if err := ctx.Err(); err != nil {
					setError(err)
					return
				}
				job := int(next.Add(1) - 1)
				if job >= count {
					return
				}
				if err := work(job); err != nil {
					setError(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	return firstErr
}
