package global

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestWallClockSlotExtrapolatesFromReplayAnchor(t *testing.T) {
	wallClock.seed(100)
	SetSlot(100)

	assert.Equal(t, uint64(100), WallClockSlot())

	wallClock.at = time.Now().Add(-800 * time.Millisecond)
	assert.Equal(t, uint64(102), WallClockSlot())
}

func TestWallClockSlotDoesNotRewindOnLaggyReplay(t *testing.T) {
	wallClock.seed(100)
	SetSlot(100)

	wallClock.at = time.Now().Add(-1200 * time.Millisecond)
	assert.Equal(t, uint64(103), WallClockSlot())

	// Replay finally executes an older slot; wall clock must not jump backward.
	SetSlot(101)
	assert.Equal(t, uint64(103), WallClockSlot())
}

func TestWallClockSlotResyncsWhenReplayCatchesUp(t *testing.T) {
	wallClock.seed(100)
	SetSlot(100)

	wallClock.at = time.Now().Add(-1200 * time.Millisecond)
	assert.Equal(t, uint64(103), WallClockSlot())

	SetSlot(105)
	assert.Equal(t, uint64(105), WallClockSlot())
}

func TestWallClockSlotFallsBackToReplaySlotBeforeSeed(t *testing.T) {
	wallClock.mu.Lock()
	wallClock.seeded = false
	wallClock.mu.Unlock()
	instance.SetSlot(42)
	assert.Equal(t, uint64(42), WallClockSlot())
}
