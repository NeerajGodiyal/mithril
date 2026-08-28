package snapshot

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/cockroachdb/pebble/objstorage/objstorageprovider"
	"github.com/cockroachdb/pebble/sstable"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/gagliardetto/solana-go"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
)

var MaxConcurrentFlushers int = DefaultSnapshotMaxConcurrentFlushers

type shardRequest struct {
	k solana.PublicKey
	v accountsdb.AccountIndexEntry
}

// ShardProgressCallback is called with (bytesDone, totalBytes) to report shard flush progress
type ShardProgressCallback func(bytesDone, totalBytes int64)

// ShardLogger manages multiple sharded log files
type ShardLogger struct {
	shards     []*shard
	filePrefix string
	wg         *sync.WaitGroup
	flushSem   *semaphore.Weighted

	// Progress tracking
	totalBytes atomic.Int64 // total bytes written to shard logs
	bytesDone  atomic.Int64 // bytes flushed to cache
	onProgress ShardProgressCallback

	// closed flag to prevent sends after Close is called (defensive)
	closed    atomic.Bool
	closeOnce sync.Once
	sendMu    sync.RWMutex
}

// shard represents a single log shard
type shard struct {
	id       int
	writer   *bufio.Writer
	file     *os.File
	requests chan shardRequest
	logSize  int
	flushSem *semaphore.Weighted
	parent   *ShardLogger // parent for progress reporting
}

// NewShardLogger creates a new ShardLogger with the specified number of
// shards for logging entries. Entries are flushed to shardedSetter
// when log reaches a certain size or on shard closure.
func NewShardLogger(numShards int, filePrefix string) *ShardLogger {
	sl, err := NewShardLoggerWithError(numShards, filePrefix)
	if err != nil {
		panic(fmt.Sprintf("shardlogger setup: %v", err))
	}
	return sl
}

// NewShardLoggerWithError is the recoverable constructor used by durable
// bootstrap paths. NewShardLogger remains for callers that rely on its legacy
// panic-on-setup-failure behavior.
func NewShardLoggerWithError(numShards int, filePrefix string) (*ShardLogger, error) {
	return newShardLogger(numShards, filePrefix, os.Create)
}

func newShardLogger(numShards int, filePrefix string, createFile func(string) (*os.File, error)) (*ShardLogger, error) {
	if numShards <= 0 {
		numShards = DefaultSnapshotIndexShards
	}
	if numShards > 1000 {
		return nil, fmt.Errorf("numShards=%d > 1000 is too many shards", numShards)
	}
	flushers := snapshotMaxConcurrentFlushers()
	sl := &ShardLogger{
		shards:     make([]*shard, numShards),
		filePrefix: filePrefix,
		wg:         &sync.WaitGroup{},
		flushSem:   semaphore.NewWeighted(int64(flushers)),
	}

	for i := range numShards {
		shard, err := newShard(i, filePrefix, sl.flushSem, sl, createFile)
		if err != nil {
			for _, opened := range sl.shards[:i] {
				_ = opened.file.Close()
			}
			return nil, err
		}
		sl.shards[i] = shard
	}
	sl.wg.Add(numShards)
	for i := range numShards {
		go sl.shards[i].processRequests(sl.wg)
	}

	return sl, nil
}

// SetProgressCallback sets a callback to receive progress updates during shard flushes.
// The callback receives (bytesDone, totalBytes) and is called as bytes are flushed to cache.
func (sl *ShardLogger) SetProgressCallback(cb ShardProgressCallback) {
	sl.onProgress = cb
}

// TotalBytes returns the total bytes written to shard logs
func (sl *ShardLogger) TotalBytes() int64 {
	return sl.totalBytes.Load()
}

// BytesDone returns the bytes that have been flushed to cache
func (sl *ShardLogger) BytesDone() int64 {
	return sl.bytesDone.Load()
}

// newShard creates a new shard with the given ID
func newShard(id int, filePrefix string, flushSem *semaphore.Weighted, parent *ShardLogger, createFile func(string) (*os.File, error)) (*shard, error) {
	filename := filepath.Join(filePrefix, fmt.Sprintf("%03d", id))
	file, err := createFile(filename)
	if err != nil {
		return nil, fmt.Errorf("create shard %d: %w", id, err)
	}

	s := &shard{
		id:       id,
		writer:   bufio.NewWriter(file),
		file:     file,
		requests: make(chan shardRequest, 100),
		flushSem: flushSem,
		parent:   parent,
	}

	return s, nil
}

const vlen = /*Slot*/ 8 + /*FileId*/ 8 + /*Offset*/ 8

// processRequests handles incoming requests for a shard
func (s *shard) processRequests(wg *sync.WaitGroup) {
	defer wg.Done()
	var kBytes [32]byte
	var vBytes [vlen]byte
	for req := range s.requests {
		kBytes = [32]byte(req.k)
		s.writer.Write(kBytes[:])
		req.v.Marshal(&vBytes)
		s.writer.Write(vBytes[:24])

		bytesWritten := int64(len(req.k) + vlen)
		s.logSize += int(bytesWritten)

		// Track total bytes for progress reporting and notify callback
		if s.parent != nil {
			total := s.parent.totalBytes.Add(bytesWritten)
			if s.parent.onProgress != nil {
				// Notify with bytesDone=0 during streaming (before flush)
				// The callback can use totalBytes to show indexing progress
				s.parent.onProgress(0, total)
			}
		}
	}
}

func (s *shard) logToSST(ctx context.Context) error {
	if err := s.writer.Flush(); err != nil {
		_ = s.file.Close()
		return fmt.Errorf("failed to flush writer: %w", err)
	}
	filename := s.file.Name()
	if err := s.file.Close(); err != nil {
		return fmt.Errorf("failed to close file: %w", err)
	}
	if err := s.flushSem.Acquire(ctx, 1); err != nil {
		return fmt.Errorf("acquiring flush semaphore: %w", err)
	}
	defer s.flushSem.Release(1)

	// Read contents from log
	file, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("failed to reopen file for reading: %w", err)
	}
	inputOpen := true
	defer func() {
		if inputOpen {
			_ = file.Close()
		}
	}()
	fileInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", filename, err)
	}
	size := fileInfo.Size()

	const recordSize = int64(32 + vlen)
	if rem := size % recordSize; rem != 0 {
		return fmt.Errorf("filename=%s had (size=%d) %% (recordSize=%d) = %d", filename, size, recordSize, rem)
	}
	i := 0
	pairs := make([]shardRequest, size/recordSize)

	reader := bufio.NewReader(file)
	var buf [32 + vlen]byte
	for {
		_, err := io.ReadFull(reader, buf[:32+vlen])
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("logToSST read loop: %v", err)
		}
		pairs[i].k = solana.PublicKey(buf[:32])
		pairs[i].v.Unmarshal((*[24]byte)(buf[32:56]))
		i++

		// Track progress
		if s.parent != nil {
			done := s.parent.bytesDone.Add(recordSize)
			if s.parent.onProgress != nil {
				s.parent.onProgress(done, s.parent.totalBytes.Load())
			}
		}
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close shard log %s: %w", filename, err)
	}
	inputOpen = false

	// Sort
	slices.SortFunc(pairs, func(a, b shardRequest) int {
		if c := bytes.Compare(a.k[:], b.k[:]); c != 0 {
			return c
		}
		// Make the bigger slot appear first.
		if a.v.Slot > b.v.Slot {
			return -1
		} else if a.v.Slot == b.v.Slot {
			return 0
		} else {
			return 1
		}
	})
	var vBytes [vlen]byte
	// Write to SST
	sstFilename := fmt.Sprintf("%s.sst", filename)
	sstFile, err := vfs.Default.Create(sstFilename)
	if err != nil {
		return fmt.Errorf("create %s: %w", sstFilename, err)
	}
	w := sstable.NewWriter(objstorageprovider.NewFileWritable(sstFile), sstable.WriterOptions{})
	lastWritten := -1
	for i, kv := range pairs {
		if lastWritten >= 0 && bytes.Equal(kv.k[:], pairs[lastWritten].k[:]) {
			continue
		}
		kv.v.Marshal(&vBytes)
		if err := w.Set(kv.k[:], vBytes[:]); err != nil {
			_ = w.Close()
			return fmt.Errorf("writing to SST: %w", err)
		}
		lastWritten = i
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("finish shard SST: %w", err)
	}
	if err := os.Remove(filename); err != nil {
		return fmt.Errorf("remove shard log %s: %w", filename, err)
	}
	return nil
}

// EnqueueRequest adds a request to the appropriate shard
func (sl *ShardLogger) EnqueueRequest(k solana.PublicKey, v accountsdb.AccountIndexEntry) {
	sl.sendMu.RLock()
	defer sl.sendMu.RUnlock()
	if sl.closed.Load() {
		mlog.Log.Errorf("unexpectedly still receiving requests after shard logger is closed!")
		return // Already closed
	}
	keyPrefix := binary.BigEndian.Uint64(k[:8])
	shardSize := math.MaxUint64 / uint64(len(sl.shards))
	shardIdx := int(keyPrefix / shardSize)
	if shardIdx >= len(sl.shards) {
		shardIdx = len(sl.shards) - 1
	}
	sl.shards[shardIdx].requests <- shardRequest{k, v}
}

// Close closes all shards and their files
func (sl *ShardLogger) Close(ctx context.Context) error {
	return sl.CloseWithProgress(ctx, nil)
}

// CloseWithProgress closes all shards with optional progress callback.
// The callback is called after each shard flush completes with (completed, total) counts.
func (sl *ShardLogger) CloseWithProgress(ctx context.Context, onProgress func(completed, total int)) error {
	sl.stopRequests()

	total := len(sl.shards)
	var completed atomic.Int32

	flushWg := &errgroup.Group{}
	for _, s := range sl.shards {
		flushWg.Go(func() error {
			err := s.logToSST(ctx)
			if onProgress != nil {
				onProgress(int(completed.Add(1)), total)
			}
			return err
		})
	}
	return flushWg.Wait()
}

// Abort stops the shard workers and closes their files without producing SSTs.
// It is safe after CloseWithProgress and is used when bootstrap fails early.
func (sl *ShardLogger) Abort() {
	if sl == nil {
		return
	}
	sl.stopRequests()
	for _, s := range sl.shards {
		_ = s.file.Close()
	}
}

func (sl *ShardLogger) stopRequests() {
	sl.closeOnce.Do(func() {
		sl.sendMu.Lock()
		sl.closed.Store(true)
		for _, s := range sl.shards {
			close(s.requests)
		}
		sl.sendMu.Unlock()
		sl.wg.Wait()
	})
}
