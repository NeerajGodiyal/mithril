package mcp

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxPendingApprovals  = 32
	maxSystemctlOutput   = 32 * 1024
	statusTimeout        = 5 * time.Second
	actionTimeout        = 2 * time.Minute
	postconditionTimeout = 30 * time.Second
	postconditionPoll    = 250 * time.Millisecond
	controlAuditTimeout  = 5 * time.Second
	auditRetryInterval   = 2 * time.Second
)

var systemdUnitPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@:-]{0,126}\.service$`)

// ValidateSystemdServiceUnit accepts one explicit .service unit name.
func ValidateSystemdServiceUnit(unit string) error {
	if !systemdUnitPattern.MatchString(unit) {
		return errors.New("systemd unit must name one .service unit")
	}
	return nil
}

type serviceStatus struct {
	Unit                            string `json:"unit"`
	Scope                           string `json:"scope"`
	LoadState                       string `json:"load_state"`
	ActiveState                     string `json:"active_state"`
	SubState                        string `json:"sub_state"`
	Result                          string `json:"result,omitempty"`
	NRestarts                       uint64 `json:"restart_count"`
	MainPID                         uint64 `json:"main_pid"`
	InvocationID                    string `json:"invocation_id,omitempty"`
	ActiveEnterTimestampMonotonic   uint64 `json:"active_enter_timestamp_monotonic"`
	InactiveEnterTimestampMonotonic uint64 `json:"inactive_enter_timestamp_monotonic"`
	Job                             string `json:"job,omitempty"`
}

type serviceStatusInput struct{}

type serviceStatusOutput struct {
	ObservedAt string        `json:"observed_at"`
	Status     serviceStatus `json:"status"`
}

type prepareServiceActionInput struct {
	Action        string `json:"action" jsonschema:"lifecycle action to approve: start, stop, or restart"`
	ApproverKeyID string `json:"approver_key_id,omitempty" jsonschema:"configured approver key ID; optional when exactly one key is configured"`
}

type prepareServiceActionOutput struct {
	ApprovalRequired bool          `json:"approval_required"`
	ActionID         string        `json:"action_id"`
	Summary          string        `json:"summary"`
	ExpiresAt        string        `json:"expires_at"`
	Challenge        string        `json:"challenge"`
	NextStep         string        `json:"next_step"`
	Status           serviceStatus `json:"status"`
}

type executeServiceActionInput struct {
	ApprovalBundle ServiceApprovalBundle `json:"approval_bundle" jsonschema:"authorization and audit bundle produced by mithril mcp approve"`
}

type executeServiceActionOutput struct {
	ActionID                string                `json:"action_id"`
	Action                  string                `json:"action"`
	Unit                    string                `json:"unit"`
	Phase                   operationPhase        `json:"phase"`
	DispatchMayHaveOccurred bool                  `json:"dispatch_may_have_occurred"`
	DispatchAccepted        bool                  `json:"dispatch_accepted"`
	LocalAuditDurable       bool                  `json:"local_audit_durable"`
	AuditAcknowledged       bool                  `json:"audit_acknowledged"`
	StatusBefore            serviceStatus         `json:"status_before"`
	StatusObserved          *serviceStatus        `json:"status_observed,omitempty"`
	ReasonCode              string                `json:"reason_code,omitempty"`
	NodeReadiness           *serviceNodeReadiness `json:"node_readiness,omitempty"`
	UpdatedAt               string                `json:"updated_at"`
	NewActionAllowedAt      string                `json:"new_action_allowed_at,omitempty"`
	NextStep                string                `json:"next_step"`
}

type verifyServiceActionInput struct {
	ActionID string `json:"action_id" jsonschema:"action ID returned by mithril_prepare_service_action"`
}

type verifyServiceActionOutput = executeServiceActionOutput

type serviceNodeReadiness struct {
	Assessed             bool    `json:"assessed"`
	DiagnosisStatus      string  `json:"diagnosis_status"`
	EvidenceComplete     bool    `json:"evidence_complete"`
	SafeForAutomation    bool    `json:"safe_for_automation"`
	SlotProgressObserved bool    `json:"slot_progress_observed"`
	FirstSlot            *uint64 `json:"first_slot,omitempty"`
	LaterSlot            *uint64 `json:"later_slot,omitempty"`
}

type serviceAuditStatus struct {
	localDurable   bool
	acknowledged   bool
	remoteRejected bool
}

type serviceRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type execServiceRunner struct {
	env []string
}

type boundedCommandBuffer struct {
	data []byte
	max  int
}

func (b *boundedCommandBuffer) Write(p []byte) (int, error) {
	if len(b.data)+len(p) > b.max {
		return 0, errors.New("command output exceeded limit")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

func (runner execServiceRunner) Run(ctx context.Context, path string, args ...string) ([]byte, error) {
	var stdout boundedCommandBuffer
	stdout.max = maxSystemctlOutput
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = append([]string(nil), runner.env...)
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, errors.New("systemctl command failed")
	}
	return stdout.data, nil
}

func newExecServiceRunner(scope string) execServiceRunner {
	env := []string{
		"LANG=C",
		"LC_ALL=C",
		"SYSTEMD_COLORS=0",
		"SYSTEMD_PAGER=",
	}
	if scope == "user" {
		runtimeDir := fmt.Sprintf("/run/user/%d", os.Geteuid())
		env = append(
			env,
			"XDG_RUNTIME_DIR="+runtimeDir,
			"DBUS_SESSION_BUS_ADDRESS=unix:path="+runtimeDir+"/bus",
		)
	}
	return execServiceRunner{env: env}
}

type serviceController struct {
	cfg       Config
	authority approvalAuthority
	runtime   *controlRuntime
	runner    serviceRunner
	now       func() time.Time
	random    io.Reader
	readiness func(context.Context, Config) *serviceNodeReadiness

	// allowedActions is this deployment's explicit allowlist. Empty means no
	// action may run, which is the correct failure for a misconfiguration.
	allowedActions map[serviceAction]bool
	// executable is the systemctl identity recorded at startup, re-verified
	// before every dispatch. nil only where no executable was resolvable,
	// in which case dispatch has already been refused upstream.
	executable *executableIdentity
	// executableErr records why identity resolution failed, so dispatch can say
	// what is wrong instead of failing open.
	executableErr error

	mu      sync.Mutex
	pending map[[approvalNonceBytes]byte]approvalClaims
}

type controlRuntime struct {
	state        *controlStateStore
	audit        *controlAuditTrail
	approvalKeys map[string]ed25519.PublicKey
	targetID     string
	unit         string
	scope        string
	deliveryStop context.CancelFunc
	deliveryDone chan struct{}
	retryEvery   time.Duration
}

func newServiceController(cfg Config, authority approvalAuthority) *serviceController {
	return newServiceControllerWithRuntime(cfg, authority, nil)
}

func newServiceControllerWithRuntime(
	cfg Config,
	authority approvalAuthority,
	runtime *controlRuntime,
) *serviceController {
	normalized := cfg.normalized()
	c := &serviceController{
		cfg:       normalized,
		authority: authority,
		runtime:   runtime,
		runner:    newExecServiceRunner(normalized.SystemdScope),
		now:       time.Now,
		random:    rand.Reader,
		readiness: assessServiceNodeReadiness,
		pending:   make(map[[approvalNonceBytes]byte]approvalClaims),
	}
	// A parse failure leaves the allowlist empty, which refuses every action.
	// Startup validation rejects this configuration before serving, so an
	// empty map here means something is already wrong and dispatch must not
	// fall back to permitting anything.
	if allowed, err := parseAllowedServiceActions(normalized.AllowedServiceActions); err == nil {
		c.allowedActions = allowed
	}
	// Record the executable identity once, so every later dispatch can prove
	// the binary has not been swapped since. A resolution failure is kept, not
	// dropped: dispatch must refuse rather than run an unverified binary.
	id, err := resolveExecutable(normalized.SystemctlPath)
	if err != nil {
		c.executableErr = err
	} else {
		c.executable = &id
	}
	return c
}

func openControlRuntime(
	ctx context.Context,
	cfg Config,
	authority approvalAuthority,
) (*controlRuntime, error) {
	if cfg.Profile != ProfileOperator {
		return nil, nil
	}
	state, err := newControlStateStore(filepath.Join(cfg.ControlStateDir, "operation.json"))
	if err != nil {
		return nil, errors.New("operator control state is unavailable")
	}
	historicalKeys := authority.publicKeys
	if cfg.ApproverHistoryKeysDir != cfg.ApproverKeysDir {
		historicalKeys, err = loadApproverPublicKeys(cfg.ApproverHistoryKeysDir)
		if err != nil {
			return nil, errors.New("operator historical approver keys are unavailable")
		}
	}
	verificationKeys, err := mergeApproverPublicKeys(
		authority.publicKeys,
		historicalKeys,
	)
	if err != nil {
		return nil, errors.New("operator approver key sets conflict")
	}
	verifier := newControlAuditApprovalVerifierWithActive(
		verificationKeys,
		authority.publicKeys,
	)
	audit, err := openControlAuditTrail(
		ctx,
		cfg.ControlStateDir,
		cfg.AuditClientConfigPath,
		verifier,
	)
	if err != nil {
		return nil, err
	}
	runtime := &controlRuntime{
		state:        state,
		audit:        audit,
		approvalKeys: verificationKeys,
		targetID:     authority.targetID,
		unit:         cfg.SystemdUnit,
		scope:        cfg.SystemdScope,
	}
	if err := runtime.recover(ctx, time.Now().UTC()); err != nil {
		_ = runtime.close()
		return nil, err
	}
	runtime.startDeliveryWorker()
	return runtime, nil
}

func (runtime *controlRuntime) close() error {
	if runtime == nil || runtime.audit == nil {
		return nil
	}
	if runtime.deliveryStop != nil {
		runtime.deliveryStop()
		<-runtime.deliveryDone
	}
	syncCtx, cancel := context.WithTimeout(context.Background(), controlAuditTimeout)
	_ = runtime.audit.syncPending(syncCtx)
	cancel()
	return runtime.audit.close()
}

func (runtime *controlRuntime) startDeliveryWorker() {
	if runtime == nil || runtime.audit == nil || runtime.deliveryStop != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	runtime.deliveryStop = cancel
	runtime.deliveryDone = make(chan struct{})
	interval := runtime.retryEvery
	if interval <= 0 {
		interval = auditRetryInterval
	}
	go func() {
		defer close(runtime.deliveryDone)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				attemptCtx, attemptCancel := context.WithTimeout(ctx, controlAuditTimeout)
				_ = runtime.audit.syncPending(attemptCtx)
				attemptCancel()
			}
		}
	}()
}

func (runtime *controlRuntime) ensureAudited(
	ctx context.Context,
	operation serviceOperation,
) (bool, error) {
	if runtime == nil || runtime.audit == nil {
		return false, errors.New("operator control audit is unavailable")
	}
	if last, ok := runtime.audit.lastEvent(); ok &&
		operationMatchesEvent(operation, last) {
		if err := runtime.audit.syncPending(ctx); err != nil {
			return false, err
		}
		return true, nil
	}
	fields := controlAuditEventFields{
		Timestamp:  time.Unix(operation.UpdatedAtUnix, 0).UTC(),
		AfterHash:  operation.AfterHash,
		Outcome:    operation.Outcome,
		ReasonCode: operation.ReasonCode,
	}
	appendEvent := runtime.audit.appendAndAcknowledge
	if operation.DispatchMayHaveOccurred || operation.Phase.terminal() {
		appendEvent = runtime.audit.appendLocalAndQueue
	}
	_, err := appendEvent(ctx, operation, fields)
	return err == nil, err
}

func (runtime *controlRuntime) ensureAuditedBeforeDispatch(
	ctx context.Context,
	operation serviceOperation,
) (bool, error) {
	auditCtx, cancel := context.WithTimeout(ctx, controlAuditTimeout)
	defer cancel()
	return runtime.ensureAudited(auditCtx, operation)
}

func (runtime *controlRuntime) recover(ctx context.Context, now time.Time) error {
	if runtime == nil || runtime.state == nil || runtime.audit == nil {
		return errors.New("operator control runtime is incomplete")
	}
	return runtime.state.withTransaction(ctx, func(transaction *controlStateTransaction) error {
		current := transaction.operation()
		if current == nil {
			last, ok := runtime.audit.lastEvent()
			if !ok {
				return runtime.audit.syncPending(ctx)
			}
			restored, err := operationFromControlAuditEvent(last)
			if err != nil {
				return errors.New("operator control state could not be reconstructed from audit")
			}
			if err := verifyServiceOperationApproval(restored, runtime.approvalKeys); err != nil {
				return errors.New("reconstructed operator control approval is invalid")
			}
			if err := transaction.restore(restored); err != nil {
				return errors.New("operator control state restoration failed")
			}
			current = &restored
		}
		if (runtime.targetID != "" && current.TargetID != runtime.targetID) ||
			(runtime.unit != "" && current.Unit != runtime.unit) ||
			(runtime.scope != "" && current.Scope != runtime.scope) {
			return errors.New("operator control state does not match the configured target")
		}
		if err := verifyServiceOperationApproval(*current, runtime.approvalKeys); err != nil {
			return errors.New("operator control state approval is invalid")
		}
		if _, err := runtime.ensureAudited(ctx, *current); err != nil {
			return errors.New("operator control audit recovery failed")
		}

		var next serviceOperation
		var err error
		switch current.Phase {
		case phasePrepared:
			next, err = current.transitionedWithResult(
				phaseFailed,
				now,
				&current.StatusBefore,
				"failed",
				"server_restarted_before_dispatch",
				false,
				false,
			)
		case phaseDispatchStarted:
			next, err = current.transitionedAfterDispatch(
				phaseOutcomeUnknown,
				now,
				nil,
				"outcome_unknown",
				"server_restarted_during_action",
				current.DispatchAccepted,
			)
		case phaseDispatched, phaseVerifying:
			next, err = current.transitionedWithResult(
				phaseOutcomeUnknown,
				now,
				nil,
				"outcome_unknown",
				"server_restarted_during_action",
				true,
				current.DispatchAccepted,
			)
		default:
			return nil
		}
		if err != nil {
			return err
		}
		if err := transaction.save(next); err != nil {
			return err
		}
		if _, err := runtime.ensureAudited(ctx, next); err != nil {
			return errors.New("operator control audit recovery failed")
		}
		return nil
	})
}

func verifyServiceOperationApproval(
	operation serviceOperation,
	publicKeys map[string]ed25519.PublicKey,
) error {
	binding, err := VerifyControlApprovalEvidence(
		operation.Approval,
		publicKeys,
	)
	if err != nil {
		return err
	}
	if binding.ServerSession != operation.ServerSession ||
		binding.TargetID != operation.TargetID ||
		binding.ActionID != operation.ID ||
		binding.Action != string(operation.Action) ||
		binding.Unit != operation.Unit ||
		binding.Scope != operation.Scope ||
		binding.BeforeHash != operation.BeforeHash ||
		binding.ApproverKeyID != operation.Approval.ApproverKeyID ||
		binding.IssuedAtUnix != operation.Approval.IssuedAtUnix ||
		binding.ExpiresAtUnix != operation.Approval.ExpiresAtUnix {
		return errors.New("control operation approval does not match its durable state")
	}
	return nil
}

func validateAndLoadOperatorConfig(cfg Config) (approvalAuthority, error) {
	return validateAndLoadOperatorConfigWithResolver(cfg, resolveExecutable)
}

func validateAndLoadOperatorConfigWithResolver(
	cfg Config,
	resolve func(string) (executableIdentity, error),
) (approvalAuthority, error) {
	return validateAndLoadOperatorConfigWithResolvers(
		cfg,
		resolve,
		loadApproverPublicKeys,
		rand.Reader,
	)
}

func validateAndLoadOperatorConfigWithResolvers(
	cfg Config,
	resolve func(string) (executableIdentity, error),
	loadKeys func(string) (map[string]ed25519.PublicKey, error),
	random io.Reader,
) (approvalAuthority, error) {
	profile, err := ParseProfile(string(cfg.Profile))
	if err != nil || profile != ProfileOperator {
		return approvalAuthority{}, nil
	}
	if cfg.ApprovalTTLSeconds != 0 && (cfg.ApprovalTTLSeconds < MinApprovalTTLSeconds || cfg.ApprovalTTLSeconds > MaxApprovalTTLSeconds) {
		return approvalAuthority{}, fmt.Errorf("operator approval TTL must be between %d and %d seconds", MinApprovalTTLSeconds, MaxApprovalTTLSeconds)
	}
	cfg = cfg.normalized()
	if !cfg.ControlEnabled {
		return approvalAuthority{}, errors.New("operator profile requires lifecycle control to be enabled")
	}
	// An explicit allowlist is mandatory. Refusing to start is the right
	// failure: a node that silently permits every lifecycle action is worse
	// than one that will not serve.
	if _, err := parseAllowedServiceActions(cfg.AllowedServiceActions); err != nil {
		return approvalAuthority{}, err
	}
	if err := ValidateSystemdServiceUnit(cfg.SystemdUnit); err != nil {
		return approvalAuthority{}, errors.New("operator systemd unit must name one .service unit")
	}
	if cfg.SystemdScope != "system" && cfg.SystemdScope != "user" {
		return approvalAuthority{}, errors.New("operator systemd scope must be system or user")
	}
	if !filepath.IsAbs(cfg.SystemctlPath) || filepath.Clean(cfg.SystemctlPath) != cfg.SystemctlPath {
		return approvalAuthority{}, errors.New("operator systemctl path must be a clean absolute path")
	}
	if _, err := resolve(cfg.SystemctlPath); err != nil {
		return approvalAuthority{}, fmt.Errorf("operator systemctl path is unsafe: %w", err)
	}
	if !filepath.IsAbs(cfg.ControlStateDir) ||
		filepath.Clean(cfg.ControlStateDir) != cfg.ControlStateDir ||
		cfg.ControlStateDir == string(filepath.Separator) {
		return approvalAuthority{}, errors.New("operator control state directory must be a clean absolute path")
	}
	if !filepath.IsAbs(cfg.AuditClientConfigPath) ||
		filepath.Clean(cfg.AuditClientConfigPath) != cfg.AuditClientConfigPath {
		return approvalAuthority{}, errors.New("operator audit client config must be a clean absolute path")
	}
	return newApprovalAuthority(cfg, random, loadKeys)
}

// specFor resolves an action through the registry AND the operator allowlist.
// Both must permit it: the registry bounds what exists, the allowlist bounds
// what this deployment chose to expose.
func (c *serviceController) specFor(raw string) (serviceActionSpec, error) {
	action, err := parseServiceAction(raw)
	if err != nil {
		return serviceActionSpec{}, err
	}
	if len(c.allowedActions) == 0 || !c.allowedActions[action] {
		return serviceActionSpec{}, fmt.Errorf("action %s is not in this deployment's allowed action list", action)
	}
	return serviceActionRegistry[action], nil
}

func (c *serviceController) requireRuntime() (*controlRuntime, error) {
	if c.runtime == nil || c.runtime.state == nil || c.runtime.audit == nil {
		return nil, errors.New("operator control runtime is unavailable")
	}
	return c.runtime, nil
}

func (c *serviceController) checkOperationBarrier(
	ctx context.Context,
	now time.Time,
) error {
	runtime, err := c.requireRuntime()
	if err != nil {
		return err
	}
	// State is not the only one-action barrier: the audit store independently
	// rejects a second nonterminal action and retains the approval hold.
	return runtime.state.withTransaction(ctx, func(transaction *controlStateTransaction) error {
		current := transaction.operation()
		if current != nil {
			acknowledged, err := runtime.ensureAudited(ctx, *current)
			if err != nil || !acknowledged {
				return errors.New("operator control audit is not synchronized")
			}
			if current.blocksNewOperation(now) {
				if current.Phase.terminal() {
					allowedAt := time.Unix(
						current.newOperationAllowedAtUnix(),
						0,
					).UTC().Format(time.RFC3339)
					return fmt.Errorf(
						"previous approval generation has not expired; a fresh action may be prepared after %s",
						allowedAt,
					)
				}
				return fmt.Errorf("service action %s is still %s", current.ID, current.Phase)
			}
			return nil
		}
		if err := runtime.audit.syncPending(ctx); err != nil {
			return errors.New("operator control audit is not synchronized")
		}
		return nil
	})
}

func (c *serviceController) registerPendingApproval(
	ctx context.Context,
	claims approvalClaims,
) error {
	runtime, err := c.requireRuntime()
	if err != nil {
		return err
	}
	return runtime.state.withTransaction(ctx, func(transaction *controlStateTransaction) error {
		now := c.now().UTC()
		current := transaction.operation()
		if current != nil {
			acknowledged, auditErr := runtime.ensureAudited(ctx, *current)
			if auditErr != nil || !acknowledged {
				return errors.New("operator control audit is not synchronized")
			}
			if current.blocksNewOperation(now) {
				if current.Phase.terminal() {
					allowedAt := time.Unix(
						current.newOperationAllowedAtUnix(),
						0,
					).UTC().Format(time.RFC3339)
					return fmt.Errorf(
						"previous approval generation has not expired; a fresh action may be prepared after %s",
						allowedAt,
					)
				}
				return fmt.Errorf("service action %s is still %s", current.ID, current.Phase)
			}
		} else if err := runtime.audit.syncPending(ctx); err != nil {
			return errors.New("operator control audit is not synchronized")
		}

		c.mu.Lock()
		defer c.mu.Unlock()
		for nonce, pending := range c.pending {
			if pending.ExpiresAtUnix <= now.Unix() {
				delete(c.pending, nonce)
			}
		}
		if len(c.pending) >= maxPendingApprovals {
			return errors.New("too many pending approvals; wait for one to expire")
		}
		c.pending[claims.Nonce] = claims
		return nil
	})
}

func (c *serviceController) systemctlArgs(commandAndArgs ...string) []string {
	args := make([]string, 0, len(commandAndArgs)+2)
	if c.cfg.SystemdScope == "user" {
		args = append(args, "--user")
	}
	args = append(args, "--no-ask-password")
	args = append(args, commandAndArgs...)
	return args
}

const serviceProperties = "LoadState,ActiveState,SubState,Result,NRestarts,MainPID,InvocationID,ActiveEnterTimestampMonotonic,InactiveEnterTimestampMonotonic,Job"

func (c *serviceController) status(ctx context.Context) (serviceStatus, error) {
	if err := c.verifyExecutable(); err != nil {
		return serviceStatus{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	args := c.systemctlArgs("show", c.cfg.SystemdUnit, "--no-pager", "--property="+serviceProperties)
	data, err := c.runner.Run(ctx, c.cfg.SystemctlPath, args...)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return serviceStatus{}, ctxErr
		}
		return serviceStatus{}, errors.New("systemctl status failed")
	}
	return parseServiceStatus(c.cfg.SystemdUnit, c.cfg.SystemdScope, data)
}

func (c *serviceController) verifyExecutable() error {
	if c.executable == nil {
		if c.executableErr != nil {
			return c.executableErr
		}
		return errors.New("systemctl executable identity is unavailable")
	}
	return c.executable.verifyUnchanged()
}

func parseServiceStatus(unit, scope string, data []byte) (serviceStatus, error) {
	values := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return serviceStatus{}, errors.New("systemctl returned malformed status")
		}
		if _, duplicate := values[key]; duplicate {
			return serviceStatus{}, errors.New("systemctl returned duplicate status property")
		}
		values[key] = value
	}
	for _, key := range []string{"LoadState", "ActiveState", "SubState"} {
		if values[key] == "" {
			return serviceStatus{}, errors.New("systemctl status is incomplete")
		}
	}
	parseUint := func(key string) (uint64, error) {
		if values[key] == "" {
			return 0, nil
		}
		value, err := strconv.ParseUint(values[key], 10, 64)
		if err != nil {
			return 0, errors.New("systemctl status contains an invalid numeric property")
		}
		return value, nil
	}
	nRestarts, err := parseUint("NRestarts")
	if err != nil {
		return serviceStatus{}, err
	}
	mainPID, err := parseUint("MainPID")
	if err != nil {
		return serviceStatus{}, err
	}
	activeAt, err := parseUint("ActiveEnterTimestampMonotonic")
	if err != nil {
		return serviceStatus{}, err
	}
	inactiveAt, err := parseUint("InactiveEnterTimestampMonotonic")
	if err != nil {
		return serviceStatus{}, err
	}
	return serviceStatus{
		Unit:                            unit,
		Scope:                           scope,
		LoadState:                       boundedSystemdValue(values["LoadState"]),
		ActiveState:                     boundedSystemdValue(values["ActiveState"]),
		SubState:                        boundedSystemdValue(values["SubState"]),
		Result:                          boundedSystemdValue(values["Result"]),
		NRestarts:                       nRestarts,
		MainPID:                         mainPID,
		InvocationID:                    boundedSystemdValue(values["InvocationID"]),
		ActiveEnterTimestampMonotonic:   activeAt,
		InactiveEnterTimestampMonotonic: inactiveAt,
		Job:                             boundedSystemdValue(values["Job"]),
	}, nil
}

func boundedSystemdValue(value string) string {
	value = redactUntrustedText(value)
	value, _ = truncateUTF8Bytes(value, 256)
	return value
}

func serviceStateHash(status serviceStatus) string {
	data, _ := json.Marshal(status)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (c *serviceController) prepare(
	ctx context.Context,
	action string,
	requestedKeyID string,
) (prepareServiceActionOutput, error) {
	if !c.authority.configured() {
		return prepareServiceActionOutput{}, errors.New("operator approval authority is unavailable")
	}
	parsed, err := parseServiceAction(action)
	if err != nil {
		return prepareServiceActionOutput{}, err
	}
	spec, err := c.specFor(string(parsed))
	if err != nil {
		return prepareServiceActionOutput{}, err
	}
	keyID, err := c.authority.resolveKeyID(requestedKeyID)
	if err != nil {
		return prepareServiceActionOutput{}, err
	}
	now := c.now().UTC()
	if err := c.checkOperationBarrier(ctx, now); err != nil {
		return prepareServiceActionOutput{}, err
	}
	status, err := c.status(ctx)
	if err != nil {
		return prepareServiceActionOutput{}, err
	}
	if err := validateActionPreState(spec, status); err != nil {
		return prepareServiceActionOutput{}, err
	}
	actionID, err := randomApprovalID(c.random)
	if err != nil {
		return prepareServiceActionOutput{}, errors.New("failed to create approval challenge")
	}
	var nonce [approvalNonceBytes]byte
	if _, err := io.ReadFull(c.random, nonce[:]); err != nil {
		return prepareServiceActionOutput{}, errors.New("failed to create approval challenge")
	}
	claims := approvalClaims{
		Version:       approvalVersion,
		Domain:        serviceApprovalDomain,
		ServerSession: c.authority.serverSession,
		TargetID:      c.authority.targetID,
		ActionID:      actionID,
		Action:        parsed,
		Unit:          c.cfg.SystemdUnit,
		Scope:         c.cfg.SystemdScope,
		BeforeHash:    serviceStateHash(status),
		Nonce:         nonce,
		IssuedAtUnix:  now.Unix(),
		ExpiresAtUnix: now.Add(time.Duration(c.cfg.ApprovalTTLSeconds) * time.Second).Unix(),
		ApproverKeyID: keyID,
	}
	challenge, err := encodeApprovalChallenge(claims, status)
	if err != nil {
		return prepareServiceActionOutput{}, errors.New("failed to encode approval challenge")
	}
	if err := c.registerPendingApproval(ctx, claims); err != nil {
		return prepareServiceActionOutput{}, err
	}

	return prepareServiceActionOutput{
		ApprovalRequired: true,
		ActionID:         claims.ActionID,
		Summary:          fmt.Sprintf("%s %s (%s scope)", parsed, c.cfg.SystemdUnit, c.cfg.SystemdScope),
		ExpiresAt:        time.Unix(claims.ExpiresAtUnix, 0).UTC().Format(time.RFC3339),
		Challenge:        challenge,
		NextStep:         "On the separate approver host, run mithril mcp approve and paste this challenge at its hidden prompt. Return the complete approval bundle to this same MCP session before it expires.",
		Status:           status,
	}, nil
}

func sameApproval(a, b approvalClaims) bool {
	return a == b
}

// consumeApproval verifies both signed bundle members before atomically
// consuming the pending bearer nonce. The returned evidence is non-authorizing
// and can be bound to durable operation state; the bearer token is not kept.
func (c *serviceController) consumeApproval(
	bundle ServiceApprovalBundle,
) (approvalClaims, ControlApprovalEvidence, error) {
	approved, evidence, err := verifyServiceApprovalBundle(bundle, c.authority, c.now())
	if err != nil {
		return approvalClaims{}, ControlApprovalEvidence{}, err
	}
	if approved.Unit != c.cfg.SystemdUnit || approved.Scope != c.cfg.SystemdScope {
		return approvalClaims{}, ControlApprovalEvidence{}, errors.New("approval token does not match this service")
	}

	c.mu.Lock()
	pending, ok := c.pending[approved.Nonce]
	if ok && sameApproval(pending, approved) {
		// Every pending challenge was prepared against the same mutable
		// service surface. Accepting one invalidates its siblings before any
		// caller can race a second dispatch from the old state.
		clear(c.pending)
	} else {
		ok = false
	}
	c.mu.Unlock()
	if !ok {
		return approvalClaims{}, ControlApprovalEvidence{}, errors.New("approval token is unknown, already used, or no longer pending")
	}
	return approved, evidence, nil
}

func (c *serviceController) execute(
	ctx context.Context,
	bundle ServiceApprovalBundle,
) (executeServiceActionOutput, error) {
	runtime, err := c.requireRuntime()
	if err != nil {
		return executeServiceActionOutput{}, err
	}
	var output executeServiceActionOutput
	err = runtime.state.withTransaction(ctx, func(transaction *controlStateTransaction) error {
		now := c.now().UTC()
		if current := transaction.operation(); current != nil {
			acknowledged, auditErr := runtime.ensureAudited(ctx, *current)
			if auditErr != nil || !acknowledged {
				return errors.New("operator control audit is not synchronized")
			}
			if current.blocksNewOperation(now) {
				if current.Phase.terminal() {
					allowedAt := time.Unix(
						current.newOperationAllowedAtUnix(),
						0,
					).UTC().Format(time.RFC3339)
					return fmt.Errorf(
						"previous approval generation has not expired; a fresh action may be prepared after %s",
						allowedAt,
					)
				}
				return fmt.Errorf("service action %s is still %s", current.ID, current.Phase)
			}
		} else if err := runtime.audit.syncPending(ctx); err != nil {
			return errors.New("operator control audit is not synchronized")
		}

		approved, evidence, err := c.consumeApproval(bundle)
		if err != nil {
			return err
		}
		spec, err := c.specFor(string(approved.Action))
		if err != nil {
			return err
		}
		status, err := c.status(ctx)
		if err != nil {
			return err
		}
		if serviceStateHash(status) != approved.BeforeHash {
			return errors.New("service state changed after approval was prepared; prepare a new action")
		}
		if err := validateActionPreState(spec, status); err != nil {
			return err
		}
		acceptedAt := c.now().UTC()
		if err := validateApprovalTime(
			approved.IssuedAtUnix,
			approved.ExpiresAtUnix,
			acceptedAt,
		); err != nil {
			return err
		}

		operation, err := newServiceOperation(
			approved.ActionID,
			approved.ServerSession,
			approved.TargetID,
			approved.Action,
			status,
			evidence,
			acceptedAt,
			acceptedAt.Add(postconditionTimeout),
		)
		if err != nil {
			return err
		}
		if err := transaction.save(operation); err != nil {
			return err
		}
		acknowledged, err := runtime.ensureAuditedBeforeDispatch(ctx, operation)
		if err != nil || !acknowledged {
			output, err = c.failBeforeDispatch(
				transaction,
				operation,
				acknowledged,
				err,
				"audit_not_acknowledged",
				nil,
			)
			return err
		}

		if err := validateApprovalTime(
			operation.Approval.IssuedAtUnix,
			operation.Approval.ExpiresAtUnix,
			c.now(),
		); err != nil {
			output, err = c.failBeforeDispatch(
				transaction,
				operation,
				true,
				nil,
				"approval_expired_before_dispatch",
				nil,
			)
			return err
		}
		if err := c.verifyExecutable(); err != nil {
			output, err = c.failBeforeDispatch(
				transaction,
				operation,
				true,
				nil,
				"systemctl_identity_changed",
				nil,
			)
			return err
		}
		operation, err = operation.transitionedWithResult(
			phaseDispatchStarted,
			c.now(),
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
		acknowledged, err = runtime.ensureAuditedBeforeDispatch(ctx, operation)
		if err != nil || !acknowledged {
			output, err = c.failBeforeDispatch(
				transaction,
				operation,
				acknowledged,
				err,
				"audit_not_acknowledged",
				nil,
			)
			return err
		}
		if reasonCode, observed := c.dispatchReadinessFailure(
			ctx,
			spec,
			operation,
		); reasonCode != "" {
			output, err = c.failBeforeDispatch(
				transaction,
				operation,
				true,
				nil,
				reasonCode,
				observed,
			)
			return err
		}

		// Once the audited dispatch boundary is crossed, losing the requesting
		// MCP connection must not turn into an implicit cancellation. The
		// systemd job is manager-owned and may continue after its client exits,
		// so keep waiting under a separate, bounded server context.
		actionCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx),
			actionTimeout,
		)
		args := c.systemctlArgs(
			"--job-mode=fail",
			spec.verb,
			c.cfg.SystemdUnit,
		)
		_, dispatchErr := c.runner.Run(
			actionCtx,
			c.cfg.SystemctlPath,
			args...,
		)
		cancel()
		postDispatchCtx, postDispatchCancel := context.WithTimeout(
			context.Background(),
			controlAuditTimeout,
		)
		defer postDispatchCancel()
		if dispatchErr != nil {
			operation, err = operation.transitionedAfterDispatch(
				phaseOutcomeUnknown,
				c.now(),
				nil,
				"outcome_unknown",
				"systemctl_result_unknown",
				false,
			)
			if err != nil {
				return err
			}
			if err := transaction.save(operation); err != nil {
				return err
			}
			acknowledged, auditErr := runtime.ensureAudited(
				postDispatchCtx,
				operation,
			)
			auditStatus := c.auditStatusAfterEnsure(
				operation,
				acknowledged,
				auditErr,
			)
			output = serviceOperationOutput(operation, auditStatus)
			return nil
		}

		operation, err = operation.transitionedAfterDispatch(
			phaseDispatched,
			c.now(),
			nil,
			"",
			"",
			true,
		)
		if err != nil {
			return err
		}
		if err := transaction.save(operation); err != nil {
			return err
		}
		acknowledged, auditErr := runtime.ensureAudited(
			postDispatchCtx,
			operation,
		)
		auditStatus := c.auditStatusAfterEnsure(
			operation,
			acknowledged,
			auditErr,
		)
		if !auditStatus.localDurable {
			output = serviceOperationOutput(operation, auditStatus)
			return nil
		}

		verificationCtx, verificationCancel := context.WithTimeout(
			context.WithoutCancel(ctx),
			postconditionTimeout+statusTimeout,
		)
		defer verificationCancel()
		operation, auditStatus, err = c.reconcileServiceOperation(
			verificationCtx,
			transaction,
			operation,
			auditStatus,
		)
		if err != nil {
			return err
		}
		output = serviceOperationOutput(operation, auditStatus)
		return nil
	})
	if err != nil {
		return executeServiceActionOutput{}, err
	}
	if output.Phase == phaseSucceeded &&
		(output.Action == string(actionStart) ||
			output.Action == string(actionRestart)) &&
		c.readiness != nil {
		applyServiceNodeReadiness(&output, c.readiness(ctx, c.cfg))
	}
	return output, nil
}

func (c *serviceController) failBeforeDispatch(
	transaction *controlStateTransaction,
	operation serviceOperation,
	acknowledged bool,
	auditErr error,
	reasonCode string,
	observed *serviceStatus,
) (executeServiceActionOutput, error) {
	auditStatus := c.auditStatusAfterEnsure(
		operation,
		acknowledged,
		auditErr,
	)
	if !auditStatus.localDurable {
		retryCtx, cancel := context.WithTimeout(
			context.Background(),
			controlAuditTimeout,
		)
		acknowledged, auditErr = c.runtime.ensureAudited(retryCtx, operation)
		cancel()
		auditStatus = c.auditStatusAfterEnsure(
			operation,
			acknowledged,
			auditErr,
		)
	}
	if !auditStatus.localDurable {
		output := serviceOperationOutput(operation, auditStatus)
		output.ReasonCode = "audit_not_durable"
		output.NextStep = "This request did not invoke systemctl. Restore local audit durability, restarting the MCP server if needed, then call mithril_verify_service_action with this action_id; do not repeat the action."
		return output, nil
	}
	statusAfter := operation.StatusBefore
	if observed != nil {
		statusAfter = *observed
	}
	next, err := operation.transitionedWithResult(
		phaseFailed,
		c.now(),
		&statusAfter,
		"failed",
		reasonCode,
		false,
		false,
	)
	if err != nil {
		return executeServiceActionOutput{}, err
	}
	if err := transaction.save(next); err != nil {
		return executeServiceActionOutput{}, err
	}
	auditCtx, cancel := context.WithTimeout(
		context.Background(),
		controlAuditTimeout,
	)
	defer cancel()
	acknowledged, auditErr = c.runtime.ensureAudited(auditCtx, next)
	auditStatus = c.auditStatusAfterEnsure(
		next,
		acknowledged,
		auditErr,
	)
	return serviceOperationOutput(next, auditStatus), nil
}

func (c *serviceController) dispatchReadinessFailure(
	ctx context.Context,
	spec serviceActionSpec,
	operation serviceOperation,
) (string, *serviceStatus) {
	if err := validateApprovalTime(
		operation.Approval.IssuedAtUnix,
		operation.Approval.ExpiresAtUnix,
		c.now(),
	); err != nil {
		return "approval_expired_before_dispatch", nil
	}
	if err := c.verifyExecutable(); err != nil {
		return "systemctl_identity_changed", nil
	}
	current, err := c.status(ctx)
	if err != nil {
		return "systemd_status_unavailable_before_dispatch", nil
	}
	if serviceStateHash(current) != operation.BeforeHash ||
		validateActionPreState(spec, current) != nil {
		return "service_state_changed_before_dispatch", &current
	}
	if err := validateApprovalTime(
		operation.Approval.IssuedAtUnix,
		operation.Approval.ExpiresAtUnix,
		c.now(),
	); err != nil {
		return "approval_expired_before_dispatch", &current
	}
	if err := c.verifyExecutable(); err != nil {
		return "systemctl_identity_changed", &current
	}
	return "", nil
}

func (c *serviceController) reconcileServiceOperation(
	ctx context.Context,
	transaction *controlStateTransaction,
	operation serviceOperation,
	auditStatus serviceAuditStatus,
) (serviceOperation, serviceAuditStatus, error) {
	var err error
	if operation.Phase == phaseDispatchStarted {
		operation, err = operation.transitionedAfterDispatch(
			phaseOutcomeUnknown,
			c.now(),
			nil,
			"outcome_unknown",
			"dispatch_completion_unrecorded",
			false,
		)
		if err != nil {
			return serviceOperation{}, auditStatus, err
		}
		if err := transaction.save(operation); err != nil {
			return serviceOperation{}, auditStatus, err
		}
		auditStatus = c.auditAfterDispatch(operation)
		return operation, auditStatus, nil
	}
	if operation.Phase == phaseOutcomeUnknown {
		return operation, auditStatus, nil
	}
	if operation.Phase == phaseDispatched {
		operation, err = operation.transitionedWithResult(
			phaseVerifying,
			c.now(),
			nil,
			"",
			"",
			true,
			operation.DispatchAccepted,
		)
		if err != nil {
			return serviceOperation{}, auditStatus, err
		}
		if err := transaction.save(operation); err != nil {
			return serviceOperation{}, auditStatus, err
		}
		auditStatus = c.auditAfterDispatch(operation)
		if !auditStatus.localDurable {
			return operation, auditStatus, nil
		}
	}
	if operation.Phase != phaseVerifying {
		return operation, auditStatus, nil
	}

	deadline := time.Unix(operation.DeadlineUnix, 0).UTC()
	for {
		current, statusErr := c.status(ctx)
		deadlineReached := !c.now().UTC().Before(deadline)
		if statusErr == nil {
			result := evaluateServicePostcondition(
				operation.Action,
				operation.StatusBefore,
				servicePostconditionObservation{
					Current:                  current,
					ObservationAuthenticated: true,
					DeadlineReached:          deadlineReached,
				},
			)
			switch result {
			case postconditionSucceeded:
				operation, err = operation.transitionedWithResult(
					phaseSucceeded,
					c.now(),
					&current,
					"succeeded",
					"postcondition_verified",
					true,
					operation.DispatchAccepted,
				)
			case postconditionOutcomeUnknown:
				operation, err = operation.transitionedWithResult(
					phaseOutcomeUnknown,
					c.now(),
					&current,
					"outcome_unknown",
					"postcondition_not_proven",
					true,
					operation.DispatchAccepted,
				)
			default:
				// Keep polling below.
			}
			if err != nil {
				return serviceOperation{}, auditStatus, err
			}
			if result == postconditionSucceeded ||
				result == postconditionOutcomeUnknown {
				if err := transaction.save(operation); err != nil {
					return serviceOperation{}, auditStatus, err
				}
				return operation, c.auditAfterDispatch(operation), nil
			}
		} else if deadlineReached {
			operation, err = operation.transitionedWithResult(
				phaseOutcomeUnknown,
				c.now(),
				nil,
				"outcome_unknown",
				"status_unavailable",
				true,
				operation.DispatchAccepted,
			)
			if err != nil {
				return serviceOperation{}, auditStatus, err
			}
			if err := transaction.save(operation); err != nil {
				return serviceOperation{}, auditStatus, err
			}
			return operation, c.auditAfterDispatch(operation), nil
		}

		timer := time.NewTimer(postconditionPoll)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			operation, err = operation.transitionedWithResult(
				phaseOutcomeUnknown,
				c.now(),
				nil,
				"outcome_unknown",
				"request_cancelled_during_verification",
				true,
				operation.DispatchAccepted,
			)
			if err != nil {
				return serviceOperation{}, auditStatus, err
			}
			if err := transaction.save(operation); err != nil {
				return serviceOperation{}, auditStatus, err
			}
			return operation, c.auditAfterDispatch(operation), nil
		case <-timer.C:
		}
	}
}

func (c *serviceController) auditAfterDispatch(
	operation serviceOperation,
) serviceAuditStatus {
	auditCtx, cancel := context.WithTimeout(
		context.Background(),
		controlAuditTimeout,
	)
	defer cancel()
	acknowledged, err := c.runtime.ensureAudited(auditCtx, operation)
	return c.auditStatusAfterEnsure(operation, acknowledged, err)
}

func (c *serviceController) auditStatusAfterEnsure(
	operation serviceOperation,
	acknowledged bool,
	err error,
) serviceAuditStatus {
	localDurable := err == nil
	if !localDurable {
		if last, ok := c.runtime.audit.lastEvent(); ok {
			localDurable = operationMatchesEvent(operation, last)
		}
	}
	return serviceAuditStatus{
		localDurable:   localDurable,
		acknowledged:   acknowledged,
		remoteRejected: errors.Is(err, errControlAuditRemoteRejected),
	}
}

func serviceOperationOutput(
	operation serviceOperation,
	auditStatus serviceAuditStatus,
) executeServiceActionOutput {
	var newActionAllowedAt string
	if operation.Phase.terminal() {
		newActionAllowedAt = time.Unix(
			operation.newOperationAllowedAtUnix(),
			0,
		).UTC().Format(time.RFC3339)
	}
	nextStep := "No further service action is required."
	switch operation.Phase {
	case phaseFailed:
		nextStep = "systemctl was not invoked. Inspect reason_code. After " +
			newActionAllowedAt +
			", prepare and separately approve a fresh action only if it is still intended."
	case phaseOutcomeUnknown:
		nextStep = "The exact systemd result is unknown and will not be retried. Never repeat this action automatically. Inspect independent systemd evidence. After " +
			newActionAllowedAt +
			", call mithril_service_status, then prepare and separately approve a fresh action only if it is still intended."
	case phaseVerifying, phaseDispatched, phaseDispatchStarted:
		nextStep = "Call mithril_verify_service_action with this action_id; do not repeat the action while its outcome is uncertain."
	case phasePrepared:
		nextStep = "systemctl was not invoked. Submit the matching approval bundle or let it expire."
	}
	if !auditStatus.localDurable {
		nextStep = "Restore local audit durability, restarting the MCP server if needed, then call mithril_verify_service_action with this action_id; do not repeat the action."
	} else if !auditStatus.acknowledged {
		if auditStatus.remoteRejected {
			nextStep = "The result is durable locally, but the off-host audit receiver rejected it as non-retryable. Check receiver capacity and its target, key, and client policy; control remains blocked."
		} else {
			nextStep = "The result is durable locally, but off-host audit delivery is pending. Restore receiver connectivity, then call mithril_verify_service_action with this action_id; do not repeat the action."
		}
	}
	return executeServiceActionOutput{
		ActionID:                operation.ID,
		Action:                  string(operation.Action),
		Unit:                    operation.Unit,
		Phase:                   operation.Phase,
		DispatchMayHaveOccurred: operation.DispatchMayHaveOccurred,
		DispatchAccepted:        operation.DispatchAccepted,
		LocalAuditDurable:       auditStatus.localDurable,
		AuditAcknowledged:       auditStatus.acknowledged,
		StatusBefore:            operation.StatusBefore,
		StatusObserved:          operation.StatusAfter,
		ReasonCode:              operation.ReasonCode,
		UpdatedAt: time.Unix(
			operation.UpdatedAtUnix,
			0,
		).UTC().Format(time.RFC3339),
		NewActionAllowedAt: newActionAllowedAt,
		NextStep:           nextStep,
	}
}

func (c *serviceController) verify(
	ctx context.Context,
	actionID string,
) (verifyServiceActionOutput, error) {
	if !approvalIDPattern.MatchString(actionID) {
		return verifyServiceActionOutput{}, errors.New("action_id is invalid")
	}
	runtime, err := c.requireRuntime()
	if err != nil {
		return verifyServiceActionOutput{}, err
	}
	var output verifyServiceActionOutput
	err = runtime.state.withTransaction(ctx, func(transaction *controlStateTransaction) error {
		operation := transaction.operation()
		if operation == nil || operation.ID != actionID {
			return errors.New("service action was not found")
		}
		if err := verifyServiceOperationApproval(*operation, runtime.approvalKeys); err != nil {
			return errors.New("service action approval evidence is invalid")
		}
		acknowledged, auditErr := runtime.ensureAudited(ctx, *operation)
		auditStatus := c.auditStatusAfterEnsure(
			*operation,
			acknowledged,
			auditErr,
		)
		if auditErr != nil &&
			!operation.DispatchMayHaveOccurred &&
			!auditStatus.localDurable {
			return errors.New("operator control audit is not synchronized")
		}
		if !auditStatus.localDurable {
			output = serviceOperationOutput(*operation, auditStatus)
			return nil
		}
		if operation.Phase.terminal() || operation.Phase == phasePrepared {
			output = serviceOperationOutput(*operation, auditStatus)
			return nil
		}
		reconciled, auditStatus, err := c.reconcileServiceOperation(
			ctx,
			transaction,
			*operation,
			auditStatus,
		)
		if err != nil {
			return err
		}
		output = serviceOperationOutput(reconciled, auditStatus)
		return nil
	})
	if err != nil {
		return verifyServiceActionOutput{}, err
	}
	if output.Phase == phaseSucceeded &&
		(output.Action == string(actionStart) ||
			output.Action == string(actionRestart)) &&
		c.readiness != nil {
		applyServiceNodeReadiness(&output, c.readiness(ctx, c.cfg))
	}
	return output, nil
}

func applyServiceNodeReadiness(
	output *executeServiceActionOutput,
	readiness *serviceNodeReadiness,
) {
	output.NodeReadiness = readiness
	if readiness != nil &&
		readiness.Assessed &&
		readiness.EvidenceComplete &&
		readiness.SafeForAutomation &&
		readiness.SlotProgressObserved {
		return
	}
	const guidance = "systemd completed successfully, but Mithril node readiness is not yet proven. Inspect node_readiness and call mithril_diagnose before relying on the node."
	if output.NextStep == "" {
		output.NextStep = guidance
		return
	}
	output.NextStep += " " + guidance
}

func assessServiceNodeReadiness(
	ctx context.Context,
	cfg Config,
) *serviceNodeReadiness {
	diagnosis := runDiagnosisWithHostCollector(
		ctx,
		cfg,
		diagnoseInput{},
		collectHostHealth,
	)
	readiness := &serviceNodeReadiness{
		Assessed:          true,
		DiagnosisStatus:   diagnosis.Status,
		EvidenceComplete:  diagnosis.EvidenceComplete,
		SafeForAutomation: diagnosis.SafeForAutomation,
	}
	if diagnosis.RPCSnapshot == nil || cfg.RPCURL == "" {
		return readiness
	}
	first := diagnosis.RPCSnapshot.AbsoluteSlot
	readiness.FirstSlot = &first
	timer := time.NewTimer(time.Second)
	select {
	case <-ctx.Done():
		if !timer.Stop() {
			<-timer.C
		}
		return readiness
	case <-timer.C:
	}
	client, err := newMithrilRPCClient(cfg.RPCURL)
	if err != nil {
		return readiness
	}
	sampleCtx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	second, err := client.getSlotInfo(sampleCtx)
	if err != nil {
		return readiness
	}
	later := second.AbsoluteSlot
	readiness.LaterSlot = &later
	readiness.SlotProgressObserved = later > first
	return readiness
}

func registerControlTools(
	server *mcpsdk.Server,
	cfg Config,
	authority approvalAuthority,
	runtime *controlRuntime,
) {
	controller := newServiceControllerWithRuntime(cfg, authority, runtime)
	addTool(server, cfg, &mcpsdk.Tool{
		Name:        "mithril_service_status",
		Annotations: annReadOnlyLocal,
		Description: "Read the fixed Mithril systemd unit state. The unit and system/user scope are configured when the MCP process starts.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ serviceStatusInput) (*mcpsdk.CallToolResult, serviceStatusOutput, error) {
		status, err := controller.status(ctx)
		return nil, serviceStatusOutput{
			ObservedAt: controller.now().UTC().Format(time.RFC3339),
			Status:     status,
		}, err
	})

	addTool(server, cfg, &mcpsdk.Tool{
		Name:        "mithril_prepare_service_action",
		Annotations: annControlPrepare,
		// The description names this deployment's actual allowlist. Advertising
		// actions that dispatch will refuse would send a caller into a retry
		// loop against a rule it cannot see.
		Description: "Prepare " + describeAllowedActions(controller.allowedActions) + " for the fixed Mithril service. This does not execute the action; it returns a short-lived challenge for separate operator approval.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in prepareServiceActionInput) (*mcpsdk.CallToolResult, prepareServiceActionOutput, error) {
		out, err := controller.prepare(ctx, in.Action, in.ApproverKeyID)
		return nil, out, err
	})

	addTool(server, cfg, &mcpsdk.Tool{
		Name:        "mithril_execute_service_action",
		Annotations: annControlExecute,
		Description: "Execute one prepared lifecycle action using a matching, unexpired, single-use Ed25519 approval bundle. Returns an exact systemd result or an explicit unknown result that is never retried automatically.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in executeServiceActionInput) (*mcpsdk.CallToolResult, executeServiceActionOutput, error) {
		out, err := controller.execute(ctx, in.ApprovalBundle)
		return nil, out, err
	})

	addTool(server, cfg, &mcpsdk.Tool{
		Name:        "mithril_verify_service_action",
		Annotations: annControlVerify,
		Description: "Continue verification for an in-progress lifecycle action by its action ID. This never repeats the systemd action; a terminal unknown result remains unchanged.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in verifyServiceActionInput) (*mcpsdk.CallToolResult, verifyServiceActionOutput, error) {
		out, err := controller.verify(ctx, in.ActionID)
		return nil, out, err
	})
}
