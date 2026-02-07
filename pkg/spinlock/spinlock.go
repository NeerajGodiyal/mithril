package spinlock

import "sync/atomic"

// SpinLock loops an atomic CAS in an attempt to avoid goroutines being placed
// in the WAITING and then RUNNABLE state for some milliseconds by the
// scheduler.
type SpinLock struct {
	state atomic.Int32
}

func (s *SpinLock) Lock() {
	for !s.state.CompareAndSwap(0, 1) {
	}
}

func (s *SpinLock) Unlock() {
	s.state.Store(0)
}
