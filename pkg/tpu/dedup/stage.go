package dedup

import (
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
		stats.InPackets++
		stats.InBytes += uint64(len(data))
	}

	view, err := wire.Sanitize(data)
	if err != nil {
		if stats != nil {
			stats.DroppedSanitize++
		}
		if onDrop != nil {
			onDrop()
		}
		return false
	}

	if cache.Seen(xxhash.Sum64(view.FirstSignature())) {
		if stats != nil {
			stats.DroppedDedup++
		}
		if onDrop != nil {
			onDrop()
		}
		return false
	}

	if stats != nil {
		stats.OutPackets++
		stats.OutBytes += uint64(len(data))
	}
	return true
}

// ProcessOne is for microbenchmarks and unit tests without a downstream channel.
func ProcessOne(pkt packet.Packet, cache *Cache, stats *Stats) {
	if FilterWire(pkt.Data(), cache, stats, func() { pkt.Release() }) {
		pkt.Release()
	}
}
