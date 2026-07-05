package blockstream

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"sort"
	"sync"
	"time"

	b "github.com/Overclock-Validator/mithril/pkg/block"
	gossipclient "github.com/Overclock-Validator/mithril/pkg/gossip"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/turbine"
)

// TurbinePrewarm collects live turbine blocks BEFORE replay exists. The
// repair protocol serves data shreds only, one per round trip — a slot that
// airs before the receiver joins must be fetched shred by shred with zero
// FEC leverage, while a slot received live completes from roughly half of
// each FEC set for free. Starting collection the moment current-epoch
// stakes are known (the incremental snapshot manifest parse, a minute
// before the AccountsDB finishes building) turns the expensive pre-join
// repair hole into spooled blocks handed to the BlockSource at replay
// start.
type TurbinePrewarm struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	spool    map[uint64]*b.Block
	floor    uint64
	capacity int
	dropped  int
	stopped  bool
}

// TurbinePrewarmConfig mirrors the BlockSource's turbine wiring; the
// prewarm receiver is torn down (freeing the bind port) before the
// BlockSource constructs its own.
type TurbinePrewarmConfig struct {
	BindAddr         string
	GossipEntrypoint string
	GossipBindAddr   string
	AdvertisedIP     string
	ShredVersion     uint16
	AlpenglowAddr    string
	Identity         ed25519.PrivateKey
	LeaderForSlot    turbine.LeaderForSlotFunc
	FloorSlot        uint64 // resume frontier: spool floor and assembler retention floor
	MaxSpoolBlocks   int
}

// StartTurbinePrewarm joins gossip, starts a shred receiver with the
// retention floor pinned at the resume frontier, and spools completed
// blocks. Fail-open by design: any error means "no prewarm", never a
// blocked boot.
func StartTurbinePrewarm(cfg TurbinePrewarmConfig) (*TurbinePrewarm, error) {
	if cfg.BindAddr == "" || cfg.GossipEntrypoint == "" {
		return nil, fmt.Errorf("turbine prewarm needs a bind address and gossip entrypoint")
	}
	if cfg.MaxSpoolBlocks <= 0 {
		// Below the staging buffer's 256-slot cap so the handover injection
		// never evicts.
		cfg.MaxSpoolBlocks = 192
	}

	ctx, cancel := context.WithCancel(context.Background())

	gossipCfg := gossipclient.Config{
		Entrypoint:    cfg.GossipEntrypoint,
		BindAddr:      cfg.GossipBindAddr,
		TVUAddr:       cfg.BindAddr,
		AlpenglowAddr: cfg.AlpenglowAddr,
		AdvertisedIP:  cfg.AdvertisedIP,
		ShredVersion:  cfg.ShredVersion,
		Identity:      cfg.Identity,
		Name:          gossipclient.ClientName,
	}
	client, err := gossipclient.NewClient(gossipCfg)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("prewarm gossip client: %w", err)
	}

	receiver := turbine.NewUDPReceiver(cfg.BindAddr)
	if cfg.LeaderForSlot != nil {
		receiver.SetLeaderForSlot(cfg.LeaderForSlot)
	}
	if err := receiver.SetRepairPeerSource(client.Identity(), client.RepairPeers); err != nil {
		cancel()
		return nil, fmt.Errorf("prewarm repair setup: %w", err)
	}
	if cfg.FloorSlot > 0 {
		receiver.SetRetentionFloor(cfg.FloorSlot)
	}

	pw := &TurbinePrewarm{
		cancel:   cancel,
		done:     make(chan struct{}),
		spool:    make(map[uint64]*b.Block),
		floor:    cfg.FloorSlot,
		capacity: cfg.MaxSpoolBlocks,
	}

	go func() {
		if err := client.Run(ctx); err != nil && ctx.Err() == nil {
			mlog.Log.FileOnlyf("turbine prewarm gossip exited: %v", err)
		}
	}()
	receiverDone := make(chan error, 1)
	go func() {
		receiverDone <- receiver.Run(ctx)
	}()

	select {
	case err := <-receiverDone:
		cancel()
		close(pw.done)
		return nil, fmt.Errorf("prewarm receiver failed to start: %v", err)
	case rerr := <-receiver.Ready():
		if rerr != nil {
			cancel()
			close(pw.done)
			return nil, fmt.Errorf("prewarm receiver not ready: %v", rerr)
		}
	case <-time.After(15 * time.Second):
		cancel()
		close(pw.done)
		return nil, fmt.Errorf("prewarm receiver not ready within 15s")
	}

	go pw.drain(receiver)
	return pw, nil
}

// drain spools completed blocks (lowest slots win: they are the ones repair
// cannot fetch cheaply later) and discards receiver errors quietly.
func (pw *TurbinePrewarm) drain(receiver *turbine.UDPReceiver) {
	defer close(pw.done)
	blocks := receiver.Blocks()
	errs := receiver.Errors()
	for {
		select {
		case blk, ok := <-blocks:
			if !ok {
				return
			}
			pw.add(blk)
		case _, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
		}
	}
}

func (pw *TurbinePrewarm) add(blk *b.Block) {
	if blk == nil {
		return
	}
	pw.mu.Lock()
	defer pw.mu.Unlock()
	if pw.stopped {
		return
	}
	if pw.floor > 0 && blk.Slot < pw.floor {
		return
	}
	if _, exists := pw.spool[blk.Slot]; exists {
		return
	}
	pw.spool[blk.Slot] = blk
	// Over capacity: evict the HIGHEST slot. The low end borders the resume
	// frontier and is what emission needs first — and what repair would
	// otherwise fetch one shred at a time; the high end is re-fetchable
	// cheaply near the tip later.
	for len(pw.spool) > pw.capacity {
		var victim uint64
		first := true
		for slot := range pw.spool {
			if first || slot > victim {
				victim = slot
				first = false
			}
		}
		delete(pw.spool, victim)
		pw.dropped++
	}
}

// Handover stops collection (freeing the turbine bind port for the
// BlockSource) and returns the spooled blocks in ascending slot order,
// along with how many overflowed the spool.
func (pw *TurbinePrewarm) Handover() ([]*b.Block, int) {
	pw.Stop()
	pw.mu.Lock()
	defer pw.mu.Unlock()
	blocks := make([]*b.Block, 0, len(pw.spool))
	for _, blk := range pw.spool {
		blocks = append(blocks, blk)
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].Slot < blocks[j].Slot })
	pw.spool = nil
	return blocks, pw.dropped
}

// Stop tears the prewarm receiver and gossip session down and waits for the
// drainer to exit. Idempotent.
func (pw *TurbinePrewarm) Stop() {
	pw.mu.Lock()
	if pw.stopped {
		pw.mu.Unlock()
		<-pw.done
		return
	}
	pw.stopped = true
	pw.mu.Unlock()
	pw.cancel()
	<-pw.done
}
