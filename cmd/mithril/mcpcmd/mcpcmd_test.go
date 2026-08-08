package mcpcmd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"github.com/spf13/viper"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/config"
	"github.com/Overclock-Validator/mithril/pkg/mcp"
	"github.com/spf13/cobra"
)

func TestRestoreControlCommandUsesVerifiedRestoreAPI(t *testing.T) {
	previousConfig := restoreControl
	previousRestore := restoreControlState
	t.Cleanup(func() {
		restoreControl = previousConfig
		restoreControlState = previousRestore
		restoreControlCmd.SetOut(nil)
	})

	restoreControl = mcp.ControlRestoreConfig{
		ControlStateDir:       "/private/control",
		ApproverKeysDir:       "/private/approvers",
		AuditClientConfigPath: "/private/audit-client.json",
		TargetID:              "node-mainnet-1",
		SystemdUnit:           "mithril.service",
		SystemdScope:          "system",
	}
	calls := 0
	restoreControlState = func(
		_ context.Context,
		config mcp.ControlRestoreConfig,
	) (mcp.ControlRestoreResult, error) {
		calls++
		if config != restoreControl {
			t.Fatalf("restore config = %+v", config)
		}
		return mcp.ControlRestoreResult{
			Records:       5,
			TipHash:       strings.Repeat("a", 64),
			ActionID:      "action-1",
			Phase:         "outcome_unknown",
			StateRestored: true,
		}, nil
	}
	var output bytes.Buffer
	restoreControlCmd.SetOut(&output)
	if err := restoreControlCmd.RunE(&restoreControlCmd, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("restore calls = %d", calls)
	}
	var result mcp.ControlRestoreResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Records != 5 ||
		result.ActionID != "action-1" ||
		result.Phase != "outcome_unknown" ||
		!result.StateRestored {
		t.Fatalf("restore output = %+v", result)
	}

	restoreControl.SystemdUnit = ""
	if err := restoreControlCmd.RunE(&restoreControlCmd, nil); err == nil {
		t.Fatal("incomplete restore configuration was accepted")
	}
	if calls != 1 {
		t.Fatal("incomplete configuration reached the restore API")
	}
}

func resolvedConfig() (mcp.Config, error) {
	return resolvedConfigWithOverrides(resolvedConfigOverrides{})
}

func clearMCPNodeSettingEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"MITHRIL_ACCOUNTS_PATH", "MITHRIL_SNAPSHOTS_PATH", "MITHRIL_SHREDSTORE_PATH",
		"MITHRIL_LOG_DIR", "MITHRIL_STATE_PATH", "MITHRIL_REPLAY_PATH", "MITHRIL_NODE_CGROUP_PATH",
		"MITHRIL_METRICS_URL", "MITHRIL_RPC_URL", "MITHRIL_PPROF_URL", "MITHRIL_BLOCK_SOURCE",
		"MITHRIL_MCP_APPROVAL_TTL_SECONDS",
	} {
		value, present := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if present {
				_ = os.Setenv(name, value)
			} else {
				_ = os.Unsetenv(name)
			}
		})
	}
}

func setConfigFileForTest(t *testing.T, path string) {
	t.Helper()
	original := config.ConfigFile
	t.Cleanup(func() { config.ConfigFile = original })
	config.ConfigFile = path
}

func setRemoteConfigFlagsForTest(t *testing.T, target, binary, remoteConfig string) {
	t.Helper()
	originalTarget := configSSHTarget
	originalBinary := configRemoteBinary
	originalConfig := configRemoteConfig
	t.Cleanup(func() {
		configSSHTarget = originalTarget
		configRemoteBinary = originalBinary
		configRemoteConfig = originalConfig
	})
	configSSHTarget = target
	configRemoteBinary = binary
	configRemoteConfig = remoteConfig
}

func writeConfigFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "node.toml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func approvalTestDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("trusted approval key ownership is unavailable on Windows")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestApproveCommandRequiresExactInteractiveConfirmation(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	summary := mcp.ServiceApprovalSummary{
		Action:        "restart",
		Unit:          "mithril.service",
		Scope:         "system",
		TargetID:      "node-mainnet-1",
		ActionID:      "action-123",
		ApproverKeyID: "key-123",
		ExpiresAt:     now.Add(time.Minute).Unix(),
		Status: mcp.ServiceApprovalStatus{
			LoadState:   "loaded",
			ActiveState: "active",
			SubState:    "running",
			NRestarts:   2,
			MainPID:     1234,
		},
		Consequence: "Restarts the fixed Mithril service.",
	}
	wantBundle := mcp.ServiceApprovalBundle{
		AuthorizationToken: "approved-token",
		AuditAttestation:   "audit-attestation",
	}
	inspect := func(challenge string, gotNow time.Time) (mcp.ServiceApprovalSummary, error) {
		if challenge != "prepared-challenge" || !gotNow.Equal(now) {
			t.Fatalf("inspector inputs = %q, %s", challenge, gotNow)
		}
		return summary, nil
	}
	signCalls := 0
	sign := func(challenge, keyPath string, gotNow time.Time) (mcp.ServiceApprovalBundle, error) {
		signCalls++
		if challenge != "prepared-challenge" || keyPath != "/secure/approval.key" || !gotNow.Equal(now.Add(time.Second)) {
			t.Fatalf("signer inputs = %q, %q, %s", challenge, keyPath, gotNow)
		}
		return wantBundle, nil
	}
	clockCalls := 0
	clock := func() time.Time {
		clockCalls++
		return now.Add(time.Duration(clockCalls-1) * time.Second)
	}

	var stdout, stderr bytes.Buffer
	err := runApproveCommand(
		strings.NewReader("APPROVE\n"),
		&stdout,
		&stderr,
		"prepared-challenge",
		"/secure/approval.key",
		true,
		clock,
		inspect,
		sign,
	)
	if err != nil {
		t.Fatal(err)
	}
	if signCalls != 1 {
		t.Fatalf("signer calls = %d, want 1", signCalls)
	}
	var gotBundle mcp.ServiceApprovalBundle
	if err := json.Unmarshal(stdout.Bytes(), &gotBundle); err != nil || gotBundle != wantBundle {
		t.Fatalf("approval stdout = %q, decoded %+v, error %v", stdout.String(), gotBundle, err)
	}
	prompt := stderr.String()
	for _, field := range []string{
		"Target: node-mainnet-1",
		"Action ID: action-123",
		"Action: restart",
		"Unit: mithril.service",
		"Scope: system",
		"Current state: loaded/active/running (PID 1234, restarts 2)",
		"Consequence: Restarts the fixed Mithril service.",
		"Approver key ID: key-123",
		"Expires: 2026-07-20T12:01:00Z",
		"Type APPROVE",
	} {
		if !strings.Contains(prompt, field) {
			t.Errorf("approval prompt is missing %q: %q", field, prompt)
		}
	}
	for _, secret := range []string{wantBundle.AuthorizationToken, wantBundle.AuditAttestation, "prepared-challenge", "/secure/approval.key"} {
		if strings.Contains(prompt, secret) {
			t.Errorf("approval prompt exposed %q: %q", secret, prompt)
		}
	}
}

func TestApproveCommandReadsChallengeFromTerminalWhenArgumentIsOmitted(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	summary := mcp.ServiceApprovalSummary{
		Action: "start", Unit: "mithril.service", Scope: "system",
		TargetID: "node-1", ActionID: "action-1", ApproverKeyID: "key-1",
		ExpiresAt: now.Add(time.Minute).Unix(),
		Status: mcp.ServiceApprovalStatus{
			LoadState: "loaded", ActiveState: "inactive", SubState: "dead",
		},
		Consequence: "Starts the fixed Mithril service.",
	}
	inspectedChallenge := ""
	inspect := func(challenge string, gotNow time.Time) (mcp.ServiceApprovalSummary, error) {
		inspectedChallenge = challenge
		return summary, nil
	}
	signedChallenge := ""
	sign := func(challenge, keyPath string, gotNow time.Time) (mcp.ServiceApprovalBundle, error) {
		signedChallenge = challenge
		return mcp.ServiceApprovalBundle{AuthorizationToken: "token", AuditAttestation: "audit"}, nil
	}

	var stdout, stderr bytes.Buffer
	err := runApproveCommand(
		strings.NewReader("pasted-challenge\nAPPROVE\n"),
		&stdout,
		&stderr,
		"",
		"/secure/approval.key",
		true,
		func() time.Time { return now },
		inspect,
		sign,
	)
	if err != nil {
		t.Fatal(err)
	}
	if inspectedChallenge != "pasted-challenge" || signedChallenge != "pasted-challenge" {
		t.Fatalf("challenge inspector/signer inputs = %q/%q", inspectedChallenge, signedChallenge)
	}
	if !strings.Contains(stderr.String(), "Paste the prepared service-action challenge") {
		t.Fatalf("zero-argument prompt = %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "pasted-challenge") {
		t.Fatalf("challenge was copied into program output: %q", stderr.String())
	}
	var bundle mcp.ServiceApprovalBundle
	if err := json.Unmarshal(stdout.Bytes(), &bundle); err != nil ||
		bundle.AuthorizationToken != "token" || bundle.AuditAttestation != "audit" {
		t.Fatalf("approval stdout = %q, bundle=%+v, error=%v", stdout.String(), bundle, err)
	}
}

type approvalTestTerminal struct {
	*strings.Reader
	fd uintptr
}

func (terminal *approvalTestTerminal) Fd() uintptr {
	return terminal.fd
}

func TestApproveCommandDisablesEchoForTerminalChallenge(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	summary := mcp.ServiceApprovalSummary{
		Action: "stop", Unit: "mithril.service", Scope: "system",
		TargetID: "node-1", ActionID: "action-1", ApproverKeyID: "key-1",
		ExpiresAt:   now.Add(time.Minute).Unix(),
		Status:      mcp.ServiceApprovalStatus{LoadState: "loaded", ActiveState: "active", SubState: "running"},
		Consequence: "Stops the fixed Mithril service.",
	}
	originalRead := readApprovalChallengeWithoutEcho
	readCalls := 0
	readApprovalChallengeWithoutEcho = func(fd int) ([]byte, error) {
		readCalls++
		if fd != 42 {
			t.Fatalf("no-echo reader fd = %d, want 42", fd)
		}
		return []byte("hidden-challenge"), nil
	}
	t.Cleanup(func() { readApprovalChallengeWithoutEcho = originalRead })

	var stdout, stderr bytes.Buffer
	terminal := &approvalTestTerminal{Reader: strings.NewReader("APPROVE\n"), fd: 42}
	err := runApproveCommand(
		terminal,
		&stdout,
		&stderr,
		"",
		"/secure/approval.key",
		true,
		func() time.Time { return now },
		func(challenge string, _ time.Time) (mcp.ServiceApprovalSummary, error) {
			if challenge != "hidden-challenge" {
				t.Fatalf("inspected challenge = %q", challenge)
			}
			return summary, nil
		},
		func(challenge, _ string, _ time.Time) (mcp.ServiceApprovalBundle, error) {
			if challenge != "hidden-challenge" {
				t.Fatalf("signed challenge = %q", challenge)
			}
			return mcp.ServiceApprovalBundle{AuthorizationToken: "token", AuditAttestation: "audit"}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if readCalls != 1 {
		t.Fatalf("no-echo challenge reads = %d, want 1", readCalls)
	}
	if strings.Contains(stderr.String(), "hidden-challenge") {
		t.Fatalf("terminal challenge was copied into output: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "press Enter: \nMithril service action approval") {
		t.Fatalf("no-echo prompt did not restore the terminal line: %q", stderr.String())
	}
}

func TestApproveCommandBoundsPastedChallenge(t *testing.T) {
	inspectCalls := 0
	inspect := func(string, time.Time) (mcp.ServiceApprovalSummary, error) {
		inspectCalls++
		return mcp.ServiceApprovalSummary{}, nil
	}
	var stdout, stderr bytes.Buffer
	err := runApproveCommand(
		strings.NewReader(strings.Repeat("A", maxApprovalChallengeInputBytes+1)+"\nAPPROVE\n"),
		&stdout,
		&stderr,
		"",
		"/secure/approval.key",
		true,
		time.Now,
		inspect,
		func(string, string, time.Time) (mcp.ServiceApprovalBundle, error) {
			return mcp.ServiceApprovalBundle{}, nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "bounded") {
		t.Fatalf("oversized pasted challenge error = %v", err)
	}
	if inspectCalls != 0 || stdout.Len() != 0 {
		t.Fatalf("oversized challenge reached approval logic: inspect=%d stdout=%q", inspectCalls, stdout.String())
	}

	originalRead := readApprovalChallengeWithoutEcho
	readApprovalChallengeWithoutEcho = func(int) ([]byte, error) {
		return bytes.Repeat([]byte{'A'}, maxApprovalChallengeInputBytes+1), nil
	}
	t.Cleanup(func() { readApprovalChallengeWithoutEcho = originalRead })
	stdout.Reset()
	stderr.Reset()
	err = runApproveCommand(
		&approvalTestTerminal{Reader: strings.NewReader("APPROVE\n"), fd: 42},
		&stdout,
		&stderr,
		"",
		"/secure/approval.key",
		true,
		time.Now,
		inspect,
		func(string, string, time.Time) (mcp.ServiceApprovalBundle, error) {
			return mcp.ServiceApprovalBundle{}, nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "bounded") || inspectCalls != 0 || stdout.Len() != 0 {
		t.Fatalf("oversized no-echo challenge error=%v inspect=%d stdout=%q", err, inspectCalls, stdout.String())
	}
}

func TestApproveCommandRejectsNonInteractiveOrInexactInput(t *testing.T) {
	summary := mcp.ServiceApprovalSummary{
		Action:        "stop",
		Unit:          "mithril.service",
		Scope:         "user",
		TargetID:      "node-1",
		ActionID:      "action-1",
		ApproverKeyID: "key-1",
		ExpiresAt:     time.Date(2026, 7, 20, 12, 1, 0, 0, time.UTC).Unix(),
		Status:        mcp.ServiceApprovalStatus{LoadState: "loaded", ActiveState: "active", SubState: "running"},
		Consequence:   "Stops the fixed Mithril service.",
	}
	inspectCalls := 0
	inspect := func(string, time.Time) (mcp.ServiceApprovalSummary, error) {
		inspectCalls++
		return summary, nil
	}
	signCalls := 0
	sign := func(string, string, time.Time) (mcp.ServiceApprovalBundle, error) {
		signCalls++
		return mcp.ServiceApprovalBundle{
			AuthorizationToken: "must-not-be-printed",
			AuditAttestation:   "must-not-be-printed",
		}, nil
	}

	var stdout, stderr bytes.Buffer
	err := runApproveCommand(
		strings.NewReader("APPROVE\n"), &stdout, &stderr,
		"challenge", "/secure/key", false, time.Now, inspect, sign,
	)
	if err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("non-interactive error = %v", err)
	}
	if inspectCalls != 0 || signCalls != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("non-interactive invocation reached approval logic: inspect=%d sign=%d stdout=%q stderr=%q",
			inspectCalls, signCalls, stdout.String(), stderr.String())
	}

	for _, input := range []string{"", "approve\n", "APPROVE \n", "YES\n", strings.Repeat("A", maxApprovalConfirmationBytes+1)} {
		stdout.Reset()
		stderr.Reset()
		beforeSign := signCalls
		err := runApproveCommand(
			strings.NewReader(input), &stdout, &stderr,
			"challenge", "/secure/key", true, time.Now, inspect, sign,
		)
		if err == nil || !strings.Contains(err.Error(), "not approved") {
			t.Errorf("confirmation %q error = %v", input, err)
		}
		if stdout.Len() != 0 {
			t.Errorf("confirmation %q wrote stdout %q", input, stdout.String())
		}
		if signCalls != beforeSign {
			t.Errorf("confirmation %q called the signer", input)
		}
		if strings.Contains(stderr.String(), "must-not-be-printed") {
			t.Errorf("confirmation %q exposed signed material on stderr", input)
		}
	}
}

func TestResolveApprovalKeyPath(t *testing.T) {
	t.Setenv("MITHRIL_MCP_APPROVER_PRIVATE_KEY_FILE", "/environment/approval.key")
	if got, err := resolveApprovalKeyPath(""); err != nil || got != "/environment/approval.key" {
		t.Fatalf("environment key path = %q, %v", got, err)
	}
	if got, err := resolveApprovalKeyPath("/flag/approval.key"); err != nil || got != "/flag/approval.key" {
		t.Fatalf("flag key path = %q, %v", got, err)
	}
	if _, err := resolveApprovalKeyPath("relative/key"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative flag key error = %v", err)
	}
	if _, err := resolveApprovalKeyPath("/secure/../approval.key"); err == nil || !strings.Contains(err.Error(), "clean absolute") {
		t.Fatalf("unclean flag key error = %v", err)
	}
	t.Setenv("MITHRIL_MCP_APPROVER_PRIVATE_KEY_FILE", "relative/key")
	if _, err := resolveApprovalKeyPath(""); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative environment key error = %v", err)
	}
	t.Setenv("MITHRIL_MCP_APPROVER_PRIVATE_KEY_FILE", "")
	if _, err := resolveApprovalKeyPath(""); err == nil {
		t.Fatal("missing approval key path was accepted")
	}
}

func TestApproveCommandHasNoNonInteractiveBypass(t *testing.T) {
	if approveCmd.Flags().Lookup("approval-key-file") == nil {
		t.Fatal("approve command is missing --approval-key-file")
	}
	if approveCmd.Flags().Lookup("yes") != nil {
		t.Fatal("approve command exposes a non-interactive --yes bypass")
	}
	if err := approveCmd.Args(&approveCmd, nil); err != nil {
		t.Fatalf("approve command rejected its preferred zero-argument flow: %v", err)
	}
	if err := approveCmd.Args(&approveCmd, []string{"one", "two"}); err == nil {
		t.Fatal("approve command accepted multiple challenges")
	}
}

func TestCreateApprovalKeyPairIsRandomPrivateAndExclusive(t *testing.T) {
	dir := approvalTestDir(t)
	privatePath := filepath.Join(dir, "approval.seed")
	publicPath := filepath.Join(dir, "approval.pub")
	want := bytes.Repeat([]byte{0x5a}, 32)
	keyID, err := createApprovalKeyPair(privatePath, publicPath, bytes.NewReader(want))
	if err != nil {
		t.Fatal(err)
	}
	gotPrivate, err := os.ReadFile(privatePath)
	if err != nil {
		t.Fatal(err)
	}
	gotPublic, err := os.ReadFile(publicPath)
	if err != nil {
		t.Fatal(err)
	}
	privateInfo, _ := os.Stat(privatePath)
	publicInfo, _ := os.Stat(publicPath)
	wantPublic := ed25519.NewKeyFromSeed(want).Public().(ed25519.PublicKey)
	if !bytes.Equal(gotPrivate, want) || !bytes.Equal(gotPublic, wantPublic) ||
		privateInfo.Mode().Perm() != 0o600 || publicInfo.Mode().Perm() != 0o440 ||
		keyID == "" {
		t.Fatalf("approval key pair private=%x public=%x modes=%#o/%#o id=%q",
			gotPrivate, gotPublic, privateInfo.Mode().Perm(), publicInfo.Mode().Perm(), keyID)
	}
	recoveredID, err := createApprovalKeyPair(privatePath, publicPath, strings.NewReader("short"))
	if err != nil {
		t.Fatalf("matching durable key pair was not idempotent: %v", err)
	}
	if recoveredID != keyID {
		t.Fatalf("idempotent key ID = %q, want %q", recoveredID, keyID)
	}
	after, _ := os.ReadFile(privatePath)
	if !bytes.Equal(after, want) {
		t.Fatal("existing approval private key changed")
	}

	otherPublicPath := filepath.Join(dir, "other.pub")
	if err := os.WriteFile(otherPublicPath, make([]byte, ed25519.PublicKeySize), 0o440); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(otherPublicPath, 0o440); err != nil {
		t.Fatal(err)
	}
	if _, err := createApprovalKeyPair(privatePath, otherPublicPath, strings.NewReader("short")); err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched existing public key error = %v", err)
	}
	after, _ = os.ReadFile(privatePath)
	if !bytes.Equal(after, want) {
		t.Fatal("mismatched public key changed the existing private seed")
	}
}

func TestCreateApprovalKeyPairRejectsExistingPublicKeyWithWrongMode(t *testing.T) {
	tests := []struct {
		name string
		mode os.FileMode
	}{
		{name: "owner_read_only", mode: 0o400},
		{name: "world_readable", mode: 0o444},
		{name: "owner_writable", mode: 0o600},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := approvalTestDir(t)
			privatePath := filepath.Join(dir, "approval.seed")
			publicPath := filepath.Join(dir, "approval.pub")
			seed := bytes.Repeat([]byte{0x5a}, ed25519.SeedSize)
			if _, err := createApprovalKeyPair(privatePath, publicPath, bytes.NewReader(seed)); err != nil {
				t.Fatal(err)
			}
			privateBefore, err := os.ReadFile(privatePath)
			if err != nil {
				t.Fatal(err)
			}
			publicBefore, err := os.ReadFile(publicPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(publicPath, test.mode); err != nil {
				t.Fatal(err)
			}

			if _, err := createApprovalKeyPair(
				privatePath,
				publicPath,
				strings.NewReader("short"),
			); err == nil || !strings.Contains(err.Error(), "0440") {
				t.Fatalf("existing public key mode %#o error = %v", test.mode, err)
			}
			privateAfter, err := os.ReadFile(privatePath)
			if err != nil {
				t.Fatal(err)
			}
			publicAfter, err := os.ReadFile(publicPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(privateAfter, privateBefore) || !bytes.Equal(publicAfter, publicBefore) {
				t.Fatal("rejected public key mode changed the key pair")
			}
		})
	}
}

func TestCreateApprovalKeyPairRejectsUnsafePathAndEntropyFailure(t *testing.T) {
	dir := approvalTestDir(t)
	privatePath := filepath.Join(dir, "approval.seed")
	publicPath := filepath.Join(dir, "approval.pub")
	if _, err := createApprovalKeyPair("relative.key", publicPath, bytes.NewReader(make([]byte, 32))); err == nil {
		t.Fatal("relative approval key path was accepted")
	}
	if _, err := createApprovalKeyPair(privatePath, privatePath, bytes.NewReader(make([]byte, 32))); err == nil {
		t.Fatal("one path was accepted for both keys")
	}
	if _, err := createApprovalKeyPair(privatePath, publicPath, strings.NewReader("short")); err == nil {
		t.Fatal("short random source was accepted")
	}
	if _, err := os.Stat(privatePath); !os.IsNotExist(err) {
		t.Fatalf("entropy failure left a key file behind: %v", err)
	}
}

func TestCreateApprovalKeyPairRejectsSymlinkedAncestorsAndUnsafeDirectories(t *testing.T) {
	dir := approvalTestDir(t)
	privateDir := filepath.Join(dir, "private")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	symlinkDir := filepath.Join(dir, "private-link")
	if err := os.Symlink(privateDir, symlinkDir); err != nil {
		t.Fatal(err)
	}
	privatePath := filepath.Join(symlinkDir, "approval.seed")
	publicPath := filepath.Join(privateDir, "approval.pub")
	if _, err := createApprovalKeyPair(privatePath, publicPath, bytes.NewReader(make([]byte, 32))); err == nil ||
		!strings.Contains(err.Error(), "ancestors") {
		t.Fatalf("symlinked ancestor error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(privateDir, "approval.seed")); !os.IsNotExist(err) {
		t.Fatalf("symlinked destination was created: %v", err)
	}

	unsafePrivateDir := filepath.Join(dir, "unsafe-private")
	if err := os.Mkdir(unsafePrivateDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafePrivateDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := createApprovalKeyPair(
		filepath.Join(unsafePrivateDir, "approval.seed"),
		filepath.Join(privateDir, "other.pub"),
		bytes.NewReader(make([]byte, 32)),
	); err == nil || !strings.Contains(err.Error(), "group or other access") {
		t.Fatalf("unsafe private directory error = %v", err)
	}

	unsafePublicDir := filepath.Join(dir, "unsafe-public")
	if err := os.Mkdir(unsafePublicDir, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafePublicDir, 0o770); err != nil {
		t.Fatal(err)
	}
	safePrivatePath := filepath.Join(privateDir, "safe.seed")
	if _, err := createApprovalKeyPair(
		safePrivatePath,
		filepath.Join(unsafePublicDir, "approval.pub"),
		bytes.NewReader(make([]byte, 32)),
	); err == nil || !strings.Contains(err.Error(), "group or other writable") {
		t.Fatalf("unsafe public directory error = %v", err)
	}
	if _, err := os.Stat(safePrivatePath); !os.IsNotExist(err) {
		t.Fatalf("public-path preflight failure created the private key: %v", err)
	}
}

func TestCreateApprovalKeyPairPreflightsBothDestinations(t *testing.T) {
	dir := approvalTestDir(t)
	privatePath := filepath.Join(dir, "approval.seed")
	publicPath := filepath.Join(dir, "approval.pub")
	if err := os.WriteFile(publicPath, []byte("existing"), 0o440); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(publicPath, 0o440); err != nil {
		t.Fatal(err)
	}
	if _, err := createApprovalKeyPair(privatePath, publicPath, bytes.NewReader(make([]byte, 32))); err == nil {
		t.Fatal("existing public key destination was accepted")
	}
	if _, err := os.Stat(privatePath); !os.IsNotExist(err) {
		t.Fatalf("public-key preflight failure created a private key: %v", err)
	}
	got, err := os.ReadFile(publicPath)
	if err != nil || string(got) != "existing" {
		t.Fatalf("existing public key changed to %q: %v", got, err)
	}
}

func TestApprovalKeyCreationSyncsDirectories(t *testing.T) {
	dir := approvalTestDir(t)
	originalSync := syncApprovalKeyDirectory
	syncCalls := 0
	syncApprovalKeyDirectory = func(root *os.Root) error {
		syncCalls++
		return originalSync(root)
	}
	t.Cleanup(func() { syncApprovalKeyDirectory = originalSync })

	_, err := createApprovalKeyPair(
		filepath.Join(dir, "approval.seed"),
		filepath.Join(dir, "approval.pub"),
		bytes.NewReader(make([]byte, 32)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if syncCalls != 2 {
		t.Fatalf("approval key directory sync calls = %d, want 2", syncCalls)
	}
}

func TestApprovalKeyWriteFailureDoesNotPublishPartialFinalPath(t *testing.T) {
	dir := approvalTestDir(t)
	path := filepath.Join(dir, "approval.seed")
	destination, err := prepareApprovalKeyDestination(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer destination.root.Close()

	originalFileSync := syncApprovalKeyFile
	syncApprovalKeyFile = func(*os.File) error {
		return errors.New("injected file sync failure")
	}
	t.Cleanup(func() { syncApprovalKeyFile = originalFileSync })

	if err := writeApprovalKeyFile(destination, bytes.Repeat([]byte{0x5a}, 32), 0o600); err == nil {
		t.Fatal("injected file sync failure was ignored")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("failed staged write published a final key path: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed staged write left files behind: %v", entries)
	}
}

func TestApprovalKeyPairResumesAfterInterruptedPublicKeyCreation(t *testing.T) {
	dir := approvalTestDir(t)
	privatePath := filepath.Join(dir, "approval.seed")
	publicPath := filepath.Join(dir, "approval.pub")
	wantSeed := bytes.Repeat([]byte{0x7b}, ed25519.SeedSize)

	originalFileSync := syncApprovalKeyFile
	syncCalls := 0
	syncApprovalKeyFile = func(file *os.File) error {
		syncCalls++
		if syncCalls == 2 {
			return errors.New("injected public-key sync failure")
		}
		return originalFileSync(file)
	}
	t.Cleanup(func() { syncApprovalKeyFile = originalFileSync })
	_, err := createApprovalKeyPair(privatePath, publicPath, bytes.NewReader(wantSeed))
	syncApprovalKeyFile = originalFileSync
	if err == nil {
		t.Fatal("injected public-key failure was ignored")
	}
	if got, readErr := os.ReadFile(privatePath); readErr != nil || !bytes.Equal(got, wantSeed) {
		t.Fatalf("durable private seed after interruption = %x, %v", got, readErr)
	}
	if _, statErr := os.Stat(publicPath); !os.IsNotExist(statErr) {
		t.Fatalf("interrupted public key was published: %v", statErr)
	}

	keyID, err := createApprovalKeyPair(privatePath, publicPath, strings.NewReader("short"))
	if err != nil {
		t.Fatalf("valid private seed did not resume public-key creation: %v", err)
	}
	publicKey, err := os.ReadFile(publicPath)
	if err != nil {
		t.Fatal(err)
	}
	wantPublic := ed25519.NewKeyFromSeed(wantSeed).Public().(ed25519.PublicKey)
	if !bytes.Equal(publicKey, wantPublic) || keyID == "" {
		t.Fatalf("resumed public key = %x, want %x; key ID=%q", publicKey, wantPublic, keyID)
	}
}

func TestApprovalKeyFailureNeverRemovesAReplacementPath(t *testing.T) {
	dir := approvalTestDir(t)
	path := filepath.Join(dir, "approval.seed")
	destination, err := prepareApprovalKeyDestination(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer destination.root.Close()

	replacement := []byte("replacement-owned-by-another-operation")
	originalSync := syncApprovalKeyDirectory
	syncApprovalKeyDirectory = func(*os.Root) error {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, replacement, 0o600); err != nil {
			t.Fatal(err)
		}
		return nil
	}
	t.Cleanup(func() { syncApprovalKeyDirectory = originalSync })

	if err := writeApprovalKeyFile(destination, bytes.Repeat([]byte{0x5a}, 32), 0o600); err == nil {
		t.Fatal("replacement of the created key path was not detected")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, replacement) {
		t.Fatalf("failure cleanup changed the replacement path: %q", got)
	}
}

func TestDiscoverStoragePathsUsesReadOnlyExistenceProbe(t *testing.T) {
	dir := t.TempDir()
	var probed []string
	paths := discoverStoragePaths("/home/tester", func(path string) (os.FileInfo, error) {
		probed = append(probed, path)
		return os.Stat(dir)
	})
	if len(probed) != 1 || probed[0] != "/mnt/mithril-accounts" {
		t.Fatalf("stat probes = %v, want one accounts-root probe", probed)
	}
	if paths.Accounts != "/mnt/mithril-accounts" || paths.Logs != "/mnt/mithril-logs" || paths.Snapshots != "/mnt/mithril-ledger/snapshots" || paths.Shredstore != "/mnt/mithril-ledger/shredstore" {
		t.Fatalf("production paths = %+v", paths)
	}
}

func TestDiscoverStoragePathsFallsBackToHome(t *testing.T) {
	paths := discoverStoragePaths("/home/tester", func(string) (os.FileInfo, error) {
		return nil, errors.New("not present")
	})
	base := filepath.Join("/home/tester", ".mithril")
	if paths.Accounts != filepath.Join(base, "accounts") ||
		paths.Snapshots != filepath.Join(base, "snapshots") ||
		paths.Logs != filepath.Join(base, "logs") ||
		paths.Shredstore != filepath.Join(base, "shredstore") {
		t.Fatalf("fallback paths = %+v", paths)
	}
}

func TestResolvedConfigEnvironmentPathsWin(t *testing.T) {
	setConfigFileForTest(t, "")
	t.Setenv("MITHRIL_LOG_DIR", "/configured/logs")
	t.Setenv("MITHRIL_ACCOUNTS_PATH", "/configured/accounts")
	t.Setenv("MITHRIL_SNAPSHOTS_PATH", "/configured/snapshots")
	t.Setenv("MITHRIL_SHREDSTORE_PATH", "/configured/shredstore")
	t.Setenv("MITHRIL_STATE_PATH", "/configured/state.json")
	t.Setenv("MITHRIL_REPLAY_PATH", "/configured/replay.jsonl")
	t.Setenv("MITHRIL_PPROF_URL", "http://127.0.0.1:7777")
	t.Setenv("MITHRIL_MCP_PROFILE", "diagnostic")

	cfg, err := resolvedConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AccountsDir != "/configured/accounts" || cfg.SnapshotsDir != "/configured/snapshots" || cfg.ShredstoreDir != "/configured/shredstore" ||
		cfg.LogDir != "/configured/logs" || cfg.StatePath != "/configured/state.json" || cfg.ReplayPath != "/configured/replay.jsonl" || cfg.PprofURL != "http://127.0.0.1:7777" {
		t.Fatalf("environment path overrides were not preserved: %+v", cfg)
	}
	if cfg.Profile != mcp.ProfileDiagnostic {
		t.Fatalf("Profile = %q, want %q", cfg.Profile, mcp.ProfileDiagnostic)
	}
}

func TestResolvedConfigPreservesExplicitlyDisabledMetrics(t *testing.T) {
	clearMCPNodeSettingEnv(t)
	setConfigFileForTest(t, "")
	t.Setenv("MITHRIL_METRICS_URL", "")
	cfg, err := resolvedConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MetricsURL != "" {
		t.Fatalf("explicitly disabled metrics URL = %q", cfg.MetricsURL)
	}
}

func TestResolvedConfigRejectsInvalidBlockSource(t *testing.T) {
	clearMCPNodeSettingEnv(t)
	setConfigFileForTest(t, "")
	t.Setenv("MITHRIL_BLOCK_SOURCE", "lightbrigner")
	if _, err := resolvedConfig(); err == nil || !strings.Contains(err.Error(), "block source") {
		t.Fatalf("invalid block source error = %v", err)
	}
}

func TestResolvedConfigWithoutNodeConfigLeavesBlockSourceUnknown(t *testing.T) {
	clearMCPNodeSettingEnv(t)
	setConfigFileForTest(t, "")

	cfg, err := resolvedConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BlockSource != "" {
		t.Fatalf("standalone block source = %q, want unknown", cfg.BlockSource)
	}
}

func TestResolvedConfigRejectsInvalidApprovalTTL(t *testing.T) {
	clearMCPNodeSettingEnv(t)
	setConfigFileForTest(t, "")
	for _, value := range []string{"not-a-number", "0", "10", "301"} {
		t.Setenv("MITHRIL_MCP_APPROVAL_TTL_SECONDS", value)
		if _, err := resolvedConfig(); err == nil || !strings.Contains(err.Error(), "APPROVAL_TTL_SECONDS") {
			t.Fatalf("approval TTL %q error = %v", value, err)
		}
	}
}

func TestResolvedConfigUsesExplicitNodeSettings(t *testing.T) {
	clearMCPNodeSettingEnv(t)

	path := writeConfigFile(t, `
[storage]
accounts = '/node/accounts'
logs = '/node/logs'
snapshots = '/storage/snapshots'
shredstore = '/node/shredstore'
blockstore = '/legacy/shredstore'
[snapshot]
download_path = '/node/snapshots'
[ledger]
path = '/older/shredstore'
[rpc]
bind_address = '192.0.2.10'
port = 7788
[tuning.pprof]
port = 6677
[lightbringer]
enabled = true
`)
	setConfigFileForTest(t, path)
	cfg, err := resolvedConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AccountsDir != "/node/accounts" || cfg.SnapshotsDir != "/node/snapshots" || cfg.ShredstoreDir != "/node/shredstore" ||
		cfg.LogDir != "/node/logs" || cfg.StatePath != "/node/accounts/mithril_state.json" || cfg.ReplayPath != "/node/logs/replay_timings.jsonl" ||
		cfg.RPCURL != "http://127.0.0.1:7788" || cfg.PprofURL != "http://127.0.0.1:6677" || cfg.BlockSource != "lightbringer" {
		t.Fatalf("node storage paths were not applied: %+v", cfg)
	}
}

func TestResolvedConfigMatchesNodeBlockSourceRules(t *testing.T) {
	tests := []struct {
		name string
		toml string
		want string
	}{
		{
			name: "omitted cluster and source",
			toml: "[storage]\naccounts = '/node/accounts'\n",
			want: "turbine",
		},
		{
			name: "alpenglow default",
			toml: "[network]\ncluster = 'alpenglow'\n",
			want: "turbine",
		},
		{
			name: "mainnet beta default",
			toml: "[network]\ncluster = 'mainnet-beta'\n",
			want: "rpc",
		},
		{
			name: "testnet default",
			toml: "[network]\ncluster = 'testnet'\n",
			want: "rpc",
		},
		{
			name: "devnet default",
			toml: "[network]\ncluster = 'devnet'\n",
			want: "rpc",
		},
		{
			name: "lightbringer replaces protocol default",
			toml: "[lightbringer]\nenabled = true\n",
			want: "lightbringer",
		},
		{
			name: "explicit rpc survives lightbringer",
			toml: "[network]\ncluster = 'alpenglow'\n[block]\nsource = 'rpc'\n[lightbringer]\nenabled = true\n",
			want: "rpc",
		},
		{
			name: "explicit turbine on classic cluster",
			toml: "[network]\ncluster = 'mainnet-beta'\n[block]\nsource = 'turbine'\n",
			want: "turbine",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearMCPNodeSettingEnv(t)
			setConfigFileForTest(t, writeConfigFile(t, test.toml))

			cfg, err := resolvedConfig()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.BlockSource != test.want {
				t.Fatalf("block source = %q, want %q", cfg.BlockSource, test.want)
			}
		})
	}
}

func TestResolvedConfigRejectsInvalidNodeClusterOrBlockSource(t *testing.T) {
	tests := []struct {
		name    string
		toml    string
		wantErr string
	}{
		{
			name:    "cluster",
			toml:    "[network]\ncluster = 'localnet'\n",
			wantErr: "invalid network.cluster",
		},
		{
			name:    "block source",
			toml:    "[block]\nsource = 'unknown'\n",
			wantErr: "invalid block.source",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearMCPNodeSettingEnv(t)
			t.Setenv("MITHRIL_BLOCK_SOURCE", "rpc")
			setConfigFileForTest(t, writeConfigFile(t, test.toml))

			if _, err := resolvedConfig(); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("invalid node config error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestResolvedConfigRejectsRelativeFilePaths(t *testing.T) {
	clearMCPNodeSettingEnv(t)

	setConfigFileForTest(t, writeConfigFile(t, "[storage]\naccounts = 'relative/accounts'\nlogs = 'relative/logs'\n"))
	if _, err := resolvedConfig(); err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("relative node storage paths error = %v", err)
	}

	config.ConfigFile = ""
	t.Setenv("MITHRIL_STATE_PATH", "relative/state.json")
	if _, err := resolvedConfig(); err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("relative environment path error = %v", err)
	}
}

func TestResolvedConfigEnvironmentOverridesNodeConfig(t *testing.T) {
	path := writeConfigFile(t, `
[storage]
accounts = '/node/accounts'
logs = '/node/logs'
snapshots = '/node/snapshots'
shredstore = '/node/shredstore'
[rpc]
port = 7788
[tuning.pprof]
port = 6677
`)
	setConfigFileForTest(t, path)
	t.Setenv("MITHRIL_LOG_DIR", "/env/logs")
	t.Setenv("MITHRIL_ACCOUNTS_PATH", "/env/accounts")
	t.Setenv("MITHRIL_SNAPSHOTS_PATH", "/env/snapshots")
	t.Setenv("MITHRIL_SHREDSTORE_PATH", "/env/shredstore")
	t.Setenv("MITHRIL_STATE_PATH", "/env/state.json")
	t.Setenv("MITHRIL_REPLAY_PATH", "/env/replay.jsonl")
	t.Setenv("MITHRIL_RPC_URL", "http://127.0.0.1:8898")
	t.Setenv("MITHRIL_PPROF_URL", "http://127.0.0.1:6068")
	t.Setenv("MITHRIL_BLOCK_SOURCE", "turbine")

	cfg, err := resolvedConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AccountsDir != "/env/accounts" || cfg.SnapshotsDir != "/env/snapshots" || cfg.ShredstoreDir != "/env/shredstore" ||
		cfg.LogDir != "/env/logs" || cfg.StatePath != "/env/state.json" || cfg.ReplayPath != "/env/replay.jsonl" ||
		cfg.RPCURL != "http://127.0.0.1:8898" || cfg.PprofURL != "http://127.0.0.1:6068" || cfg.BlockSource != "turbine" {
		t.Fatalf("environment did not override node config: %+v", cfg)
	}
}

func TestResolvedConfigHonorsDisabledAndLegacyPorts(t *testing.T) {
	clearMCPNodeSettingEnv(t)
	path := filepath.Join(t.TempDir(), "node.toml")
	setConfigFileForTest(t, path)

	if err := os.WriteFile(path, []byte("[rpc]\nport = 0\n[tuning.pprof]\nport = -1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := resolvedConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RPCURL != "" || cfg.PprofURL != "" {
		t.Fatalf("disabled node endpoints were not preserved: %+v", cfg)
	}

	if err := os.WriteFile(path, []byte("[rpc]\nport = 8891\n[tuning.pprof]\nport = 0\n[development.pprof]\nport = 6061\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = resolvedConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RPCURL != "http://127.0.0.1:8891" || cfg.PprofURL != "http://127.0.0.1:6061" {
		t.Fatalf("node pprof fallback/RPC ports were not applied: %+v", cfg)
	}
}

func TestResolvedConfigExplicitNodeConfigUsesDisabledEndpointDefaults(t *testing.T) {
	clearMCPNodeSettingEnv(t)
	setConfigFileForTest(t, writeConfigFile(t, "[development.pprof]\nport = 6061\n"))

	cfg, err := resolvedConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RPCURL != "" || cfg.PprofURL != "" {
		t.Fatalf("omitted node endpoints were not disabled: %+v", cfg)
	}
}

func TestResolvedConfigUsesNodeLogDefaultWhenConfigOmitsStorageLogs(t *testing.T) {
	clearMCPNodeSettingEnv(t)
	setConfigFileForTest(t, writeConfigFile(t, "[storage]\naccounts = '/custom/accounts'\n"))

	cfg, err := resolvedConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogDir != "/mnt/mithril-logs" || cfg.ReplayPath != "/mnt/mithril-logs/replay_timings.jsonl" || cfg.StatePath != "/custom/accounts/mithril_state.json" {
		t.Fatalf("explicit node config paths do not match node defaults: %+v", cfg)
	}
}

func TestResolvedConfigExplicitEmptyEnvironmentOverridesNodeConfig(t *testing.T) {
	clearMCPNodeSettingEnv(t)
	path := writeConfigFile(t, `
[rpc]
port = 8891
[tuning.pprof]
port = 6061
`)
	setConfigFileForTest(t, path)
	for _, name := range []string{
		"MITHRIL_ACCOUNTS_PATH", "MITHRIL_SNAPSHOTS_PATH", "MITHRIL_SHREDSTORE_PATH",
		"MITHRIL_RPC_URL", "MITHRIL_PPROF_URL", "MITHRIL_BLOCK_SOURCE",
	} {
		t.Setenv(name, "")
	}

	cfg, err := resolvedConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AccountsDir != "" || cfg.SnapshotsDir != "" || cfg.ShredstoreDir != "" ||
		cfg.RPCURL != "" || cfg.PprofURL != "" {
		t.Fatalf("explicit empty environment values did not clear node settings: %+v", cfg)
	}
}

func TestResolvedConfigPreservesDisabledFileLogging(t *testing.T) {
	clearMCPNodeSettingEnv(t)

	setConfigFileForTest(t, writeConfigFile(t, "[storage]\naccounts = '/node/accounts'\nlogs = ''\n"))
	cfg, err := resolvedConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogDir != "" || cfg.ReplayPath != "" || cfg.StatePath != "/node/accounts/mithril_state.json" {
		t.Fatalf("disabled file logging was not preserved: %+v", cfg)
	}

	t.Setenv("MITHRIL_LOG_DIR", "/env/logs")
	cfg, err = resolvedConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogDir != "/env/logs" || cfg.ReplayPath != "/env/logs/replay_timings.jsonl" {
		t.Fatalf("environment did not override disabled file logging: %+v", cfg)
	}
}

func TestResolvedConfigRejectsExplicitMissingOrInvalidConfig(t *testing.T) {
	setConfigFileForTest(t, filepath.Join(t.TempDir(), "missing.toml"))
	if _, err := resolvedConfig(); err == nil || !strings.Contains(err.Error(), "read MCP node config") {
		t.Fatalf("missing explicit config error = %v", err)
	}
	config.ConfigFile = writeConfigFile(t, "[storage\naccounts = ???")
	if _, err := resolvedConfig(); err == nil || !strings.Contains(err.Error(), "read MCP node config") {
		t.Fatalf("invalid explicit config error = %v", err)
	}
	config.ConfigFile = writeConfigFile(t, "[rpc]\nport = 70000\n")
	if _, err := resolvedConfig(); err == nil || !strings.Contains(err.Error(), "rpc.port") {
		t.Fatalf("invalid RPC port error = %v", err)
	}
	config.ConfigFile = writeConfigFile(t, "[rpc]\nport = 'not-a-number'\n[tuning.pprof]\nport = 'also-not-a-number'\n")
	if _, err := resolvedConfig(); err == nil || !strings.Contains(err.Error(), "rpc.port must be an integer") {
		t.Fatalf("wrong RPC port type error = %v", err)
	}
	config.ConfigFile = writeConfigFile(t, "[rpc]\nport = 8899\n[tuning.pprof]\nport = 'not-a-number'\n")
	if _, err := resolvedConfig(); err == nil || !strings.Contains(err.Error(), "tuning.pprof.port must be an integer") {
		t.Fatalf("wrong pprof port type error = %v", err)
	}
}

func TestCLIExplainsClientOwnedStdioAndRemoteCommand(t *testing.T) {
	if !MCPCmd.SilenceErrors || !MCPCmd.SilenceUsage || !serveCmd.SilenceErrors || !serveCmd.SilenceUsage || !configCmd.SilenceErrors || !configCmd.SilenceUsage {
		t.Fatal("MCP runtime/config errors must not print duplicate errors or full usage")
	}
	if MCPCmd.RunE == nil || !serveCmd.Hidden {
		t.Fatal("mithril mcp must own the server entry point and keep the legacy serve alias hidden")
	}
	if err := MCPCmd.Args(&MCPCmd, []string{"unexpected"}); err == nil {
		t.Fatal("mithril mcp accepted a positional argument")
	}
	for _, command := range []*cobra.Command{&MCPCmd, &serveCmd} {
		for _, name := range []string{"profile", "enable-control", "allow-action", "approver-keys-dir", "control-target-id", "systemd-unit", "systemd-scope", "systemctl-path", "approval-ttl-seconds"} {
			if command.Flags().Lookup(name) == nil {
				t.Errorf("%s is missing --%s", command.CommandPath(), name)
			}
		}
	}
	text := MCPCmd.Long
	normalized := strings.Join(strings.Fields(text), " ")
	for _, want := range []string{"launches the stdio server as a child process", "ssh -T NODE mithril mcp", "SSH remote command", "not a daemon"} {
		if !strings.Contains(normalized, want) {
			t.Errorf("CLI help is missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(strings.ToLower(text), "ssh tunnel") {
		t.Fatalf("CLI help still describes stdio as an SSH tunnel:\n%s", text)
	}
	for _, want := range []string{"stdio has no authentication", "SSH identity is the"} {
		if !strings.Contains(normalized, want) {
			t.Errorf("CLI help is missing trust-boundary text %q:\n%s", want, text)
		}
	}
	for _, want := range []string{"Any stdio-capable MCP client", "command-and-arguments entry", "mithril mcp config", "File paths must be absolute"} {
		if !strings.Contains(normalized, want) {
			t.Errorf("CLI help is missing client-neutral guidance %q:\n%s", want, text)
		}
	}
}

func TestInteractiveMCPExplainsClientLaunchInsteadOfWaiting(t *testing.T) {
	original := interactiveStdio
	interactiveStdio = func() bool { return true }
	t.Cleanup(func() { interactiveStdio = original })

	var output bytes.Buffer
	cmd := MCPCmd
	cmd.SetOut(&output)
	if err := runServe(&cmd, nil); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, want := range []string{"stdio server", "not an interactive shell", "mithril mcp config"} {
		if !strings.Contains(text, want) {
			t.Errorf("interactive guidance is missing %q: %s", want, text)
		}
	}
}

func TestInteractiveMCPValidatesBeforePrintingHint(t *testing.T) {
	original := interactiveStdio
	interactiveStdio = func() bool { return true }
	t.Cleanup(func() { interactiveStdio = original })
	setConfigFileForTest(t, "")
	clearMCPNodeSettingEnv(t)
	t.Setenv("MITHRIL_MCP_PROFILE", "")

	tests := []struct {
		name    string
		flags   []string
		wantErr string
	}{
		{
			name:    "unknown profile",
			flags:   []string{"--profile", "not-a-profile"},
			wantErr: "unknown MCP profile",
		},
		{
			name:    "operator flag in monitor profile",
			flags:   []string{"--enable-control"},
			wantErr: "require --profile operator",
		},
		{
			name:    "operator profile without control",
			flags:   []string{"--profile", "operator"},
			wantErr: "requires lifecycle control",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cmd := newServeFlagCmd(t, test.flags...)
			cmd.Use = "mcp"
			var output bytes.Buffer
			cmd.SetOut(&output)

			err := runServe(cmd, nil)

			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, test.wantErr)
			}
			if output.Len() != 0 {
				t.Fatalf("interactive hint masked validation error: %s", output.String())
			}
		})
	}

	config.ConfigFile = filepath.Join(t.TempDir(), "missing.toml")
	cmd := newServeFlagCmd(t)
	cmd.Use = "mcp"
	var output bytes.Buffer
	cmd.SetOut(&output)
	if err := runServe(cmd, nil); err == nil {
		t.Fatal("missing --config path was accepted")
	}
	if output.Len() != 0 {
		t.Fatalf("interactive hint masked config-path error: %s", output.String())
	}

	config.ConfigFile = ""
	t.Setenv("MITHRIL_BLOCK_SOURCE", "unknown")
	cmd = newServeFlagCmd(t)
	cmd.Use = "mcp"
	output.Reset()
	cmd.SetOut(&output)
	if err := runServe(cmd, nil); err == nil || !strings.Contains(err.Error(), "unknown Mithril block source") {
		t.Fatalf("invalid block source error = %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("interactive hint masked block-source error: %s", output.String())
	}
}

func TestConfigCommandPrintsPortableStdioEntry(t *testing.T) {
	originalExecutable := currentExecutable
	t.Cleanup(func() { currentExecutable = originalExecutable })
	currentExecutable = func() (string, error) { return "/opt/mithril", nil }
	setConfigFileForTest(t, "")

	var output bytes.Buffer
	cmd := configCmd
	cmd.SetOut(&output)
	if err := cmd.RunE(&cmd, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(strings.Fields(output.String()), " "), `{ "command": "/opt/mithril", "args": [ "mcp" ] }`; got != want {
		t.Fatalf("portable config = %s", output.String())
	}
}

func TestConfigCommandPreservesExplicitNodeConfig(t *testing.T) {
	originalExecutable := currentExecutable
	t.Cleanup(func() { currentExecutable = originalExecutable })
	currentExecutable = func() (string, error) { return "/opt/mithril", nil }
	setConfigFileForTest(t, filepath.Join("relative", "node.toml"))
	wantConfigPath, err := filepath.Abs(config.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	cmd := configCmd
	cmd.SetOut(&output)
	if err := cmd.RunE(&cmd, nil); err != nil {
		t.Fatal(err)
	}
	var entry stdioConfigEntry
	if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(entry.Args, " "), "mcp --config "+wantConfigPath; got != want {
		t.Fatalf("portable config args = %q, want %q", got, want)
	}
}

func TestConfigCommandPrintsRemoteStdioEntry(t *testing.T) {
	originalExecutable := currentExecutable
	t.Cleanup(func() { currentExecutable = originalExecutable })
	currentExecutable = func() (string, error) {
		t.Fatal("remote config must not inspect the local executable")
		return "", nil
	}
	setConfigFileForTest(t, "")
	setRemoteConfigFlagsForTest(t, "operator@example-node", "/opt/Mithril's bin/mithril", "/etc/mithril/node config.toml")

	var output bytes.Buffer
	cmd := configCmd
	cmd.SetOut(&output)
	if err := cmd.RunE(&cmd, nil); err != nil {
		t.Fatal(err)
	}
	var entry stdioConfigEntry
	if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Command != "ssh" {
		t.Fatalf("remote command = %q, want ssh", entry.Command)
	}
	wantArgs := []string{
		"-T",
		"-a",
		"-x",
		"-o",
		"IgnoreUnknown=ForkAfterAuthentication,SessionType,StdinNull,RemoteCommand",
		"-o",
		"BatchMode=yes",
		"-o",
		"ConnectTimeout=10",
		"-o",
		"ServerAliveInterval=15",
		"-o",
		"ServerAliveCountMax=2",
		"-o",
		"ClearAllForwardings=yes",
		"-o",
		"Tunnel=no",
		"-o",
		"PermitLocalCommand=no",
		"-o",
		"StdinNull=no",
		"-o",
		"ForkAfterAuthentication=no",
		"-o",
		"SessionType=default",
		"-o",
		"RemoteCommand=none",
		"-o",
		"ControlPath=none",
		"--",
		"operator@example-node",
		`exec '/opt/Mithril'"'"'s bin/mithril' 'mcp' '--config' '/etc/mithril/node config.toml'`,
	}
	if strings.Join(entry.Args, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Fatalf("remote args = %#v, want %#v", entry.Args, wantArgs)
	}
}

func TestRemoteStdioConfigCanPinDiagnosticProfile(t *testing.T) {
	entry, err := remoteStdioConfigWithOperator("node-alias", "/usr/local/bin/mithril", "diagnostic", "", operatorLaunchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wantCommand := `exec '/usr/local/bin/mithril' 'mcp' '--profile' 'diagnostic'`
	if got := entry.Args[len(entry.Args)-1]; got != wantCommand {
		t.Fatalf("remote command = %q, want %q", got, wantCommand)
	}
	if _, err := remoteStdioConfigWithOperator("node-alias", "/usr/local/bin/mithril", "unsafe", "", operatorLaunchOptions{}); err == nil {
		t.Fatal("invalid remote config profile was accepted")
	}
}

func TestGeneratedOperatorConfigIsComplete(t *testing.T) {
	operator := operatorLaunchOptions{
		Enabled:         true,
		AllowedActions:  []string{"restart", "start", "restart"},
		ApproverKeysDir: "/etc/mithril-mcp/approvers",
		ControlTargetID: "node-mainnet-1",
		SystemdUnit:     "mithril-mainnet.service",
		SystemdScope:    "system",
		SystemctlPath:   "/usr/bin/systemctl",
		TTLSeconds:      45,
	}
	local, err := portableStdioConfigWithOperator("/opt/mithril", "operator", "", operator)
	if err != nil {
		t.Fatal(err)
	}
	want := "mcp --profile operator --enable-control --approver-keys-dir /etc/mithril-mcp/approvers --control-target-id node-mainnet-1 --allow-action start --allow-action restart --systemd-unit mithril-mainnet.service --systemd-scope system --systemctl-path /usr/bin/systemctl --approval-ttl-seconds 45"
	if got := strings.Join(local.Args, " "); got != want {
		t.Fatalf("operator config args = %q, want %q", got, want)
	}
	serveFlags := newServeFlagCmd(t, local.Args[1:]...)
	parsed, err := applyServeOperatorFlags(serveFlags, mcp.Config{Profile: mcp.ProfileOperator})
	if err != nil {
		t.Fatalf("generated operator args do not parse: %v", err)
	}
	if got := strings.Join(parsed.AllowedServiceActions, ","); got != "start,restart" {
		t.Fatalf("generated operator allowlist = %q, want start,restart", got)
	}
	if parsed.ApproverHistoryKeysDir != operator.ApproverKeysDir {
		t.Fatalf(
			"generated operator history keys = %q, want active directory %q",
			parsed.ApproverHistoryKeysDir,
			operator.ApproverKeysDir,
		)
	}
	remote, err := remoteStdioConfigWithOperator("node", "/opt/mithril", "operator", "", operator)
	if err != nil {
		t.Fatal(err)
	}
	for _, wantPart := range []string{"'--enable-control'", "'--allow-action'", "'start'", "'restart'", "'/etc/mithril-mcp/approvers'", "'node-mainnet-1'", "'mithril-mainnet.service'", "'45'"} {
		if !strings.Contains(remote.Args[len(remote.Args)-1], wantPart) {
			t.Errorf("remote operator command is missing %s: %s", wantPart, remote.Args[len(remote.Args)-1])
		}
	}
}

func TestGeneratedOperatorConfigFailsClosed(t *testing.T) {
	if _, err := portableStdioConfigWithOperator("/opt/mithril", "operator", "", operatorLaunchOptions{}); err == nil || !strings.Contains(err.Error(), "requires --enable-control") {
		t.Fatalf("incomplete operator config error = %v", err)
	}
	if _, err := portableStdioConfigWithOperator("/opt/mithril", "operator", "", operatorLaunchOptions{
		Enabled: true, ApproverKeysDir: "/secure/approvers", ControlTargetID: "node-1",
	}); err == nil || !strings.Contains(err.Error(), "--allow-action") {
		t.Fatalf("missing operator allowlist error = %v", err)
	}
	for _, operator := range []operatorLaunchOptions{
		{Enabled: true, AllowedActions: []string{"restart"}, ApproverKeysDir: "relative", ControlTargetID: "node-1"},
		{Enabled: true, AllowedActions: []string{"restart"}, ApproverKeysDir: "/secure/approvers", ControlTargetID: "bad target"},
		{Enabled: true, AllowedActions: []string{"restart"}, ApproverKeysDir: "/secure/approvers", ControlTargetID: "node-1", TTLSeconds: 10},
		{Enabled: true, AllowedActions: []string{"restart"}, ApproverKeysDir: "/secure/approvers", ControlTargetID: "node-1", TTLSet: true},
		{Enabled: true, AllowedActions: []string{"restart"}, ApproverKeysDir: "/secure/approvers", ControlTargetID: "node-1", SystemdScope: "global"},
		{Enabled: true, AllowedActions: []string{"restart"}, ApproverKeysDir: "/secure/approvers", ControlTargetID: "node-1", SystemdUnit: "mithril.target"},
		{Enabled: true, AllowedActions: []string{"reload"}, ApproverKeysDir: "/secure/approvers", ControlTargetID: "node-1"},
	} {
		if _, err := portableStdioConfigWithOperator("/opt/mithril", "operator", "", operator); err == nil {
			t.Fatalf("unsafe operator config was accepted: %+v", operator)
		}
	}
	if _, err := portableStdioConfigWithOperator("/opt/mithril", "monitor", "", operatorLaunchOptions{Enabled: true}); err == nil {
		t.Fatal("control options were accepted outside operator profile")
	}
}

func TestRemoteStdioConfigRejectsUnsafeInputs(t *testing.T) {
	tests := []struct {
		name   string
		target string
		binary string
		config string
	}{
		{name: "missing target", binary: "/opt/mithril"},
		{name: "target option", target: "-oProxyCommand=bad", binary: "/opt/mithril"},
		{name: "target whitespace", target: "node alias", binary: "/opt/mithril"},
		{name: "target shell syntax", target: "node;command", binary: "/opt/mithril"},
		{name: "target missing user", target: "@node", binary: "/opt/mithril"},
		{name: "target missing host", target: "operator@", binary: "/opt/mithril"},
		{name: "target host option", target: "operator@-node", binary: "/opt/mithril"},
		{name: "target host without name", target: "operator@...", binary: "/opt/mithril"},
		{name: "target multiple users", target: "one@two@node", binary: "/opt/mithril"},
		{name: "target mismatched bracket", target: "operator@[::1", binary: "/opt/mithril"},
		{name: "missing binary", target: "node"},
		{name: "relative binary", target: "node", binary: "bin/mithril"},
		{name: "unclean binary", target: "node", binary: "/opt/../bin/mithril"},
		{name: "binary control character", target: "node", binary: "/opt/mithril\nnext"},
		{name: "relative config", target: "node", binary: "/opt/mithril", config: "config.toml"},
		{name: "unclean config", target: "node", binary: "/opt/mithril", config: "/etc/./config.toml"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := remoteStdioConfigWithOperator(test.target, test.binary, "", test.config, operatorLaunchOptions{}); err == nil {
				t.Fatal("unsafe remote config input was accepted")
			}
		})
	}
}

func TestPOSIXShellQuoteKeepsRemoteArgumentsLiteral(t *testing.T) {
	tests := map[string]string{
		"":                       `''`,
		"/opt/mithril":           `'/opt/mithril'`,
		"/opt/a b/$HOME;command": `'/opt/a b/$HOME;command'`,
		"/opt/Mithril's/bin":     `'/opt/Mithril'"'"'s/bin'`,
	}
	for input, want := range tests {
		if got := posixShellQuote(input); got != want {
			t.Errorf("posixShellQuote(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestConfigCommandKeepsLocalAndRemoteConfigSeparate(t *testing.T) {
	setConfigFileForTest(t, "/local/node.toml")
	setRemoteConfigFlagsForTest(t, "node-alias", "/opt/mithril", "/remote/node.toml")
	cmd := configCmd
	if err := cmd.RunE(&cmd, nil); err == nil || !strings.Contains(err.Error(), "local --config") {
		t.Fatalf("local and remote config error = %v", err)
	}
}

func TestConfigCommandRejectsRemoteFlagsWithoutSSH(t *testing.T) {
	setConfigFileForTest(t, "")
	setRemoteConfigFlagsForTest(t, "", "/opt/mithril", "")
	cmd := configCmd
	if err := cmd.RunE(&cmd, nil); err == nil || !strings.Contains(err.Error(), "require --ssh") {
		t.Fatalf("remote flag without SSH error = %v", err)
	}
}

func TestPortableStdioConfigCanPinDiagnosticProfile(t *testing.T) {
	entry, err := portableStdioConfigWithOperator("/opt/mithril", "diagnostic", "", operatorLaunchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(entry.Args, " "), "mcp --profile diagnostic"; got != want {
		t.Fatalf("portable config args = %q, want %q", got, want)
	}
	if _, err := portableStdioConfigWithOperator("/opt/mithril", "unsafe", "", operatorLaunchOptions{}); err == nil {
		t.Fatal("invalid portable config profile was accepted")
	}
}

func TestResolvedConfigProfileOverride(t *testing.T) {
	setConfigFileForTest(t, "")
	t.Setenv("MITHRIL_MCP_PROFILE", "diagnostic")

	cfg, err := resolvedConfigWithOverrides(resolvedConfigOverrides{})
	if err != nil || cfg.Profile != mcp.ProfileDiagnostic {
		t.Fatalf("environment profile = %q, %v; want diagnostic", cfg.Profile, err)
	}
	monitor := mcp.ProfileMonitor
	cfg, err = resolvedConfigWithOverrides(resolvedConfigOverrides{Profile: &monitor})
	if err != nil || cfg.Profile != mcp.ProfileMonitor {
		t.Fatalf("explicit profile = %q, %v; want monitor", cfg.Profile, err)
	}
	t.Setenv("MITHRIL_MCP_PROFILE", "unsafe")
	if _, err := resolvedConfigWithOverrides(resolvedConfigOverrides{}); err == nil {
		t.Fatal("invalid environment profile was silently downgraded")
	}
	cfg, err = resolvedConfigWithOverrides(resolvedConfigOverrides{Profile: &monitor})
	if err != nil || cfg.Profile != mcp.ProfileMonitor {
		t.Fatalf("explicit profile did not override invalid environment profile: %q, %v", cfg.Profile, err)
	}
}

func TestResolvedConfigApprovalTTLOverride(t *testing.T) {
	clearMCPNodeSettingEnv(t)
	setConfigFileForTest(t, "")
	t.Setenv("MITHRIL_MCP_APPROVAL_TTL_SECONDS", "invalid")
	ttl := uint64(45)
	cfg, err := resolvedConfigWithOverrides(resolvedConfigOverrides{ApprovalTTLSeconds: &ttl})
	if err != nil || cfg.ApprovalTTLSeconds != ttl {
		t.Fatalf("explicit approval TTL did not override invalid environment value: %d, %v", cfg.ApprovalTTLSeconds, err)
	}
}

func TestDurableExecutablePathRejectsGoRun(t *testing.T) {
	if _, err := durableExecutablePath("/private/tmp/go-build123/b001/exe/mithril"); err == nil {
		t.Fatal("ephemeral go run executable was accepted")
	}
	cache := t.TempDir()
	t.Setenv("GOCACHE", cache)
	if _, err := durableExecutablePath(filepath.Join(cache, "00", "cache-hash", "mithril")); err == nil {
		t.Fatal("executable inside the configured Go cache was accepted")
	}
	got, err := durableExecutablePath("/opt/mithril")
	if err != nil || got != "/opt/mithril" {
		t.Fatalf("durableExecutablePath = %q, %v", got, err)
	}
}

// newServeFlagCmd builds a command carrying the serve flag set and applies the
// given flags, mirroring how cobra reports Changed() at runtime.
func newServeFlagCmd(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "serve", RunE: func(*cobra.Command, []string) error { return nil }}
	bindServeFlags(cmd)
	cmd.SetArgs(args)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("parse flags %v: %v", args, err)
	}
	return cmd
}

func TestApplyServeOperatorFlagsRequiresOperatorProfile(t *testing.T) {
	controlFlags := [][]string{
		{"--enable-control"},
		{"--allow-action", "restart"},
		{"--approver-keys-dir", "/etc/mithril-mcp/approvers"},
		{"--control-target-id", "node-1"},
		{"--systemd-unit", "mithril.service"},
		{"--systemd-scope", "user"},
		{"--systemctl-path", "/usr/bin/systemctl"},
		{"--approval-ttl-seconds", "60"},
	}

	for _, profile := range []mcp.Profile{mcp.ProfileMonitor, mcp.ProfileDiagnostic} {
		for _, flags := range controlFlags {
			cmd := newServeFlagCmd(t, flags...)
			_, err := applyServeOperatorFlags(cmd, mcp.Config{Profile: profile})
			if err == nil {
				t.Errorf("profile %q accepted control flag %v; lifecycle control must require --profile operator",
					profile, flags)
				continue
			}
			if !strings.Contains(err.Error(), "operator") {
				t.Errorf("error for %v under %q = %q, want it to name the operator requirement", flags, profile, err)
			}
		}
	}
}

func TestApplyServeOperatorFlagsMapsEachFlag(t *testing.T) {
	cmd := newServeFlagCmd(t,
		"--enable-control",
		"--allow-action", "restart",
		"--allow-action", "start",
		"--approver-keys-dir", "/etc/mithril-mcp/approvers",
		"--control-target-id", "node-1",
		"--systemd-unit", "mithril.service",
		"--systemd-scope", "user",
		"--systemctl-path", "/usr/bin/systemctl",
	)
	cfg, err := applyServeOperatorFlags(cmd, mcp.Config{Profile: mcp.ProfileOperator})
	if err != nil {
		t.Fatalf("operator profile rejected its own flags: %v", err)
	}
	if !cfg.ControlEnabled {
		t.Error("--enable-control did not set ControlEnabled")
	}
	if got, want := strings.Join(cfg.AllowedServiceActions, ","), "start,restart"; got != want {
		t.Errorf("AllowedServiceActions = %q, want %q", got, want)
	}
	if cfg.ApproverKeysDir != "/etc/mithril-mcp/approvers" {
		t.Errorf("ApproverKeysDir = %q", cfg.ApproverKeysDir)
	}
	if cfg.ApproverHistoryKeysDir != cfg.ApproverKeysDir {
		t.Errorf(
			"ApproverHistoryKeysDir = %q, want active directory %q",
			cfg.ApproverHistoryKeysDir,
			cfg.ApproverKeysDir,
		)
	}
	if cfg.ControlTargetID != "node-1" {
		t.Errorf("ControlTargetID = %q", cfg.ControlTargetID)
	}
	if cfg.SystemdUnit != "mithril.service" {
		t.Errorf("SystemdUnit = %q", cfg.SystemdUnit)
	}
	if cfg.SystemdScope != "user" {
		t.Errorf("SystemdScope = %q", cfg.SystemdScope)
	}
	if cfg.SystemctlPath != "/usr/bin/systemctl" {
		t.Errorf("SystemctlPath = %q", cfg.SystemctlPath)
	}
}

func TestApproverHistoryDirectoryDefaultFollowsActiveOverride(t *testing.T) {
	const (
		defaultKeys = "/etc/mithril-mcp/approvers"
		activeKeys  = "/srv/mithril/active-approvers"
		historyKeys = "/srv/mithril/approver-history"
	)
	t.Setenv("MITHRIL_MCP_APPROVER_HISTORY_KEYS_DIR", "")

	cmd := newServeFlagCmd(t, "--approver-keys-dir", activeKeys)
	cfg, err := applyServeOperatorFlags(cmd, mcp.Config{
		Profile:                mcp.ProfileOperator,
		ApproverKeysDir:        defaultKeys,
		ApproverHistoryKeysDir: defaultKeys,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ApproverHistoryKeysDir != activeKeys {
		t.Fatalf(
			"default history keys = %q, want active override %q",
			cfg.ApproverHistoryKeysDir,
			activeKeys,
		)
	}

	cmd = newServeFlagCmd(t,
		"--approver-keys-dir", activeKeys,
		"--approver-history-keys-dir", historyKeys,
	)
	cfg, err = applyServeOperatorFlags(cmd, mcp.Config{
		Profile:                mcp.ProfileOperator,
		ApproverKeysDir:        defaultKeys,
		ApproverHistoryKeysDir: defaultKeys,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ApproverHistoryKeysDir != historyKeys {
		t.Fatalf(
			"explicit history keys = %q, want %q",
			cfg.ApproverHistoryKeysDir,
			historyKeys,
		)
	}

	t.Setenv("MITHRIL_MCP_APPROVER_HISTORY_KEYS_DIR", historyKeys)
	cmd = newServeFlagCmd(t, "--approver-keys-dir", activeKeys)
	cfg, err = applyServeOperatorFlags(cmd, mcp.Config{
		Profile:                mcp.ProfileOperator,
		ApproverKeysDir:        defaultKeys,
		ApproverHistoryKeysDir: historyKeys,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ApproverHistoryKeysDir != historyKeys {
		t.Fatalf(
			"environment history keys = %q, want %q",
			cfg.ApproverHistoryKeysDir,
			historyKeys,
		)
	}
}

func TestApplyServeOperatorFlagsLeavesConfigUntouchedWhenUnset(t *testing.T) {
	cmd := newServeFlagCmd(t)
	in := mcp.Config{Profile: mcp.ProfileMonitor, SystemdUnit: "preexisting.service"}
	got, err := applyServeOperatorFlags(cmd, in)
	if err != nil {
		t.Fatalf("no control flags passed, yet the monitor profile was rejected: %v", err)
	}
	if got.SystemdUnit != "preexisting.service" || got.ControlEnabled {
		t.Errorf("config was modified without any control flag: %+v", got)
	}
}

func TestResolvedServeConfigValidatesProfileAndTTL(t *testing.T) {
	cmd := newServeFlagCmd(t, "--profile", "not-a-profile")
	if _, err := resolvedServeConfig(cmd); err == nil {
		t.Error("an unknown profile name was accepted")
	}

	for _, ttl := range []string{"0", "1", "14", "301", "100000"} {
		cmd := newServeFlagCmd(t, "--profile", "operator", "--approval-ttl-seconds", ttl)
		_, err := resolvedServeConfig(cmd)
		if err == nil {
			t.Errorf("--approval-ttl-seconds=%s was accepted; it is outside %d..%d",
				ttl, mcp.MinApprovalTTLSeconds, mcp.MaxApprovalTTLSeconds)
			continue
		}
		if !strings.Contains(err.Error(), "approval-ttl-seconds") {
			t.Errorf("TTL error for %s = %q, want it to name the flag", ttl, err)
		}
	}
}

func TestSignServiceApprovalRejectsBadKeyAndChallenge(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "approval.key")
	if err := os.WriteFile(keyPath, []byte(strings.Repeat("k", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		challenge string
		keyPath   string
	}{
		{"missing key file", "any-challenge", filepath.Join(dir, "absent.key")},
		{"relative key path", "any-challenge", "approval.key"},
		{"empty key path", "any-challenge", ""},
		{"malformed challenge", "not-a-challenge", keyPath},
		{"empty challenge", "", keyPath},
		{"malformed versioned challenge", "v1.invalid", keyPath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bundle, err := signServiceApproval(tc.challenge, tc.keyPath, now)
			if err == nil {
				t.Fatalf("accepted invalid input, returning bundle=%+v", bundle)
			}
			if bundle.AuthorizationToken != "" || bundle.AuditAttestation != "" {
				t.Errorf("failed approval still returned signed material: %+v", bundle)
			}
			// A world-readable key file is refused by the loader; that error
			// must not carry key bytes into the operator's terminal.
			if strings.Contains(err.Error(), strings.Repeat("k", 32)) {
				t.Errorf("error leaked key material: %v", err)
			}
		})
	}
}

func TestConfirmServiceApprovalAcceptsOnlyExactApprove(t *testing.T) {
	summary := mcp.ServiceApprovalSummary{
		Action: "restart", Unit: "mithril.service", Scope: "system",
		TargetID: "node-1", ActionID: "action-1", ApproverKeyID: "key-1",
		ExpiresAt: time.Date(2026, 7, 28, 12, 1, 0, 0, time.UTC).Unix(),
		Status: mcp.ServiceApprovalStatus{
			LoadState: "loaded", ActiveState: "active", SubState: "running",
		},
		Consequence: "Restarts the fixed Mithril service.",
	}

	rejected := []string{
		"", "\n", "approve\n", "Approve\n", "APPROVED\n", " APPROVE\n", "APPROVE \n",
		"yes\n", "y\n", "APPROVE APPROVE\n", strings.Repeat("A", 200) + "\n",
		"APPROVE" + strings.Repeat(" ", 100) + "\n",
	}
	for _, input := range rejected {
		var errBuf bytes.Buffer
		if err := confirmServiceApproval(strings.NewReader(input), &errBuf, summary); err == nil {
			t.Errorf("confirmation %q was accepted; only the exact word APPROVE may authorize an action", input)
		}
	}

	for _, input := range []string{"APPROVE\n", "APPROVE\r\n", "APPROVE"} {
		var errBuf bytes.Buffer
		if err := confirmServiceApproval(strings.NewReader(input), &errBuf, summary); err != nil {
			t.Errorf("confirmation %q was rejected: %v", input, err)
		}
	}

	// The prompt must state exactly what is being authorized, or the operator
	// cannot tell which action they are approving.
	var errBuf bytes.Buffer
	_ = confirmServiceApproval(strings.NewReader("APPROVE\n"), &errBuf, summary)
	for _, want := range []string{
		"node-1", "action-1", "restart", "mithril.service", "system",
		"loaded/active/running", "Restarts the fixed Mithril service.",
		"key-1", "2026-07-28T12:01:00Z",
	} {
		if !strings.Contains(errBuf.String(), want) {
			t.Errorf("approval prompt omits %q: %s", want, errBuf.String())
		}
	}
}

func TestConfirmServiceApprovalNeverWritesTerminalControls(t *testing.T) {
	summary := mcp.ServiceApprovalSummary{
		Action: "restart", Unit: "mithril.service", Scope: "system",
		TargetID: "node-1", ActionID: "action-1", ApproverKeyID: "key-1",
		ExpiresAt: time.Date(2026, 7, 20, 12, 5, 0, 0, time.UTC).Unix(),
		Status: mcp.ServiceApprovalStatus{
			LoadState: "loaded", ActiveState: "active", SubState: "running\x1b[2J",
		},
	}
	var prompt bytes.Buffer
	if err := confirmServiceApproval(strings.NewReader("APPROVE\n"), &prompt, summary); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(prompt.Bytes(), []byte("\x1b")) {
		t.Fatalf("approval prompt contains terminal control bytes: %q", prompt.Bytes())
	}
}

func TestConfiguredIntegerRejectsNonIntegerValues(t *testing.T) {
	accepted := map[string]any{
		"int":      int(42),
		"int8":     int8(8),
		"int16":    int16(16),
		"int32":    int32(32),
		"int64":    int64(64),
		"uint":     uint(42),
		"uint8":    uint8(8),
		"uint16":   uint16(16),
		"uint32":   uint32(32),
		"uint64":   uint64(64),
		"zero":     int(0),
		"negative": int(-1),
		"maxint64": int64(math.MaxInt64),
	}
	for name, value := range accepted {
		v := viper.New()
		v.Set("k", value)
		got, err := configuredInteger(v, "k")
		if err != nil {
			t.Errorf("%s: configuredInteger rejected %v (%T): %v", name, value, value, err)
			continue
		}
		if want := reflect.ValueOf(value); want.CanInt() && got != want.Int() {
			t.Errorf("%s: got %d, want %d", name, got, want.Int())
		}
	}

	rejected := map[string]any{
		"string":       "42",
		"empty string": "",
		"float":        4.2,
		"whole float":  float64(42), // still not an integer type
		"bool":         true,
		"slice":        []int{1},
		"map":          map[string]int{"a": 1},
		"nil":          nil,
		// Above MaxInt64: converting would wrap to a negative limit.
		"uint64 overflow": uint64(math.MaxInt64) + 1,
	}
	for name, value := range rejected {
		v := viper.New()
		v.Set("k", value)
		if got, err := configuredInteger(v, "k"); err == nil {
			t.Errorf("%s: configuredInteger accepted %v (%T), returning %d", name, value, value, got)
		}
	}

	// An absent key is not an integer either.
	if _, err := configuredInteger(viper.New(), "missing"); err == nil {
		t.Error("an unset key was accepted as an integer")
	}
}

// Remote configuration must remain fixed argv, never a shell command.
func FuzzRemoteStdioConfigNeverInjects(f *testing.F) {
	seeds := []struct{ target, binary, profile, remoteConfig string }{
		{"mithril-mcp-target", "/usr/local/bin/mithril", "monitor", ""},
		{"user@host", "/usr/local/bin/mithril", "diagnostic", "/etc/mithril/config.toml"},
		{"host; rm -rf /", "/usr/local/bin/mithril", "monitor", ""},
		{"host$(whoami)", "/usr/local/bin/mithril", "monitor", ""},
		{"host`id`", "/bin/mithril", "monitor", ""},
		{"-oProxyCommand=evil", "/bin/mithril", "monitor", ""},
		{"host\nProxyCommand evil", "/bin/mithril", "monitor", ""},
		{"[fd00::1]", "/bin/mithril", "monitor", ""},
		{"host", "relative/path", "monitor", ""},
		{"host", "/bin/mithril; evil", "monitor", ""},
		{"host", "/bin/mithril", "monitor", "relative.toml"},
		{"", "", "", ""},
	}
	for _, s := range seeds {
		f.Add(s.target, s.binary, s.profile, s.remoteConfig)
	}

	f.Fuzz(func(t *testing.T, target, binary, profile, remoteConfig string) {
		entry, err := remoteStdioConfigWithOperator(target, binary, profile, remoteConfig, operatorLaunchOptions{})
		if err != nil {
			return // rejection is always acceptable
		}

		// Accepted: the shape must be a fixed argv, not a shell string.
		if entry.Command != "ssh" {
			t.Fatalf("accepted entry runs %q, not ssh", entry.Command)
		}
		if len(entry.Args) == 0 {
			t.Fatal("accepted entry has no arguments")
		}

		// The target must appear as exactly one whole argument. If it were
		// split or concatenated, caller input would have become structure.
		found := 0
		for _, a := range entry.Args {
			if a == target {
				found++
			}
		}
		if found != 1 {
			t.Fatalf("target %q appears %d times as a distinct argument in %q", target, found, entry.Args)
		}

		for _, a := range entry.Args {
			// A newline in any argument can split a forced command or an
			// authorized_keys line on the remote side.
			if strings.ContainsAny(a, "\n\r\x00") {
				t.Fatalf("argument %q carries a newline or NUL: %q", a, entry.Args)
			}
		}

		// The remote binary must be absolute, or the remote PATH decides what
		// actually runs.
		if !strings.HasPrefix(binary, "/") {
			t.Fatalf("accepted a non-absolute remote binary %q", binary)
		}
	})
}
