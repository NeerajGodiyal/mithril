package accountsdb

import (
	"context"
	"errors"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetAccountsBatchGroupedReadsPreserveOrder(t *testing.T) {
	db, _ := newFoldTestDb(t)
	defer db.CloseDb()

	const accountCount = appendVecReadChunkSize*2 + 7
	delta := make([]*accounts.Account, accountCount)
	keys := make([]solana.PublicKey, accountCount)
	for i := range delta {
		keys[i][0] = byte(i + 1)
		keys[i][1] = byte((i + 1) >> 8)
		delta[i] = &accounts.Account{
			Key:       keys[i],
			Lamports:  uint64(1000 + i),
			Owner:     [32]byte{7},
			RentEpoch: uint64(i),
			Data:      []byte{byte(i), byte(i >> 8)},
		}
	}
	_, err := db.CommitBatch([]accounts.SlotDelta{{Slot: 100, Delta: delta}}, 100, nil, nil)
	require.NoError(t, err)
	for _, key := range keys {
		db.CommonAcctsCache.Delete(key)
		db.VoteAcctCache.Delete(key)
	}

	missing := solana.PublicKey{0xff, 0xff}
	request := []solana.PublicKey{keys[accountCount-1], missing, keys[0], keys[64], keys[64]}
	out, err := db.GetAccountsBatch(context.Background(), 100, request)
	require.NoError(t, err)
	require.Len(t, out, len(request))
	assert.Equal(t, request[0], out[0].Key)
	assert.Equal(t, uint64(1000+accountCount-1), out[0].Lamports)
	assert.Equal(t, missing, out[1].Key)
	assert.Zero(t, out[1].Lamports)
	assert.Equal(t, uint64(^uint64(0)), out[1].RentEpoch)
	assert.Equal(t, uint64(1000), out[2].Lamports)
	assert.Equal(t, uint64(1064), out[3].Lamports)
	assert.Equal(t, out[3].Data, out[4].Data)
}

func TestRunBatchWorkersBoundsConcurrency(t *testing.T) {
	const jobs = 10_000
	var active atomic.Int64
	var peak atomic.Int64
	var completed atomic.Int64
	err := runBatchWorkers(context.Background(), jobs, func(int) error {
		now := active.Add(1)
		for {
			old := peak.Load()
			if now <= old || peak.CompareAndSwap(old, now) {
				break
			}
		}
		runtime.Gosched()
		completed.Add(1)
		active.Add(-1)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, int64(jobs), completed.Load())
	assert.LessOrEqual(t, peak.Load(), int64(max(1, runtime.GOMAXPROCS(0)*2)))
}

func TestRunBatchWorkersReturnsContextAndWorkErrors(t *testing.T) {
	want := errors.New("boom")
	err := runBatchWorkers(context.Background(), 100, func(job int) error {
		if job == 0 {
			return want
		}
		return nil
	})
	assert.ErrorIs(t, err, want)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = runBatchWorkers(ctx, 1, func(int) error {
		t.Fatal("cancelled batch must not start work")
		return nil
	})
	assert.ErrorIs(t, err, context.Canceled)
}

func BenchmarkGetAccountsBatchColdFoldSegment(b *testing.B) {
	db, _ := newFoldTestDb(b)
	defer db.CloseDb()

	const accountCount = 4096
	delta := make([]*accounts.Account, accountCount)
	storedKeys := make([]solana.PublicKey, accountCount)
	keys := make([]solana.PublicKey, accountCount)
	for i := range delta {
		storedKeys[i][0] = byte(i)
		storedKeys[i][1] = byte(i >> 8)
		delta[i] = &accounts.Account{
			Key:       storedKeys[i],
			Lamports:  uint64(i + 1),
			Owner:     [32]byte{7},
			RentEpoch: math.MaxUint64,
			Data:      make([]byte, 64),
		}
	}
	for i := range keys {
		// Request in a deterministic permutation rather than append order.
		requestIdx := (i * 4051) & (accountCount - 1)
		keys[i] = storedKeys[requestIdx]
	}
	_, err := db.CommitBatch([]accounts.SlotDelta{{Slot: 100, Delta: delta}}, 100, nil, nil)
	require.NoError(b, err)

	evict := func() {
		for _, key := range keys {
			db.CommonAcctsCache.Delete(key)
			db.VoteAcctCache.Delete(key)
		}
	}

	b.Run("former-goroutine-per-account", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			b.StopTimer()
			evict()
			b.StartTimer()
			out, err := legacyAccountsBatchForBenchmark(context.Background(), db, 100, keys)
			if err != nil || len(out) != len(keys) {
				b.Fatalf("legacy batch: len=%d err=%v", len(out), err)
			}
		}
	})

	b.Run("bounded-grouped-read", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			b.StopTimer()
			evict()
			b.StartTimer()
			out, err := db.GetAccountsBatch(context.Background(), 100, keys)
			if err != nil || len(out) != len(keys) {
				b.Fatalf("grouped batch: len=%d err=%v", len(out), err)
			}
		}
	})
}

// legacyAccountsBatchForBenchmark preserves the previous implementation only
// as a benchmark baseline: one goroutine per cold key, gated by a semaphore.
func legacyAccountsBatchForBenchmark(ctx context.Context, db *AccountsDb, slot uint64, keys []solana.PublicKey) ([]*accounts.Account, error) {
	out := db.getStoreInProgressAccounts(keys)
	sem := make(chan struct{}, runtime.GOMAXPROCS(0)*2)
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	for i, key := range keys {
		if out[i] != nil {
			continue
		}
		wg.Add(1)
		go func(idx int, pubkey solana.PublicKey) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := ctx.Err(); err != nil {
				errOnce.Do(func() { firstErr = err })
				return
			}
			acct, err := db.getStoredAccount(slot, pubkey)
			if err != nil && err != ErrNoAccount {
				errOnce.Do(func() { firstErr = err })
				return
			}
			out[idx] = batchAccountOrPlaceholder(pubkey, acct)
		}(i, key)
	}
	wg.Wait()
	return out, firstErr
}
