package mcp

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const activeServiceStatus = `LoadState=loaded
ActiveState=active
SubState=running
Result=success
NRestarts=2
MainPID=1234
ActiveEnterTimestampMonotonic=100
InactiveEnterTimestampMonotonic=50
Job=
`

type fakeServiceRunner struct {
	status []byte
	err    error
	calls  [][]string
}

func (f *fakeServiceRunner) Run(_ context.Context, path string, args ...string) ([]byte, error) {
	call := append([]string{path}, args...)
	f.calls = append(f.calls, call)
	for _, arg := range args {
		if arg == "show" {
			return f.status, f.err
		}
	}
	return nil, f.err
}

func testServiceController(runner serviceRunner, now time.Time) *serviceController {
	return &serviceController{
		cfg: Config{
			Profile:            ProfileOperator,
			SystemdUnit:        "mithril.service",
			SystemdScope:       "system",
			SystemctlPath:      "/usr/bin/systemctl",
			ApprovalTTLSeconds: 60,
		}.normalized(),
		key:     []byte(strings.Repeat("k", minApprovalKeyBytes)),
		runner:  runner,
		now:     func() time.Time { return now },
		random:  strings.NewReader(strings.Repeat("n", 64)),
		pending: make(map[string]approvalClaims),
	}
}

func TestServiceApprovalLifecycle(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	runner := &fakeServiceRunner{status: []byte(activeServiceStatus)}
	controller := testServiceController(runner, now)

	prepared, err := controller.prepare(context.Background(), "restart")
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.ApprovalRequired || prepared.Challenge == "" || prepared.Status.MainPID != 1234 ||
		!strings.Contains(prepared.NextStep, "server host") || !strings.Contains(prepared.NextStep, "same MCP session") {
		t.Fatalf("prepared action = %+v", prepared)
	}
	if _, err := controller.execute(context.Background(), prepared.Challenge); err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("unapproved challenge execution error = %v", err)
	}
	claims, token, err := ApproveServiceChallenge(prepared.Challenge, controller.key, now)
	if err != nil || claims.Action != "restart" || token == "" {
		t.Fatalf("approve = claims %+v token=%t err=%v", claims, token != "", err)
	}
	executed, err := controller.execute(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if !executed.Queued || executed.Action != "restart" {
		t.Fatalf("execute = %+v", executed)
	}
	wantAction := []string{"/usr/bin/systemctl", "--no-ask-password", "--no-block", "restart", "mithril.service"}
	if got := runner.calls[len(runner.calls)-1]; !reflect.DeepEqual(got, wantAction) {
		t.Fatalf("systemctl action = %v, want %v", got, wantAction)
	}
	for _, call := range runner.calls {
		if !containsString(call, "--no-ask-password") {
			t.Fatalf("systemctl call may prompt for authorization: %v", call)
		}
	}
	if _, err := controller.execute(context.Background(), token); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("replayed token error = %v", err)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestServiceApprovalBindsStateAndExpiry(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	runner := &fakeServiceRunner{status: []byte(activeServiceStatus)}
	controller := testServiceController(runner, now)
	prepared, err := controller.prepare(context.Background(), "stop")
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := ApproveServiceChallenge(prepared.Challenge, controller.key, now)
	if err != nil {
		t.Fatal(err)
	}
	runner.status = []byte(strings.Replace(activeServiceStatus, "MainPID=1234", "MainPID=5678", 1))
	if _, err := controller.execute(context.Background(), token); err == nil || !strings.Contains(err.Error(), "state changed") {
		t.Fatalf("changed-state error = %v", err)
	}

	prepared, err = controller.prepare(context.Background(), "stop")
	if err != nil {
		t.Fatal(err)
	}
	controller.now = func() time.Time { return now.Add(2 * time.Minute) }
	if _, _, err := ApproveServiceChallenge(prepared.Challenge, controller.key, controller.now()); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired challenge error = %v", err)
	}
}

func TestServiceApprovalExpiresBeforeAction(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	runner := &fakeServiceRunner{status: []byte(activeServiceStatus)}
	controller := testServiceController(runner, now)
	prepared, err := controller.prepare(context.Background(), "restart")
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := ApproveServiceChallenge(prepared.Challenge, controller.key, now)
	if err != nil {
		t.Fatal(err)
	}
	nowCalls := 0
	controller.now = func() time.Time {
		nowCalls++
		if nowCalls == 1 {
			return now
		}
		return now.Add(2 * time.Minute)
	}
	if _, err := controller.execute(context.Background(), token); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired-before-action error = %v", err)
	}
	for _, call := range runner.calls {
		for _, arg := range call {
			if arg == "restart" {
				t.Fatalf("expired approval executed systemctl: %v", call)
			}
		}
	}
}

func TestValidateApprovalTimeRejectsOverflowedLifetime(t *testing.T) {
	claims := approvalClaims{IssuedAt: math.MinInt64, ExpiresAt: math.MaxInt64}
	if err := validateApprovalTime(claims, time.Unix(0, 0)); err == nil {
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
	controller := testServiceController(&fakeServiceRunner{status: []byte(activeServiceStatus)}, time.Now())
	for _, action := range []string{"", "kill", "restart; reboot"} {
		if _, err := controller.prepare(context.Background(), action); err == nil {
			t.Fatalf("unsafe action %q accepted", action)
		}
	}
	if _, _, err := ApproveServiceChallenge("not-a-token", controller.key, time.Now()); err == nil {
		t.Fatal("malformed challenge accepted")
	}
}

func TestValidateOperatorConfigAndApprovalKey(t *testing.T) {
	dir := t.TempDir()
	systemctl := filepath.Join(dir, "systemctl")
	keyPath := filepath.Join(dir, "approval.key")
	if err := os.WriteFile(systemctl, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte(strings.Repeat("x", minApprovalKeyBytes)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Profile:            ProfileOperator,
		ControlEnabled:     true,
		SystemdUnit:        "mithril.service",
		SystemdScope:       "system",
		SystemctlPath:      systemctl,
		ApprovalKeyPath:    keyPath,
		ApprovalTTLSeconds: 60,
	}
	key, err := validateAndLoadOperatorConfig(cfg)
	if err != nil {
		t.Fatalf("valid operator config: %v", err)
	}
	clear(key)
	zeroTTL := cfg
	zeroTTL.ApprovalTTLSeconds = 0
	key, err = validateAndLoadOperatorConfig(zeroTTL)
	if err != nil {
		t.Fatalf("unset operator TTL should use the default: %v", err)
	}
	clear(key)
	directoryKey := cfg
	directoryKey.ApprovalKeyPath = dir
	if _, err := validateAndLoadOperatorConfig(directoryKey); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory approval key error = %v", err)
	}
	for _, ttl := range []uint64{MinApprovalTTLSeconds - 1, MaxApprovalTTLSeconds + 1} {
		invalidTTL := cfg
		invalidTTL.ApprovalTTLSeconds = ttl
		if _, err := validateAndLoadOperatorConfig(invalidTTL); err == nil || !strings.Contains(err.Error(), "approval TTL") {
			t.Errorf("invalid operator TTL %d error = %v", ttl, err)
		}
	}
	invalidUnit := cfg
	invalidUnit.SystemdUnit = "mithril.target"
	if _, err := validateAndLoadOperatorConfig(invalidUnit); err == nil || !strings.Contains(err.Error(), "systemd unit") {
		t.Fatalf("invalid systemd unit error = %v", err)
	}
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := validateAndLoadOperatorConfig(cfg); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("unsafe key permissions error = %v", err)
	}
	cfg.ControlEnabled = false
	if _, err := validateAndLoadOperatorConfig(cfg); err == nil || !strings.Contains(err.Error(), "control") {
		t.Fatalf("disabled operator config error = %v", err)
	}
	if _, err := validateAndLoadOperatorConfig(Config{Profile: ProfileMonitor}); err != nil {
		t.Fatalf("monitor profile should ignore controls: %v", err)
	}
}

func TestServiceRunnerErrorsAreFixed(t *testing.T) {
	runner := &fakeServiceRunner{err: errors.New("secret stderr")}
	controller := testServiceController(runner, time.Now())
	_, err := controller.status(context.Background())
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("runner error = %v", err)
	}
}
