package quicserver

import (
	"net/netip"
	"sync"
	"sync/atomic"
)

type connectionGate struct {
	open atomic.Int32
	max  int32
}

func newConnectionGate(max int) *connectionGate {
	return &connectionGate{max: int32(max)}
}

func (g *connectionGate) tryAcquire() bool {
	for {
		cur := g.open.Load()
		if cur >= g.max {
			return false
		}
		if g.open.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

func (g *connectionGate) release() {
	g.open.Add(-1)
}

type perIPGate struct {
	mu    sync.Mutex
	max   int32
	count map[netip.Addr]int32
}

func newPerIPGate(max int) *perIPGate {
	return &perIPGate{
		max:   int32(max),
		count: make(map[netip.Addr]int32),
	}
}

func (g *perIPGate) tryAcquire(ip netip.Addr) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	cur := g.count[ip]
	if cur >= g.max {
		return false
	}
	g.count[ip] = cur + 1
	return true
}

func (g *perIPGate) release(ip netip.Addr) {
	g.mu.Lock()
	defer g.mu.Unlock()

	cur := g.count[ip]
	if cur <= 1 {
		delete(g.count, ip)
		return
	}
	g.count[ip] = cur - 1
}
