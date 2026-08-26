package blockstream

import (
	"context"
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/turbine"
)

func TestTurbinePrewarmAdvanceFloorDropsFullSnapshotWork(t *testing.T) {
	receiver := turbine.NewUDPReceiver("127.0.0.1:0")
	pw := &TurbinePrewarm{
		receiver:  receiver,
		spool:     map[uint64]*block.Block{99: {Slot: 99}, 100: {Slot: 100}, 101: {Slot: 101}},
		floor:     99,
		probeNext: 99 + prewarmProbeSlots,
		capacity:  192,
	}

	pw.AdvanceFloor(101)
	if pw.floor != 101 || pw.probeNext != 101+prewarmProbeSlots {
		t.Fatalf("advanced frontier = floor %d probe %d", pw.floor, pw.probeNext)
	}
	if _, ok := pw.spool[99]; ok {
		t.Fatal("full-snapshot block below incremental frontier was retained")
	}
	if _, ok := pw.spool[100]; ok {
		t.Fatal("block immediately below incremental frontier was retained")
	}
	if _, ok := pw.spool[101]; !ok {
		t.Fatal("incremental-frontier block was discarded")
	}
	if got := receiver.Stats().PriorityRepairSlots; got != prewarmProbeSlots {
		t.Fatalf("priority repair slots = %d, want %d", got, prewarmProbeSlots)
	}

	pw.add(&block.Block{Slot: 100})
	if _, ok := pw.spool[100]; ok {
		t.Fatal("old block was re-added after advancing the frontier")
	}
}

func TestTurbinePrewarmStopWaitsForGossip(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	gossipDone := make(chan struct{})
	close(done)

	pw := &TurbinePrewarm{cancel: cancel, done: done, gossipDone: gossipDone}
	stopped := make(chan struct{}, 2)
	for range 2 {
		go func() {
			pw.Stop()
			stopped <- struct{}{}
		}()
	}

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("prewarm stop did not cancel its workers")
	}
	select {
	case <-stopped:
		t.Fatal("prewarm stop returned before gossip released its socket")
	default:
	}
	close(gossipDone)
	for range 2 {
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatal("prewarm stop did not return after gossip exited")
		}
	}
}
