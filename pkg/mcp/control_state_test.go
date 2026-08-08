//go:build unix

package mcp

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/internal/controlaudit"
)

func testServiceOperation(t *testing.T, phase operationPhase) serviceOperation {
	t.Helper()
	now := time.Unix(1_700_000_000, 0)
	claims := []byte{0xa0}
	proof := []byte{1}
	var authorizationHash [32]byte
	authorizationHash[0] = 1
	var nonceHash [32]byte
	nonceHash[0] = 2
	approval := ControlApprovalEvidence{
		Version:                   approvalVersion,
		Domain:                    serviceApprovalAuditDomain,
		AuthorizationClaimsSHA256: authorizationHash,
		ActionID:                  "action-1",
		ApproverKeyID:             "approver-1",
		NonceSHA256:               nonceHash,
		IssuedAtUnix:              now.Unix(),
		ExpiresAtUnix:             now.Add(time.Minute).Unix(),
		ClaimsCBOR:                claims,
		Proof:                     proof,
	}
	approval.EvidenceSHA256 = approvalEvidenceHash(approval.Domain, claims, proof)
	status := serviceStatus{
		Unit:        "mithril-test.service",
		Scope:       "user",
		LoadState:   "loaded",
		ActiveState: "active",
		SubState:    "running",
		MainPID:     42,
	}
	op, err := newServiceOperation(
		"action-1",
		"session-1",
		"target-1",
		actionRestart,
		status,
		approval,
		now,
		now.Add(time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	setTestOperationLifecycle(t, &op, phase)
	return op
}

func setTestOperationLifecycle(t *testing.T, op *serviceOperation, phase operationPhase) {
	t.Helper()
	op.Phase = phase
	op.StatusAfter = nil
	op.AfterHash = ""
	op.Outcome = ""
	op.ReasonCode = ""
	op.DispatchMayHaveOccurred = false
	op.DispatchAccepted = false
	switch phase {
	case phasePrepared, phaseDispatchStarted:
	case phaseDispatched:
		op.DispatchMayHaveOccurred = true
		op.DispatchAccepted = true
	case phaseVerifying:
		op.DispatchMayHaveOccurred = true
		op.DispatchAccepted = true
	case phaseSucceeded, phaseFailed:
		status := op.StatusBefore
		op.StatusAfter = &status
		op.AfterHash = serviceStateHash(status)
		op.Outcome = string(phase)
		op.ReasonCode = "postcondition_observed"
		op.DispatchMayHaveOccurred = phase == phaseSucceeded
		op.DispatchAccepted = phase == phaseSucceeded
	case phaseOutcomeUnknown:
		op.Outcome = string(phase)
		op.ReasonCode = "postcondition_deadline"
		op.DispatchMayHaveOccurred = true
	default:
		t.Fatalf("unsupported test phase %q", phase)
	}
}

func testControlStateStore(t *testing.T) *controlStateStore {
	t.Helper()
	dir := secureTempDir(t)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := newControlStateStore(filepath.Join(dir, "operation.json"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestControlStateRoundTripAndCanonicalInput(t *testing.T) {
	store := testControlStateStore(t)
	want := testServiceOperation(t, phasePrepared)
	if err := store.withLock(context.Background(), func(current *serviceOperation) (*serviceOperation, error) {
		if current != nil {
			t.Fatal("new store was not empty")
		}
		return &want, nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != want.ID || got.Phase != phasePrepared {
		t.Fatalf("loaded operation = %+v", got)
	}

	raw, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	raw = append([]byte(" "), raw...)
	if err := os.WriteFile(store.path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.load(); err == nil {
		t.Fatal("noncanonical state was accepted")
	}
}

func TestControlStateSerializesCallersAndHonoursContext(t *testing.T) {
	store := testControlStateStore(t)
	op := testServiceOperation(t, phasePrepared)
	entered := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- store.withLock(context.Background(), func(current *serviceOperation) (*serviceOperation, error) {
			close(entered)
			<-release
			return &op, nil
		})
	}()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	err := store.withLock(ctx, func(current *serviceOperation) (*serviceOperation, error) {
		t.Fatal("second callback ran while lock was held")
		return current, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock error = %v", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestControlStateLockSpansProcesses(t *testing.T) {
	store := testControlStateStore(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- store.withLock(context.Background(), func(current *serviceOperation) (*serviceOperation, error) {
			close(entered)
			<-release
			return current, nil
		})
	}()
	<-entered

	cmd := exec.Command(os.Args[0], "-test.run=^TestControlStateLockHelper$")
	cmd.Env = append(os.Environ(),
		"MITHRIL_CONTROL_LOCK_HELPER=1",
		"MITHRIL_CONTROL_LOCK_PATH="+store.path,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper failed: %v: %s", err, output)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestControlStateLockHelper(t *testing.T) {
	if os.Getenv("MITHRIL_CONTROL_LOCK_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	store, err := newControlStateStore(os.Getenv("MITHRIL_CONTROL_LOCK_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	err = store.withLock(ctx, func(current *serviceOperation) (*serviceOperation, error) {
		return current, errors.New("subprocess unexpectedly acquired the lock")
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock error = %v", err)
	}
}

func TestControlStateConcurrentAcceptanceHasOneWinner(t *testing.T) {
	store := testControlStateStore(t)
	op := testServiceOperation(t, phasePrepared)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- store.withLock(context.Background(), func(current *serviceOperation) (*serviceOperation, error) {
				if current != nil && !current.Phase.terminal() {
					return current, errors.New("operation already active")
				}
				next := op
				return &next, nil
			})
		}()
	}
	wg.Wait()
	close(results)
	var success, rejected int
	for err := range results {
		if err == nil {
			success++
		} else {
			rejected++
		}
	}
	if success != 1 || rejected != 1 {
		t.Fatalf("success=%d rejected=%d", success, rejected)
	}
}

func TestControlStateEnforcesPhaseTransitionsAndIdentity(t *testing.T) {
	store := testControlStateStore(t)
	prepared := testServiceOperation(t, phasePrepared)
	if err := store.withTransaction(context.Background(), func(transaction *controlStateTransaction) error {
		if err := transaction.save(prepared); err != nil {
			return err
		}
		dispatched := prepared
		dispatched.Phase = phaseDispatched
		if err := transaction.save(dispatched); err == nil {
			t.Fatal("prepared skipped dispatch_started")
		}
		started := prepared
		started.Phase = phaseDispatchStarted
		if err := transaction.save(started); err != nil {
			return err
		}
		changed := started
		changed.TargetID = "other-target"
		changed.Phase = phaseDispatched
		if err := transaction.save(changed); err == nil {
			t.Fatal("operation identity changed across a transition")
		}
		staleDeadline, err := started.transitionedWithResult(
			phaseOutcomeUnknown,
			time.Unix(started.UpdatedAtUnix+1, 0),
			nil,
			string(phaseOutcomeUnknown),
			"dispatch_result_ambiguous",
			true,
			false,
		)
		if err != nil {
			return err
		}
		if err := transaction.save(staleDeadline); err == nil {
			t.Fatal("postcondition deadline was not re-anchored after dispatch")
		}
		unknown, err := started.transitionedAfterDispatch(
			phaseOutcomeUnknown,
			time.Unix(started.UpdatedAtUnix+1, 0),
			nil,
			string(phaseOutcomeUnknown),
			"dispatch_result_ambiguous",
			false,
		)
		if err != nil {
			return err
		}
		if err := transaction.save(unknown); err != nil {
			return err
		}
		failed := unknown
		failed.Phase = phaseFailed
		if err := transaction.save(failed); err == nil {
			t.Fatal("outcome_unknown moved directly to failed")
		}
		verifying, err := unknown.transitionedWithResult(
			phaseVerifying,
			time.Unix(unknown.UpdatedAtUnix+1, 0),
			nil,
			"",
			"",
			true,
			false,
		)
		if err == nil {
			if err := transaction.save(verifying); err == nil {
				t.Fatal("outcome_unknown resumed verification")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestControlStateTransactionCopiesApprovalEvidence(t *testing.T) {
	store := testControlStateStore(t)
	operation := testServiceOperation(t, phasePrepared)
	wantClaims := bytes.Clone(operation.Approval.ClaimsCBOR)
	wantProof := bytes.Clone(operation.Approval.Proof)

	if err := store.withTransaction(
		t.Context(),
		func(transaction *controlStateTransaction) error {
			if err := transaction.save(operation); err != nil {
				return err
			}
			operation.Approval.ClaimsCBOR[0] ^= 0xff
			operation.Approval.Proof[0] ^= 0xff

			first := transaction.operation()
			if first == nil ||
				!bytes.Equal(first.Approval.ClaimsCBOR, wantClaims) ||
				!bytes.Equal(first.Approval.Proof, wantProof) {
				t.Fatalf("saved operation aliases caller data: %+v", first)
			}
			first.Approval.ClaimsCBOR[0] ^= 0xff
			first.Approval.Proof[0] ^= 0xff

			second := transaction.operation()
			if second == nil ||
				!bytes.Equal(second.Approval.ClaimsCBOR, wantClaims) ||
				!bytes.Equal(second.Approval.Proof, wantProof) {
				t.Fatalf("operation copy aliases transaction state: %+v", second)
			}
			return nil
		},
	); err != nil {
		t.Fatal(err)
	}
}

func TestServiceOperationLifecycleMatchesAuditModel(t *testing.T) {
	tests := []struct {
		name     string
		phase    operationPhase
		after    bool
		outcome  string
		reason   string
		may      bool
		accepted bool
		valid    bool
	}{
		{"prepared", phasePrepared, false, "", "", false, false, true},
		{"dispatch started", phaseDispatchStarted, false, "", "", false, false, true},
		{"dispatched", phaseDispatched, false, "", "", true, true, true},
		{"verifying ambiguous", phaseVerifying, false, "", "", true, false, false},
		{"succeeded", phaseSucceeded, true, "succeeded", "postcondition_observed", true, true, true},
		{"failed before dispatch", phaseFailed, true, "failed", "precondition_changed", false, false, true},
		{"outcome unknown", phaseOutcomeUnknown, false, "outcome_unknown", "postcondition_deadline", true, false, true},
		{"prepared with attempt", phasePrepared, false, "", "", true, false, false},
		{"dispatched without acceptance", phaseDispatched, false, "", "", true, false, false},
		{"verifying with result", phaseVerifying, false, "failed", "postcondition_failed", true, true, false},
		{"succeeded without after state", phaseSucceeded, false, "succeeded", "postcondition_observed", true, true, false},
		{"failed without reason", phaseFailed, true, "failed", "", false, false, false},
		{"unknown without attempt", phaseOutcomeUnknown, false, "outcome_unknown", "postcondition_deadline", false, false, false},
		{"accepted without attempt", phaseFailed, true, "failed", "precondition_changed", false, true, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			op := testServiceOperation(t, phasePrepared)
			op.Phase = test.phase
			op.StatusAfter = nil
			op.AfterHash = ""
			if test.after {
				status := op.StatusBefore
				op.StatusAfter = &status
				op.AfterHash = serviceStateHash(status)
			}
			op.Outcome = test.outcome
			op.ReasonCode = test.reason
			op.DispatchMayHaveOccurred = test.may
			op.DispatchAccepted = test.accepted

			stateValid := op.validate() == nil
			auditValid := controlaudit.ValidateLifecycleFields(
				controlaudit.Phase(test.phase),
				op.AfterHash,
				test.outcome,
				test.reason,
				test.may,
				test.accepted,
			) == nil
			if stateValid != auditValid {
				t.Fatalf("state valid=%v audit valid=%v", stateValid, auditValid)
			}
			if stateValid != test.valid {
				t.Fatalf("valid=%v, want %v", stateValid, test.valid)
			}
		})
	}
}

func TestServiceOperationRequiresMatchingAfterState(t *testing.T) {
	op := testServiceOperation(t, phaseSucceeded)
	op.StatusAfter = nil
	if err := op.validate(); err == nil {
		t.Fatal("after-state hash without status was accepted")
	}

	op = testServiceOperation(t, phaseSucceeded)
	replacement := byte('0')
	if op.AfterHash[0] == replacement {
		replacement = '1'
	}
	op.AfterHash = string(replacement) + op.AfterHash[1:]
	if err := op.validate(); err == nil {
		t.Fatal("mismatched after-state hash was accepted")
	}
}

func TestServiceOperationRejectsUnboundedCheckpointStatus(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*serviceStatus)
	}{
		{
			name: "oversized",
			mutate: func(status *serviceStatus) {
				status.Result = strings.Repeat("x", 257)
			},
		},
		{
			name: "multiline",
			mutate: func(status *serviceStatus) {
				status.Job = "job\nforged-field"
			},
		},
		{
			name: "invalid invocation",
			mutate: func(status *serviceStatus) {
				status.InvocationID = "not-an-invocation-id"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			operation := testServiceOperation(t, phasePrepared)
			test.mutate(&operation.StatusBefore)
			operation.BeforeHash = serviceStateHash(operation.StatusBefore)
			if err := operation.validate(); err == nil {
				t.Fatal("unsafe persisted service status was accepted")
			}
		})
	}
}

func TestTerminalOperationBlocksSiblingApprovalUntilExpiry(t *testing.T) {
	op := testServiceOperation(t, phaseSucceeded)
	barrier := time.Unix(op.StartedAtUnix+int64(MaxApprovalTTLSeconds), 0)
	if !op.blocksNewOperation(barrier.Add(-time.Second)) {
		t.Fatal("terminal operation released its sibling-approval barrier early")
	}
	if op.blocksNewOperation(barrier) {
		t.Fatal("expired sibling approvals still blocked a new operation")
	}
	setTestOperationLifecycle(t, &op, phaseOutcomeUnknown)
	if !op.blocksNewOperation(barrier.Add(-time.Second)) {
		t.Fatal("outcome_unknown released its approval barrier early")
	}
	if op.blocksNewOperation(barrier) {
		t.Fatal("outcome_unknown kept the approval barrier after expiry")
	}
}

func TestControlStateRejectsUnsafeStorage(t *testing.T) {
	dir := secureTempDir(t)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := newControlStateStore(filepath.Join(dir, "state")); err == nil {
		t.Fatal("broad directory permissions were accepted")
	}

	private := secureTempDir(t)
	if err := os.Chmod(private, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(private, "target")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(private, "state")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	store, err := newControlStateStore(link)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.load(); err == nil {
		t.Fatal("symlinked state was accepted")
	}
}
