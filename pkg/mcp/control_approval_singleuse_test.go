package mcp

import (
	"context"
	"strings"
	"testing"
	"time"
)

// An approval authorises exactly one dispatch. Replaying a captured bundle must
// be refused, and the refusal must come from the approval itself rather than
// from whatever else happens to be true at the time.
//
// Nothing pinned that. Deleting `clear(c.pending)` from consumeApproval left
// the entire package green, because the only test covering replay goes through
// execute(), where a state barrier — one operation at a time, held until the
// previous approval generation can no longer be valid — refuses the second
// attempt first. That barrier is real protection, but it is a different
// mechanism with a different lifetime, and it made the nonce untestable through
// that path: the assertion accepted either refusal, and the one it actually
// got was never the nonce's.
//
// So this calls consumeApproval directly. Nothing downstream can supply the
// error, which is the whole point.
func TestAnApprovalNonceCannotBeConsumedTwice(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	runner := &fakeServiceRunner{status: []byte(activeServiceStatus)}
	controller := testServiceController(t, runner, now)

	prepared, err := controller.prepare(context.Background(), "restart", "")
	if err != nil {
		t.Fatal(err)
	}
	bundle := approveTestChallenge(t, prepared.Challenge, now)

	if _, _, err := controller.consumeApproval(bundle); err != nil {
		t.Fatalf("a freshly approved bundle was refused: %v", err)
	}

	_, _, err = controller.consumeApproval(bundle)
	if err == nil {
		t.Fatal("the same approval was consumed twice; a captured bundle would replay")
	}
	// The message is asserted because "some error occurred" is exactly what let
	// this invariant go unpinned: a different guard's refusal read as proof.
	if !strings.Contains(err.Error(), "already used") {
		t.Fatalf("refused, but not as a spent approval: %v", err)
	}
}

// Accepting one approval must also invalidate its siblings: every pending
// challenge was prepared against the same service state, so a second one would
// dispatch from a picture that is no longer true.
func TestAcceptingOneApprovalInvalidatesTheOthersPreparedBesideIt(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	runner := &fakeServiceRunner{status: []byte(activeServiceStatus)}
	controller := testServiceController(t, runner, now)

	first, err := controller.prepare(context.Background(), "restart", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := controller.prepare(context.Background(), "restart", "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Challenge == second.Challenge {
		t.Fatal("fixture error: two prepares produced one challenge")
	}

	if _, _, err := controller.consumeApproval(approveTestChallenge(t, first.Challenge, now)); err != nil {
		t.Fatalf("the first approval was refused: %v", err)
	}
	_, _, err = controller.consumeApproval(approveTestChallenge(t, second.Challenge, now))
	if err == nil {
		t.Fatal("a sibling approval survived, so two dispatches could race one state")
	}
	if !strings.Contains(err.Error(), "already used") {
		t.Fatalf("the sibling was refused, but not as no-longer-pending: %v", err)
	}
}
