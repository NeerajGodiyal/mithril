package forge

import (
	"github.com/Overclock-Validator/mithril/pkg/blockprod"
	"github.com/Overclock-Validator/mithril/pkg/costmodel"
	"github.com/Overclock-Validator/mithril/pkg/tpu/packet"
)

// BankSource supplies the active leader working bank.
type BankSource interface {
	WorkingBank() *blockprod.WorkingBank
}

// Stats tracks IOD forge sink outcomes.
type Stats struct {
	InPackets           uint64
	InBytes             uint64
	Accepted            uint64
	DroppedNoBank         uint64
	DroppedVote           uint64
	DroppedParse          uint64
	DroppedCost           uint64
	DroppedExecution      uint64
	DroppedBlockCost      uint64
	DroppedAccountCost    uint64
	DroppedAllocCost      uint64
	DroppedBatchBytes     uint64
}

// Sink is an immediate-or-drop forge stage wired after sigverify.
type Sink struct {
	banks BankSource
	stats Stats
}

func NewSink(banks BankSource) *Sink {
	return &Sink{banks: banks}
}

func (s *Sink) Stats() Stats {
	return s.stats
}

func (s *Sink) Receive(pkt packet.Packet) {
	data := pkt.Data()
	s.stats.InPackets++
	s.stats.InBytes += uint64(len(data))

	bank := s.banks.WorkingBank()
	if bank == nil {
		s.stats.DroppedNoBank++
		pkt.Release()
		return
	}

	result, reason := bank.Forge(data)
	switch result {
	case blockprod.ForgeAccepted:
		s.stats.Accepted++
	case blockprod.ForgeDroppedVote:
		s.stats.DroppedVote++
	case blockprod.ForgeDroppedParse:
		s.stats.DroppedParse++
	case blockprod.ForgeDroppedCost:
		s.stats.DroppedCost++
		s.recordCostDrop(reason)
	case blockprod.ForgeDroppedExecution:
		s.stats.DroppedExecution++
	default:
		s.stats.DroppedNoBank++
	}
	pkt.Release()
}

func (s *Sink) recordCostDrop(reason costmodel.ExceedReason) {
	switch reason {
	case costmodel.ExceedBlockCost:
		s.stats.DroppedBlockCost++
	case costmodel.ExceedWritableAccountCost:
		s.stats.DroppedAccountCost++
	case costmodel.ExceedAllocatedDataSize:
		s.stats.DroppedAllocCost++
	case costmodel.ExceedBatchBytes:
		s.stats.DroppedBatchBytes++
	}
}
