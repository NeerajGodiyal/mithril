package mcp

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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
	approvalTokenVersion  = 1
	maxApprovalKeyBytes   = 4096
	minApprovalKeyBytes   = 32
	maxApprovalTokenBytes = 4096
	maxPendingApprovals   = 32
	maxSystemctlOutput    = 32 * 1024
	statusTimeout         = 5 * time.Second
	actionTimeout         = 10 * time.Second
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
	ActiveEnterTimestampMonotonic   uint64 `json:"active_enter_timestamp_monotonic"`
	InactiveEnterTimestampMonotonic uint64 `json:"inactive_enter_timestamp_monotonic"`
	Job                             string `json:"job,omitempty"`
}

type serviceStatusInput struct{}

type prepareServiceActionInput struct {
	Action string `json:"action" jsonschema:"lifecycle action to approve: start, stop, or restart"`
}

type prepareServiceActionOutput struct {
	ApprovalRequired bool          `json:"approval_required"`
	Summary          string        `json:"summary"`
	ExpiresAt        string        `json:"expires_at"`
	Challenge        string        `json:"challenge"`
	NextStep         string        `json:"next_step"`
	Status           serviceStatus `json:"status"`
}

type executeServiceActionInput struct {
	ApprovalToken string `json:"approval_token" jsonschema:"short-lived token produced by mithril mcp approve"`
}

type executeServiceActionOutput struct {
	Action       string        `json:"action"`
	Unit         string        `json:"unit"`
	Queued       bool          `json:"queued"`
	StatusBefore serviceStatus `json:"status_before"`
	NextStep     string        `json:"next_step"`
}

type approvalClaims struct {
	Version   int    `json:"v"`
	Purpose   string `json:"purpose"`
	Action    string `json:"action"`
	Unit      string `json:"unit"`
	Scope     string `json:"scope"`
	StateHash string `json:"state_hash"`
	Nonce     string `json:"nonce"`
	IssuedAt  int64  `json:"issued_at"`
	ExpiresAt int64  `json:"expires_at"`
}

// ServiceApprovalSummary contains the non-secret action fields shown by the
// interactive approval CLI.
type ServiceApprovalSummary struct {
	Action    string
	Unit      string
	Scope     string
	ExpiresAt int64
}

type serviceRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type execServiceRunner struct{}

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

func (execServiceRunner) Run(ctx context.Context, path string, args ...string) ([]byte, error) {
	var stdout boundedCommandBuffer
	stdout.max = maxSystemctlOutput
	cmd := exec.CommandContext(ctx, path, args...)
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

type serviceController struct {
	cfg    Config
	key    []byte
	runner serviceRunner
	now    func() time.Time
	random io.Reader

	mu      sync.Mutex
	pending map[string]approvalClaims
}

func newServiceController(cfg Config, approvalKey []byte) *serviceController {
	c := &serviceController{
		cfg:     cfg.normalized(),
		key:     approvalKey,
		runner:  execServiceRunner{},
		now:     time.Now,
		random:  rand.Reader,
		pending: make(map[string]approvalClaims),
	}
	return c
}

func validateAndLoadOperatorConfig(cfg Config) ([]byte, error) {
	profile, err := ParseProfile(string(cfg.Profile))
	if err != nil || profile != ProfileOperator {
		return nil, nil
	}
	if cfg.ApprovalTTLSeconds != 0 && (cfg.ApprovalTTLSeconds < MinApprovalTTLSeconds || cfg.ApprovalTTLSeconds > MaxApprovalTTLSeconds) {
		return nil, fmt.Errorf("operator approval TTL must be between %d and %d seconds", MinApprovalTTLSeconds, MaxApprovalTTLSeconds)
	}
	cfg = cfg.normalized()
	if !cfg.ControlEnabled {
		return nil, errors.New("operator profile requires lifecycle control to be enabled")
	}
	if err := ValidateSystemdServiceUnit(cfg.SystemdUnit); err != nil {
		return nil, errors.New("operator systemd unit must name one .service unit")
	}
	if cfg.SystemdScope != "system" && cfg.SystemdScope != "user" {
		return nil, errors.New("operator systemd scope must be system or user")
	}
	if !filepath.IsAbs(cfg.SystemctlPath) || filepath.Clean(cfg.SystemctlPath) != cfg.SystemctlPath {
		return nil, errors.New("operator systemctl path must be a clean absolute path")
	}
	info, err := os.Stat(cfg.SystemctlPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return nil, errors.New("operator systemctl path must be an executable regular file")
	}
	return readApprovalKey(cfg.ApprovalKeyPath)
}

func readApprovalKey(path string) ([]byte, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("operator approval key path must be a clean absolute path")
	}
	linkInfo, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("MCP approval key file is unavailable")
	}
	if linkInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("MCP approval key file must not be a symlink")
	}
	if !linkInfo.Mode().IsRegular() {
		return nil, errors.New("MCP approval key must be a regular file")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|nonBlockingOpenFlag, 0)
	if err != nil {
		return nil, errors.New("MCP approval key file is unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(linkInfo, info) {
		return nil, errors.New("MCP approval key must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("MCP approval key permissions must deny group and other access")
	}
	key, err := io.ReadAll(io.LimitReader(f, maxApprovalKeyBytes+1))
	if err != nil {
		return nil, errors.New("MCP approval key file is unreadable")
	}
	if len(key) < minApprovalKeyBytes || len(key) > maxApprovalKeyBytes {
		return nil, fmt.Errorf("MCP approval key must contain %d to %d bytes", minApprovalKeyBytes, maxApprovalKeyBytes)
	}
	return key, nil
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

const serviceProperties = "LoadState,ActiveState,SubState,Result,NRestarts,MainPID,ActiveEnterTimestampMonotonic,InactiveEnterTimestampMonotonic,Job"

func (c *serviceController) status(ctx context.Context) (serviceStatus, error) {
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

func normalizeServiceAction(raw string) (string, error) {
	action := strings.ToLower(strings.TrimSpace(raw))
	switch action {
	case "start", "stop", "restart":
		return action, nil
	default:
		return "", errors.New("action must be start, stop, or restart")
	}
}

func validateActionState(action string, status serviceStatus) error {
	if status.LoadState != "loaded" {
		return fmt.Errorf("service unit is not loaded (load_state=%s)", status.LoadState)
	}
	active := status.ActiveState == "active" || status.ActiveState == "activating" || status.ActiveState == "reloading"
	switch action {
	case "start":
		if active {
			return errors.New("service is already active or starting")
		}
	case "stop":
		if !active {
			return errors.New("service is not active")
		}
	case "restart":
		if !active {
			return errors.New("service must be active before restart")
		}
	}
	return nil
}

func serviceStateHash(status serviceStatus) string {
	data, _ := json.Marshal(status)
	sum := sha256.Sum256(data)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func encodeApproval(claims approvalClaims, key []byte) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func decodeApproval(token string, key []byte) (approvalClaims, error) {
	if len(token) == 0 || len(token) > maxApprovalTokenBytes {
		return approvalClaims{}, errors.New("approval token is missing or too large")
	}
	payloadPart, signaturePart, ok := strings.Cut(token, ".")
	if !ok || strings.Contains(signaturePart, ".") {
		return approvalClaims{}, errors.New("approval token is malformed")
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadPart)
	if err != nil || len(payload) > maxApprovalTokenBytes {
		return approvalClaims{}, errors.New("approval token is malformed")
	}
	signature, err := base64.RawURLEncoding.DecodeString(signaturePart)
	if err != nil {
		return approvalClaims{}, errors.New("approval token is malformed")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return approvalClaims{}, errors.New("approval token signature is invalid")
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	var claims approvalClaims
	if err := decoder.Decode(&claims); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return approvalClaims{}, errors.New("approval token payload is invalid")
	}
	if claims.Version != approvalTokenVersion {
		return approvalClaims{}, errors.New("approval token version is unsupported")
	}
	return claims, nil
}

func validateApprovalTime(claims approvalClaims, now time.Time) error {
	nowUnix := now.Unix()
	latestIssuedAt := nowUnix
	if latestIssuedAt <= math.MaxInt64-5 {
		latestIssuedAt += 5
	} else {
		latestIssuedAt = math.MaxInt64
	}
	maxTTL := int64(MaxApprovalTTLSeconds)
	if claims.IssuedAt > latestIssuedAt || claims.ExpiresAt <= nowUnix || claims.ExpiresAt <= claims.IssuedAt ||
		claims.IssuedAt > math.MaxInt64-maxTTL || claims.ExpiresAt > claims.IssuedAt+maxTTL {
		return errors.New("approval token is expired or has an invalid lifetime")
	}
	return nil
}

func (c *serviceController) prepare(ctx context.Context, action string) (prepareServiceActionOutput, error) {
	if len(c.key) < minApprovalKeyBytes {
		return prepareServiceActionOutput{}, errors.New("operator approval key is unavailable")
	}
	action, err := normalizeServiceAction(action)
	if err != nil {
		return prepareServiceActionOutput{}, err
	}
	status, err := c.status(ctx)
	if err != nil {
		return prepareServiceActionOutput{}, err
	}
	if err := validateActionState(action, status); err != nil {
		return prepareServiceActionOutput{}, err
	}
	nonceBytes := make([]byte, 18)
	if _, err := io.ReadFull(c.random, nonceBytes); err != nil {
		return prepareServiceActionOutput{}, errors.New("failed to create approval challenge")
	}
	now := c.now().UTC()
	claims := approvalClaims{
		Version:   approvalTokenVersion,
		Purpose:   "prepare",
		Action:    action,
		Unit:      c.cfg.SystemdUnit,
		Scope:     c.cfg.SystemdScope,
		StateHash: serviceStateHash(status),
		Nonce:     base64.RawURLEncoding.EncodeToString(nonceBytes),
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(time.Duration(c.cfg.ApprovalTTLSeconds) * time.Second).Unix(),
	}
	challenge, err := encodeApproval(claims, c.key)
	if err != nil {
		return prepareServiceActionOutput{}, errors.New("failed to encode approval challenge")
	}
	c.mu.Lock()
	for nonce, pending := range c.pending {
		if pending.ExpiresAt <= now.Unix() {
			delete(c.pending, nonce)
		}
	}
	if len(c.pending) >= maxPendingApprovals {
		c.mu.Unlock()
		return prepareServiceActionOutput{}, errors.New("too many pending approvals; wait for one to expire")
	}
	c.pending[claims.Nonce] = claims
	c.mu.Unlock()

	return prepareServiceActionOutput{
		ApprovalRequired: true,
		Summary:          fmt.Sprintf("%s %s (%s scope)", action, c.cfg.SystemdUnit, c.cfg.SystemdScope),
		ExpiresAt:        time.Unix(claims.ExpiresAt, 0).UTC().Format(time.RFC3339),
		Challenge:        challenge,
		NextStep:         "Approve this challenge interactively on the MCP server host, then return the token to this same MCP session before it expires.",
		Status:           status,
	}, nil
}

func sameApproval(a, b approvalClaims) bool {
	return a.Version == b.Version && a.Action == b.Action && a.Unit == b.Unit && a.Scope == b.Scope &&
		a.StateHash == b.StateHash && a.Nonce == b.Nonce && a.IssuedAt == b.IssuedAt && a.ExpiresAt == b.ExpiresAt
}

func (c *serviceController) execute(ctx context.Context, token string) (executeServiceActionOutput, error) {
	if len(c.key) < minApprovalKeyBytes {
		return executeServiceActionOutput{}, errors.New("operator approval key is unavailable")
	}
	approved, err := decodeApproval(token, c.key)
	if err != nil {
		return executeServiceActionOutput{}, err
	}
	if approved.Purpose != "approved" {
		return executeServiceActionOutput{}, errors.New("approval token was not confirmed by an operator")
	}
	if err := validateApprovalTime(approved, c.now()); err != nil {
		return executeServiceActionOutput{}, err
	}
	if approved.Unit != c.cfg.SystemdUnit || approved.Scope != c.cfg.SystemdScope {
		return executeServiceActionOutput{}, errors.New("approval token does not match this service")
	}

	c.mu.Lock()
	pending, ok := c.pending[approved.Nonce]
	if ok {
		delete(c.pending, approved.Nonce)
	}
	c.mu.Unlock()
	if !ok || !sameApproval(pending, approved) {
		return executeServiceActionOutput{}, errors.New("approval token is unknown, already used, or no longer pending")
	}

	status, err := c.status(ctx)
	if err != nil {
		return executeServiceActionOutput{}, err
	}
	if serviceStateHash(status) != approved.StateHash {
		return executeServiceActionOutput{}, errors.New("service state changed after approval was prepared; prepare a new action")
	}
	if err := validateActionState(approved.Action, status); err != nil {
		return executeServiceActionOutput{}, err
	}
	if err := validateApprovalTime(approved, c.now()); err != nil {
		return executeServiceActionOutput{}, err
	}
	actionCtx, cancel := context.WithTimeout(ctx, actionTimeout)
	defer cancel()
	args := c.systemctlArgs("--no-block", approved.Action, c.cfg.SystemdUnit)
	if _, err := c.runner.Run(actionCtx, c.cfg.SystemctlPath, args...); err != nil {
		if ctxErr := actionCtx.Err(); ctxErr != nil {
			return executeServiceActionOutput{}, ctxErr
		}
		return executeServiceActionOutput{}, errors.New("systemctl action failed")
	}
	return executeServiceActionOutput{
		Action:       approved.Action,
		Unit:         approved.Unit,
		Queued:       true,
		StatusBefore: status,
		NextStep:     "Call mithril_service_status until the requested state is reached.",
	}, nil
}

// ApproveServiceChallenge verifies a prepared challenge and signs the exact
// action as operator-approved. Interactive confirmation belongs in the CLI.
func ApproveServiceChallenge(challenge string, key []byte, now time.Time) (ServiceApprovalSummary, string, error) {
	claims, err := decodeApproval(challenge, key)
	if err != nil {
		return ServiceApprovalSummary{}, "", err
	}
	if claims.Purpose != "prepare" {
		return ServiceApprovalSummary{}, "", errors.New("challenge purpose is invalid")
	}
	if _, err := normalizeServiceAction(claims.Action); err != nil {
		return ServiceApprovalSummary{}, "", err
	}
	if ValidateSystemdServiceUnit(claims.Unit) != nil || (claims.Scope != "system" && claims.Scope != "user") || claims.Nonce == "" || claims.StateHash == "" {
		return ServiceApprovalSummary{}, "", errors.New("challenge payload is invalid")
	}
	if err := validateApprovalTime(claims, now); err != nil {
		return ServiceApprovalSummary{}, "", err
	}
	claims.Purpose = "approved"
	token, err := encodeApproval(claims, key)
	if err != nil {
		return ServiceApprovalSummary{}, "", err
	}
	return ServiceApprovalSummary{
		Action:    claims.Action,
		Unit:      claims.Unit,
		Scope:     claims.Scope,
		ExpiresAt: claims.ExpiresAt,
	}, token, nil
}

// LoadApprovalKey is used by the interactive CLI without exposing key bytes
// through MCP output.
func LoadApprovalKey(path string) ([]byte, error) { return readApprovalKey(path) }

func registerControlTools(server *mcpsdk.Server, cfg Config, approvalKey []byte) {
	controller := newServiceController(cfg, approvalKey)
	addTool(server, cfg, &mcpsdk.Tool{
		Name:        "mithril_service_status",
		Annotations: annReadOnlyLocal,
		Description: "Read the fixed Mithril systemd unit state. The unit and system/user scope are configured when the MCP process starts.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ serviceStatusInput) (*mcpsdk.CallToolResult, serviceStatus, error) {
		status, err := controller.status(ctx)
		return nil, status, err
	})

	addTool(server, cfg, &mcpsdk.Tool{
		Name:        "mithril_prepare_service_action",
		Annotations: annControlPrepare,
		Description: "Prepare start, stop, or restart for the fixed Mithril service. This does not execute the action; it returns a short-lived challenge for separate operator approval.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in prepareServiceActionInput) (*mcpsdk.CallToolResult, prepareServiceActionOutput, error) {
		out, err := controller.prepare(ctx, in.Action)
		return nil, out, err
	})

	addTool(server, cfg, &mcpsdk.Tool{
		Name:        "mithril_execute_service_action",
		Annotations: annControlExecute,
		Description: "Execute one prepared lifecycle action using a matching, unexpired, single-use operator approval token. The action is queued without blocking; check service status afterward.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in executeServiceActionInput) (*mcpsdk.CallToolResult, executeServiceActionOutput, error) {
		out, err := controller.execute(ctx, in.ApprovalToken)
		return nil, out, err
	})
}
