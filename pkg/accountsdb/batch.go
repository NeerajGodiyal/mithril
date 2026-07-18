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

func (db *AccountsDb) GetAccountsBatch(ctx context.Context, slot uint64, pks []solana.PublicKey) ([]*accounts.Account, error) {
	return db.getAccountsBatch(ctx, slot, pks)
}

// GetAccountsBatchShared is the immutable-parent fast path consumed by replay.
// AccountsDb read-cache values have always been shared; the distinct method
// makes that ownership explicit and lets an unrooted overlay avoid cloning
// those values before transaction execution copy-on-writes them.
func (db *AccountsDb) GetAccountsBatchShared(ctx context.Context, slot uint64, pks []solana.PublicKey) ([]*accounts.Account, error) {
	return db.getAccountsBatch(ctx, slot, pks)
}

func (db *AccountsDb) getAccountsBatch(ctx context.Context, slot uint64, pks []solana.PublicKey) ([]*accounts.Account, error) {
	if len(pks) == 0 {
		return nil, nil
	}

	out := db.getStoreInProgressAccounts(pks)
	cold := make([]int, 0, len(pks))
	for i, pk := range pks {
		if out[i] != nil {
			continue
		}
		if acct, ok := db.getCachedAccount(pk); ok {
			out[i] = batchAccountOrPlaceholder(pk, acct)
			continue
		}
		cold = append(cold, i)
	}
	if len(cold) == 0 {
		return out, nil
	}
	if db.Index == nil {
		for _, idx := range cold {
			out[idx] = missingAccount(pks[idx])
		}
		return out, nil
	}

	// Resolve every cold key's index location with a fixed-size worker pool.
	// The old implementation launched one goroutine per miss and merely gated
	// them with a semaphore, creating tens of thousands of goroutine stacks and
	// scheduler operations on a busy block.
	locations := make([]batchAccountLocation, len(cold))
	found := make([]bool, len(cold))
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
	if err != nil {
		return nil, err
	}

	groups := make(map[appendVecID][]batchAccountLocation)
	for i, location := range locations {
		if !found[i] {
			continue
		}
		id := appendVecID{slot: location.entry.Slot, fileID: location.entry.FileId}
		groups[id] = append(groups[id], location)
	}

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

	// Each worker opens an appendvec once per small chunk, instead of once per
	// account. A stale/unlinked compaction location falls back to the existing
	// one-account read, which re-fetches the index and retries once.
	err = runBatchWorkers(ctx, len(chunks), func(job int) error {
		chunk := chunks[job]
		path := filepath.Join(db.AcctsDir, fmt.Sprintf("%d.%d", chunk.id.slot, chunk.id.fileID))
		file, openErr := os.Open(path)
		if openErr != nil {
			return db.retryBatchLocations(ctx, slot, out, chunk.locations)
		}
		defer file.Close()

		for _, location := range chunk.locations {
			if err := ctx.Err(); err != nil {
				return err
			}
			acct, readErr := readBatchAccountAt(file, path, location)
			if readErr != nil {
				if err := db.retryBatchLocation(slot, out, location); err != nil {
					return err
				}
				continue
			}
			db.cacheReadAccount(location.pubkey, acct)
			out[location.outputIdx] = batchAccountOrPlaceholder(location.pubkey, acct)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
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

func (db *AccountsDb) retryBatchLocations(ctx context.Context, slot uint64, out []*accounts.Account, locations []batchAccountLocation) error {
	for _, location := range locations {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := db.retryBatchLocation(slot, out, location); err != nil {
			return err
		}
	}
	return nil
}

func (db *AccountsDb) retryBatchLocation(slot uint64, out []*accounts.Account, location batchAccountLocation) error {
	acct, err := db.getStoredAccount(slot, location.pubkey)
	if err != nil && err != ErrNoAccount {
		return err
	}
	out[location.outputIdx] = batchAccountOrPlaceholder(location.pubkey, acct)
	return nil
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
