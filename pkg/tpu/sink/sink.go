package sink

import (
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

func (s *Noop) Receive(pkt packet.Packet) {
	s.Stats.InPackets++
	s.Stats.InBytes += uint64(pkt.Len())
	pkt.Release()
}

// Run drains verified packets into the receiver until the channel closes.
func Run(in <-chan packet.Packet, recv Receiver) {
	for pkt := range in {
		recv.Receive(pkt)
	}
}
