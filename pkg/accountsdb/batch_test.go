package accountsdb

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/addresses"
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

func TestGetAccountsBatchStatsAndSmallBatchAdmissions(t *testing.T) {
	db, _ := newFoldTestDb(t)
	defer db.CloseDb()

	common := foldAcct(1, 100, []byte{1, 2, 3})
	vote := foldAcct(2, 200, []byte{4, 5})
	vote.Owner = addresses.VoteProgramAddr
	_, err := db.CommitBatch([]accounts.SlotDelta{{Slot: 100, Delta: []*accounts.Account{common, vote}}}, 100, nil, nil)
	require.NoError(t, err)
	db.CommonAcctsCache.Delete(common.Key)
	db.VoteAcctCache.Delete(vote.Key)
	missing := solana.PublicKey{9}
	keys := []solana.PublicKey{common.Key, vote.Key, missing}

	out, stats, err := db.GetAccountsBatchSharedWithStats(context.Background(), 100, keys)
	require.NoError(t, err)
	require.Len(t, out, len(keys))
	assert.Equal(t, uint64(len(keys)), stats.RequestedKeys)
	assert.Equal(t, uint64(len(keys)), stats.DurableKeys)
	assert.Equal(t, uint64(2), stats.IndexHits)
	assert.Equal(t, uint64(1), stats.IndexMisses)
	assert.Equal(t, uint64(1), stats.UniqueAppendVecs)
	assert.Equal(t, uint64(1), stats.AppendVecChunks)
	assert.Equal(t, uint64(2), stats.AppendVecAccounts)
	assert.Equal(t, uint64(2), stats.DecodedAccountObjects)
	assert.Equal(t, uint64(5), stats.DecodedAccountBytes)
	assert.Equal(t, uint64(1), stats.PlaceholderObjects)
	assert.Equal(t, uint64(1), stats.CommonCacheAdmissions)
	assert.Equal(t, uint64(1), stats.VoteCacheAdmissions)
	assert.Zero(t, stats.CommonCacheAdmissionsSkipped)
	assert.Positive(t, stats.IndexLookupNanoseconds)
	assert.Positive(t, stats.AppendVecReadNanoseconds)

	_, second, err := db.GetAccountsBatchSharedWithStats(context.Background(), 100, keys)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), second.CacheHits, "small batch should reuse admitted common and vote accounts")
	assert.Equal(t, uint64(1), second.IndexMisses, "negative entries are deliberately not cached yet")
}

func TestLargeBatchDoesNotFloodCommonCache(t *testing.T) {
	db, _ := newFoldTestDb(t)
	defer db.CloseDb()

	count := commonAccountCacheCapacity + 1
	delta := make([]*accounts.Account, count)
	keys := make([]solana.PublicKey, count)
	for i := range keys {
		binary.LittleEndian.PutUint32(keys[i][:4], uint32(i+1))
		delta[i] = &accounts.Account{Key: keys[i], Lamports: uint64(i + 1), Owner: [32]byte{7}}
	}
	_, err := db.CommitBatch([]accounts.SlotDelta{{Slot: 100, Delta: delta}}, 100, nil, nil)
	require.NoError(t, err)

	_, first, err := db.GetAccountsBatchSharedWithStats(context.Background(), 100, keys)
	require.NoError(t, err)
	assert.Equal(t, uint64(count), first.IndexHits)
	assert.Equal(t, uint64(count), first.CommonCacheAdmissionsSkipped)
	assert.Zero(t, first.CommonCacheAdmissions)
	assert.False(t, db.CommonAcctsCache.Has(keys[0]))
	assert.False(t, db.CommonAcctsCache.Has(keys[len(keys)-1]))

	_, second, err := db.GetAccountsBatchSharedWithStats(context.Background(), 100, keys)
	require.NoError(t, err)
	assert.Zero(t, second.CacheHits, "one-shot scan must not churn cache or masquerade as reusable heat")
	assert.Equal(t, uint64(count), second.IndexHits)
}

func TestRefreshReadCachesIsScanResistantAndCoherent(t *testing.T) {
	db, _ := newFoldTestDb(t)
	defer db.CloseDb()

	key := solana.PublicKey{1}
	cold := &accounts.Account{Key: key, Lamports: 10, Owner: [32]byte{7}}
	db.refreshReadCaches([]*accounts.Account{cold})
	assert.False(t, db.CommonAcctsCache.Has(key), "fold must not admit a cold common account")

	db.CommonAcctsCache.Set(key, cold)
	updated := &accounts.Account{Key: key, Lamports: 20, Owner: [32]byte{7}}
	db.refreshReadCaches([]*accounts.Account{updated})
	got, ok := db.CommonAcctsCache.Get(key)
	require.True(t, ok)
	assert.Same(t, updated, got, "resident common entry must be refreshed")

	vote := &accounts.Account{Key: key, Lamports: 30, Owner: addresses.VoteProgramAddr}
	db.refreshReadCaches([]*accounts.Account{vote})
	assert.False(t, db.CommonAcctsCache.Has(key), "owner transition must evict common cache")
	got, ok = db.VoteAcctCache.Get(key)
	require.True(t, ok)
	assert.Same(t, vote, got)

	backToCommon := &accounts.Account{Key: key, Lamports: 40, Owner: [32]byte{7}}
	db.refreshReadCaches([]*accounts.Account{backToCommon})
	assert.False(t, db.VoteAcctCache.Has(key), "owner transition must evict vote cache")
	assert.False(t, db.CommonAcctsCache.Has(key), "cold common transition should not be admitted")

	db.CommonAcctsCache.Set(key, backToCommon)
	db.VoteAcctCache.Set(key, vote)
	db.refreshReadCaches([]*accounts.Account{{Key: key}})
	assert.False(t, db.CommonAcctsCache.Has(key))
	assert.False(t, db.VoteAcctCache.Has(key))
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

	const accountCount = 8192
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

func BenchmarkCommonCacheBulkAdmission(b *testing.B) {
	const accountCount = 20_000
	db := &AccountsDb{}
	db.InitCaches()
	accts := make([]*accounts.Account, accountCount)
	for i := range accts {
		var key solana.PublicKey
		binary.LittleEndian.PutUint32(key[:4], uint32(i+1))
		accts[i] = &accounts.Account{Key: key, Lamports: uint64(i + 1), Owner: [32]byte{7}}
	}

	b.Run("flood-5k-cache", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			for _, acct := range accts {
				db.cacheBatchReadAccount(acct.Key, acct, true)
			}
		}
	})
	b.Run("skip-one-shot-admission", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			for _, acct := range accts {
				db.cacheBatchReadAccount(acct.Key, acct, false)
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
