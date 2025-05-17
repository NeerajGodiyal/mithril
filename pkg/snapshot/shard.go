package snapshot

import (
	"encoding/binary"
	"math/rand"
	"sync"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/statsd"
	"github.com/cespare/xxhash"
	"github.com/leslie-fei/fastcache"
)

type shardedSetterReq struct {
	K    []byte
	V    []byte
	Slot uint64
}

type shardedSetter struct {
	cache      fastcache.Cache
	inputChans []chan shardedSetterReq
	wg         *sync.WaitGroup
}

// NewShardedSetter creates a new shardedSetter with the specified number of shards
func NewShardedSetter(cache fastcache.Cache, numShards int, bufsz int) *shardedSetter {
	s := &shardedSetter{
		cache:      cache,
		inputChans: make([]chan shardedSetterReq, numShards),
		wg:         &sync.WaitGroup{},
	}
	for i := 0; i < numShards; i++ {
		s.inputChans[i] = make(chan shardedSetterReq, bufsz)
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

	ch := s.inputChans[chanIndex]
	for req := range ch {
		start := time.Now()
		shouldSet := true
		dst, err := s.cache.Get(req.K)
		if err == nil {
			if len(dst) >= 8 {
				currentSlot := binary.LittleEndian.Uint64(dst[:8])
				if currentSlot >= req.Slot {
					shouldSet = false
				}
			}
		}

		if shouldSet {
			err = s.cache.Set(req.K, req.V)
			if err != nil {
				mlog.Log.Infof("failed to set value for %s: %s", req.K, err)
			}
		}

		if rand.Intn(100) == 0 {
			statsd.Timing("tasks.set_if_slot_higher.latency", time.Since(start), nil, 0.01)
			statsd.Gauge("tasks.set_if_slot_higher.queue_size", float64(len(ch)), nil, 0.01)
		}
	}
}

func (s *shardedSetter) EnqueueRequest(key []byte, value []byte, slot uint64) {
	// Calculate shard index from key hash the same way the hashmap does,
	// so each worker has exclusive access to a shard of the hashmap
	hash := xxhash.Sum64(key)
	index := hash % uint64(len(s.inputChans))

	s.inputChans[int(index)] <- shardedSetterReq{
		K:    key,
		V:    value,
		Slot: slot,
	}
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
