package blockstream

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"sync"
	"time"

	gossipclient "github.com/Overclock-Validator/mithril/pkg/gossip"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/turbine"
	"github.com/gagliardetto/solana-go"
)

// TurbinePrewarm collects live turbine blocks BEFORE replay exists. The
// repair protocol serves data shreds only, one per round trip — a slot that
// airs before the receiver joins must be fetched shred by shred with zero
// FEC leverage, while a slot received live completes from roughly half of
// each FEC set for free. Starting collection the moment current-epoch
// stakes are known (the incremental snapshot manifest parse, a minute
// before the AccountsDB finishes building) turns the expensive pre-join
// repair hole into raw verified shreds that the BlockSource revalidates at
// replay start.
type TurbinePrewarm struct {
	cancel     context.CancelFunc
	done       chan struct{}
	gossipDone chan struct{}
	receiver   *turbine.UDPReceiver

	mu        sync.Mutex
	floor     uint64
	probeNext uint64 // next frontier slot the rolling probe will pin
	stopped   bool
}

// prewarmProbeSlots: the replay frontier slots the prewarm actively repairs
// during the AccountsDB build. These are the slots replay needs FIRST and
// the ones guaranteed to have aired before the receiver joined (zero FEC
// leverage, one data shred per round trip) — every one completed during the
// build is head-of-line stall time deleted from catchup. The probe rides
// the normal repair client (priority pins -> HighestWindowIndex discovery ->
// metered followups -> assembler/spool), so everything it fetches is kept,
// and its response statistics double as the boot-time peer-quality
// measurement behind the rate recommendation logged at handover. The
// window ROLLS: each completed probe slot pins the next frontier slot, so
// a long snapshot build keeps the repair budget on the frontier instead of
// idling after a fixed prefix completes.
const prewarmProbeSlots = 64

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
	StakesForSlot    func(slot uint64) map[solana.PublicKey]uint64
	EpochForSlot     func(slot uint64) uint64
	RootSlot         func() uint64
	UseChaCha8       bool
	DedupAddrs       bool
	FloorSlot        uint64 // resume frontier: spool floor and assembler retention floor
	// ShredSpoolDir is required. Prewarm stores verified raw shreds only;
	// the live receiver revalidates them under the slot policy in force at
	// handover instead of trusting blocks assembled during bootstrap.
	ShredSpoolDir string
	// RepairMaxRequestsPerSecond mirrors the BlockSource's repair rate
	// ceiling override (0 = default).
	RepairMaxRequestsPerSecond int
}

// StartTurbinePrewarm joins gossip and starts a shred receiver with the
// retention floor pinned at the resume frontier. Fail-open by design: any
// error means "no prewarm", never a blocked boot.
func StartTurbinePrewarm(cfg TurbinePrewarmConfig) (*TurbinePrewarm, error) {
	if cfg.BindAddr == "" || cfg.GossipEntrypoint == "" {
		return nil, fmt.Errorf("turbine prewarm needs a bind address and gossip entrypoint")
	}
	if cfg.ShredSpoolDir == "" {
		return nil, fmt.Errorf("turbine prewarm needs a shred spool directory")
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
	receiver.SetShredVersion(cfg.ShredVersion)
	if cfg.LeaderForSlot != nil {
		receiver.SetLeaderForSlot(cfg.LeaderForSlot)
	}
	if cfg.StakesForSlot != nil {
		if err := receiver.SetRetransmit(turbine.RetransmitConfig{
			Identity:     client.Identity(),
			Peers:        client,
			Stakes:       cfg.StakesForSlot,
			EpochForSlot: cfg.EpochForSlot,
			RootSlot:     cfg.RootSlot,
			UseChaCha8:   cfg.UseChaCha8,
			DedupAddrs:   cfg.DedupAddrs,
		}); err != nil {
			cancel()
			return nil, fmt.Errorf("prewarm retransmit setup: %w", err)
		}
	}
	if err := receiver.SetRepairPeerSource(client.Identity(), client.RepairPeers); err != nil {
		cancel()
		return nil, fmt.Errorf("prewarm repair setup: %w", err)
	}
	receiver.SetRepairRequestRate(cfg.RepairMaxRequestsPerSecond)
	if cfg.FloorSlot > 0 {
		receiver.SetRetentionFloor(cfg.FloorSlot)
	}
	spool, err := turbine.OpenShredSpool(cfg.ShredSpoolDir, shredSpoolMaxBytes)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open prewarm shred spool: %w", err)
	}
	receiver.SetShredSpool(spool)
	// Assemble the resume-adjacent window in RAM to drive the rolling repair
	// probe. Only the verified raw shreds survive handover.
	receiver.SetHydrationWindow(cfg.FloorSlot, cfg.FloorSlot+repairCatchupLiveDeliverWindow)

	if cfg.FloorSlot > 0 {
		receiver.PrioritizeRepairRange(cfg.FloorSlot, cfg.FloorSlot+prewarmProbeSlots-1)
	}

	pw := &TurbinePrewarm{
		cancel:     cancel,
		done:       make(chan struct{}),
		gossipDone: make(chan struct{}),
		receiver:   receiver,
		floor:      cfg.FloorSlot,
		probeNext:  cfg.FloorSlot + prewarmProbeSlots,
	}

	go func() {
		defer close(pw.gossipDone)
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
		<-pw.gossipDone
		spool.Close()
		close(pw.done)
		return nil, fmt.Errorf("prewarm receiver failed to start: %v", err)
	case rerr := <-receiver.Ready():
		if rerr != nil {
			cancel()
			<-receiverDone
			<-pw.gossipDone
			spool.Close()
			close(pw.done)
			return nil, fmt.Errorf("prewarm receiver not ready: %v", rerr)
		}
	case <-time.After(15 * time.Second):
		cancel()
		<-receiverDone
		<-pw.gossipDone
		spool.Close()
		close(pw.done)
		return nil, fmt.Errorf("prewarm receiver not ready within 15s")
	}

	metricsPublisher := &turbineReceiverMetricsPublisher{}
	metricsPublisher.publish(receiver, true)
	go pw.drain(receiver, metricsPublisher)
	return pw, nil
}

// drain observes completed blocks to roll the frontier probe. Raw verified
// shreds are already persisted by the receiver and are revalidated after
// handover; completed blocks are deliberately not retained here.
func (pw *TurbinePrewarm) drain(receiver *turbine.UDPReceiver, metricsPublisher *turbineReceiverMetricsPublisher) {
	defer close(pw.done)
	defer metricsPublisher.publish(receiver, false)
	statsTicker := time.NewTicker(10 * time.Second)
	defer statsTicker.Stop()
	blocks := receiver.Blocks()
	errs := receiver.Errors()
	for {
		select {
		case blk, ok := <-blocks:
			if !ok {
				return
			}
			if blk != nil {
				pw.noteCompleted(blk.Slot)
			}
		case _, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
		case <-statsTicker.C:
			metricsPublisher.publish(receiver, true)
		}
	}
}

func (pw *TurbinePrewarm) noteCompleted(slot uint64) {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	if pw.stopped {
		return
	}
	if pw.floor > 0 && slot < pw.floor {
		return
	}
	// Roll the probe frontier: a completed slot inside the probe window
	// frees repair budget — pin the next frontier slot so a long build
	// keeps filling forward instead of idling once the prefix completes.
	if pw.floor > 0 && slot < pw.probeNext && pw.receiver != nil {
		pw.receiver.PrioritizeRepairSlot(pw.probeNext)
		pw.probeNext++
	}
}

// AdvanceFloor moves a full-snapshot prewarm to the fresher incremental
// manifest frontier. Blocks below that frontier cannot be replayed, so keeping
// them would spend repair bandwidth on work that bootstrap will discard.
func (pw *TurbinePrewarm) AdvanceFloor(floor uint64) {
	if pw == nil || floor == 0 {
		return
	}
	pw.mu.Lock()
	defer pw.mu.Unlock()
	if pw.stopped || floor <= pw.floor {
		return
	}
	pw.floor = floor
	pw.probeNext = floor + prewarmProbeSlots
	if pw.receiver != nil {
		pw.receiver.SetRetentionFloor(floor)
		pw.receiver.SetHydrationWindow(floor, floor+repairCatchupLiveDeliverWindow)
		pw.receiver.PrioritizeRepairRange(floor, floor+prewarmProbeSlots-1)
	}
}

// Handover stops collection, closes the spool, and frees the turbine bind
// port. The BlockSource then opens the same spool and revalidates every raw
// shred with its current slot policy.
func (pw *TurbinePrewarm) Handover() {
	pw.logProbeOutcome() // before Stop: receiver stats die with the receiver
	pw.Stop()
}

// logProbeOutcome reports the boot-time frontier probe: how many of the
// replay-frontier slots were fully repaired before replay starts, and what
// the response statistics say about the peer set's headroom for the
// configurable rate ceiling. One console line; the per-peer table goes to
// the repair-peers file log.
func (pw *TurbinePrewarm) logProbeOutcome() {
	pw.mu.Lock()
	receiver, floor, stopped := pw.receiver, pw.floor, pw.stopped
	pw.mu.Unlock()
	if receiver == nil || stopped {
		return
	}
	completed := 0
	if floor > 0 {
		for slot := floor; slot < floor+prewarmProbeSlots; slot++ {
			if receiver.SlotCompleted(slot) {
				completed++
			}
		}
	}
	r := receiver.Stats().Repair
	outcomes := r.Responses + r.LateResponses + r.Timeouts
	if r.Requests == 0 || outcomes == 0 {
		mlog.Log.FileOnlyf("prewarm repair probe: no repair traffic during boot (requests %d) — no rate assessment", r.Requests)
		return
	}
	timeoutPct := 100 * r.Timeouts / outcomes
	var verdict string
	switch {
	case timeoutPct <= 10 && r.RespondingPeers >= 30:
		verdict = "healthy with headroom — the auto rate controller will ramp toward its ceiling on its own; pin block.repair_max_requests_per_second only if you need to cap it lower"
	case timeoutPct <= 25:
		verdict = "healthy at the current rate ceiling"
	case timeoutPct <= 50:
		verdict = "elevated timeouts — keep the current rate"
	default:
		verdict = "poor peer response quality — do not raise the rate"
	}
	mlog.Log.Infof("prewarm repair probe: %d/%d frontier slots fully repaired before replay | responses %d (late %d), timeouts %d (%d%%), avg %dms, peers responding %d/%d — %s",
		completed, prewarmProbeSlots, r.Responses, r.LateResponses, r.Timeouts, timeoutPct,
		r.AvgResponseMillis, r.RespondingPeers, r.Peers, verdict)
	logRepairPeerTable(receiver, "boot probe")
}

// Stop tears the prewarm receiver and gossip session down and waits for the
// drainer to exit. Idempotent.
func (pw *TurbinePrewarm) Stop() {
	pw.mu.Lock()
	if pw.stopped {
		pw.mu.Unlock()
		<-pw.done
		<-pw.gossipDone
		return
	}
	pw.stopped = true
	pw.mu.Unlock()
	pw.cancel()
	<-pw.done
	<-pw.gossipDone
}
