package global

import (
	"sync"
	"time"
)

// defaultSlotDuration matches the classic cluster target. Alpenglow validator
// startup overrides it before seeding the anchor.
const defaultSlotDuration = 400 * time.Millisecond

type wallClockAnchor struct {
	mu       sync.Mutex
	slot     uint64
	at       time.Time
	seeded   bool
	duration time.Duration
}

var wallClock = wallClockAnchor{duration: defaultSlotDuration}

// SetWallClockSlotDuration selects the protocol's target slot duration. Call it
// before SeedWallClockSlot so extrapolation starts from one coherent anchor.
func SetWallClockSlotDuration(d time.Duration) {
	if d <= 0 {
		d = defaultSlotDuration
	}
	wallClock.mu.Lock()
	wallClock.duration = d
	wallClock.mu.Unlock()
}

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
	duration := wallClock.duration
	wallClock.mu.Unlock()
	if !seeded {
		return Slot()
	}
	if duration <= 0 {
		duration = defaultSlotDuration
	}
	return slot + uint64(time.Since(at)/duration)
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
	duration := w.duration
	if duration <= 0 {
		duration = defaultSlotDuration
	}
	extrapolated := w.slot + uint64(now.Sub(w.at)/duration)
	if slot >= extrapolated {
		w.slot = slot
		w.at = now
	}
}
