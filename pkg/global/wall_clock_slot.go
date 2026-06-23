package global

import (
	"sync"
	"time"
)

// Mainnet slot duration (400ms). Matches replay/sysvar nsPerSlot.
const slotDuration = 400 * time.Millisecond

type wallClockAnchor struct {
	mu     sync.Mutex
	slot   uint64
	at     time.Time
	seeded bool
}

var wallClock wallClockAnchor

// SeedWallClockSlot initializes wall-clock slot extrapolation from a network or
// state-file slot before replay has executed any blocks.
func SeedWallClockSlot(slot uint64) {
	wallClock.seed(slot)
}

// WallClockSlot returns the estimated current cluster slot from wall clock,
// extrapolated forward from the last replayed slot anchor. Skipped slots do
// not rewind the estimate when replay lags behind real time.
func WallClockSlot() uint64 {
	wallClock.mu.Lock()
	seeded := wallClock.seeded
	slot := wallClock.slot
	at := wallClock.at
	wallClock.mu.Unlock()
	if !seeded {
		return Slot()
	}
	return slot + uint64(time.Since(at)/slotDuration)
}

func notifyWallClockSlot(slot uint64) {
	wallClock.advance(slot)
}

func (w *wallClockAnchor) seed(slot uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.slot = slot
	w.at = time.Now()
	w.seeded = true
}

func (w *wallClockAnchor) advance(slot uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	if !w.seeded {
		w.slot = slot
		w.at = now
		w.seeded = true
		return
	}
	extrapolated := w.slot + uint64(now.Sub(w.at)/slotDuration)
	if slot >= extrapolated {
		w.slot = slot
		w.at = now
	}
}
