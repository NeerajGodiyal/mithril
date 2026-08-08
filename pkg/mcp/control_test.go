package mcp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/internal/controlaudit"
)

const activeServiceStatus = `LoadState=loaded
ActiveState=active
SubState=running
Result=success
NRestarts=2
MainPID=1234
InvocationID=11111111111111111111111111111111
ActiveEnterTimestampMonotonic=100
InactiveEnterTimestampMonotonic=50
Job=
`

type fakeServiceRunner struct {
	status             []byte
	err                error
	statusErr          error
	actionErr          error
	calls              [][]string
	statusCalls        int
	beforeStatusReturn func(int)
	beforeAction       func(context.Context)
}

func (f *fakeServiceRunner) Run(ctx context.Context, path string, args ...string) ([]byte, error) {
	call := append([]string{path}, args...)
	f.calls = append(f.calls, call)
	for _, arg := range args {
		if arg == "show" {
			f.statusCalls++
			if f.beforeStatusReturn != nil {
				f.beforeStatusReturn(f.statusCalls)
			}
			if f.statusErr != nil {
				return nil, f.statusErr
			}
			return f.status, f.err
		}
	}
	if f.beforeAction != nil {
		f.beforeAction(ctx)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.actionErr != nil {
		return nil, f.actionErr
	}
	if slices.Contains(args, "restart") {
		f.status = []byte(strings.NewReplacer(
			"InvocationID=11111111111111111111111111111111",
			"InvocationID=22222222222222222222222222222222",
			"ActiveEnterTimestampMonotonic=100",
			"ActiveEnterTimestampMonotonic=200",
		).Replace(activeServiceStatus))
	}
	if slices.Contains(args, "stop") {
		f.status = []byte(strings.NewReplacer(
			"ActiveState=active", "ActiveState=inactive",
			"SubState=running", "SubState=dead",
			"MainPID=1234", "MainPID=0",
			"InactiveEnterTimestampMonotonic=50",
			"InactiveEnterTimestampMonotonic=200",
		).Replace(activeServiceStatus))
	}
	return nil, f.err
}

func testSystemctl(t *testing.T) (string, *executableIdentity) {
	t.Helper()
	dir := secureTempDir(t)
	path := filepath.Join(dir, "systemctl")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing fake systemctl: %v", err)
	}
	id, err := resolveTestExecutable(path)
	if err != nil {
		t.Fatalf("resolving fake systemctl: %v", err)
	}
	return path, &id
}

func secureTempDir(t *testing.T) string {
	t.Helper()
	workingDirectory, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(workingDirectory, ".mithril-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func resolveTestExecutable(path string) (executableIdentity, error) {
	return resolveExecutableOwnedBy(path, uint32(os.Getuid()))
}

func testApproverPrivateKey() ed25519.PrivateKey {
	seed := bytes.Repeat([]byte{0x42}, ed25519.SeedSize)
	return ed25519.NewKeyFromSeed(seed)
}

func testApprovalAuthority() approvalAuthority {
	privateKey := testApproverPrivateKey()
	publicKey := privateKey.Public().(ed25519.PublicKey)
	keyID := approverKeyID(publicKey)
	return approvalAuthority{
		publicKeys:    map[string]ed25519.PublicKey{keyID: publicKey},
		serverSession: strings.Repeat("s", 32),
		targetID:      "test-node",
	}
}

func approveTestChallenge(t *testing.T, challenge string, now time.Time) ServiceApprovalBundle {
	t.Helper()
	bundle, err := ApproveServiceChallenge(challenge, testApproverPrivateKey(), now)
	if err != nil {
		t.Fatalf("approve challenge: %v", err)
	}
	return bundle
}

func writeTestApproverDirectory(t *testing.T, parent string) string {
	t.Helper()
	dir := filepath.Join(parent, "approvers")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	publicKey := testApproverPrivateKey().Public().(ed25519.PublicKey)
	if err := os.WriteFile(filepath.Join(dir, "operator.pub"), publicKey, 0o440); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "operator.pub"), 0o440); err != nil {
		t.Fatal(err)
	}
	return dir
}

func validateTestOperatorConfig(cfg Config) (approvalAuthority, error) {
	return validateAndLoadOperatorConfigWithResolvers(
		cfg,
		resolveTestExecutable,
		func(path string) (map[string]ed25519.PublicKey, error) {
			return loadApproverPublicKeysOwnedBy(path, uint32(os.Getuid()))
		},
		strings.NewReader(strings.Repeat("r", approvalNonceBytes)),
	)
}

func testServiceController(t *testing.T, runner serviceRunner, now time.Time) *serviceController {
	t.Helper()
	controller, _ := testServiceControllerWithRemote(t, runner, now)
	return controller
}

func testServiceControllerWithRemote(
	t *testing.T,
	runner serviceRunner,
	now time.Time,
) (*serviceController, *testControlAuditRemote) {
	t.Helper()
	systemctl, identity := testSystemctl(t)
	stateDir := realTempDir(t)
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	state, err := newControlStateStore(filepath.Join(stateDir, "operation.json"))
	if err != nil {
		t.Fatal(err)
	}
	authority := testApprovalAuthority()
	remote := &testControlAuditRemote{}
	audit, err := openControlAuditTrailWithRemote(
		context.Background(),
		filepath.Join(stateDir, controlAuditStoreName),
		newControlAuditApprovalVerifier(authority.publicKeys),
		remote,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &controlRuntime{
		state:        state,
		audit:        audit,
		approvalKeys: authority.publicKeys,
	}
	t.Cleanup(func() {
		_ = runtime.close()
	})
	randomBytes := make([]byte, 4096)
	for i := range randomBytes {
		randomBytes[i] = byte(i%251 + 1)
	}
	return &serviceController{
		cfg: Config{
			Profile:            ProfileOperator,
			SystemdUnit:        "mithril.service",
			SystemdScope:       "system",
			SystemctlPath:      systemctl,
			ApprovalTTLSeconds: 60,
		}.normalized(),
		authority:  authority,
		runtime:    runtime,
		executable: identity,
		runner:     runner,
		now:        func() time.Time { return now },
		random:     bytes.NewReader(randomBytes),
		readiness: func(context.Context, Config) *serviceNodeReadiness {
			return &serviceNodeReadiness{
				Assessed:             true,
				DiagnosisStatus:      diagnosticHealthy,
				EvidenceComplete:     true,
				SafeForAutomation:    true,
				SlotProgressObserved: true,
			}
		},
		pending:        make(map[[approvalNonceBytes]byte]approvalClaims),
		allowedActions: map[serviceAction]bool{actionStart: true, actionStop: true, actionRestart: true},
	}, remote
}

func TestServiceApprovalLifecycle(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	runner := &fakeServiceRunner{status: []byte(activeServiceStatus)}
	controller := testServiceController(t, runner, now)

	prepared, err := controller.prepare(context.Background(), "restart", "")
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.ApprovalRequired || prepared.Challenge == "" || prepared.Status.MainPID != 1234 ||
		!strings.Contains(prepared.NextStep, "separate approver host") ||
		!strings.Contains(prepared.NextStep, "same MCP session") {
		t.Fatalf("prepared action = %+v", prepared)
	}
	if _, err := controller.execute(context.Background(), ServiceApprovalBundle{
		AuthorizationToken: prepared.Challenge,
		AuditAttestation:   prepared.Challenge,
	}); err == nil {
		t.Fatal("an unsigned challenge was accepted for execution")
	}

	bundle := approveTestChallenge(t, prepared.Challenge, now)
	approved, evidence, err := verifyServiceApprovalBundle(bundle, controller.authority, now)
	if err != nil || approved.Action != actionRestart || evidence.Domain != serviceApprovalAuditDomain ||
		len(evidence.ClaimsCBOR) == 0 || len(evidence.Proof) != ed25519.SignatureSize {
		t.Fatalf("verified approval = %+v, evidence = %+v, error = %v", approved, evidence, err)
	}
	executed, err := controller.execute(context.Background(), bundle)
	if err != nil {
		t.Fatal(err)
	}
	if executed.Phase != phaseSucceeded ||
		!executed.DispatchAccepted ||
		!executed.LocalAuditDurable ||
		!executed.AuditAcknowledged ||
		executed.Action != "restart" {
		t.Fatalf("execute = %+v", executed)
	}
	if updatedAt, err := time.Parse(time.RFC3339, executed.UpdatedAt); err != nil ||
		!updatedAt.Equal(now) {
		t.Fatalf("execute updated_at = %q, want %s", executed.UpdatedAt, now)
	}
	wantAction := []string{
		controller.cfg.SystemctlPath,
		"--no-ask-password",
		"--job-mode=fail",
		"restart",
		"mithril.service",
	}
	if !slices.ContainsFunc(runner.calls, func(call []string) bool {
		return reflect.DeepEqual(call, wantAction)
	}) {
		t.Fatalf("systemctl calls = %v, missing action %v", runner.calls, wantAction)
	}
	for _, call := range runner.calls {
		if !slices.Contains(call, "--no-ask-password") {
			t.Fatalf("systemctl call may prompt for authorization: %v", call)
		}
	}
	if _, err := controller.execute(context.Background(), bundle); err == nil ||
		(!strings.Contains(err.Error(), "already used") &&
			!strings.Contains(err.Error(), "previous approval")) {
		t.Fatalf("replayed token error = %v", err)
	}
}

func TestServiceActionCompletesAfterClientCancellation(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	requestCtx, cancelRequest := context.WithCancel(t.Context())
	runner := &fakeServiceRunner{
		status: []byte(activeServiceStatus),
		beforeAction: func(context.Context) {
			cancelRequest()
		},
	}
	controller := testServiceController(t, runner, now)
	prepared, err := controller.prepare(t.Context(), "restart", "")
	if err != nil {
		t.Fatal(err)
	}
	executed, err := controller.execute(
		requestCtx,
		approveTestChallenge(t, prepared.Challenge, now),
	)
	if err != nil {
		t.Fatal(err)
	}
	if requestCtx.Err() != context.Canceled {
		t.Fatalf("request context error = %v, want canceled", requestCtx.Err())
	}
	if executed.Phase != phaseSucceeded ||
		!executed.DispatchAccepted ||
		executed.ReasonCode != "postcondition_verified" {
		t.Fatalf("execute after client cancellation = %+v", executed)
	}
}

func TestPermanentAuditRejectionIsActionable(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	runner := &fakeServiceRunner{status: []byte(activeServiceStatus)}
	controller, remote := testServiceControllerWithRemote(t, runner, now)
	prepared, err := controller.prepare(t.Context(), "restart", "")
	if err != nil {
		t.Fatal(err)
	}
	remote.mu.Lock()
	remote.appendErr = controlaudit.ErrPermanentRejection
	remote.mu.Unlock()

	executed, err := controller.execute(
		t.Context(),
		approveTestChallenge(t, prepared.Challenge, now),
	)
	if err != nil {
		t.Fatal(err)
	}
	if executed.Phase != phaseFailed ||
		executed.DispatchMayHaveOccurred ||
		!executed.LocalAuditDurable ||
		executed.AuditAcknowledged ||
		!strings.Contains(executed.NextStep, "rejected it as non-retryable") ||
		!strings.Contains(executed.NextStep, "receiver capacity") {
		t.Fatalf("permanent audit rejection = %+v", executed)
	}
	if got := countServiceActionCalls(runner.calls); got != 0 {
		t.Fatalf("permanent audit rejection dispatched %d service actions", got)
	}
}

func TestApplyServiceNodeReadiness(t *testing.T) {
	ready := executeServiceActionOutput{
		NextStep: "No further service action is required.",
	}
	applyServiceNodeReadiness(&ready, &serviceNodeReadiness{
		Assessed:             true,
		EvidenceComplete:     true,
		SafeForAutomation:    true,
		SlotProgressObserved: true,
	})
	if ready.NextStep != "No further service action is required." {
		t.Fatalf("ready next step = %q", ready.NextStep)
	}

	pending := executeServiceActionOutput{
		NextStep: "The result is durable locally, but off-host audit delivery is pending.",
	}
	applyServiceNodeReadiness(&pending, &serviceNodeReadiness{
		Assessed:         true,
		EvidenceComplete: true,
	})
	if !strings.Contains(pending.NextStep, "off-host audit delivery is pending") ||
		!strings.Contains(pending.NextStep, "node readiness is not yet proven") ||
		!strings.Contains(pending.NextStep, "mithril_diagnose") {
		t.Fatalf("pending next step = %q", pending.NextStep)
	}
}

func TestAmbiguousServiceActionIgnoresForeignTransition(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	runner := &fakeServiceRunner{
		status:    []byte(activeServiceStatus),
		actionErr: errors.New("systemctl result unavailable"),
	}
	controller := testServiceController(t, runner, now)
	prepared, err := controller.prepare(t.Context(), "restart", "")
	if err != nil {
		t.Fatal(err)
	}
	bundle := approveTestChallenge(t, prepared.Challenge, now)
	executed, err := controller.execute(t.Context(), bundle)
	if err != nil {
		t.Fatal(err)
	}
	if executed.Phase != phaseOutcomeUnknown ||
		!executed.DispatchMayHaveOccurred ||
		executed.DispatchAccepted ||
		!strings.Contains(executed.NextStep, "will not be retried") ||
		executed.NewActionAllowedAt == "" {
		t.Fatalf("ambiguous execute = %+v", executed)
	}
	actionCalls := countServiceActionCalls(runner.calls)
	if actionCalls != 1 {
		t.Fatalf("action calls after execute = %d", actionCalls)
	}

	runner.status = []byte(strings.NewReplacer(
		"InvocationID=11111111111111111111111111111111",
		"InvocationID=33333333333333333333333333333333",
		"ActiveEnterTimestampMonotonic=100",
		"ActiveEnterTimestampMonotonic=300",
	).Replace(activeServiceStatus))
	verified, err := controller.verify(t.Context(), prepared.ActionID)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Phase != phaseOutcomeUnknown ||
		verified.ReasonCode == "" {
		t.Fatalf("verified action = %+v", verified)
	}
	if got := countServiceActionCalls(runner.calls); got != actionCalls {
		t.Fatalf("verification redispatched systemctl: before=%d after=%d", actionCalls, got)
	}
}

func TestRepeatedUnknownVerificationDoesNotGrowAuditOrRedispatch(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	runner := &fakeServiceRunner{
		status:    []byte(activeServiceStatus),
		actionErr: errors.New("systemctl result unavailable"),
	}
	controller := testServiceController(t, runner, now)
	prepared, err := controller.prepare(t.Context(), "restart", "")
	if err != nil {
		t.Fatal(err)
	}
	executed, err := controller.execute(
		t.Context(),
		approveTestChallenge(t, prepared.Challenge, now),
	)
	if err != nil {
		t.Fatal(err)
	}
	if executed.Phase != phaseOutcomeUnknown {
		t.Fatalf("execute phase = %s, want %s", executed.Phase, phaseOutcomeUnknown)
	}
	actionCalls := countServiceActionCalls(runner.calls)
	before := requireControlAuditSummary(t, controller.runtime.audit.store)
	controller.now = func() time.Time {
		return now.Add(postconditionTimeout + time.Second)
	}

	for attempt := 0; attempt < 2; attempt++ {
		verified, err := controller.verify(t.Context(), prepared.ActionID)
		if err != nil {
			t.Fatal(err)
		}
		if verified.Phase != phaseOutcomeUnknown ||
			!verified.LocalAuditDurable {
			t.Fatalf("verification %d = %+v", attempt+1, verified)
		}
		if after := requireControlAuditSummary(t, controller.runtime.audit.store); after != before {
			t.Fatalf("verification %d grew audit: before=%+v after=%+v", attempt+1, before, after)
		}
		if got := countServiceActionCalls(runner.calls); got != actionCalls {
			t.Fatalf("verification %d redispatched: before=%d after=%d", attempt+1, actionCalls, got)
		}
	}
}

func TestUnauditedPredispatchPhaseDoesNotAdvance(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name           string
		breakAudit     func(*serviceController, *testControlAuditRemote)
		wantPhase      operationPhase
		recoveredPhase operationPhase
		wantError      bool
	}{
		{
			name: "prepared",
			breakAudit: func(controller *serviceController, _ *testControlAuditRemote) {
				if err := os.Chmod(controller.runtime.audit.path, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantPhase:      phasePrepared,
			recoveredPhase: phaseFailed,
			wantError:      true,
		},
		{
			name: "dispatch started",
			breakAudit: func(controller *serviceController, remote *testControlAuditRemote) {
				remote.beforeReturn = func(event controlaudit.Event) {
					if event.Phase != controlaudit.PhasePrepared {
						return
					}
					remote.beforeReturn = nil
					if err := os.Chmod(controller.runtime.audit.path, 0o644); err != nil {
						t.Fatal(err)
					}
				}
			},
			wantPhase:      phaseDispatchStarted,
			recoveredPhase: phaseOutcomeUnknown,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeServiceRunner{status: []byte(activeServiceStatus)}
			controller, remote := testServiceControllerWithRemote(t, runner, now)
			prepared, err := controller.prepare(t.Context(), "restart", "")
			if err != nil {
				t.Fatal(err)
			}
			test.breakAudit(controller, remote)

			executed, err := controller.execute(
				t.Context(),
				approveTestChallenge(t, prepared.Challenge, now),
			)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "audit is not synchronized") {
					t.Fatalf("execute error = %v", err)
				}
				if got := countServiceActionCalls(runner.calls); got != 0 {
					t.Fatalf("unsafe action reached systemctl %d time(s)", got)
				}
				if err := os.Chmod(controller.runtime.audit.path, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := controller.runtime.state.withTransaction(
					t.Context(),
					func(transaction *controlStateTransaction) error {
						if current := transaction.operation(); current != nil {
							t.Fatalf("failed audit check created state: %+v", current)
						}
						return nil
					},
				); err != nil {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if executed.Phase != test.wantPhase ||
				executed.LocalAuditDurable ||
				executed.ReasonCode != "audit_not_durable" {
				t.Fatalf("execute = %+v", executed)
			}
			if got := countServiceActionCalls(runner.calls); got != 0 {
				t.Fatalf("unaudited action reached systemctl %d time(s)", got)
			}
			if err := os.Chmod(controller.runtime.audit.path, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := controller.runtime.state.withTransaction(
				t.Context(),
				func(transaction *controlStateTransaction) error {
					current := transaction.operation()
					if current == nil || current.Phase != test.wantPhase {
						t.Fatalf("stored operation = %+v", current)
					}
					return nil
				},
			); err != nil {
				t.Fatal(err)
			}

			if err := controller.runtime.audit.close(); !errors.Is(err, controlaudit.ErrStoreUncertain) {
				t.Fatalf("close after live audit mutation error = %v", err)
			}
			audit, err := openControlAuditTrailWithRemote(
				t.Context(),
				controller.runtime.audit.path,
				newControlAuditApprovalVerifier(controller.authority.publicKeys),
				remote,
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			recovered := &controlRuntime{
				state:        controller.runtime.state,
				audit:        audit,
				approvalKeys: controller.authority.publicKeys,
			}
			t.Cleanup(func() {
				_ = recovered.close()
			})
			if err := recovered.recover(t.Context(), now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := recovered.state.withTransaction(
				t.Context(),
				func(transaction *controlStateTransaction) error {
					current := transaction.operation()
					if current == nil || current.Phase != test.recoveredPhase {
						t.Fatalf("recovered operation = %+v", current)
					}
					return nil
				},
			); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFullAuditQueueCannotAdvanceUnauditedState(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	runner := &fakeServiceRunner{status: []byte(activeServiceStatus)}
	controller, remote := testServiceControllerWithRemote(t, runner, now)
	operation := approvedTestOperation(t, controller, now, "restart")

	if err := controller.runtime.state.withTransaction(
		t.Context(),
		func(transaction *controlStateTransaction) error {
			if err := transaction.save(operation); err != nil {
				return err
			}
			if acknowledged, err := controller.runtime.ensureAudited(
				t.Context(),
				operation,
			); err != nil || !acknowledged {
				return errors.New("prepared phase was not acknowledged")
			}
			var err error
			operation, err = operation.transitionedWithResult(
				phaseDispatchStarted,
				now.Add(time.Second),
				nil,
				"",
				"",
				false,
				false,
			)
			if err != nil {
				return err
			}
			if err := transaction.save(operation); err != nil {
				return err
			}
			if acknowledged, err := controller.runtime.ensureAudited(
				t.Context(),
				operation,
			); err != nil || !acknowledged {
				return errors.New("dispatch-started phase was not acknowledged")
			}
			operation, err = operation.transitionedAfterDispatch(
				phaseDispatched,
				now.Add(2*time.Second),
				nil,
				"",
				"",
				true,
			)
			if err != nil {
				return err
			}
			return transaction.save(operation)
		},
	); err != nil {
		t.Fatal(err)
	}

	remote.mu.Lock()
	remote.down = true
	remote.mu.Unlock()
	controller.runtime.audit.mu.Lock()
	controller.runtime.audit.pending = make(
		[]controlaudit.Event,
		maxControlAuditPending,
	)
	controller.runtime.audit.mu.Unlock()

	verified, err := controller.verify(t.Context(), operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Phase != phaseDispatched ||
		verified.LocalAuditDurable ||
		verified.AuditAcknowledged {
		t.Fatalf("verification = %+v", verified)
	}
	if err := controller.runtime.state.withTransaction(
		t.Context(),
		func(transaction *controlStateTransaction) error {
			current := transaction.operation()
			if current == nil || current.Phase != phaseDispatched {
				t.Fatalf("stored operation advanced without audit: %+v", current)
			}
			return nil
		},
	); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalAuditGapRequiresVerificationWithoutRedispatch(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	runner := &fakeServiceRunner{status: []byte(activeServiceStatus)}
	controller, remote := testServiceControllerWithRemote(t, runner, now)
	prepared, err := controller.prepare(t.Context(), "restart", "")
	if err != nil {
		t.Fatal(err)
	}
	runner.beforeStatusReturn = func(call int) {
		if call != 4 {
			return
		}
		runner.beforeStatusReturn = nil
		remote.mu.Lock()
		remote.down = true
		remote.mu.Unlock()
		controller.runtime.audit.mu.Lock()
		controller.runtime.audit.pending = make(
			[]controlaudit.Event,
			maxControlAuditPending,
		)
		controller.runtime.audit.mu.Unlock()
	}

	executed, err := controller.execute(
		t.Context(),
		approveTestChallenge(t, prepared.Challenge, now),
	)
	if err != nil {
		t.Fatal(err)
	}
	if executed.Phase != phaseSucceeded ||
		executed.LocalAuditDurable ||
		!strings.Contains(executed.NextStep, "mithril_verify_service_action") ||
		!strings.Contains(executed.NextStep, "do not repeat") {
		t.Fatalf("execute = %+v", executed)
	}
	actionCalls := countServiceActionCalls(runner.calls)

	controller.runtime.audit.mu.Lock()
	clear(controller.runtime.audit.pending)
	controller.runtime.audit.pending = nil
	controller.runtime.audit.mu.Unlock()
	remote.mu.Lock()
	remote.down = false
	remote.mu.Unlock()

	verified, err := controller.verify(t.Context(), prepared.ActionID)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Phase != phaseSucceeded ||
		!verified.LocalAuditDurable ||
		!verified.AuditAcknowledged ||
		verified.NextStep != "No further service action is required." {
		t.Fatalf("verified = %+v", verified)
	}
	if got := countServiceActionCalls(runner.calls); got != actionCalls {
		t.Fatalf("verification redispatched systemctl: before=%d after=%d", actionCalls, got)
	}
}

func TestDispatchRechecksAuthorityAfterAuditAcknowledgement(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name       string
		reasonCode string
		change     func(*serviceController, *fakeServiceRunner, *time.Time)
	}{
		{
			name:       "approval expired",
			reasonCode: "approval_expired_before_dispatch",
			change: func(
				_ *serviceController,
				_ *fakeServiceRunner,
				current *time.Time,
			) {
				*current = now.Add(61 * time.Second)
			},
		},
		{
			name:       "service state changed",
			reasonCode: "service_state_changed_before_dispatch",
			change: func(
				_ *serviceController,
				runner *fakeServiceRunner,
				_ *time.Time,
			) {
				runner.status = []byte(strings.NewReplacer(
					"ActiveState=active", "ActiveState=inactive",
					"SubState=running", "SubState=dead",
					"MainPID=1234", "MainPID=0",
					"InactiveEnterTimestampMonotonic=50",
					"InactiveEnterTimestampMonotonic=200",
				).Replace(activeServiceStatus))
			},
		},
		{
			name:       "systemctl identity changed",
			reasonCode: "systemctl_identity_changed",
			change: func(
				controller *serviceController,
				_ *fakeServiceRunner,
				_ *time.Time,
			) {
				if err := os.WriteFile(
					controller.cfg.SystemctlPath,
					[]byte("#!/bin/sh\nexit 1\n"),
					0o755,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := now
			runner := &fakeServiceRunner{status: []byte(activeServiceStatus)}
			controller, remote := testServiceControllerWithRemote(t, runner, now)
			controller.now = func() time.Time { return current }
			prepared, err := controller.prepare(t.Context(), "restart", "")
			if err != nil {
				t.Fatal(err)
			}
			changed := false
			remote.beforeReturn = func(event controlaudit.Event) {
				if event.Phase != controlaudit.PhaseDispatchStarted {
					return
				}
				remote.beforeReturn = nil
				changed = true
				test.change(controller, runner, &current)
			}

			executed, err := controller.execute(
				t.Context(),
				approveTestChallenge(t, prepared.Challenge, now),
			)
			if err != nil {
				t.Fatal(err)
			}
			if !changed {
				t.Fatal("dispatch-started acknowledgement hook did not run")
			}
			if executed.Phase != phaseFailed ||
				executed.ReasonCode != test.reasonCode ||
				executed.DispatchMayHaveOccurred ||
				!executed.LocalAuditDurable ||
				!executed.AuditAcknowledged {
				t.Fatalf("execute = %+v", executed)
			}
			if got := countServiceActionCalls(runner.calls); got != 0 {
				t.Fatalf("stale authority reached systemctl %d time(s)", got)
			}
		})
	}
}

func TestPostconditionDeadlineStartsAfterDispatch(t *testing.T) {
	startedAt := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	current := startedAt
	runner := &fakeServiceRunner{status: []byte(activeServiceStatus)}
	controller, remote := testServiceControllerWithRemote(t, runner, startedAt)
	controller.now = func() time.Time { return current }
	prepared, err := controller.prepare(t.Context(), "restart", "")
	if err != nil {
		t.Fatal(err)
	}
	remote.beforeReturn = func(event controlaudit.Event) {
		if event.Phase != controlaudit.PhaseDispatchStarted {
			return
		}
		remote.beforeReturn = nil
		current = startedAt.Add(45 * time.Second)
	}

	executed, err := controller.execute(
		t.Context(),
		approveTestChallenge(t, prepared.Challenge, startedAt),
	)
	if err != nil {
		t.Fatal(err)
	}
	if executed.Phase != phaseSucceeded {
		t.Fatalf("execute = %+v", executed)
	}
	if err := controller.runtime.state.withTransaction(
		t.Context(),
		func(transaction *controlStateTransaction) error {
			operation := transaction.operation()
			want := current.Add(postconditionTimeout).Unix()
			if operation == nil || operation.DeadlineUnix != want {
				t.Fatalf("operation deadline = %+v, want %d", operation, want)
			}
			return nil
		},
	); err != nil {
		t.Fatal(err)
	}
}

func TestPostDispatchAuditOutageRetriesWithoutAnotherToolCall(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	runner := &fakeServiceRunner{status: []byte(activeServiceStatus)}
	controller, remote := testServiceControllerWithRemote(t, runner, now)
	remote.downAfter = 2

	prepared, err := controller.prepare(t.Context(), "restart", "")
	if err != nil {
		t.Fatal(err)
	}
	executed, err := controller.execute(
		t.Context(),
		approveTestChallenge(t, prepared.Challenge, now),
	)
	if err != nil {
		t.Fatal(err)
	}
	if executed.Phase != phaseSucceeded ||
		!executed.LocalAuditDurable ||
		executed.AuditAcknowledged {
		t.Fatalf("execute during receiver outage = %+v", executed)
	}
	if summary := requireControlAuditSummary(t, controller.runtime.audit.store); summary.Records != 5 {
		t.Fatalf("local audit records = %d, want 5", summary.Records)
	}

	remote.mu.Lock()
	remote.down = false
	remote.mu.Unlock()
	controller.runtime.retryEvery = 5 * time.Millisecond
	controller.runtime.startDeliveryWorker()

	deadline := time.Now().Add(2 * time.Second)
	for {
		remote.mu.Lock()
		received := len(remote.received)
		remote.mu.Unlock()
		controller.runtime.audit.mu.Lock()
		pending := len(controller.runtime.audit.pending)
		controller.runtime.audit.mu.Unlock()
		if received == 5 && pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background audit delivery stalled: received=%d pending=%d", received, pending)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAuditReceiverMustAcknowledgeBeforeDispatch(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	runner := &fakeServiceRunner{status: []byte(activeServiceStatus)}
	controller, remote := testServiceControllerWithRemote(t, runner, now)
	prepared, err := controller.prepare(t.Context(), "restart", "")
	if err != nil {
		t.Fatal(err)
	}
	remote.mu.Lock()
	remote.down = true
	remote.mu.Unlock()

	out, err := controller.execute(
		t.Context(),
		approveTestChallenge(t, prepared.Challenge, now),
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Phase != phaseFailed ||
		out.DispatchMayHaveOccurred ||
		!out.LocalAuditDurable ||
		out.AuditAcknowledged ||
		out.ReasonCode != "audit_not_acknowledged" {
		t.Fatalf("unacknowledged action = %+v", out)
	}
	if got := countServiceActionCalls(runner.calls); got != 0 {
		t.Fatalf("unacknowledged action reached systemctl %d time(s)", got)
	}
}

func TestControlRuntimeRecoversCrashBoundariesWithoutRedispatch(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name       string
		install    func(serviceOperation) (serviceOperation, error)
		wantPhase  operationPhase
		wantMayRun bool
	}{
		{
			name: "before dispatch",
			install: func(operation serviceOperation) (serviceOperation, error) {
				return operation, nil
			},
			wantPhase: phaseFailed,
		},
		{
			name: "at dispatch authority point",
			install: func(operation serviceOperation) (serviceOperation, error) {
				return operation.transitionedWithResult(
					phaseDispatchStarted,
					now.Add(time.Second),
					nil,
					"",
					"",
					false,
					false,
				)
			},
			wantPhase:  phaseOutcomeUnknown,
			wantMayRun: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeServiceRunner{status: []byte(activeServiceStatus)}
			controller := testServiceController(t, runner, now)
			operation := approvedTestOperation(t, controller, now, "restart")

			if err := controller.runtime.state.withTransaction(
				t.Context(),
				func(transaction *controlStateTransaction) error {
					if err := transaction.save(operation); err != nil {
						return err
					}
					if _, err := controller.runtime.ensureAudited(
						t.Context(),
						operation,
					); err != nil {
						return err
					}
					installed, err := test.install(operation)
					if err != nil {
						return err
					}
					if installed.Phase == operation.Phase {
						return nil
					}
					if err := transaction.save(installed); err != nil {
						return err
					}
					_, err = controller.runtime.ensureAudited(
						t.Context(),
						installed,
					)
					return err
				},
			); err != nil {
				t.Fatal(err)
			}
			actionCalls := countServiceActionCalls(runner.calls)
			if err := controller.runtime.recover(
				t.Context(),
				now.Add(2*time.Second),
			); err != nil {
				t.Fatal(err)
			}
			recovered, err := controller.runtime.state.load()
			if err != nil {
				t.Fatal(err)
			}
			if recovered == nil ||
				recovered.Phase != test.wantPhase ||
				recovered.DispatchMayHaveOccurred != test.wantMayRun {
				t.Fatalf("recovered operation = %+v", recovered)
			}
			if got := countServiceActionCalls(runner.calls); got != actionCalls {
				t.Fatalf("recovery dispatched systemctl: before=%d after=%d", actionCalls, got)
			}
		})
	}
}

func TestControlRuntimeRejectsStateWithWrongApprovalSignature(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	controller := testServiceController(
		t,
		&fakeServiceRunner{status: []byte(activeServiceStatus)},
		now,
	)
	operation := approvedTestOperation(t, controller, now, "restart")
	operation.Approval.Proof = bytes.Clone(operation.Approval.Proof)
	operation.Approval.Proof[0] ^= 0xff
	operation.Approval.EvidenceSHA256 = approvalEvidenceHash(
		operation.Approval.Domain,
		operation.Approval.ClaimsCBOR,
		operation.Approval.Proof,
	)
	if err := controller.runtime.state.withTransaction(
		t.Context(),
		func(transaction *controlStateTransaction) error {
			return transaction.save(operation)
		},
	); err != nil {
		t.Fatal(err)
	}
	if err := controller.runtime.recover(
		t.Context(),
		now.Add(time.Second),
	); err == nil || !strings.Contains(err.Error(), "approval") {
		t.Fatalf("tampered state recovery error = %v", err)
	}
}

func TestBearerApprovalIsNeverPersisted(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	controller := testServiceController(
		t,
		&fakeServiceRunner{status: []byte(activeServiceStatus)},
		now,
	)
	prepared, err := controller.prepare(t.Context(), "restart", "")
	if err != nil {
		t.Fatal(err)
	}
	bundle := approveTestChallenge(t, prepared.Challenge, now)
	if _, err := controller.execute(t.Context(), bundle); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		controller.runtime.state.path,
		controller.runtime.audit.path,
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(bundle.AuthorizationToken)) {
			t.Fatalf("bearer authorization token reached durable file %s", path)
		}
		if bytes.Contains(raw, []byte(prepared.Challenge)) {
			t.Fatalf("unsigned challenge reached durable file %s", path)
		}
	}
}

func approvedTestOperation(
	t *testing.T,
	controller *serviceController,
	now time.Time,
	action string,
) serviceOperation {
	t.Helper()
	prepared, err := controller.prepare(t.Context(), action, "")
	if err != nil {
		t.Fatal(err)
	}
	approved, evidence, err := verifyServiceApprovalBundle(
		approveTestChallenge(t, prepared.Challenge, now),
		controller.authority,
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := newServiceOperation(
		approved.ActionID,
		approved.ServerSession,
		approved.TargetID,
		approved.Action,
		prepared.Status,
		evidence,
		now,
		now.Add(postconditionTimeout),
	)
	if err != nil {
		t.Fatal(err)
	}
	return operation
}

func countServiceActionCalls(calls [][]string) int {
	count := 0
	for _, call := range calls {
		if slices.Contains(call, "--job-mode=fail") {
			count++
		}
	}
	return count
}

func TestServiceApprovalBindsStateAndExpiry(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	runner := &fakeServiceRunner{status: []byte(activeServiceStatus)}
	controller := testServiceController(t, runner, now)
	prepared, err := controller.prepare(context.Background(), "stop", "")
	if err != nil {
		t.Fatal(err)
	}
	bundle := approveTestChallenge(t, prepared.Challenge, now)
	runner.status = []byte(strings.Replace(activeServiceStatus, "MainPID=1234", "MainPID=5678", 1))
	if _, err := controller.execute(context.Background(), bundle); err == nil || !strings.Contains(err.Error(), "state changed") {
		t.Fatalf("changed-state error = %v", err)
	}

	prepared, err = controller.prepare(context.Background(), "stop", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApproveServiceChallenge(prepared.Challenge, testApproverPrivateKey(), now.Add(2*time.Minute)); err == nil ||
		!strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired challenge error = %v", err)
	}
}

func TestServiceApprovalExpiresBeforeAction(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	runner := &fakeServiceRunner{status: []byte(activeServiceStatus)}
	controller := testServiceController(t, runner, now)
	prepared, err := controller.prepare(context.Background(), "restart", "")
	if err != nil {
		t.Fatal(err)
	}
	bundle := approveTestChallenge(t, prepared.Challenge, now)
	nowCalls := 0
	controller.now = func() time.Time {
		nowCalls++
		if nowCalls == 1 {
			return now
		}
		return now.Add(2 * time.Minute)
	}
	if _, err := controller.execute(context.Background(), bundle); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired-before-action error = %v", err)
	}
	for _, call := range runner.calls {
		if slices.Contains(call, "restart") {
			t.Fatalf("expired approval executed systemctl: %v", call)
		}
	}
}

func TestValidateApprovalTimeRejectsOverflowedLifetime(t *testing.T) {
	if err := validateApprovalTime(math.MinInt64, math.MaxInt64, time.Unix(0, 0)); err == nil {
		t.Fatal("overflowed approval lifetime was accepted")
	}
}

func TestServiceControlRejectsUnsafeInputs(t *testing.T) {
	status, err := parseServiceStatus("mithril.service", "system", []byte(activeServiceStatus))
	if err != nil || status.ActiveState != "active" || status.NRestarts != 2 {
		t.Fatalf("status = %+v err=%v", status, err)
	}
	for _, data := range []string{
		"LoadState=loaded\nActiveState=active\n",
		strings.Replace(activeServiceStatus, "MainPID=1234", "MainPID=not-a-number", 1),
		activeServiceStatus + "ActiveState=inactive\n",
		"LoadState loaded\n",
	} {
		if _, err := parseServiceStatus("mithril.service", "system", []byte(data)); err == nil {
			t.Fatalf("malformed status accepted: %q", data)
		}
	}
	controller := testServiceController(t, &fakeServiceRunner{status: []byte(activeServiceStatus)}, time.Now())
	for _, action := range []string{"", "kill", "restart; reboot"} {
		if _, err := controller.prepare(context.Background(), action, ""); err == nil {
			t.Fatalf("unsafe action %q accepted", action)
		}
	}
	if _, err := ApproveServiceChallenge("not-a-token", testApproverPrivateKey(), time.Now()); err == nil {
		t.Fatal("malformed challenge accepted")
	}
}

func TestValidateOperatorConfigAndApproverKeys(t *testing.T) {
	dir := realTempDir(t)
	systemctl := filepath.Join(dir, "systemctl")
	if err := os.WriteFile(systemctl, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	approvers := writeTestApproverDirectory(t, dir)
	cfg := Config{
		Profile:               ProfileOperator,
		ControlEnabled:        true,
		SystemdUnit:           "mithril.service",
		SystemdScope:          "system",
		SystemctlPath:         systemctl,
		ApproverKeysDir:       approvers,
		ControlTargetID:       "node-mainnet-1",
		ApprovalTTLSeconds:    60,
		ControlStateDir:       filepath.Join(dir, "control"),
		AuditClientConfigPath: filepath.Join(dir, "audit-client.json"),
		AllowedServiceActions: []string{"restart"},
	}
	authority, err := validateTestOperatorConfig(cfg)
	if err != nil || !authority.configured() {
		t.Fatalf("valid operator config: authority=%+v error=%v", authority, err)
	}
	zeroTTL := cfg
	zeroTTL.ApprovalTTLSeconds = 0
	if _, err := validateTestOperatorConfig(zeroTTL); err != nil {
		t.Fatalf("unset operator TTL should use the default: %v", err)
	}
	for _, ttl := range []uint64{MinApprovalTTLSeconds - 1, MaxApprovalTTLSeconds + 1} {
		invalidTTL := cfg
		invalidTTL.ApprovalTTLSeconds = ttl
		if _, err := validateTestOperatorConfig(invalidTTL); err == nil || !strings.Contains(err.Error(), "approval TTL") {
			t.Errorf("invalid operator TTL %d error = %v", ttl, err)
		}
	}
	invalidUnit := cfg
	invalidUnit.SystemdUnit = "mithril.target"
	if _, err := validateTestOperatorConfig(invalidUnit); err == nil || !strings.Contains(err.Error(), "systemd unit") {
		t.Fatalf("invalid systemd unit error = %v", err)
	}
	invalidTarget := cfg
	invalidTarget.ControlTargetID = "bad target"
	if _, err := validateTestOperatorConfig(invalidTarget); err == nil || !strings.Contains(err.Error(), "target ID") {
		t.Fatalf("invalid target error = %v", err)
	}
	if err := os.Chmod(approvers, 0o770); err != nil {
		t.Fatal(err)
	}
	if _, err := validateTestOperatorConfig(cfg); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("unsafe key directory permissions error = %v", err)
	}
	cfg.ControlEnabled = false
	if _, err := validateTestOperatorConfig(cfg); err == nil || !strings.Contains(err.Error(), "control") {
		t.Fatalf("disabled operator config error = %v", err)
	}
	if _, err := validateTestOperatorConfig(Config{Profile: ProfileMonitor}); err != nil {
		t.Fatalf("monitor profile should ignore controls: %v", err)
	}
}

func TestServiceRunnerErrorsAreFixed(t *testing.T) {
	runner := &fakeServiceRunner{err: errors.New("secret stderr")}
	controller := testServiceController(t, runner, time.Now())
	_, err := controller.status(context.Background())
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("runner error = %v", err)
	}
}

func TestSystemctlEnvironmentIsFixedByScope(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/tmp/attacker-bus")
	t.Setenv("SYSTEMD_BUS_TIMEOUT", "999999")
	system := newExecServiceRunner("system")
	if slices.ContainsFunc(system.env, func(value string) bool {
		return strings.HasPrefix(value, "DBUS_SESSION_BUS_ADDRESS=") ||
			strings.HasPrefix(value, "SYSTEMD_BUS_TIMEOUT=")
	}) {
		t.Fatalf("system scope inherited a bus override: %v", system.env)
	}
	user := newExecServiceRunner("user")
	wantRuntimeDir := "XDG_RUNTIME_DIR=/run/user/" + strconv.Itoa(os.Geteuid())
	wantBus := "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/" +
		strconv.Itoa(os.Geteuid()) + "/bus"
	if !slices.Contains(user.env, wantRuntimeDir) ||
		!slices.Contains(user.env, wantBus) {
		t.Fatalf("user scope environment = %v", user.env)
	}
	if slices.Contains(user.env, "DBUS_SESSION_BUS_ADDRESS=unix:path=/tmp/attacker-bus") {
		t.Fatal("user scope inherited an untrusted bus address")
	}
}

func TestLoadApproverPrivateKeyRequiresExactPrivateFile(t *testing.T) {
	dir := secureTempDir(t)
	path := filepath.Join(dir, "approver.seed")
	seed := bytes.Repeat([]byte{0x24}, ed25519.SeedSize)
	if err := os.WriteFile(path, seed, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadApproverPrivateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Seed(), seed) {
		t.Fatal("loaded private key does not match its seed")
	}

	for name, mutate := range map[string]func(t *testing.T) string{
		"missing":  func(t *testing.T) string { return filepath.Join(dir, "missing") },
		"relative": func(t *testing.T) string { return "approver.seed" },
		"wrong size": func(t *testing.T) string {
			p := filepath.Join(dir, "short")
			if err := os.WriteFile(p, seed[:ed25519.SeedSize-1], 0o600); err != nil {
				t.Fatal(err)
			}
			return p
		},
		"unsafe permissions": func(t *testing.T) string {
			p := filepath.Join(dir, "world-readable")
			if err := os.WriteFile(p, seed, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(p, 0o644); err != nil {
				t.Fatal(err)
			}
			return p
		},
		"symlink": func(t *testing.T) string {
			p := filepath.Join(dir, "link")
			if err := os.Symlink(path, p); err != nil {
				t.Fatal(err)
			}
			return p
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadApproverPrivateKey(mutate(t)); err == nil {
				t.Fatal("unsafe private key source was accepted")
			}
		})
	}
}
