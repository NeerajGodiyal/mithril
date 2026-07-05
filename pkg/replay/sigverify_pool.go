package replay

import (
	"runtime"
	"sync"
)

// Transaction signature verification runs CONCURRENT with execution: the
// snapshot is enqueued before the transaction executes, and each block's
// WaitGroup joins at ProcessBlock exit, so a block is never reported
// executed with unverified signatures. This used to spawn one goroutine per
// transaction — a 30k-tx block launched 30k goroutines mid-execution,
// paying spawn cost and scheduler churn against the execution workers. A
// fixed pool does the same work with zero per-transaction spawns.
//
// Sizing: half the machine. The heaviest observed blocks (~30k txs in a
// ~600ms execution window) demand roughly three cores of ed25519; half of
// GOMAXPROCS clears that with headroom on typical hardware while leaving
// the other half to the execution lanes. The bounded queue turns a
// verification backlog into gentle backpressure on the enqueuers instead of
// an unbounded goroutine pileup; the block-end WaitGroup drain is where any
// residual lag surfaces (inside the exec time, same as before).
const sigverifyQueueDepth = 8192

type sigverifyJob struct {
	snapshot *sigverifySnapshot
	wg       *sync.WaitGroup
}

var (
	sigverifyOnce  sync.Once
	sigverifyQueue chan sigverifyJob
)

// enqueueSigverify hands a snapshot to the verification pool. The caller
// must have added to wg; a worker calls verifySignatures, which Done()s it.
// An invalid signature panics in the worker — a deliberate halt, identical
// to the per-goroutine behavior it replaces.
func enqueueSigverify(snapshot *sigverifySnapshot, wg *sync.WaitGroup) {
	sigverifyOnce.Do(func() {
		sigverifyQueue = make(chan sigverifyJob, sigverifyQueueDepth)
		workers := max(2, runtime.GOMAXPROCS(0)/2)
		for i := 0; i < workers; i++ {
			go func() {
				for job := range sigverifyQueue {
					verifySignatures(job.snapshot, job.wg)
				}
			}()
		}
	})
	sigverifyQueue <- sigverifyJob{snapshot: snapshot, wg: wg}
}
