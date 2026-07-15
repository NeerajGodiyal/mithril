package sink

import (
	"sync/atomic"

	"github.com/Overclock-Validator/mithril/pkg/tpu/packet"
)

// Receiver accepts verified transactions for downstream processing.
type Receiver interface {
	Receive(pkt packet.Packet)
}

// Stats tracks sink throughput.
type Stats struct {
	InPackets uint64
	InBytes   uint64
}

// Noop discards verified packets.
type Noop struct {
	Stats Stats
}

// Snapshot returns a race-free copy while packets are still arriving.
func (s *Noop) Snapshot() Stats {
	if s == nil {
		return Stats{}
	}
	return Stats{
		InPackets: atomic.LoadUint64(&s.Stats.InPackets),
		InBytes:   atomic.LoadUint64(&s.Stats.InBytes),
	}
}

func (s *Noop) Receive(pkt packet.Packet) {
	atomic.AddUint64(&s.Stats.InPackets, 1)
	atomic.AddUint64(&s.Stats.InBytes, uint64(pkt.Len()))
	pkt.Release()
}

// Run drains verified packets into the receiver until the channel closes.
func Run(in <-chan packet.Packet, recv Receiver) {
	for pkt := range in {
		recv.Receive(pkt)
	}
}
