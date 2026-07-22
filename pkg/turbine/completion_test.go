package turbine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/fixtures"
	"github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/gagliardetto/solana-go"
)

type addPacketResult struct {
	block *block.Block
	err   error
}

func feedFixturePrefix(t *testing.T, assembler *SlotAssembler, packets [][]byte) {
	t.Helper()
	for idx, packet := range packets {
		blk, err := assembler.AddPacket(packet)
		if err != nil {
			t.Fatalf("AddPacket(%d): %v", idx, err)
		}
		if blk != nil {
			t.Fatalf("AddPacket(%d) completed before final packet", idx)
		}
	}
}

func waitSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func TestSlotAssemblerCompletionRunsOutsideMutexAndClaimsOnce(t *testing.T) {
	packets := fixtures.DataShreds(t, "mainnet", 102815960)
	assembler := NewSlotAssembler()
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	var verifyCalls atomic.Int32
	assembler.verifyTransactions = func(context.Context, *block.Block) error {
		verifyCalls.Add(1)
		startedOnce.Do(func() { close(started) })
		<-release
		return nil
	}
	callbackObserved := make(chan bool, 1)
	assembler.SetOnComplete(func(slot uint64, _, _ uint32) {
		callbackObserved <- assembler.SlotCompleted(slot)
	})

	feedFixturePrefix(t, assembler, packets[:len(packets)-1])
	result := make(chan addPacketResult, 1)
	go func() {
		blk, err := assembler.AddPacket(packets[len(packets)-1])
		result <- addPacketResult{block: blk, err: err}
	}()
	waitSignal(t, started, "blocked transaction verifier")

	edges := make(chan [2]uint64, 1)
	go func() {
		latest, full := assembler.ShredEdges()
		edges <- [2]uint64{latest, full}
	}()
	select {
	case got := <-edges:
		if got != [2]uint64{102815960, 102815960} {
			t.Fatalf("shred edges while verifier blocked = %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("assembler mutex remained held during signature verification")
	}

	duplicate := make(chan addPacketResult, 1)
	go func() {
		blk, err := assembler.AddPacket(packets[len(packets)-1])
		duplicate <- addPacketResult{block: blk, err: err}
	}()
	select {
	case got := <-duplicate:
		if got.block != nil || got.err != nil {
			t.Fatalf("duplicate in-flight completion = block %v, err %v", got.block, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("duplicate shred blocked behind completed-slot processing")
	}

	releasedAt := time.Now()
	close(release)
	var got addPacketResult
	select {
	case got = <-result:
	case <-time.After(3 * time.Second):
		t.Fatal("completion did not finish after verifier release")
	}
	if got.err != nil || got.block == nil {
		t.Fatalf("completion = block %v, err %v", got.block, got.err)
	}
	if !got.block.TransactionSignaturesVerified() {
		t.Fatal("successful parallel verification did not set trust marker")
	}
	if timings, ok := got.block.TurbineIngressTimings(); !ok {
		t.Fatal("successful completion did not carry per-slot ingress timings")
	} else if timings.TransactionSigverify <= 0 || timings.BlockDecode <= 0 || timings.TransactionParse <= 0 {
		t.Fatalf("incomplete per-slot ingress timings: %+v", timings)
	}

	if got.block.ShredFullNanos == 0 || got.block.ShredFullNanos > releasedAt.UnixNano() {
		t.Fatalf("full timestamp %d was not captured before verifier release %d", got.block.ShredFullNanos, releasedAt.UnixNano())
	}
	if verifyCalls.Load() != 1 {
		t.Fatalf("verifier calls = %d, want one completion claim", verifyCalls.Load())
	}
	select {
	case completed := <-callbackObserved:
		if !completed {
			t.Fatal("callback did not observe completed marker")
		}
	case <-time.After(time.Second):
		t.Fatal("completion callback deadlocked on assembler mutex")
	}
}

func TestSlotAssemblerResetSupersedesInFlightCompletion(t *testing.T) {
	packets := fixtures.DataShreds(t, "mainnet", 102815960)
	assembler := NewSlotAssembler()
	started := make(chan struct{})
	release := make(chan struct{})
	assembler.verifyTransactions = func(context.Context, *block.Block) error {
		close(started)
		<-release
		return nil
	}
	var callbacks atomic.Int32
	assembler.SetOnComplete(func(uint64, uint32, uint32) { callbacks.Add(1) })

	feedFixturePrefix(t, assembler, packets[:len(packets)-1])
	result := make(chan addPacketResult, 1)
	go func() {
		blk, err := assembler.AddPacket(packets[len(packets)-1])
		result <- addPacketResult{block: blk, err: err}
	}()
	waitSignal(t, started, "in-flight completion")

	assembler.ResetSlot(102815960)
	if blk, err := assembler.AddPacket(packets[0]); err != nil || blk != nil {
		t.Fatalf("replacement first packet = block %v, err %v", blk, err)
	}
	close(release)
	select {
	case got := <-result:
		if got.block != nil || got.err != nil {
			t.Fatalf("stale completion = block %v, err %v", got.block, got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stale completion did not drain")
	}
	if assembler.SlotCompleted(102815960) {
		t.Fatal("stale generation marked replacement slot completed")
	}
	if _, ok := assembler.HeadShredDetail(102815960); !ok {
		t.Fatal("stale completion deleted replacement slot state")
	}
	if callbacks.Load() != 0 {
		t.Fatalf("stale completion invoked callback %d times", callbacks.Load())
	}
}

func TestSlotAssemblerVerificationFailureRemainsFailClosed(t *testing.T) {
	packets := fixtures.DataShreds(t, "mainnet", 102815960)
	assembler := NewSlotAssembler()
	verifyErr := errors.New("test invalid transaction signature")
	assembler.verifyTransactions = func(context.Context, *block.Block) error { return verifyErr }
	var callbacks atomic.Int32
	assembler.SetOnComplete(func(uint64, uint32, uint32) { callbacks.Add(1) })

	feedFixturePrefix(t, assembler, packets[:len(packets)-1])
	blk, err := assembler.AddPacket(packets[len(packets)-1])
	if !errors.Is(err, verifyErr) || blk != nil {
		t.Fatalf("invalid block completion = block %v, err %v", blk, err)
	}
	if assembler.SlotCompleted(102815960) {
		t.Fatal("invalid block was marked completed")
	}
	count, latest := assembler.SlotAssemblyErrors(102815960)
	if count == 0 || !strings.Contains(latest, verifyErr.Error()) {
		t.Fatalf("verification failure was not retained: count=%d latest=%q", count, latest)
	}
	if callbacks.Load() != 0 {
		t.Fatal("invalid block invoked completion callback")
	}
}

func generatedAlpenglowDataShreds(t *testing.T) []*Shred {
	t.Helper()
	leader := testShredLeader(t)
	gen := ShredGenerator{Slot: 100, ParentSlot: 99, Version: 7, ReferenceTick: 63}
	chainedRoot := solana.Hash{5}
	var nextData, nextCode uint32
	var dataShreds []*Shred
	for _, component := range buildAlpenglowSlot(t) {
		packets, root, newData, newCode, err := gen.MakeShredsFromData(
			leader, component.payload, component.isLastInSlot,
			chainedRoot, nextData, nextCode,
		)
		if err != nil {
			t.Fatalf("make component %s: %v", component.name, err)
		}
		chainedRoot, nextData, nextCode = root, newData, newCode
		for _, packet := range packets {
			shred, err := ParseShred(packet)
			if err != nil {
				t.Fatalf("parse component %s: %v", component.name, err)
			}
			if shred.Type == ShredTypeData {
				dataShreds = append(dataShreds, shred)
			}
		}
	}
	return dataShreds
}

func TestSlotAssemblerFinalizationHonorsHintLearnedDuringVerification(t *testing.T) {
	shreds := generatedAlpenglowDataShreds(t)
	assembler := NewSlotAssembler()
	started := make(chan struct{})
	release := make(chan struct{})
	assembler.verifyTransactions = func(context.Context, *block.Block) error {
		close(started)
		<-release
		return nil
	}
	for idx, shred := range shreds[:len(shreds)-1] {
		if blk, err := assembler.AddShred(shred); err != nil || blk != nil {
			t.Fatalf("AddShred(%d) = block %v, err %v", idx, blk, err)
		}
	}
	result := make(chan addPacketResult, 1)
	go func() {
		blk, err := assembler.AddShred(shreds[len(shreds)-1])
		result <- addPacketResult{block: blk, err: err}
	}()
	waitSignal(t, started, "Alpenglow verification")

	assembler.SetKnownAlpenglowBlockID(100, solana.Hash{0xff})
	close(release)
	select {
	case got := <-result:
		if got.block != nil || got.err != nil {
			t.Fatalf("late contradictory hint completion = block %v, err %v", got.block, got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Alpenglow completion did not drain")
	}
	if assembler.SlotCompleted(100) {
		t.Fatal("block contradicting a hint learned during verification completed")
	}
	count, slot, _, want := assembler.NonCanonicalBlockIDStats()
	if count != 1 || slot != 100 || want != (solana.Hash{0xff}) {
		t.Fatalf("noncanonical stats = count %d slot %d want %s", count, slot, want)
	}
}
