package replay

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/rootedevents"
	"github.com/Overclock-Validator/mithril/pkg/state"
	"github.com/Overclock-Validator/mithril/pkg/tpu/txfixture"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testRootedEventIdentity(slot, parent uint64) rootedevents.SlotIdentity {
	return rootedevents.SlotIdentity{
		Slot: slot, ParentSlot: parent, Blockhash: [32]byte{byte(slot + 1)}, ParentBlockhash: [32]byte{byte(parent + 1)},
	}
}

func testRootedEventObservation(t testing.TB, index uint32) rootedevents.TransactionObservation {
	t.Helper()
	tx, err := solana.TransactionFromBytes(txfixture.MustSignedTransferWire(0))
	require.NoError(t, err)
	observation, err := PrepareRootedTransactionObservation(tx, index)
	require.NoError(t, err)
	observation.Succeeded = true
	return observation
}

// slowCommitter delays each CommitBatch so tests can observe the in-flight
// window; it also records call times to prove the loop was never blocked.
type slowCommitter struct {
	fakeCommitter
	mu    sync.Mutex
	delay time.Duration
}

type orderedCommitter struct {
	fakeCommitter
	order *[]string
}

func (c *orderedCommitter) CommitBatch(deltas []accounts.SlotDelta, throughSlot uint64, bankhashes map[uint64][32]byte, resumeCtx []byte) (accountsdb.BatchCommitResult, error) {
	*c.order = append(*c.order, "account-commit")
	return c.fakeCommitter.CommitBatch(deltas, throughSlot, bankhashes, resumeCtx)
}

func TestFoldJobSnapshotsStatusOnLoopAndReferenceRidesManifest(t *testing.T) {
	rootDir := t.TempDir()
	fc := &fakeCommitter{durable: accounts.NewMemAccounts()}
	tail := asyncTestTail(fc, 5, 6, 7)
	originalCtx := tail.contexts[6]
	scratch := []byte("immutable-status-at-six")
	snapshotCalled := false
	installCalled := false
	afterCommitCalled := false
	hooks := TransactionStatusCheckpointHooks{
		Snapshot: func(through uint64) ([]byte, error) {
			require.Equal(t, uint64(6), through)
			snapshotCalled = true
			return scratch, nil
		},
		Install: func(through uint64, payload []byte) (*state.TransactionStatusCheckpointRef, error) {
			require.True(t, snapshotCalled, "worker install ran before loop snapshot")
			require.Equal(t, []byte("immutable-status-at-six"), payload, "fold job did not own an immutable payload copy")
			installCalled = true
			return PrepareTransactionStatusCheckpoint(rootDir, through, payload)
		},
		AfterCommit: func(selected *state.TransactionStatusCheckpointRef) error {
			require.True(t, installCalled)
			require.Equal(t, []uint64{5, 6}, fc.committed, "retention ran before CommitBatch completed")
			require.NotNil(t, selected)
			require.Equal(t, uint64(6), selected.Root)
			afterCommitCalled = true
			return nil
		},
	}
	require.NoError(t, tail.SetTransactionStatusCheckpointHooks(hooks))

	job, err := tail.buildFoldJob(7, false)
	require.NoError(t, err)
	require.NotNil(t, job)
	require.True(t, snapshotCalled, "status capture must finish on the replay loop during job construction")
	require.False(t, installCalled, "sidecar I/O belongs to the fold worker")
	require.Nil(t, originalCtx.TransactionStatusCheckpoint, "building a job mutated the tail's retained context")

	// If a snapshotter later reuses its scratch storage, the worker still sees
	// the byte-exact payload captured when this job was built.
	scratch[0] = 'X'
	require.NoError(t, runFoldJob(fc, job))
	require.True(t, installCalled)
	require.True(t, afterCommitCalled)
	require.NotNil(t, job.ctx.TransactionStatusCheckpoint)

	var manifestCtx state.ResumeContext
	require.NoError(t, json.Unmarshal(fc.ctxs[6], &manifestCtx))
	require.Equal(t, job.ctx.TransactionStatusCheckpoint, manifestCtx.TransactionStatusCheckpoint)
	require.Equal(t, uint64(6), manifestCtx.TransactionStatusCheckpoint.Root)
	payload, err := ReadTransactionStatusCheckpoint(rootDir, manifestCtx.TransactionStatusCheckpoint)
	require.NoError(t, err)
	require.Equal(t, []byte("immutable-status-at-six"), payload)
}

func TestFoldJobRootedEventsRideManifest(t *testing.T) {
	rootDir := t.TempDir()
	fc := &fakeCommitter{durable: accounts.NewMemAccounts()}
	tail := asyncTestTail(fc, 5, 6)
	afterCommitCalled := false
	require.NoError(t, tail.SetRootedEventHooks(RootedEventHooks{
		FinalitySourceForSlot: fixedRootedEventFinality(rootedevents.FinalityRPCFinalized),
		Install: func(deltas []accounts.SlotDelta, metadata map[uint64]rootedevents.SlotMeta) (*state.RootedEventBatchRef, error) {
			return rootedevents.PrepareSidecar(rootDir, deltas, metadata)
		},
		AfterCommit: func(selected *state.RootedEventBatchRef) error {
			require.Equal(t, []uint64{5, 6}, fc.committed, "retention ran before CommitBatch completed")
			require.NotNil(t, selected)
			afterCommitCalled = true
			return nil
		},
	}))
	require.NoError(t, tail.RecordRootedEventSlot(testRootedEventIdentity(5, 4), []rootedevents.TransactionObservation{testRootedEventObservation(t, 0)}))
	require.NoError(t, tail.RecordRootedEventSlot(testRootedEventIdentity(6, 5), nil))

	job, err := tail.buildFoldJob(6, false)
	require.NoError(t, err)
	require.NoError(t, runFoldJob(fc, job))
	require.True(t, afterCommitCalled)

	var manifestCtx state.ResumeContext
	require.NoError(t, json.Unmarshal(fc.ctxs[6], &manifestCtx))
	require.NotNil(t, manifestCtx.RootedEventBatch)
	require.Equal(t, uint64(5), manifestCtx.RootedEventBatch.FromSlot)
	require.Equal(t, uint64(6), manifestCtx.RootedEventBatch.ThroughSlot)
	var events []rootedevents.Event
	require.NoError(t, rootedevents.ReadSidecar(rootDir, manifestCtx.RootedEventBatch, func(event rootedevents.Event) error {
		events = append(events, event)
		return nil
	}))
	require.Len(t, events, 5)
	require.Equal(t, rootedevents.TransactionExecuted, events[0].Kind)
	require.Equal(t, rootedevents.SlotRooted, events[len(events)-1].Kind)

	tail.applyFoldJob(job)
	require.Empty(t, tail.identities)
	require.Empty(t, tail.transactions)
	require.Zero(t, tail.transactionBytes)
}

func TestFoldJobRootedEventsRequireEverySlotCapture(t *testing.T) {
	fc := &fakeCommitter{durable: accounts.NewMemAccounts()}
	tail := asyncTestTail(fc, 5, 6)
	require.NoError(t, tail.SetRootedEventHooks(RootedEventHooks{
		FinalitySourceForSlot: fixedRootedEventFinality(rootedevents.FinalityRPCFinalized),
		Install: func([]accounts.SlotDelta, map[uint64]rootedevents.SlotMeta) (*state.RootedEventBatchRef, error) {
			t.Fatal("install must not run when a slot was never captured")
			return nil, nil
		},
	}))
	require.NoError(t, tail.RecordRootedEventSlot(testRootedEventIdentity(5, 4), nil))
	tail.identities[6] = testRootedEventIdentity(6, 5) // isolate the missing-capture guard from lineage validation

	job, err := tail.buildFoldJob(6, false)
	require.ErrorContains(t, err, "no transaction capture recorded for slot 6")
	require.Nil(t, job)
	require.Empty(t, fc.committed)
}

func TestForcedFoldSelectsRootedEvents(t *testing.T) {
	rootDir := t.TempDir()
	fc := &fakeCommitter{durable: accounts.NewMemAccounts()}
	tail := newUnrootedTail(&fakeDurable{}, fc, 512, 2, "")
	tail.Add(5, []*accounts.Account{testAccount(1, 5)}, testHashBytes(5))
	tail.SetContext(5, &state.ResumeContext{Slot: 5})
	require.NoError(t, tail.SetRootedEventHooks(RootedEventHooks{
		FinalitySourceForSlot: fixedRootedEventFinality(rootedevents.FinalityRPCFinalized),
		Install: func(deltas []accounts.SlotDelta, metadata map[uint64]rootedevents.SlotMeta) (*state.RootedEventBatchRef, error) {
			return rootedevents.PrepareSidecar(rootDir, deltas, metadata)
		},
	}))
	require.NoError(t, tail.RecordRootedEventSlot(testRootedEventIdentity(5, 4), []rootedevents.TransactionObservation{testRootedEventObservation(t, 0)}))

	promoted, ctx, err := tail.flush(5)
	require.NoError(t, err)
	require.Equal(t, uint64(5), promoted)
	require.NotNil(t, ctx.RootedEventBatch)
	var manifestCtx state.ResumeContext
	require.NoError(t, json.Unmarshal(fc.ctxs[5], &manifestCtx))
	require.Equal(t, ctx.RootedEventBatch, manifestCtx.RootedEventBatch)
}

func TestFoldJobSidecarOrderAndReferences(t *testing.T) {
	rootDir := t.TempDir()
	var order []string
	committer := &orderedCommitter{
		fakeCommitter: fakeCommitter{durable: accounts.NewMemAccounts()},
		order:         &order,
	}
	tail := asyncTestTail(committer, 5, 6)
	require.NoError(t, tail.SetTransactionStatusCheckpointHooks(TransactionStatusCheckpointHooks{
		Snapshot: func(uint64) ([]byte, error) {
			order = append(order, "status-snapshot")
			return []byte("status"), nil
		},
		Install: func(through uint64, payload []byte) (*state.TransactionStatusCheckpointRef, error) {
			order = append(order, "status-install")
			return PrepareTransactionStatusCheckpoint(rootDir, through, payload)
		},
		AfterCommit: func(*state.TransactionStatusCheckpointRef) error {
			order = append(order, "status-after")
			return nil
		},
	}))
	require.NoError(t, tail.SetRootedEventHooks(RootedEventHooks{
		FinalitySourceForSlot: fixedRootedEventFinality(rootedevents.FinalityRPCFinalized),
		Install: func(deltas []accounts.SlotDelta, metadata map[uint64]rootedevents.SlotMeta) (*state.RootedEventBatchRef, error) {
			order = append(order, "events-install")
			return rootedevents.PrepareSidecar(rootDir, deltas, metadata)
		},
		AfterCommit: func(*state.RootedEventBatchRef) error {
			order = append(order, "events-after")
			return errors.New("advisory cleanup failure")
		},
	}))
	require.NoError(t, tail.RecordRootedEventSlot(testRootedEventIdentity(5, 4), nil))
	require.NoError(t, tail.RecordRootedEventSlot(testRootedEventIdentity(6, 5), nil))

	job, err := tail.buildFoldJob(6, false)
	require.NoError(t, err)
	require.Equal(t, []string{"status-snapshot"}, order)
	require.NoError(t, runFoldJob(committer, job), "advisory cleanup must not fail a durable fold")
	require.Equal(t, []string{
		"status-snapshot", "status-install", "events-install", "account-commit", "status-after", "events-after",
	}, order)
	require.NotNil(t, job.ctx.TransactionStatusCheckpoint)
	require.NotNil(t, job.ctx.RootedEventBatch)
}

func TestFoldJobRootedEventInstallFailureCannotCommitOrPrune(t *testing.T) {
	fc := &fakeCommitter{durable: accounts.NewMemAccounts()}
	tail := asyncTestTail(fc, 5, 6)
	require.NoError(t, tail.SetRootedEventHooks(RootedEventHooks{
		FinalitySourceForSlot: fixedRootedEventFinality(rootedevents.FinalityRPCFinalized),
		Install: func([]accounts.SlotDelta, map[uint64]rootedevents.SlotMeta) (*state.RootedEventBatchRef, error) {
			return nil, errors.New("sidecar fsync failed")
		},
	}))
	require.NoError(t, tail.RecordRootedEventSlot(testRootedEventIdentity(5, 4), nil))
	require.NoError(t, tail.RecordRootedEventSlot(testRootedEventIdentity(6, 5), nil))

	job, err := tail.buildFoldJob(6, false)
	require.NoError(t, err)
	require.ErrorContains(t, runFoldJob(fc, job), "sidecar fsync failed")
	require.Empty(t, fc.committed)
	require.Equal(t, 2, tail.overlay.HeldSlots())
	require.Contains(t, tail.transactions, uint64(5))
	require.Contains(t, tail.transactions, uint64(6))
}

func TestFoldJobCheckpointFailuresCannotReachCommitBatch(t *testing.T) {
	t.Run("snapshot failure refuses job", func(t *testing.T) {
		fc := &fakeCommitter{durable: accounts.NewMemAccounts()}
		tail := asyncTestTail(fc, 5, 6)
		require.NoError(t, tail.SetTransactionStatusCheckpointHooks(TransactionStatusCheckpointHooks{
			Snapshot: func(uint64) ([]byte, error) { return nil, errors.New("snapshot boom") },
			Install: func(uint64, []byte) (*state.TransactionStatusCheckpointRef, error) {
				t.Fatal("install must not run")
				return nil, nil
			},
		}))

		job, err := tail.buildFoldJob(6, false)
		require.ErrorContains(t, err, "snapshot boom")
		require.Nil(t, job)
		require.Empty(t, fc.throughs)
		require.Equal(t, 2, tail.overlay.HeldSlots())
	})

	t.Run("install failure refuses account manifest", func(t *testing.T) {
		fc := &fakeCommitter{durable: accounts.NewMemAccounts()}
		tail := asyncTestTail(fc, 5, 6)
		require.NoError(t, tail.SetTransactionStatusCheckpointHooks(TransactionStatusCheckpointHooks{
			Snapshot: func(uint64) ([]byte, error) { return []byte("captured"), nil },
			Install: func(uint64, []byte) (*state.TransactionStatusCheckpointRef, error) {
				return nil, errors.New("fsync boom")
			},
		}))

		job, err := tail.buildFoldJob(6, false)
		require.NoError(t, err)
		require.ErrorContains(t, runFoldJob(fc, job), "fsync boom")
		require.Empty(t, fc.throughs, "CommitBatch ran after checkpoint installation failed")
		require.Equal(t, 2, tail.overlay.HeldSlots(), "failed fold changed the speculative tail")
	})

	t.Run("commit failure does not run retention", func(t *testing.T) {
		rootDir := t.TempDir()
		fc := &fakeCommitter{durable: accounts.NewMemAccounts(), failOn: 5}
		tail := asyncTestTail(fc, 5, 6)
		afterCommitCalled := false
		require.NoError(t, tail.SetTransactionStatusCheckpointHooks(TransactionStatusCheckpointHooks{
			Snapshot: func(uint64) ([]byte, error) { return []byte("captured"), nil },
			Install: func(through uint64, payload []byte) (*state.TransactionStatusCheckpointRef, error) {
				return PrepareTransactionStatusCheckpoint(rootDir, through, payload)
			},
			AfterCommit: func(*state.TransactionStatusCheckpointRef) error {
				afterCommitCalled = true
				return nil
			},
		}))

		job, err := tail.buildFoldJob(6, false)
		require.NoError(t, err)
		require.ErrorContains(t, runFoldJob(fc, job), "commit boom")
		require.False(t, afterCommitCalled)
		require.Empty(t, fc.throughs)
	})
}

func TestForcedFoldCarriesStatusCheckpointReference(t *testing.T) {
	rootDir := t.TempDir()
	fc := &fakeCommitter{durable: accounts.NewMemAccounts()}
	tail := asyncTestTail(fc, 5)
	afterCommitCalled := false
	require.NoError(t, tail.SetTransactionStatusCheckpointHooks(TransactionStatusCheckpointHooks{
		Snapshot: func(through uint64) ([]byte, error) {
			require.Equal(t, uint64(5), through)
			return []byte("forced-partial-status"), nil
		},
		Install: func(through uint64, payload []byte) (*state.TransactionStatusCheckpointRef, error) {
			return PrepareTransactionStatusCheckpoint(rootDir, through, payload)
		},
		AfterCommit: func(selected *state.TransactionStatusCheckpointRef) error {
			require.Equal(t, []uint64{5}, fc.committed, "forced-fold retention ran before CommitBatch completed")
			require.NotNil(t, selected)
			afterCommitCalled = true
			return errors.New("advisory gc boom")
		},
	}))

	promoted, rootedCtx, err := tail.flush(5)
	require.NoError(t, err)
	require.Equal(t, uint64(5), promoted)
	require.NotNil(t, rootedCtx)
	require.NotNil(t, rootedCtx.TransactionStatusCheckpoint)
	require.True(t, afterCommitCalled)
	var manifestCtx state.ResumeContext
	require.NoError(t, json.Unmarshal(fc.ctxs[5], &manifestCtx))
	require.Equal(t, rootedCtx.TransactionStatusCheckpoint, manifestCtx.TransactionStatusCheckpoint)
}

func TestCheckpointAfterCommitRequiresDurabilityHooks(t *testing.T) {
	tail := asyncTestTail(&fakeCommitter{durable: accounts.NewMemAccounts()}, 5)
	err := tail.SetTransactionStatusCheckpointHooks(TransactionStatusCheckpointHooks{
		AfterCommit: func(*state.TransactionStatusCheckpointRef) error { return nil },
	})
	require.ErrorContains(t, err, "requires Snapshot and Install")
}

func (c *slowCommitter) CommitBatch(deltas []accounts.SlotDelta, throughSlot uint64, bankhashes map[uint64][32]byte, resumeCtx []byte) (accountsdb.BatchCommitResult, error) {
	time.Sleep(c.delay)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fakeCommitter.CommitBatch(deltas, throughSlot, bankhashes, resumeCtx)
}

func asyncTestTail(committer batchCommitter, slots ...uint64) *unrootedTail {
	tail := newUnrootedTail(&fakeDurable{}, committer, 512, 2, "")
	for i, s := range slots {
		tail.Add(s, []*accounts.Account{testAccount(byte(i+1), s)}, testHashBytes(byte(s)))
		tail.SetContext(s, &state.ResumeContext{Slot: s})
	}
	return tail
}

// The build/run/apply split preserves the sync path's semantics: the overlay
// retains the chunk while the fold is in flight (reads stay correct) and
// drops it only at apply.
func TestAsyncFoldBuildRunApply(t *testing.T) {
	fc := &fakeCommitter{durable: accounts.NewMemAccounts()}
	tail := asyncTestTail(fc, 5, 6, 7)
	for _, slot := range []uint64{5, 6, 7} {
		tail.SetContext(slot, tail.contexts[slot], testUnwindBankSysvars(t, slot, slot*10))
	}

	job, err := tail.buildFoldJob(7, false)
	require.NoError(t, err)
	require.NotNil(t, job)
	assert.Equal(t, uint64(6), job.through, "one K=2 chunk: slots 5..6")

	// In-flight: overlay still holds everything (reads unaffected).
	assert.Equal(t, 3, tail.overlay.HeldSlots())

	require.NoError(t, runFoldJob(fc, job))
	assert.Equal(t, []uint64{5, 6}, fc.committed)
	// Committed but not applied: overlay STILL holds the chunk.
	assert.Equal(t, 3, tail.overlay.HeldSlots())

	ctx := tail.applyFoldJob(job)
	require.NotNil(t, ctx)
	assert.Equal(t, uint64(6), ctx.Slot)
	assert.Equal(t, 1, tail.overlay.HeldSlots(), "only slot 7 remains")
	assert.Empty(t, tail.bankhashes[uint64(5)])
	_, has5 := tail.contexts[5]
	assert.False(t, has5, "contexts pruned through the fold")
	assert.NotContains(t, tail.bankSysvars, uint64(5), "bank sysvars pruned through the fold")
	assert.NotContains(t, tail.bankSysvars, uint64(6), "chunk-top bank sysvars pruned through the fold")
	assert.Contains(t, tail.bankSysvars, uint64(7), "unfolded bank sysvars remain retained")
}

// A partial trailing chunk builds only under force (the shutdown flush).
func TestBuildFoldJobPartialChunkOnlyWhenForced(t *testing.T) {
	fc := &fakeCommitter{durable: accounts.NewMemAccounts()}
	tail := asyncTestTail(fc, 5) // one slot < K=2

	job, err := tail.buildFoldJob(5, false)
	require.NoError(t, err)
	assert.Nil(t, job, "partial chunk must stay in RAM without force")

	job, err = tail.buildFoldJob(5, true)
	require.NoError(t, err)
	require.NotNil(t, job)
	assert.Equal(t, uint64(5), job.through)
}

// A context-less chunk-top refuses to build: committing it would produce a
// manifest recovery cannot resume from.
func TestBuildFoldJobRefusesMissingContext(t *testing.T) {
	fc := &fakeCommitter{durable: accounts.NewMemAccounts()}
	tail := newUnrootedTail(&fakeDurable{}, fc, 512, 2, "")
	tail.Add(5, []*accounts.Account{testAccount(1, 5)}, testHashBytes(5))
	tail.Add(6, []*accounts.Account{testAccount(2, 6)}, testHashBytes(6))
	tail.SetContext(5, &state.ResumeContext{Slot: 5}) // 6 (chunk top) missing

	job, err := tail.buildFoldJob(6, false)
	require.Error(t, err)
	assert.Nil(t, job)
	assert.Equal(t, 2, tail.overlay.HeldSlots(), "nothing folds on a refused build")
}

// The promoter runs jobs off-thread: enqueue returns immediately, poll is
// non-blocking during the commit, and drain settles the in-flight job.
func TestAsyncPromoterOffLoopAndDrain(t *testing.T) {
	sc := &slowCommitter{fakeCommitter: fakeCommitter{durable: accounts.NewMemAccounts()}, delay: 60 * time.Millisecond}
	tail := asyncTestTail(sc, 5, 6, 7)
	p := newAsyncPromoter(sc, nil)
	defer p.stop()

	job, err := tail.buildFoldJob(7, false)
	require.NoError(t, err)
	require.NotNil(t, job)

	start := time.Now()
	p.enqueue(job)
	require.Less(t, time.Since(start), 20*time.Millisecond, "enqueue must not block on the commit")

	// While the worker commits, the loop keeps going: poll stays nil.
	assert.Nil(t, p.poll(), "poll must be non-blocking while the fold runs")
	assert.True(t, p.inFlight)

	// Drain blocks until completion — the fork-unwind / shutdown barrier.
	res := p.drain()
	require.NotNil(t, res)
	require.NoError(t, res.err)
	assert.Equal(t, uint64(6), res.job.through)
	assert.False(t, p.inFlight)
	assert.GreaterOrEqual(t, time.Since(start), sc.delay, "drain waited for the worker")

	ctx := tail.applyFoldJob(res.job)
	require.NotNil(t, ctx)
	assert.Equal(t, 1, tail.overlay.HeldSlots())
}

func TestAsyncPromoterOrdersTailDurableGenerationAtAdmission(t *testing.T) {
	fc := &fakeCommitter{durable: accounts.NewMemAccounts()}
	tail := asyncTestTail(fc, 5, 6, 7)
	beforeAdmission := tail.captureBank(7)
	job, err := tail.buildFoldJob(7, false)
	require.NoError(t, err)

	p := newAsyncPromoter(tail.asyncCommitter, &tail.durableGeneration)
	defer p.stop()
	beforeGeneration := tail.durableGeneration.Load()
	p.enqueue(job)
	admittedGeneration := tail.durableGeneration.Load()
	require.Equal(t, beforeGeneration+1, admittedGeneration)
	_, err = beforeAdmission.GetAccount(7, testKey(3))
	require.ErrorIs(t, err, errCapturedBankStale)

	afterAdmission := tail.captureBank(7)
	res := p.drain()
	require.NotNil(t, res)
	require.NoError(t, res.err)
	tail.applyFoldJob(res.job)
	require.Equal(t, admittedGeneration, tail.durableGeneration.Load(), "worker double-advanced the durable generation")

	_, err = afterAdmission.GetAccount(7, testKey(3))
	require.NoError(t, err)
}

func TestFoldCapacityBackpressureDrainsOneAdmittedFold(t *testing.T) {
	fc := &fakeCommitter{durable: accounts.NewMemAccounts()}
	tail := newUnrootedTail(&fakeDurable{}, fc, 2, 2, "")
	for slot := uint64(1); slot <= 2; slot++ {
		tail.Add(slot, []*accounts.Account{testAccount(byte(slot), slot)}, testHashBytes(byte(slot)))
		tail.SetContext(slot, &state.ResumeContext{Slot: slot})
	}
	job, err := tail.buildFoldJob(2, false)
	require.NoError(t, err)
	require.NotNil(t, job)
	p := newAsyncPromoter(fc, nil)
	defer p.stop()
	p.enqueue(job)

	// Replay completes one more slot while the admitted fold is running. The
	// cap handler must wait for that fold, apply it, and retain a recoverable tip.
	tail.Add(3, []*accounts.Account{testAccount(3, 3)}, testHashBytes(3))
	tail.SetContext(3, &state.ResumeContext{Slot: 3})
	require.True(t, tail.OverCap())
	stillOver := settleInFlightFoldAtCapacity(tail, p, 3, func(res *foldResult) {
		if res != nil && res.err == nil {
			tail.applyFoldJob(res.job)
		}
	})
	require.False(t, stillOver)
	require.False(t, p.inFlight)
	require.Equal(t, 1, tail.overlay.HeldSlots())
	require.NotNil(t, tail.contexts[3], "cap-crossing tip context was lost")

	// The retained tip remains force-foldable on shutdown; backpressure did not
	// merely hide a context-less slot by reducing HeldSlots.
	promoted, rootedCtx, err := tail.flush(3)
	require.NoError(t, err)
	require.Equal(t, uint64(3), promoted)
	require.NotNil(t, rootedCtx)
	require.Equal(t, uint64(3), rootedCtx.Slot)
}

func TestFoldCapacityBackpressureFailsClosed(t *testing.T) {
	t.Run("in-flight fold fails", func(t *testing.T) {
		fc := &fakeCommitter{durable: accounts.NewMemAccounts(), failOn: 1}
		tail := newUnrootedTail(&fakeDurable{}, fc, 2, 2, "")
		for slot := uint64(1); slot <= 3; slot++ {
			tail.Add(slot, []*accounts.Account{testAccount(byte(slot), slot)}, testHashBytes(byte(slot)))
			tail.SetContext(slot, &state.ResumeContext{Slot: slot})
		}
		job, err := tail.buildFoldJob(3, false)
		require.NoError(t, err)
		p := newAsyncPromoter(fc, nil)
		defer p.stop()
		p.enqueue(job)

		stillOver := settleInFlightFoldAtCapacity(tail, p, 3, func(res *foldResult) {
			if res != nil && res.err == nil {
				tail.applyFoldJob(res.job)
			}
		})
		require.True(t, stillOver, "failed fold must leave the cap halt armed")
		require.False(t, p.inFlight)
		require.Equal(t, 3, tail.overlay.HeldSlots())
		require.Empty(t, fc.committed, "capacity handler retried a failed fold")
	})

	t.Run("no admitted fold", func(t *testing.T) {
		fc := &fakeCommitter{durable: accounts.NewMemAccounts()}
		tail := newUnrootedTail(&fakeDurable{}, fc, 2, 2, "")
		for slot := uint64(1); slot <= 3; slot++ {
			tail.Add(slot, nil, testHashBytes(byte(slot)))
			tail.SetContext(slot, &state.ResumeContext{Slot: slot})
		}
		p := newAsyncPromoter(fc, nil)
		defer p.stop()

		require.True(t, settleInFlightFoldAtCapacity(tail, p, 3, nil))
		require.Empty(t, fc.committed, "capacity handler admitted new fold work")
	})

	t.Run("missing tip context", func(t *testing.T) {
		fc := &fakeCommitter{durable: accounts.NewMemAccounts()}
		tail := newUnrootedTail(&fakeDurable{}, fc, 2, 2, "")
		for slot := uint64(1); slot <= 2; slot++ {
			tail.Add(slot, nil, testHashBytes(byte(slot)))
			tail.SetContext(slot, &state.ResumeContext{Slot: slot})
		}
		job, err := tail.buildFoldJob(2, false)
		require.NoError(t, err)
		p := newAsyncPromoter(fc, nil)
		defer p.stop()
		p.enqueue(job)
		tail.Add(3, nil, testHashBytes(3)) // deliberately omit SetContext(3)

		require.True(t, settleInFlightFoldAtCapacity(tail, p, 3, func(res *foldResult) {
			if res != nil && res.err == nil {
				tail.applyFoldJob(res.job)
			}
		}))
		require.True(t, p.inFlight, "context-less tip must be rejected before draining hides it")
		require.Equal(t, 3, tail.overlay.HeldSlots())
	})
}

// A failed fold leaves the tail untouched (natural retry: the same chunk
// rebuilds because nothing advanced) and reports the error via the result.
func TestAsyncPromoterFailedFoldRetries(t *testing.T) {
	fc := &fakeCommitter{durable: accounts.NewMemAccounts(), failOn: 5}
	tail := asyncTestTail(fc, 5, 6, 7)
	p := newAsyncPromoter(fc, nil)
	defer p.stop()

	job, err := tail.buildFoldJob(7, false)
	require.NoError(t, err)
	p.enqueue(job)
	res := p.drain()
	require.NotNil(t, res)
	require.Error(t, res.err)
	assert.Equal(t, 3, tail.overlay.HeldSlots(), "failed fold must not touch the overlay")

	// Retry after the fault clears: identical chunk rebuilds and succeeds.
	fc.failOn = 0
	job2, err := tail.buildFoldJob(7, false)
	require.NoError(t, err)
	require.Equal(t, job.through, job2.through, "same chunk rebuilds after a failed fold")
	p.enqueue(job2)
	res = p.drain()
	require.NoError(t, res.err)
	tail.applyFoldJob(res.job)
	assert.Equal(t, 1, tail.overlay.HeldSlots())
}

// The shutdown flush uses the same gate-derived target as normal promotion.
// Slots above min(finality, verified) stay in RAM even under a forced flush.
func TestShutdownFlushCannotFoldPastGateTarget(t *testing.T) {
	fc := &fakeCommitter{durable: accounts.NewMemAccounts()}
	tail := asyncTestTail(fc, 5, 6, 7, 8, 9)

	// The shared gate: finality says 9, the verifier has only reached 7.
	target := safePromoteTarget(9, true, 7, 0)
	require.Equal(t, uint64(7), target)

	promoted, ctx, err := tail.flush(target)
	require.NoError(t, err)
	assert.Equal(t, uint64(7), promoted, "force-flush stops exactly at the gate target")
	require.NotNil(t, ctx)
	assert.Equal(t, uint64(7), ctx.Slot)
	assert.Equal(t, []uint64{5, 6, 7}, fc.committed)
	assert.Equal(t, 2, tail.overlay.HeldSlots(), "unverified slots 8,9 must survive shutdown in RAM")

	// And the divergence floor clamps even harder than the verifier.
	target = safePromoteTarget(9, true, 7, 6)
	assert.Equal(t, uint64(5), target, "persisted-divergence floor holds promotion below the disputed slot")
}
