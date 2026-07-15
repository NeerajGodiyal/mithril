package quicserver

import "sync/atomic"

type ServerStats struct {
	TotalConnections       atomic.Uint64
	TotalNewConnections    atomic.Uint64
	ActiveStreams          atomic.Uint64
	TotalNewStreams        atomic.Uint64
	InvalidStreamSize      atomic.Uint64
	TotalStreamReadErrors  atomic.Uint64
	TotalStreamReadTimeout atomic.Uint64
	ConnectionAddFailed    atomic.Uint64
	ConnectionRemoved      atomic.Uint64
	ConnectionSetupError   atomic.Uint64
	ConnectionSetupTimeout atomic.Uint64
	RefusedTooManyOpen     atomic.Uint64
	RefusedPerIP           atomic.Uint64
	OpenConnections        atomic.Uint64
	QuicEndpointsCount     atomic.Uint64
}
