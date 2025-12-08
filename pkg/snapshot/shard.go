package snapshot

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Overclock-Validator/fastcache"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/statsd"
	"github.com/cespare/xxhash"
	"github.com/gagliardetto/solana-go"
	"golang.org/x/sync/semaphore"
)

var MaxConcurrentFlushers int = 16

type shardRequest struct {
	k solana.PublicKey
	v accountsdb.AccountIndexEntry
}

type shardedSetter struct {
	cache      fastcache.Cache
	inputChans []chan shardRequest
	wg         *sync.WaitGroup
}

// NewShardedSetter creates a new shardedSetter with the specified number of shards
func NewShardedSetter(cache fastcache.Cache, numShards int, bufsz int) *shardedSetter {
	s := &shardedSetter{
		cache:      cache,
		inputChans: make([]chan shardRequest, numShards),
		wg:         &sync.WaitGroup{},
	}
	for i := range numShards {
		s.inputChans[i] = make(chan shardRequest, bufsz)
	}
	s.wg.Add(len(s.inputChans))

	for i := range s.inputChans {
		go s.processRequests(i)
	}

	return s
}

// processRequests processes requests from a specific channel
func (s *shardedSetter) processRequests(chanIndex int) {
	defer s.wg.Done()

	var buf [24]byte
	ch := s.inputChans[chanIndex]
	reqCount := 0
	var start time.Time
	// Use a slightly awkward for loop, since for req := range ch {
	// called runtime.newobject
	var req shardRequest
	var ok bool
	for {
		if req, ok = <-ch; !ok {
			break
		}
		if reqCount%100 == 0 {
			start = time.Now()
		}
		shouldSet := true
		kb := req.k[:]
		req.v.Marshal(&buf)

		dst, err := s.cache.Get(kb)
		if err == nil {
			if len(dst) >= 8 {
				currentSlot := binary.LittleEndian.Uint64(dst[:8])
				if currentSlot >= req.v.Slot {
					shouldSet = false
				}
			}
		}

		if shouldSet {
			err = s.cache.Set(kb, buf[:])
			if err != nil {
				mlog.Log.Infof("failed to set value for %s: %s", req.k, err)
			}
		}

		if reqCount%100 == 0 {
			statsd.Timing(statsd.TaskSetIfSlotHigherLatency, uint64(time.Since(start)), nil, 0.01)
			statsd.Gauge(statsd.TasksSetIfSlotHigherQueueSize, float64(len(ch)), nil, 0.01)
		}
		reqCount++
	}
}

func (s *shardedSetter) EnqueueRequest(k solana.PublicKey, v accountsdb.AccountIndexEntry) {
	// Calculate shard index from key hash the same way the hashmap does,
	// so each worker has exclusive access to a shard of the hashmap
	hash := xxhash.Sum64(k[:])
	index := hash % uint64(len(s.inputChans))

	s.inputChans[int(index)] <- shardRequest{k, v}
}

// Stop closes all channels and waits for all goroutines to finish
func (s *shardedSetter) Stop() {
	// Close all channels
	for _, ch := range s.inputChans {
		close(ch)
	}

	// Wait for all goroutines to finish
	s.wg.Wait()
}

// ShardLogger manages multiple sharded log files
type ShardLogger struct {
	shards     []*shard
	filePrefix string
	wg         *sync.WaitGroup
	flushSem   *semaphore.Weighted
}

// shard represents a single log shard
type shard struct {
	id       int
	writer   *bufio.Writer
	file     *os.File
	requests chan shardRequest
	logSize  int
	ss       *shardedSetter
	flushSem *semaphore.Weighted
}

// NewShardLogger creates a new ShardLogger with the specified number of
// shards for logging entries. Entries are flushed to shardedSetter
// when log reaches a certain size or on shard closure.
func NewShardLogger(numShards int, filePrefix string, ss *shardedSetter) *ShardLogger {
	if numShards > 1000 {
		panic(fmt.Sprintf("numShards=%d > 1000 is too many shards", numShards))
	}
	sl := &ShardLogger{
		shards:     make([]*shard, numShards),
		filePrefix: filePrefix,
		wg:         &sync.WaitGroup{},
		flushSem:   semaphore.NewWeighted(int64(MaxConcurrentFlushers)),
	}

	sl.wg.Add(numShards)
	for i := range numShards {
		sl.shards[i] = newShard(i, filePrefix, ss, sl.flushSem)
		go sl.shards[i].processRequests(sl.wg)
	}

	return sl
}

// newShard creates a new shard with the given ID
func newShard(id int, filePrefix string, ss *shardedSetter, flushSem *semaphore.Weighted) *shard {
	filename := filepath.Join(filePrefix, fmt.Sprintf("%03d", id))
	file, err := os.Create(filename)
	if err != nil {
		panic(fmt.Sprintf("shardlogger setup: %v", err))
	}

	s := &shard{
		id:       id,
		writer:   bufio.NewWriter(file),
		file:     file,
		requests: make(chan shardRequest, 100),
		ss:       ss,
		flushSem: flushSem,
	}

	return s
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
		binary.LittleEndian.PutUint64(vBytes[0:8], req.v.Slot)
		binary.LittleEndian.PutUint64(vBytes[8:16], req.v.FileId)
		binary.LittleEndian.PutUint64(vBytes[16:24], req.v.Offset)
		s.writer.Write(vBytes[:24])

		s.logSize += len(req.k) + vlen
		if s.logSize > 256<<20 {
			if err := s.flushLogToCache(); err != nil {
				panic(err)
			}
		}
	}
}

func (s *shard) flushLogToCache() error {
	start := time.Now()
	s.flushSem.Acquire(context.TODO(), 1)
	waiting := time.Now()
	defer s.flushSem.Release(1)
	// Close/flush
	if err := s.writer.Flush(); err != nil {
		return fmt.Errorf("failed to flush writer: %w", err)
	}
	filename := s.file.Name()
	if err := s.file.Close(); err != nil {
		return fmt.Errorf("failed to close file: %w", err)
	}

	// Read contents from log
	file, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("failed to reopen file for reading: %w", err)
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	var buf [32 + vlen]byte
	for {
		_, err := io.ReadFull(reader, buf[:32+vlen])
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("flushLogToCache read loop: %v", err)
		}

		k := solana.PublicKey(buf[:32])
		v := accountsdb.AccountIndexEntry{
			Slot:   binary.LittleEndian.Uint64(buf[32:40]),
			FileId: binary.LittleEndian.Uint64(buf[40:48]),
			Offset: binary.LittleEndian.Uint64(buf[48:56]),
		}

		// Flush to cache
		s.ss.EnqueueRequest(k, v)
	}
	mlog.Log.Infof("log shard=%d waited %s and flushed size=%.2f MiB in %s",
		s.id,
		waiting.Sub(start),
		float64(s.logSize)/float64(1<<20),
		time.Since(start),
	)

	// Truncate file and replace file/writer pointers
	newFile, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("failed to truncate file: %w", err)
	}
	s.file = newFile
	s.writer = bufio.NewWriter(newFile)
	s.logSize = 0
	return nil
}

// EnqueueRequest adds a request to the appropriate shard
func (sl *ShardLogger) EnqueueRequest(k solana.PublicKey, v accountsdb.AccountIndexEntry) {
	hash := xxhash.Sum64(k[:])
	shardIdx := uint32(hash % uint64(len(sl.shards)))
	sl.shards[shardIdx].requests <- shardRequest{k, v}
}

// Close closes all shards and their files
func (sl *ShardLogger) Close() error {
	for _, s := range sl.shards {
		close(s.requests)
	}

	sl.wg.Wait()

	flushWg := &sync.WaitGroup{}
	flushWg.Add(len(sl.shards))
	for _, s := range sl.shards {
		go func() {
			defer flushWg.Done()
			s.flushLogToCache()
		}()
	}
	flushWg.Wait()
	return nil
}
