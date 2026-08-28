package blockstream

import (
	"context"
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/turbine"
	"github.com/stretchr/testify/require"
)

func TestTurbinePrewarmAdvanceFloorMovesRepairProbe(t *testing.T) {
	receiver := turbine.NewUDPReceiver("127.0.0.1:0")
	pw := &TurbinePrewarm{
		receiver:  receiver,
		floor:     99,
		probeNext: 99 + prewarmProbeSlots,
	}

	pw.AdvanceFloor(101)
	if pw.floor != 101 || pw.probeNext != 101+prewarmProbeSlots {
		t.Fatalf("advanced frontier = floor %d probe %d", pw.floor, pw.probeNext)
	}
	if got := receiver.Stats().PriorityRepairSlots; got != prewarmProbeSlots {
		t.Fatalf("priority repair slots = %d, want %d", got, prewarmProbeSlots)
	}

	pw.noteCompleted(100)
	if pw.probeNext != 101+prewarmProbeSlots {
		t.Fatal("completion below the floor advanced the probe")
	}
	pw.noteCompleted(101)
	if pw.probeNext != 102+prewarmProbeSlots {
		t.Fatalf("frontier completion did not advance the probe: %d", pw.probeNext)
	}
}

func TestTurbinePrewarmRequiresRawShredSpool(t *testing.T) {
	_, err := StartTurbinePrewarm(TurbinePrewarmConfig{
		BindAddr:         "127.0.0.1:0",
		GossipEntrypoint: "127.0.0.1:1",
	})
	require.ErrorContains(t, err, "shred spool directory")
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
