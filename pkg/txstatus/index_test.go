package txstatus

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gagliardetto/solana-go"
)

func sig(n byte) solana.Signature {
	var s solana.Signature
	s[0] = n
	return s
}

func hash(n byte) solana.Hash {
	var h solana.Hash
	h[0] = n
	return h
}

func blockHeight(value uint64) *uint64 {
	return &value
}

func submit(index *Index, signature solana.Signature, recentBlockhash solana.Hash, deadline *uint64) {
	if err := index.SubmissionAttempted(signature, recentBlockhash, deadline); err != nil {
		panic(err)
	}
	index.Forwarded(signature)
}

// testIndex builds an index with a controllable clock so retention is testable
// without sleeping.
func testIndex(t *testing.T, max int, retention time.Duration) (*Index, *time.Time) {
	t.Helper()
	now := time.Unix(1700000000, 0).UTC()
	clock := &now
	idx, err := NewIndex(Config{
		MaxReceipts: max,
		Retention:   retention,
		Now:         func() time.Time { return *clock },
	})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	return idx, clock
}

// TestUnknownSignatureIsNotFailure is the single most important property. A
// caller that cannot distinguish "I have never heard of this" from "this
// failed" will resubmit a transaction that already landed, or abandon one that
// is still in flight.
func TestUnknownSignatureIsNotFailure(t *testing.T) {
	idx, _ := testIndex(t, 16, time.Hour)

	receipt, known := idx.Lookup(sig(9))
	if known {
		t.Fatal("an unsubmitted signature was reported as known")
	}
	if receipt.Status != StatusUnknown {
		t.Errorf("unknown signature has status %v", receipt.Status)
	}
	if receipt.Status == StatusFailed {
		t.Error("unknown was collapsed into failed")
	}
}

func TestForwardingAttemptDoesNotClaimNetworkDelivery(t *testing.T) {
	idx, _ := testIndex(t, 16, time.Hour)
	s := sig(18)
	if err := idx.SubmissionAttempted(s, hash(1), blockHeight(150)); err != nil {
		t.Fatalf("SubmissionAttempted: %v", err)
	}

	if r, _ := idx.Lookup(s); r.Status != StatusSubmissionUnknown {
		t.Fatalf("attempt status = %v", r.Status)
	}
	idx.Forwarded(s)
	if r, _ := idx.Lookup(s); r.Status != StatusSubmitted {
		t.Fatalf("forwarded status = %v", r.Status)
	}
}

// TestSubmittedThenLandedThenRooted walks the success path.
func TestSubmittedThenLandedThenRooted(t *testing.T) {
	idx, _ := testIndex(t, 16, time.Hour)
	s := sig(1)

	submit(idx, s, hash(1), blockHeight(150))
	if r, _ := idx.Lookup(s); r.Status != StatusSubmitted {
		t.Fatalf("after submit, status = %v", r.Status)
	}

	idx.Landed(s, 120, "")
	r, known := idx.Lookup(s)
	if !known || r.Status != StatusLanded {
		t.Fatalf("after landing, status = %v known = %v", r.Status, known)
	}
	if r.LandedSlot != 120 {
		t.Errorf("landed slot = %d, want 120", r.LandedSlot)
	}

	// Rooting a slot at or beyond the landing slot promotes the receipt.
	idx.Rooted(120)
	if r, _ := idx.Lookup(s); r.Status != StatusRooted {
		t.Errorf("after rooting through the landing slot, status = %v", r.Status)
	}
}

// TestRootingBelowTheLandingSlotDoesNotPromote guards the boundary. Rooting
// slot 119 says nothing about a transaction that landed in 120.
func TestRootingBelowTheLandingSlotDoesNotPromote(t *testing.T) {
	idx, _ := testIndex(t, 16, time.Hour)
	s := sig(2)
	submit(idx, s, hash(1), blockHeight(200))
	idx.Landed(s, 120, "")

	idx.Rooted(119)
	if r, _ := idx.Lookup(s); r.Status != StatusLanded {
		t.Errorf("rooting below the landing slot promoted to %v", r.Status)
	}
}

// TestOnChainFailureIsForkAware covers a transaction that executes with an
// error. The failure is terminal only after its bank roots.
func TestOnChainFailureIsRecordedAsFailed(t *testing.T) {
	idx, _ := testIndex(t, 16, time.Hour)
	s := sig(3)
	submit(idx, s, hash(1), blockHeight(200))
	idx.Landed(s, 130, "InstructionError: custom program error 0x1")

	r, known := idx.Lookup(s)
	if !known {
		t.Fatal("a landed-but-failed transaction is not known")
	}
	if r.Status != StatusLandedFailed || r.Status.Terminal() {
		t.Errorf("unrooted failure status = %v terminal=%v", r.Status, r.Status.Terminal())
	}
	if r.ExecutionError == "" {
		t.Error("execution error was not retained")
	}
	if r.LandedSlot != 130 {
		t.Errorf("a failed transaction still landed; slot = %d", r.LandedSlot)
	}

	idx.Rooted(130)
	if r, _ := idx.Lookup(s); r.Status != StatusFailed || !r.Status.Terminal() {
		t.Errorf("rooted failure status = %v terminal=%v", r.Status, r.Status.Terminal())
	}
}

func TestExecutionErrorIsBoundedSingleLineText(t *testing.T) {
	idx, _ := testIndex(t, 16, time.Hour)
	s := sig(17)
	submit(idx, s, hash(1), blockHeight(300))
	idx.Landed(s, 130, "\x00first\nsecond "+string([]byte{0xff})+" "+strings.Repeat("x", 1_000))

	r, _ := idx.Lookup(s)
	if len(r.ExecutionError) > maxExecutionErrorBytes {
		t.Fatalf("execution error length = %d", len(r.ExecutionError))
	}
	if strings.ContainsAny(r.ExecutionError, "\x00\n\r") || !utf8.ValidString(r.ExecutionError) {
		t.Fatalf("execution error is not safe text: %q", r.ExecutionError)
	}
}

func TestUnrootedFailureCanUnwindAndReland(t *testing.T) {
	idx, _ := testIndex(t, 16, time.Hour)
	s := sig(16)
	submit(idx, s, hash(1), blockHeight(300))
	idx.Landed(s, 200, "InstructionError")

	idx.Unwound(200)
	if r, _ := idx.Lookup(s); r.Status != StatusUnwound || r.ExecutionError != "" {
		t.Fatalf("unwound failure = %+v", r)
	}

	idx.Landed(s, 205, "")
	if r, _ := idx.Lookup(s); r.Status != StatusLanded || r.LandedSlot != 205 {
		t.Fatalf("re-landed receipt = %+v", r)
	}
}

// TestDuplicateSubmitIsIdempotent covers a resubmit of the same signature. It
// must not reset an already-observed outcome back to submitted.
func TestDuplicateSubmitIsIdempotent(t *testing.T) {
	idx, _ := testIndex(t, 16, time.Hour)
	s := sig(4)

	submit(idx, s, hash(1), blockHeight(200))
	idx.Landed(s, 140, "")
	submit(idx, s, hash(1), blockHeight(200))

	if r, _ := idx.Lookup(s); r.Status != StatusLanded {
		t.Errorf("a duplicate submit reset the status to %v", r.Status)
	}
	if idx.Len() != 1 {
		t.Errorf("duplicate submit created %d receipts", idx.Len())
	}
}

// TestForkUnwindAndReland covers the case the plan calls out explicitly: a
// transaction can land on a fork that is later abandoned, and may then land
// again on the surviving fork.
func TestForkUnwindAndReland(t *testing.T) {
	idx, _ := testIndex(t, 16, time.Hour)
	s := sig(5)
	submit(idx, s, hash(1), blockHeight(300))
	idx.Landed(s, 200, "")

	// The fork containing slot 200 is abandoned.
	idx.Unwound(200)
	r, known := idx.Lookup(s)
	if !known {
		t.Fatal("an unwound transaction became unknown")
	}
	if r.Status != StatusUnwound {
		t.Fatalf("after unwind, status = %v", r.Status)
	}
	if r.LandedSlot != 0 {
		t.Errorf("unwound receipt still claims landing slot %d", r.LandedSlot)
	}

	// It lands again on the surviving fork.
	idx.Landed(s, 205, "")
	if r, _ := idx.Lookup(s); r.Status != StatusLanded || r.LandedSlot != 205 {
		t.Errorf("re-landed receipt = %v at slot %d", r.Status, r.LandedSlot)
	}
}

// TestUnwindLeavesEarlierSlotsAlone pins that unwinding from slot N does not
// disturb a transaction that landed before the fork point.
func TestUnwindLeavesEarlierSlotsAlone(t *testing.T) {
	idx, _ := testIndex(t, 16, time.Hour)
	early, late := sig(6), sig(7)
	submit(idx, early, hash(1), blockHeight(300))
	submit(idx, late, hash(1), blockHeight(300))
	idx.Landed(early, 199, "")
	idx.Landed(late, 201, "")

	idx.Unwound(200)

	if r, _ := idx.Lookup(early); r.Status != StatusLanded {
		t.Errorf("a transaction below the fork point was unwound: %v", r.Status)
	}
	if r, _ := idx.Lookup(late); r.Status != StatusUnwound {
		t.Errorf("a transaction at or above the fork point survived: %v", r.Status)
	}
}

// TestRootedReceiptsSurviveUnwind pins that a rooted receipt is terminal. A
// rooted slot cannot be unwound, so an unwind claiming otherwise is ignored.
func TestRootedReceiptsSurviveUnwind(t *testing.T) {
	idx, _ := testIndex(t, 16, time.Hour)
	s := sig(8)
	submit(idx, s, hash(1), blockHeight(300))
	idx.Landed(s, 210, "")
	idx.Rooted(210)

	idx.Unwound(210)
	if r, _ := idx.Lookup(s); r.Status != StatusRooted {
		t.Errorf("a rooted receipt was unwound to %v", r.Status)
	}
}

// TestExpiryWhenBlockhashWindowPasses covers a transaction that never landed.
// Once the node is past the last valid block height it can no longer land,
// which is a different and more useful answer than "unknown".
func TestExpiryWhenBlockhashWindowPasses(t *testing.T) {
	idx, _ := testIndex(t, 16, time.Hour)
	s := sig(10)
	submit(idx, s, hash(1), blockHeight(250))

	idx.ObserveBlockHeight(250)
	if r, _ := idx.Lookup(s); r.Status != StatusSubmitted {
		t.Errorf("at the last valid block height the receipt is already %v", r.Status)
	}

	idx.ObserveBlockHeight(251)
	if r, _ := idx.Lookup(s); r.Status != StatusExpired || r.Status.Terminal() {
		t.Errorf("past the observed validity height, status = %v terminal=%v", r.Status, r.Status.Terminal())
	}
}

func TestSpeculativeExpiryCanRelandAfterForkRewind(t *testing.T) {
	idx, _ := testIndex(t, 16, time.Hour)
	s := sig(19)
	submit(idx, s, hash(1), blockHeight(250))

	idx.ObserveBlockHeight(251)
	if r, _ := idx.Lookup(s); r.Status != StatusExpired {
		t.Fatalf("status before rewind = %v", r.Status)
	}

	idx.Landed(s, 240, "")
	if r, _ := idx.Lookup(s); r.Status != StatusLanded || r.LandedSlot != 240 {
		t.Fatalf("stronger landing evidence did not replace speculative expiry: %+v", r)
	}
}

func TestForkRewindRestoresPreExpiryStatus(t *testing.T) {
	for _, status := range []Status{StatusSubmissionUnknown, StatusSubmitted, StatusUnwound} {
		t.Run(status.String(), func(t *testing.T) {
			idx, _ := testIndex(t, 16, time.Hour)
			s := sig(byte(status + 30))
			if err := idx.SubmissionAttempted(s, hash(1), blockHeight(250)); err != nil {
				t.Fatalf("SubmissionAttempted: %v", err)
			}
			if status != StatusSubmissionUnknown {
				idx.Forwarded(s)
			}
			if status == StatusUnwound {
				idx.Landed(s, 240, "")
				idx.Unwound(240)
			}

			idx.ObserveBlockHeight(300)
			if got := receiptStatus(t, idx, s); got != StatusExpired {
				t.Fatalf("status after expiry = %v", got)
			}
			idx.RewindBlockHeight(251)
			if got := receiptStatus(t, idx, s); got != StatusExpired {
				t.Fatalf("status above deadline = %v", got)
			}
			idx.RewindBlockHeight(250)
			if got := receiptStatus(t, idx, s); got != status {
				t.Fatalf("status after valid rewind = %v, want %v", got, status)
			}
		})
	}
}

func TestForwardedAfterExpiryRestoresSubmittedAfterRewind(t *testing.T) {
	idx, _ := testIndex(t, 16, time.Hour)
	s := sig(63)
	if err := idx.SubmissionAttempted(s, hash(1), blockHeight(250)); err != nil {
		t.Fatal(err)
	}

	idx.ObserveBlockHeight(251)
	if got := receiptStatus(t, idx, s); got != StatusExpired {
		t.Fatalf("status after expiry = %v", got)
	}
	idx.Forwarded(s)
	if got := receiptStatus(t, idx, s); got != StatusExpired {
		t.Fatalf("forwarded changed expired status to %v", got)
	}
	idx.RewindBlockHeight(250)
	if got := receiptStatus(t, idx, s); got != StatusSubmitted {
		t.Fatalf("status after rewind = %v", got)
	}
}

func receiptStatus(t *testing.T, idx *Index, signature solana.Signature) Status {
	t.Helper()
	receipt, known := idx.Lookup(signature)
	if !known {
		t.Fatal("receipt is unknown")
	}
	return receipt.Status
}

func TestUnknownBlockhashDeadlineDoesNotFabricateExpiry(t *testing.T) {
	idx, _ := testIndex(t, 16, time.Hour)
	s := sig(15)
	submit(idx, s, hash(1), nil)

	idx.ObserveBlockHeight(1_000_000)
	r, known := idx.Lookup(s)
	if !known || r.Status != StatusSubmitted {
		t.Fatalf("unknown deadline produced status %v known=%v", r.Status, known)
	}
	if r.LastValidBlockHeight != nil {
		t.Fatalf("unknown deadline became %d", *r.LastValidBlockHeight)
	}
}

// TestLandedReceiptsDoNotExpire pins that expiry only applies to transactions
// that never landed.
func TestLandedReceiptsDoNotExpire(t *testing.T) {
	idx, _ := testIndex(t, 16, time.Hour)
	s := sig(11)
	submit(idx, s, hash(1), blockHeight(250))
	idx.Landed(s, 240, "")

	idx.ObserveBlockHeight(400)
	if r, _ := idx.Lookup(s); r.Status != StatusLanded {
		t.Errorf("a landed receipt expired: %v", r.Status)
	}
}

func TestCapacityEvictsOldestReceiptWithoutBlockingNewSubmissions(t *testing.T) {
	idx, clock := testIndex(t, 3, time.Hour)
	for i := byte(1); i <= 3; i++ {
		submit(idx, sig(i), hash(1), blockHeight(500))
		*clock = clock.Add(time.Second)
	}
	if idx.Len() != 3 {
		t.Fatalf("Len = %d, want 3", idx.Len())
	}

	if err := idx.SubmissionAttempted(sig(4), hash(1), blockHeight(500)); err != nil {
		t.Fatalf("fourth unresolved receipt: %v", err)
	}
	if idx.Len() != 3 {
		t.Errorf("capacity exceeded: Len = %d", idx.Len())
	}
	if _, known := idx.Lookup(sig(1)); known {
		t.Error("the oldest unresolved receipt was retained")
	}
	if _, known := idx.Lookup(sig(4)); !known {
		t.Error("the new receipt was not inserted")
	}

	idx.Landed(sig(2), 100, "")
	idx.Rooted(100)
	if err := idx.SubmissionAttempted(sig(5), hash(1), blockHeight(500)); err != nil {
		t.Fatalf("completed receipt did not make room: %v", err)
	}
	if _, known := idx.Lookup(sig(2)); known {
		t.Error("the completed receipt was not evicted")
	}
	if _, known := idx.Lookup(sig(5)); !known {
		t.Error("the new receipt was not inserted")
	}
}

func TestRetentionBoundsUnresolvedReceipts(t *testing.T) {
	idx, clock := testIndex(t, 16, time.Minute)
	s := sig(12)
	submit(idx, s, hash(1), blockHeight(500))

	*clock = clock.Add(30 * time.Second)
	if _, known := idx.Lookup(s); !known {
		t.Fatal("receipt dropped before its retention elapsed")
	}

	*clock = clock.Add(31 * time.Second)
	if _, known := idx.Lookup(s); known {
		t.Error("unresolved receipt outlived its retention window")
	}
}

func TestDurableRewindLowersRootWatermark(t *testing.T) {
	idx, _ := testIndex(t, 16, time.Hour)
	s := sig(20)
	submit(idx, s, hash(1), blockHeight(500))
	idx.Landed(s, 180, "")
	idx.Rooted(200)
	if r, _ := idx.Lookup(s); r.Status != StatusRooted {
		t.Fatalf("initial status = %v", r.Status)
	}

	idx.DurableRewound(149)
	idx.Unwound(150)
	idx.Landed(s, 180, "")
	if r, _ := idx.Lookup(s); r.Status != StatusLanded {
		t.Fatalf("re-land below stale root watermark became %v", r.Status)
	}
	idx.Rooted(180)
	if r, _ := idx.Lookup(s); r.Status != StatusRooted {
		t.Fatalf("re-rooted status = %v", r.Status)
	}
}

// TestLandedAndRootedForUnknownSignatureIsIgnored covers replay reporting a
// transaction we never submitted. The index tracks OUR submissions only; it
// must not start accumulating the whole chain.
func TestLandedForUnknownSignatureIsIgnored(t *testing.T) {
	idx, _ := testIndex(t, 16, time.Hour)
	idx.Landed(sig(13), 300, "")
	if idx.Len() != 0 {
		t.Errorf("a foreign transaction was indexed: Len = %d", idx.Len())
	}
	if _, known := idx.Lookup(sig(13)); known {
		t.Error("a foreign transaction became known")
	}
}

// TestConcurrentUse runs the index under the race detector from several
// goroutines, mirroring replay publishing while RPC submits and reads.
func TestConcurrentUse(t *testing.T) {
	idx, _ := testIndex(t, 512, time.Hour)
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range 100 {
				s := sig(byte(w*10 + i%10))
				submit(idx, s, hash(byte(i)), blockHeight(uint64(1000+i)))
				idx.Landed(s, uint64(500+i), "")
				idx.ObserveBlockHeight(uint64(500 + i))
				idx.Rooted(uint64(400 + i))
				idx.Unwound(uint64(900 + i))
				idx.Lookup(s)
				idx.Len()
			}
		}(worker)
	}
	wg.Wait()
}

// TestConfigIsValidated keeps an unbounded index from being constructed by
// accident — the bound is the whole point.
func TestConfigIsValidated(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no capacity":       {MaxReceipts: 0, Retention: time.Minute},
		"negative capacity": {MaxReceipts: -1, Retention: time.Minute},
		"no retention":      {MaxReceipts: 8, Retention: 0},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewIndex(cfg); err == nil {
				t.Error("an unbounded configuration was accepted")
			}
		})
	}

	if _, err := NewIndex(Config{MaxReceipts: 8, Retention: time.Minute}); err != nil {
		t.Errorf("a valid configuration was rejected: %v", err)
	}
}

// TestReceiptsAreCopies pins that a caller cannot mutate indexed state through
// a returned receipt.
func TestReceiptsAreCopies(t *testing.T) {
	idx, _ := testIndex(t, 16, time.Hour)
	s := sig(14)
	submit(idx, s, hash(1), blockHeight(500))
	idx.Landed(s, 300, "")

	r, _ := idx.Lookup(s)
	r.LandedSlot = 999
	r.Status = StatusFailed
	*r.LastValidBlockHeight = 999
	if r.LandedSlot != 999 || r.Status != StatusFailed {
		t.Fatal("the returned copy could not be modified; the test proves nothing")
	}

	if again, _ := idx.Lookup(s); again.LandedSlot != 300 ||
		again.Status != StatusLanded ||
		again.LastValidBlockHeight == nil ||
		*again.LastValidBlockHeight != 500 {
		t.Errorf("indexed receipt was mutated through a returned copy: %+v", again)
	}
}

// TestStatusStringsAreStable guards the wire representation, since these names
// reach an RPC response and an operator's screen.
func TestStatusStringsAreStable(t *testing.T) {
	want := map[Status]string{
		StatusUnknown:           "unknown",
		StatusSubmissionUnknown: "submission_unknown",
		StatusSubmitted:         "submitted",
		StatusLanded:            "landed",
		StatusLandedFailed:      "landed_failed",
		StatusRooted:            "rooted",
		StatusFailed:            "failed",
		StatusUnwound:           "unwound",
		StatusExpired:           "expired",
	}
	for status, name := range want {
		if got := status.String(); got != name {
			t.Errorf("Status(%d).String() = %q, want %q", status, got, name)
		}
	}
	if got := Status(200).String(); got != "unknown" {
		t.Errorf("an out-of-range status rendered as %q, want %q", got, "unknown")
	}
}

func TestSinkInterfaceIsSatisfied(t *testing.T) {
	idx, _ := testIndex(t, 4, time.Minute)
	var _ Sink = idx
	fmt.Fprint(nopWriter{}, idx.Len())
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
