package blockstream

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/alpenglow"
	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/turbine"
	"github.com/gagliardetto/solana-go"
)

func invalidTestLiveBlock(slot, parentSlot uint64, id, parentID solana.Hash) *b.Block {
	return &b.Block{
		Slot:                      slot,
		ParentSlot:                parentSlot,
		SourceParentSlot:          parentSlot,
		FromLiveStream:            true,
		AlpenglowBlockID:          id,
		HasAlpenglowBlockID:       true,
		AlpenglowParentBlockID:    parentID,
		HasAlpenglowParentBlockID: true,
	}
}

func waitInvalidTestDone(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestInvalidParentCannotRaceChildStagingCommit(t *testing.T) {
	parentID := solana.Hash{0x41}
	childID := solana.Hash{0x42}
	var observed, rejected atomic.Int32
	bs := NewBlockSource(&BlockSourceOpts{
		SourceType:                   BlockSourceTurbine,
		TurbineAlpenglowBlockIDHints: true,
		StartSlot:                    100,
		AlpenglowCandidateBlockSink: func(_ alpenglow.ReplayBlockObservation) {
			observed.Add(1)
		},
		AlpenglowInvalidBlockSink: func(blockID alpenglow.BlockID, _ string) error {
			if blockID.Slot == 101 && blockID.Hash == childID {
				rejected.Add(1)
			}
			return nil
		},
	})
	child := invalidTestLiveBlock(101, 100, childID, parentID)
	if !bs.observeAlpenglowCandidateBlock(child) {
		t.Fatal("child should pass initial validation before its parent is invalidated")
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	bs.beforeLiveBlockCommit = func(got *b.Block) {
		if got == child {
			close(entered)
			<-release
		}
	}
	done := make(chan struct{})
	go func() {
		bs.bufferLiveStreamBlock(child)
		close(done)
	}()
	<-entered

	bs.markInvalidAlpenglowBlockID(100, parentID)
	close(release)
	waitInvalidTestDone(t, done, "staging commit")

	bs.liveStagingMu.Lock()
	staged := bs.liveStagingBuffer[child.Slot]
	bs.liveStagingMu.Unlock()
	if staged != nil {
		t.Fatalf("invalid descendant committed to staging: %+v", staged)
	}
	if !bs.isInvalidAlpenglowBlockID(child.Slot, childID) {
		t.Fatal("child identity was not hard-tombstoned at staging commit")
	}
	if observed.Load() != 1 || rejected.Load() != 1 {
		t.Fatalf("candidate callbacks observed=%d rejected=%d, want 1/1", observed.Load(), rejected.Load())
	}
}

func TestInvalidParentCannotRaceChildReorderCommit(t *testing.T) {
	parentID := solana.Hash{0x51}
	childID := solana.Hash{0x52}
	var rejected atomic.Int32
	bs := NewBlockSource(&BlockSourceOpts{
		SourceType:                   BlockSourceTurbine,
		TurbineAlpenglowBlockIDHints: true,
		StartSlot:                    101,
		EndSlot:                      110,
		AlpenglowInvalidBlockSink: func(blockID alpenglow.BlockID, _ string) error {
			if blockID.Slot == 101 && blockID.Hash == childID {
				rejected.Add(1)
			}
			return nil
		},
	})
	child := invalidTestLiveBlock(101, 100, childID, parentID)

	entered := make(chan struct{})
	release := make(chan struct{})
	bs.beforeLiveBlockCommit = func(got *b.Block) {
		if got == child {
			close(entered)
			<-release
		}
	}
	emitterDone := make(chan struct{})
	go func() {
		bs.emitOrderedBlocks()
		close(emitterDone)
	}()
	bs.resultQueue <- fetchResult{slot: child.Slot, block: child}
	<-entered

	bs.markInvalidAlpenglowBlockID(100, parentID)
	close(release)
	close(bs.resultQueue)
	waitInvalidTestDone(t, emitterDone, "reorder emitter")

	bs.reorderMu.Lock()
	buffered := bs.reorderBuffer[child.Slot]
	bs.reorderMu.Unlock()
	if buffered != nil || bs.BufferDepth() != 0 {
		t.Fatalf("invalid descendant survived reorder commit: buffered=%v depth=%d", buffered != nil, bs.BufferDepth())
	}
	if !bs.isInvalidAlpenglowBlockID(child.Slot, childID) || rejected.Load() != 1 {
		t.Fatalf("reorder rejection tombstone=%t callbacks=%d", bs.isInvalidAlpenglowBlockID(child.Slot, childID), rejected.Load())
	}
}

func TestLiveCandidateObservationRunsOncePerBlock(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ingest bool
	}{
		{name: "ingested live block", ingest: true},
		{name: "direct live result", ingest: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var validated, observed atomic.Int32
			bs := NewBlockSource(&BlockSourceOpts{
				SourceType:                   BlockSourceTurbine,
				TurbineBindAddr:              "127.0.0.1:0",
				TurbineAlpenglowBlockIDHints: true,
				StartSlot:                    101,
				EndSlot:                      110,
				AlpenglowCandidateValidator: func(*b.Block) error {
					validated.Add(1)
					return nil
				},
				AlpenglowCandidateBlockSink: func(alpenglow.ReplayBlockObservation) {
					observed.Add(1)
				},
			})
			bs.isNearTip.Store(true)
			bs.liveHandoffSlot.Store(101)

			emitterDone := make(chan struct{})
			go func() {
				bs.emitOrderedBlocks()
				close(emitterDone)
			}()

			blk := invalidTestLiveBlock(101, 100, solana.Hash{0x53}, solana.Hash{0x52})
			if tc.ingest {
				if !bs.ingestLiveShredBlock(blk) {
					t.Fatal("live ingest unexpectedly stopped")
				}
			} else {
				bs.resultQueue <- fetchResult{slot: blk.Slot, block: blk}
			}

			select {
			case got := <-bs.streamChan:
				if got != blk {
					t.Fatalf("emitted block = %p, want %p", got, blk)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for emitted block")
			}
			close(bs.resultQueue)
			waitInvalidTestDone(t, emitterDone, "candidate observation emitter")

			if validated.Load() != 1 || observed.Load() != 1 {
				t.Fatalf("candidate callbacks validated=%d observed=%d, want 1/1", validated.Load(), observed.Load())
			}
		})
	}
}

func TestHardInvalidTombstoneCannotBeClearedByKnownHint(t *testing.T) {
	bs := NewBlockSource(&BlockSourceOpts{
		SourceType:                   BlockSourceTurbine,
		TurbineAlpenglowBlockIDHints: true,
		StartSlot:                    100,
	})
	invalidID := solana.Hash{0x61}
	siblingID := solana.Hash{0x62}
	bs.markInvalidAlpenglowBlockID(101, invalidID)

	bs.SetKnownAlpenglowBlockID(101, invalidID)
	if !bs.isInvalidAlpenglowBlockID(101, invalidID) {
		t.Fatal("known hint cleared an objective-invalid tombstone")
	}
	if got := bs.knownAlpenglowBlockIDs[101]; got == invalidID {
		t.Fatal("objective-invalid identity was restored to the known-hint cache")
	}

	bs.SetKnownAlpenglowBlockID(101, siblingID)
	if got := bs.knownAlpenglowBlockIDs[101]; got != siblingID {
		t.Fatalf("valid sibling hint = %s, want %s", got, siblingID)
	}
}

func TestReplacementReceiverDiscardsAdoptedInvalidSpoolSlot(t *testing.T) {
	const slot = uint64(700)
	dir := t.TempDir()
	seed, err := turbine.OpenShredSpool(dir, 0)
	if err != nil {
		t.Fatalf("open seed spool: %v", err)
	}
	seed.Append(slot, []byte("poisoned-complete-slot"))
	seed.MarkComplete(slot, 0, 1)
	seed.Close()

	adopted, err := turbine.OpenShredSpool(dir, 0)
	if err != nil {
		t.Fatalf("adopt spool: %v", err)
	}
	defer adopted.Close()
	if !adopted.HasSlot(slot) {
		t.Fatal("fixture did not adopt the poisoned slot")
	}

	bs := NewBlockSource(&BlockSourceOpts{
		SourceType:                   BlockSourceTurbine,
		TurbineAlpenglowBlockIDHints: true,
		StartSlot:                    slot,
	})
	invalidID := solana.Hash{0x71}
	bs.markInvalidAlpenglowBlockID(slot, invalidID)

	replacement := turbine.NewUDPReceiver("127.0.0.1:0")
	replacement.SetShredSpool(adopted)
	bs.attachAlpenglowBlockIDHintsToReceiver(replacement)
	if adopted.HasSlot(slot) {
		t.Fatal("replacement receiver retained poisoned adopted spool packets")
	}
	if _, complete := adopted.IsComplete(slot); complete {
		t.Fatal("replacement receiver retained poisoned completeness metadata")
	}

	// The tombstone is identity-specific: clearing the poisoned files must not
	// permanently ban the slot from receiving a valid sibling.
	replacement.SetKnownAlpenglowBlockID(slot, solana.Hash{0x72})
	adopted.Append(slot, []byte("valid-sibling-packet"))
	if !adopted.HasSlot(slot) {
		t.Fatal("valid sibling could not reuse the discarded slot")
	}
}

func TestQuarantineWaitsForPausedReplaySendAndDrainsIt(t *testing.T) {
	blockID := solana.Hash{0x81}
	blk := &b.Block{
		Slot:                100,
		FromLiveStream:      true,
		AlpenglowBlockID:    blockID,
		HasAlpenglowBlockID: true,
	}
	bs := NewBlockSource(&BlockSourceOpts{
		SourceType:                   BlockSourceTurbine,
		TurbineAlpenglowBlockIDHints: true,
		StartSlot:                    blk.Slot,
		EndSlot:                      110,
	})
	bs.isNearTip.Store(true)
	bs.liveHandoffSlot.Store(blk.Slot)

	sendPaused := make(chan struct{})
	releaseSend := make(chan struct{})
	bs.beforeReplayBlockSend = func(got *b.Block) {
		if got == blk {
			close(sendPaused)
			<-releaseSend
		}
	}
	emitterDone := make(chan struct{})
	go func() {
		bs.emitOrderedBlocks()
		close(emitterDone)
	}()
	bs.resultQueue <- fetchResult{slot: blk.Slot, block: blk}
	<-sendPaused

	quarantineDone := make(chan error, 1)
	go func() {
		quarantineDone <- bs.QuarantineInvalidAlpenglowBlock(blk)
	}()
	waitForBlockSourceCondition(t, func() bool {
		return bs.alpenglowQuarantineFrom.Load() == blk.Slot
	})
	select {
	case err := <-quarantineDone:
		t.Fatalf("quarantine crossed paused send early: %v", err)
	default:
	}

	close(releaseSend)
	if err := <-quarantineDone; err != nil {
		t.Fatalf("quarantine paused send: %v", err)
	}
	close(bs.resultQueue)
	waitInvalidTestDone(t, emitterDone, "paused-send emitter")

	if bs.BufferDepth() != 0 {
		t.Fatalf("quarantine left %d replay blocks queued", bs.BufferDepth())
	}
	if !bs.isInvalidAlpenglowBlockID(blk.Slot, blockID) {
		t.Fatal("paused emitted identity was not hard-tombstoned")
	}
	bs.reorderMu.Lock()
	next := bs.nextSlotToSend
	skipped := bs.skippedSlots[blk.Slot]
	bs.reorderMu.Unlock()
	if next != blk.Slot || skipped {
		t.Fatalf("quarantine frontier next=%d skipped=%t, want next=%d with no invented skip", next, skipped, blk.Slot)
	}
}
func TestQuarantineDrainsPausedDescendantAfterParentWasConsumed(t *testing.T) {
	parentID := solana.Hash{0x83}
	childID := solana.Hash{0x84}
	parent := &b.Block{
		Slot:                100,
		FromLiveStream:      true,
		AlpenglowBlockID:    parentID,
		HasAlpenglowBlockID: true,
	}
	child := invalidTestLiveBlock(101, parent.Slot, childID, parentID)
	var rootSinkCalls, childSinkCalls atomic.Int32
	bs := NewBlockSource(&BlockSourceOpts{
		SourceType:                   BlockSourceTurbine,
		TurbineAlpenglowBlockIDHints: true,
		StartSlot:                    child.Slot,
		EndSlot:                      110,
		AlpenglowInvalidBlockSink: func(blockID alpenglow.BlockID, _ string) error {
			switch blockID {
			case (alpenglow.BlockID{Slot: parent.Slot, Hash: parentID}):
				rootSinkCalls.Add(1)
			case (alpenglow.BlockID{Slot: child.Slot, Hash: childID}):
				childSinkCalls.Add(1)
			}
			return nil
		},
	})
	// Replay already consumed parent. Child is the unlocked channel send that
	// races replay discovering the parent's deterministic invalidity.
	bs.lastEmittedBlockSlot = parent.Slot
	bs.lastEmittedAlpenglowBlockID = parentID
	bs.hasLastEmittedAlpenglowBlockID = true
	bs.emittedAlpenglowBlockIDs[parent.Slot] = parentID
	bs.emittedAlpenglowBlockIDOrder = []uint64{parent.Slot}
	bs.nextSlotToSend = child.Slot
	bs.isNearTip.Store(true)
	bs.liveHandoffSlot.Store(child.Slot)

	sendPaused := make(chan struct{})
	releaseSend := make(chan struct{})
	bs.beforeReplayBlockSend = func(got *b.Block) {
		if got == child {
			close(sendPaused)
			<-releaseSend
		}
	}
	emitterDone := make(chan struct{})
	go func() {
		bs.emitOrderedBlocks()
		close(emitterDone)
	}()
	bs.resultQueue <- fetchResult{slot: child.Slot, block: child}
	<-sendPaused

	quarantineDone := make(chan error, 1)
	go func() {
		quarantineDone <- bs.QuarantineInvalidAlpenglowBlock(parent)
	}()
	waitForBlockSourceCondition(t, func() bool {
		return bs.alpenglowQuarantineFrom.Load() == parent.Slot
	})
	select {
	case err := <-quarantineDone:
		t.Fatalf("quarantine crossed paused child send early: %v", err)
	default:
	}

	close(releaseSend)
	if err := <-quarantineDone; err != nil {
		t.Fatalf("quarantine paused descendant send: %v", err)
	}
	close(bs.resultQueue)
	waitInvalidTestDone(t, emitterDone, "paused descendant emitter")

	if bs.BufferDepth() != 0 {
		t.Fatalf("quarantine left %d replay blocks queued", bs.BufferDepth())
	}
	if !bs.isInvalidAlpenglowBlockID(parent.Slot, parentID) ||
		!bs.isInvalidAlpenglowBlockID(child.Slot, childID) {
		t.Fatal("invalid emitted parent/descendant identities were not both hard-tombstoned")
	}
	if rootSinkCalls.Load() != 1 || childSinkCalls.Load() != 1 {
		t.Fatalf("invalid sink calls root=%d child=%d, want 1/1", rootSinkCalls.Load(), childSinkCalls.Load())
	}
	bs.reorderMu.Lock()
	next := bs.nextSlotToSend
	parentSkipped := bs.skippedSlots[parent.Slot]
	childSkipped := bs.skippedSlots[child.Slot]
	bs.reorderMu.Unlock()
	if next != parent.Slot || parentSkipped || childSkipped {
		t.Fatalf("quarantine frontier next=%d parent_skip=%t child_skip=%t, want next=%d with no invented skip",
			next, parentSkipped, childSkipped, parent.Slot)
	}
}

func TestQuarantineSinkErrorFailsClosedAfterCleanup(t *testing.T) {
	sinkErr := errors.New("decisive certificate names invalid block")
	blockID := solana.Hash{0x91}
	blk := &b.Block{
		Slot:                100,
		FromLiveStream:      true,
		AlpenglowBlockID:    blockID,
		HasAlpenglowBlockID: true,
	}
	var sinkCalls atomic.Int32
	bs := NewBlockSource(&BlockSourceOpts{
		SourceType:                   BlockSourceTurbine,
		TurbineAlpenglowBlockIDHints: true,
		StartSlot:                    blk.Slot,
		EndSlot:                      110,
		AlpenglowInvalidBlockSink: func(_ alpenglow.BlockID, _ string) error {
			sinkCalls.Add(1)
			return sinkErr
		},
	})
	bs.emittedAlpenglowBlockIDs[blk.Slot] = blockID
	bs.emittedAlpenglowBlockIDOrder = []uint64{blk.Slot}
	bs.lastEmittedBlockSlot = blk.Slot
	bs.lastEmittedAlpenglowBlockID = blockID
	bs.hasLastEmittedAlpenglowBlockID = true
	bs.nextSlotToSend = blk.Slot + 1
	bs.reorderBuffer[blk.Slot+1] = &b.Block{Slot: blk.Slot + 1, FromLiveStream: true}

	err := bs.QuarantineInvalidAlpenglowBlock(blk)
	if !errors.Is(err, sinkErr) {
		t.Fatalf("quarantine error = %v, want wrapped sink error", err)
	}
	if sinkCalls.Load() != 1 {
		t.Fatalf("invalid sink calls = %d, want 1", sinkCalls.Load())
	}
	if bs.stopReasonEnum() != blockSourceStopReasonInvalidAlpenglowCertificate ||
		!strings.Contains(bs.StopReason(), "objectively invalid") {
		t.Fatalf("quarantine did not fail closed: reason=%q", bs.StopReason())
	}
	if !bs.isInvalidAlpenglowBlockID(blk.Slot, blockID) {
		t.Fatal("sink conflict returned before hard tombstone cleanup")
	}
	bs.reorderMu.Lock()
	_, suffixBuffered := bs.reorderBuffer[blk.Slot+1]
	next := bs.nextSlotToSend
	bs.reorderMu.Unlock()
	if suffixBuffered || next != blk.Slot {
		t.Fatalf("sink conflict returned before rewind cleanup: suffix=%t next=%d", suffixBuffered, next)
	}
}
