package blockstream

import (
	"testing"
	"time"
)

func TestMaybeLogStuckTurbineIngestWaitsForThreshold(t *testing.T) {
	bs := NewBlockSource(&BlockSourceOpts{
		SourceType:      BlockSourceTurbine,
		TurbineBindAddr: "127.0.0.1:8001",
		StartSlot:       100,
		EndSlot:         200,
	})

	bs.isNearTip.Store(true)
	bs.lightbringerConnected.Store(true)
	bs.lastExecutedSlot.Store(150)
	bs.confirmedTip.Store(152)
	bs.nextSlotToSend = 151

	bs.maybeLogStuckTurbineIngest()
	if got := bs.turbineStuckSinceUnix.Load(); got == 0 {
		t.Fatalf("expected stuck watch to start on first observation")
	}

	bs.turbineStuckSinceUnix.Store(time.Now().Add(-3 * time.Second).Unix())
	bs.maybeLogStuckTurbineIngest()
	if got := bs.turbineStuckLogAt.Load(); got == 0 {
		t.Fatalf("expected stuck ingest INFO log after threshold")
	}

	bs.lightbringerLastStreamSlot.Store(151)
	bs.maybeLogStuckTurbineIngest()
	if got := bs.turbineStuckSinceUnix.Load(); got != 0 {
		t.Fatalf("expected stuck watch to clear after first streamed block")
	}
}

func TestMaybeLogStuckTurbineIngestIgnoredWhenNotNearTip(t *testing.T) {
	bs := NewBlockSource(&BlockSourceOpts{
		SourceType:      BlockSourceTurbine,
		TurbineBindAddr: "127.0.0.1:8001",
		StartSlot:       100,
		EndSlot:         200,
	})

	bs.lightbringerConnected.Store(true)
	bs.isNearTip.Store(false)
	bs.turbineStuckSinceUnix.Store(time.Now().Add(-10 * time.Second).Unix())

	bs.maybeLogStuckTurbineIngest()
	if got := bs.turbineStuckLogAt.Load(); got != 0 {
		t.Fatalf("expected no stuck log outside near-tip mode")
	}
}
