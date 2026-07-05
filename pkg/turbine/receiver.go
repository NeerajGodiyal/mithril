package turbine

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/gossip"
	"github.com/gagliardetto/solana-go"
)

type LeaderForSlotFunc func(slot uint64) (solana.PublicKey, bool)

type UDPReceiver struct {
	Addr string

	assembler     *SlotAssembler
	leaderForSlot LeaderForSlotFunc
	repairClient  *repairClient
	blocks        chan *block.Block
	errs          chan error
	ready         chan error
	once          sync.Once
	readyOnce     sync.Once

	spool       *ShredSpool
	hydrLo      atomic.Uint64
	hydrHi      atomic.Uint64
	hydrateKick chan struct{}
	// Hydration outcomes: whether spooled slots complete purely from disk
	// (their own data + spooled coding via recovery) or hand holes to network
	// repair — the number that says if the freshness-repair lead time is
	// sufficient or the priority window needs widening.
	hydratedSlots    atomic.Uint64
	hydratedFromDisk atomic.Uint64

	packets         atomic.Uint64
	dataShreds      atomic.Uint64
	codingShreds    atomic.Uint64
	parseErrors     atomic.Uint64
	signatureErrors atomic.Uint64
	missingLeaders  atomic.Uint64
	assemblyErrors  atomic.Uint64
	blocksEmitted   atomic.Uint64
	lastPacketUnix  atomic.Int64
	lastDataSlot    atomic.Uint64
	lastBlockSlot   atomic.Uint64
}

type ReceiverStats struct {
	Packets              uint64
	DataShreds           uint64
	CodingShreds         uint64
	ParseErrors          uint64
	SignatureErrors      uint64
	MissingLeaders       uint64
	AssemblyErrors       uint64
	BlocksEmitted        uint64
	RecoveredData        uint64
	EvictedSlots         uint64
	IgnoredOldShreds     uint64
	PriorityRepairSlots  int
	NonCanonicalBlockIDs uint64
	LastNonCanonicalSlot uint64
	LastNonCanonicalGot  solana.Hash
	LastNonCanonicalWant solana.Hash
	Repair               RepairStats
	LastPacketUnix       int64
	LastDataSlot         uint64
	LastBlockSlot        uint64
	ActiveSlots          int
	// Spool hydration outcomes: slots fed from disk, and how many of those
	// completed with zero network repair (spooled coding healed the holes).
	HydratedSlots    uint64
	HydratedFromDisk uint64
}

func NewUDPReceiver(addr string) *UDPReceiver {
	return &UDPReceiver{
		Addr:        addr,
		assembler:   NewSlotAssembler(),
		blocks:      make(chan *block.Block, 1024),
		errs:        make(chan error, 16),
		ready:       make(chan error, 1),
		hydrateKick: make(chan struct{}, 1),
	}
}

func (r *UDPReceiver) SetLeaderForSlot(fn LeaderForSlotFunc) {
	r.leaderForSlot = fn
}

func (r *UDPReceiver) SetRepairPeerSource(identity ed25519.PrivateKey, source func() []gossip.RepairPeer) error {
	client, err := newRepairClient(identity, source)
	if err != nil {
		return err
	}
	r.repairClient = client
	return nil
}

func (r *UDPReceiver) SetKnownAlpenglowBlockID(slot uint64, blockID solana.Hash) {
	if r == nil || r.assembler == nil {
		return
	}
	r.assembler.SetKnownAlpenglowBlockID(slot, blockID)
}

func (r *UDPReceiver) ResetSlot(slot uint64) {
	if r == nil || r.assembler == nil {
		return
	}
	r.assembler.ResetSlot(slot)
}

// ShredEdges reports the monotonic shred frontier from the assembler: highest
// slot with any accepted shred, and highest slot that became full.
func (r *UDPReceiver) ShredEdges() (latestShredSlot, highestFullSlot uint64) {
	return r.assembler.ShredEdges()
}

// ShredObservation reports partial shred arrivals for a slot that never
// became full (skip observability).
// SlotAssemblyErrors reports assembly-failure count and latest failure text
// for a slot still being assembled.
func (r *UDPReceiver) SlotAssemblyErrors(slot uint64) (int, string) {
	return r.assembler.SlotAssemblyErrors(slot)
}

// SlotCompleted reports whether the assembler holds a completed-slot marker —
// a completed slot silently ignores all further shreds and repair requests.
func (r *UDPReceiver) SlotCompleted(slot uint64) bool {
	return r.assembler.SlotCompleted(slot)
}

// HeadShredDetail reports the completion picture for a slot still being
// assembled (see SlotAssembler.HeadShredDetail).
func (r *UDPReceiver) HeadShredDetail(slot uint64) (HeadShredDetail, bool) {
	return r.assembler.HeadShredDetail(slot)
}

func (r *UDPReceiver) ShredObservation(slot uint64) (PartialShredObservation, bool) {
	return r.assembler.ShredObservation(slot)
}

// SetRetentionFloor pins the assembler's age cutoff for repair catchup: slots
// >= floor stay accepted however far they trail the live edge. 0 restores the
// normal lag-based retention.
// SetShredSpool attaches the on-disk shred spool. Must be called before Run.
func (r *UDPReceiver) SetShredSpool(spool *ShredSpool) {
	r.spool = spool
	// Journal completeness the moment a slot fully assembles — including
	// during hydration of adopted files, which self-heals missing markers.
	r.assembler.SetOnComplete(spool.MarkComplete)
}

// SetHydrationWindow bounds in-RAM assembly during catchup: verified shreds
// for slots beyond hi (and outside the live edge / priority pins) go to the
// on-disk spool only, and the hydrator streams slots in [lo, hi] back into
// the assembler AHEAD of replay — prefetch, not just-in-time. hi == 0
// disables the policy (normal near-tip operation).
func (r *UDPReceiver) SetHydrationWindow(lo, hi uint64) {
	r.hydrLo.Store(lo)
	r.hydrHi.Store(hi)
	if r.spool != nil && lo > 8 {
		r.spool.SetFloor(lo - 8)
	}
	// Keep the freshness-repair scan aligned with the RAM policy: with the
	// window ON, only the last spoolLiveAssemblyLag slots assemble in RAM,
	// so scanning further back would emit repair requests whose responses
	// cannot assemble. Window OFF restores the full near-tip scan.
	if hi != 0 {
		r.assembler.SetEdgeRepairLag(spoolLiveAssemblyLag)
	} else {
		r.assembler.SetEdgeRepairLag(0)
	}
	select {
	case r.hydrateKick <- struct{}{}:
	default:
	}
}

func (r *UDPReceiver) SetRetentionFloor(slot uint64) {
	r.assembler.SetRetentionFloor(slot)
}

func (r *UDPReceiver) PrioritizeRepairSlot(slot uint64) {
	if r == nil || r.assembler == nil {
		return
	}
	r.assembler.PrioritizeRepairSlot(slot)
}

func (r *UDPReceiver) PrioritizeRepairRange(start, end uint64) {
	if r == nil || r.assembler == nil {
		return
	}
	r.assembler.PrioritizeRepairRange(start, end)
}

func (r *UDPReceiver) Blocks() <-chan *block.Block {
	return r.blocks
}

func (r *UDPReceiver) Errors() <-chan error {
	return r.errs
}

func (r *UDPReceiver) Ready() <-chan error {
	return r.ready
}

func (r *UDPReceiver) Stats() ReceiverStats {
	nonCanonicalCount, nonCanonicalSlot, nonCanonicalGot, nonCanonicalWant := r.assembler.NonCanonicalBlockIDStats()
	return ReceiverStats{
		Packets:              r.packets.Load(),
		DataShreds:           r.dataShreds.Load(),
		CodingShreds:         r.codingShreds.Load(),
		ParseErrors:          r.parseErrors.Load(),
		SignatureErrors:      r.signatureErrors.Load(),
		MissingLeaders:       r.missingLeaders.Load(),
		AssemblyErrors:       r.assemblyErrors.Load(),
		BlocksEmitted:        r.blocksEmitted.Load(),
		RecoveredData:        r.assembler.RecoveredDataShreds(),
		EvictedSlots:         r.assembler.EvictedSlots(),
		IgnoredOldShreds:     r.assembler.IgnoredOldShreds(),
		PriorityRepairSlots:  r.assembler.PriorityRepairSlots(),
		NonCanonicalBlockIDs: nonCanonicalCount,
		LastNonCanonicalSlot: nonCanonicalSlot,
		LastNonCanonicalGot:  nonCanonicalGot,
		LastNonCanonicalWant: nonCanonicalWant,
		Repair:               r.repairStats(),
		LastPacketUnix:       r.lastPacketUnix.Load(),
		LastDataSlot:         r.lastDataSlot.Load(),
		LastBlockSlot:        r.lastBlockSlot.Load(),
		ActiveSlots:          r.assembler.ActiveSlots(),
		HydratedSlots:        r.hydratedSlots.Load(),
		HydratedFromDisk:     r.hydratedFromDisk.Load(),
	}
}

func (r *UDPReceiver) repairStats() RepairStats {
	if r.repairClient == nil {
		return RepairStats{}
	}
	return r.repairClient.stats()
}

func (r *UDPReceiver) signalReady(err error) {
	r.readyOnce.Do(func() {
		r.ready <- err
		close(r.ready)
	})
}

func (r *UDPReceiver) Run(ctx context.Context) error {
	defer r.once.Do(func() {
		close(r.blocks)
		close(r.errs)
	})

	udpAddr, err := net.ResolveUDPAddr("udp", r.Addr)
	if err != nil {
		r.signalReady(err)
		return err
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		r.signalReady(err)
		return err
	}
	defer conn.Close()
	// Shreds AND repair responses land on this one socket in multi-megabyte
	// bursts; the kernel buffer is the only slack while the read loop drains.
	gossip.BoostUDPReceiveBuffer(conn, gossip.TurbineUDPReceiveBufferBytes, "turbine receiver")
	r.signalReady(nil)

	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	if r.repairClient != nil {
		go r.repairClient.run(ctx, conn, r.assembler)
	}
	if r.spool != nil {
		go r.hydrateLoop(ctx)
	}

	buf := make([]byte, packetDataSize)
	for {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		r.packets.Add(1)
		r.lastPacketUnix.Store(time.Now().Unix())
		packet := buf[:n]
		if r.repairClient != nil && r.repairClient.handleRepairPing(conn, packet, addr) {
			continue
		}
		shred, err := ParseShred(packet)
		if err != nil {
			if errors.Is(err, ErrCodingShredIgnored) {
				r.codingShreds.Add(1)
				continue
			}
			r.parseErrors.Add(1)
			select {
			case r.errs <- err:
			default:
			}
			continue
		}
		switch shred.Type {
		case ShredTypeData:
			r.dataShreds.Add(1)
			// Monotonic max: out-of-order packets must not move the reported
			// latest-shred edge backward.
			for {
				cur := r.lastDataSlot.Load()
				if shred.Slot <= cur || r.lastDataSlot.CompareAndSwap(cur, shred.Slot) {
					break
				}
			}
		case ShredTypeCode:
			r.codingShreds.Add(1)
		}
		if r.leaderForSlot != nil {
			leader, ok := r.leaderForSlot(shred.Slot)
			if !ok {
				r.missingLeaders.Add(1)
				select {
				case r.errs <- fmt.Errorf("missing leader for turbine shred slot %d", shred.Slot):
				default:
				}
				continue
			}
			if err := shred.VerifySignature(leader); err != nil {
				r.signatureErrors.Add(1)
				select {
				case r.errs <- err:
				default:
				}
				continue
			}
		}
		fromRepair := false
		if r.repairClient != nil {
			fromRepair = r.repairClient.observeShredResponse(conn, packet, addr, shred)
		}
		if r.spool != nil {
			// Write-through of every VERIFIED shred (copy: the read buffer
			// is reused). Repaired shreds included — a later slot reset
			// re-hydrates from disk instead of re-fetching over the wire.
			pkt := make([]byte, len(packet))
			copy(pkt, packet)
			r.spool.Append(shred.Slot, pkt)
			if r.skipAssemblyForSpool(shred.Slot) {
				// Far-future slot during catchup: on disk is enough. The
				// hydrator streams it into RAM when replay approaches.
				continue
			}
		}
		blk, err := r.assembler.AddShredFrom(shred, fromRepair)
		if err != nil {
			if errors.Is(err, ErrDuplicateShred) {
				continue
			}
			r.assemblyErrors.Add(1)
			select {
			case r.errs <- err:
			default:
			}
			continue
		}
		if blk == nil {
			continue
		}
		if !r.emitAssembled(ctx, blk) {
			return nil
		}
	}
}

// skipAssemblyForSpool implements the catchup RAM policy: with a hydration
// window set, only the window itself, the live edge, and priority-pinned
// slots assemble in RAM; everything else lives on disk until hydrated.
func (r *UDPReceiver) skipAssemblyForSpool(slot uint64) bool {
	hi := r.hydrHi.Load()
	if hi == 0 || slot <= hi {
		return false
	}
	if latest, _ := r.assembler.ShredEdges(); slot+spoolLiveAssemblyLag >= latest {
		return false // live edge stays in RAM: broadcast + FEC completes it free
	}
	return !r.assembler.IsPrioritySlot(slot)
}

func (r *UDPReceiver) emitAssembled(ctx context.Context, blk *block.Block) bool {
	r.blocksEmitted.Add(1)
	r.lastBlockSlot.Store(blk.Slot)
	select {
	case r.blocks <- blk:
		return true
	case <-ctx.Done():
		return false
	}
}

// hydrateLoop streams spooled slots inside the hydration window into the
// assembler AHEAD of replay — batched reads a window at a time, so the
// emitter never waits on a disk load for the slot it needs next.
func (r *UDPReceiver) hydrateLoop(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	hydrated := make(map[uint64]bool)
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.hydrateKick:
		case <-ticker.C:
		}
		lo := r.hydrLo.Load()
		hi := r.hydrHi.Load()
		if hi == 0 || lo > hi {
			continue
		}
		for slot := range hydrated {
			if slot < lo {
				delete(hydrated, slot)
			}
		}
		for _, slot := range r.spool.SlotsInRange(lo, hi) {
			if r.assembler.SlotCompleted(slot) {
				hydrated[slot] = true
				continue
			}
			if hydrated[slot] {
				// Re-hydrate only if the assembler lost its state (reset).
				if _, live := r.assembler.HeadShredDetail(slot); live {
					continue
				}
			}
			packets, err := r.spool.ReadSlot(slot)
			if err != nil {
				continue
			}
			hydrated[slot] = true
			for _, pkt := range packets {
				shred, perr := ParseShred(pkt)
				if perr != nil {
					continue
				}
				blk, aerr := r.assembler.AddShredFrom(shred, false)
				if aerr != nil || blk == nil {
					continue
				}
				if !r.emitAssembled(ctx, blk) {
					return
				}
			}
			r.hydratedSlots.Add(1)
			if r.assembler.SlotCompleted(slot) {
				r.hydratedFromDisk.Add(1)
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}
}
