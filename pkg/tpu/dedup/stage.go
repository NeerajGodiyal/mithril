package dedup

import (
	"sync/atomic"

	"github.com/Overclock-Validator/mithril/pkg/tpu/packet"
	"github.com/Overclock-Validator/mithril/pkg/tpu/wire"
	"github.com/cespare/xxhash/v2"
)

// Stats tracks dedup+sanitize stage throughput.
type Stats struct {
	InPackets       uint64
	InBytes         uint64
	OutPackets      uint64
	OutBytes        uint64
	DroppedSanitize uint64
	DroppedDedup    uint64
}

// Snapshot returns a race-free copy while the stage is running.
func (s *Stats) Snapshot() Stats {
	if s == nil {
		return Stats{}
	}
	return Stats{
		InPackets:       atomic.LoadUint64(&s.InPackets),
		InBytes:         atomic.LoadUint64(&s.InBytes),
		OutPackets:      atomic.LoadUint64(&s.OutPackets),
		OutBytes:        atomic.LoadUint64(&s.OutBytes),
		DroppedSanitize: atomic.LoadUint64(&s.DroppedSanitize),
		DroppedDedup:    atomic.LoadUint64(&s.DroppedDedup),
	}
}

// Stage is a single-threaded xxhash dedup + wire sanitation funnel.
type Stage struct {
	In    <-chan packet.Packet
	Out   chan<- packet.Packet
	cache *Cache
}

// NewStage returns a dedup stage. Out should be a buffered channel.
func NewStage(in <-chan packet.Packet, out chan<- packet.Packet, cache *Cache) *Stage {
	if cache == nil {
		cache = NewCache(DefaultCacheCapacity)
	}
	return &Stage{
		In:    in,
		Out:   out,
		cache: cache,
	}
}

// Run processes packets until In is closed.
func (s *Stage) Run(stats *Stats) {
	for pkt := range s.In {
		s.handle(pkt, stats)
	}
}

func (s *Stage) handle(pkt packet.Packet, stats *Stats) {
	if s.filter(pkt, stats) {
		s.Out <- pkt
	}
}

// filter runs sanitize + dedup. Returns true when the packet should be forwarded.
// On false the packet is released.
func (s *Stage) filter(pkt packet.Packet, stats *Stats) bool {
	return FilterWire(pkt.Data(), s.cache, stats, func() { pkt.Release() })
}

// FilterWire runs sanitize+dedup on wire bytes without packet allocation.
// Returns true when the transaction would be forwarded.
func FilterWire(data []byte, cache *Cache, stats *Stats, onDrop func()) bool {
	if stats != nil {
		atomic.AddUint64(&stats.InPackets, 1)
		atomic.AddUint64(&stats.InBytes, uint64(len(data)))
	}

	view, err := wire.Sanitize(data)
	if err != nil {
		if stats != nil {
			atomic.AddUint64(&stats.DroppedSanitize, 1)
		}
		if onDrop != nil {
			onDrop()
		}
		return false
	}

	if cache.Seen(xxhash.Sum64(view.FirstSignature())) {
		if stats != nil {
			atomic.AddUint64(&stats.DroppedDedup, 1)
		}
		if onDrop != nil {
			onDrop()
		}
		return false
	}

	if stats != nil {
		atomic.AddUint64(&stats.OutPackets, 1)
		atomic.AddUint64(&stats.OutBytes, uint64(len(data)))
	}
	return true
}

// ProcessOne is for microbenchmarks and unit tests without a downstream channel.
func ProcessOne(pkt packet.Packet, cache *Cache, stats *Stats) {
	if FilterWire(pkt.Data(), cache, stats, func() { pkt.Release() }) {
		pkt.Release()
	}
}
