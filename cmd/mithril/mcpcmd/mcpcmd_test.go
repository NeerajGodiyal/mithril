package mcpcmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/config"
	"github.com/Overclock-Validator/mithril/pkg/mcp"
	"github.com/spf13/cobra"
)

func resolvedConfig() (mcp.Config, error) {
	return resolvedConfigWithOverrides(resolvedConfigOverrides{})
}

func clearMCPNodeSettingEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"MITHRIL_ACCOUNTS_PATH", "MITHRIL_SNAPSHOTS_PATH", "MITHRIL_SHREDSTORE_PATH",
		"MITHRIL_LOG_DIR", "MITHRIL_STATE_PATH", "MITHRIL_REPLAY_PATH", "MITHRIL_NODE_CGROUP_PATH",
		"MITHRIL_METRICS_URL", "MITHRIL_RPC_URL", "MITHRIL_PPROF_URL", "MITHRIL_LIGHTBRINGER_GRPC_ADDR",
		"MITHRIL_LIGHTBRINGER_INFLUXDB_URL", "MITHRIL_LIGHTBRINGER_INFLUXDB_DATABASE",
		"MITHRIL_LIGHTBRINGER_INFLUXDB_TOKEN", "MITHRIL_BLOCK_SOURCE", "MITHRIL_LIGHTBRINGER_QUIET",
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

func TestApproveCommandRequiresExactInteractiveConfirmation(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	want := serviceApproval{
		Action:    "restart",
		Unit:      "mithril.service",
		Scope:     "system",
		ExpiresAt: "2026-07-20T12:01:00Z",
		Token:     "approved-token",
	}
	loader := func(challenge, keyPath string, gotNow time.Time) (serviceApproval, error) {
		if challenge != "prepared-challenge" || keyPath != "/secure/approval.key" || !gotNow.Equal(now) {
			t.Fatalf("loader inputs = %q, %q, %s", challenge, keyPath, gotNow)
		}
		return want, nil
	}

	var stdout, stderr bytes.Buffer
	err := runApproveCommand(
		strings.NewReader("APPROVE\n"),
		&stdout,
		&stderr,
		"prepared-challenge",
		"/secure/approval.key",
		true,
		now,
		loader,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != want.Token+"\n" {
		t.Fatalf("approval stdout = %q, want only the token", got)
	}
	prompt := stderr.String()
	for _, field := range []string{
		"Action: restart",
		"Unit: mithril.service",
		"Scope: system",
		"Expires: 2026-07-20T12:01:00Z",
		"Type APPROVE",
	} {
		if !strings.Contains(prompt, field) {
			t.Errorf("approval prompt is missing %q: %q", field, prompt)
		}
	}
	for _, secret := range []string{want.Token, "prepared-challenge", "/secure/approval.key"} {
		if strings.Contains(prompt, secret) {
			t.Errorf("approval prompt exposed %q: %q", secret, prompt)
		}
	}
}

func TestApproveCommandRejectsNonInteractiveOrInexactInput(t *testing.T) {
	approval := serviceApproval{
		Action:    "stop",
		Unit:      "mithril.service",
		Scope:     "user",
		ExpiresAt: "2026-07-20T12:01:00Z",
		Token:     "must-not-be-printed",
	}
	loaderCalls := 0
	loader := func(string, string, time.Time) (serviceApproval, error) {
		loaderCalls++
		return approval, nil
	}

	var stdout, stderr bytes.Buffer
	err := runApproveCommand(strings.NewReader("APPROVE\n"), &stdout, &stderr, "challenge", "/secure/key", false, time.Now(), loader)
	if err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("non-interactive error = %v", err)
	}
	if loaderCalls != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("non-interactive invocation reached approval logic: calls=%d stdout=%q stderr=%q", loaderCalls, stdout.String(), stderr.String())
	}

	for _, input := range []string{"", "approve\n", "APPROVE \n", "YES\n", strings.Repeat("A", maxApprovalConfirmationBytes+1)} {
		stdout.Reset()
		stderr.Reset()
		err := runApproveCommand(strings.NewReader(input), &stdout, &stderr, "challenge", "/secure/key", true, time.Now(), loader)
		if err == nil || !strings.Contains(err.Error(), "not approved") {
			t.Errorf("confirmation %q error = %v", input, err)
		}
		if stdout.Len() != 0 {
			t.Errorf("confirmation %q wrote stdout %q", input, stdout.String())
		}
		if strings.Contains(stderr.String(), approval.Token) {
			t.Errorf("confirmation %q exposed the token on stderr", input)
		}
	}
}

func TestResolveApprovalKeyPath(t *testing.T) {
	t.Setenv("MITHRIL_MCP_APPROVAL_KEY_FILE", "/environment/approval.key")
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
	t.Setenv("MITHRIL_MCP_APPROVAL_KEY_FILE", "relative/key")
	if _, err := resolveApprovalKeyPath(""); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative environment key error = %v", err)
	}
	t.Setenv("MITHRIL_MCP_APPROVAL_KEY_FILE", "")
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
	if err := approveCmd.Args(&approveCmd, nil); err == nil {
		t.Fatal("approve command accepted a missing challenge")
	}
	if err := approveCmd.Args(&approveCmd, []string{"one", "two"}); err == nil {
		t.Fatal("approve command accepted multiple challenges")
	}
}

func TestCreateApprovalKeyIsRandomPrivateAndExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approval.key")
	want := bytes.Repeat([]byte{0x5a}, 32)
	if err := createApprovalKey(path, bytes.NewReader(want)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("approval key content/mode = %x/%#o", got, info.Mode().Perm())
	}
	if err := createApprovalKey(path, bytes.NewReader(bytes.Repeat([]byte{0x33}, 32))); err == nil {
		t.Fatal("existing approval key was replaced")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(after, want) {
		t.Fatal("existing approval key changed")
	}
}

func TestCreateApprovalKeyRejectsUnsafePathAndEntropyFailure(t *testing.T) {
	if err := createApprovalKey("relative.key", bytes.NewReader(make([]byte, 32))); err == nil {
		t.Fatal("relative approval key path was accepted")
	}
	path := filepath.Join(t.TempDir(), "approval.key")
	if err := createApprovalKey(path, strings.NewReader("short")); err == nil {
		t.Fatal("short random source was accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("entropy failure left a key file behind: %v", err)
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
port = 7788
[tuning.pprof]
port = 6677
[block]
lightbringer_endpoint = '127.0.0.1:5556'
[lightbringer]
enabled = true
quiet = false
grpc_addr = '127.0.0.1:5555'
influxdb_host = 'http://127.0.0.1:8181'
influxdb_database = 'node_metrics'
influxdb_token = 'NODE_INFLUX_TOKEN'
`)
	setConfigFileForTest(t, path)
	cfg, err := resolvedConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AccountsDir != "/node/accounts" || cfg.SnapshotsDir != "/node/snapshots" || cfg.ShredstoreDir != "/node/shredstore" ||
		cfg.LogDir != "/node/logs" || cfg.StatePath != "/node/accounts/mithril_state.json" || cfg.ReplayPath != "/node/logs/replay_timings.jsonl" ||
		cfg.RPCURL != "http://127.0.0.1:7788" || cfg.PprofURL != "http://127.0.0.1:6677" ||
		cfg.LightbringerGRPCAddr != "127.0.0.1:5556" || cfg.LightbringerInfluxURL != "http://127.0.0.1:8181" ||
		cfg.LightbringerInfluxDB != "node_metrics" || cfg.LightbringerInfluxTok != "NODE_INFLUX_TOKEN" || cfg.BlockSource != "lightbringer" || cfg.LightbringerQuiet == nil || *cfg.LightbringerQuiet {
		t.Fatalf("node storage paths were not applied: %+v", cfg)
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
[lightbringer]
grpc_addr = '127.0.0.1:5555'
influxdb_host = 'http://127.0.0.1:8181'
influxdb_database = 'node_metrics'
influxdb_token = 'NODE_INFLUX_TOKEN'
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
	t.Setenv("MITHRIL_LIGHTBRINGER_GRPC_ADDR", "127.0.0.1:3008")
	t.Setenv("MITHRIL_LIGHTBRINGER_INFLUXDB_URL", "http://127.0.0.1:8188")
	t.Setenv("MITHRIL_LIGHTBRINGER_INFLUXDB_DATABASE", "env_metrics")
	t.Setenv("MITHRIL_LIGHTBRINGER_INFLUXDB_TOKEN", "ENV_INFLUX_TOKEN")
	t.Setenv("MITHRIL_BLOCK_SOURCE", "turbine")
	t.Setenv("MITHRIL_LIGHTBRINGER_QUIET", "true")

	cfg, err := resolvedConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AccountsDir != "/env/accounts" || cfg.SnapshotsDir != "/env/snapshots" || cfg.ShredstoreDir != "/env/shredstore" ||
		cfg.LogDir != "/env/logs" || cfg.StatePath != "/env/state.json" || cfg.ReplayPath != "/env/replay.jsonl" ||
		cfg.RPCURL != "http://127.0.0.1:8898" || cfg.PprofURL != "http://127.0.0.1:6068" ||
		cfg.LightbringerGRPCAddr != "127.0.0.1:3008" || cfg.LightbringerInfluxURL != "http://127.0.0.1:8188" ||
		cfg.LightbringerInfluxDB != "env_metrics" || cfg.LightbringerInfluxTok != "ENV_INFLUX_TOKEN" || cfg.BlockSource != "turbine" || cfg.LightbringerQuiet == nil || !*cfg.LightbringerQuiet {
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
[lightbringer]
grpc_addr = '127.0.0.1:3001'
influxdb_host = 'http://127.0.0.1:8181'
influxdb_database = 'node_metrics'
influxdb_token = 'NODE_INFLUX_TOKEN'
`)
	setConfigFileForTest(t, path)
	for _, name := range []string{
		"MITHRIL_ACCOUNTS_PATH", "MITHRIL_SNAPSHOTS_PATH", "MITHRIL_SHREDSTORE_PATH",
		"MITHRIL_RPC_URL", "MITHRIL_PPROF_URL", "MITHRIL_LIGHTBRINGER_GRPC_ADDR",
		"MITHRIL_LIGHTBRINGER_INFLUXDB_URL", "MITHRIL_LIGHTBRINGER_INFLUXDB_DATABASE",
		"MITHRIL_LIGHTBRINGER_INFLUXDB_TOKEN", "MITHRIL_BLOCK_SOURCE", "MITHRIL_LIGHTBRINGER_QUIET",
	} {
		t.Setenv(name, "")
	}

	cfg, err := resolvedConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AccountsDir != "" || cfg.SnapshotsDir != "" || cfg.ShredstoreDir != "" ||
		cfg.RPCURL != "" || cfg.PprofURL != "" || cfg.LightbringerGRPCAddr != "" ||
		cfg.LightbringerInfluxURL != "" || cfg.LightbringerInfluxDB != "" || cfg.LightbringerInfluxTok != "" {
		t.Fatalf("explicit empty environment values did not clear node settings: %+v", cfg)
	}
}

func TestResolvedConfigDoesNotMoveNodeInfluxTokenToEnvironmentOrigin(t *testing.T) {
	clearMCPNodeSettingEnv(t)
	path := writeConfigFile(t, `
[lightbringer]
influxdb_host = 'https://node-influx.example.com'
influxdb_token = 'NODE_INFLUX_TOKEN'
`)
	setConfigFileForTest(t, path)
	t.Setenv("MITHRIL_LIGHTBRINGER_INFLUXDB_URL", "https://environment-influx.example.com")

	cfg, err := resolvedConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LightbringerInfluxURL != "https://environment-influx.example.com" || cfg.LightbringerInfluxTok != "" {
		t.Fatalf("node token crossed into an environment-selected origin: %+v", cfg)
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
		t.Fatal("mithril mcp must run the server and keep the legacy serve alias hidden")
	}
	if err := MCPCmd.Args(&MCPCmd, []string{"unexpected"}); err == nil {
		t.Fatal("mithril mcp accepted a positional argument")
	}
	for _, command := range []*cobra.Command{&MCPCmd, &serveCmd} {
		for _, name := range []string{"profile", "enable-control", "approval-key-file", "systemd-unit", "systemd-scope", "systemctl-path", "approval-ttl-seconds"} {
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
	for _, want := range []string{"stdio has no authentication", "SSH identity is the", "insecure loopback only"} {
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
		ApprovalKeyFile: "/etc/mithril/mcp-approval.key",
		SystemdUnit:     "mithril-mainnet.service",
		SystemdScope:    "system",
		SystemctlPath:   "/usr/bin/systemctl",
		TTLSeconds:      45,
	}
	local, err := portableStdioConfigWithOperator("/opt/mithril", "operator", "", operator)
	if err != nil {
		t.Fatal(err)
	}
	want := "mcp --profile operator --enable-control --approval-key-file /etc/mithril/mcp-approval.key --systemd-unit mithril-mainnet.service --systemd-scope system --systemctl-path /usr/bin/systemctl --approval-ttl-seconds 45"
	if got := strings.Join(local.Args, " "); got != want {
		t.Fatalf("operator config args = %q, want %q", got, want)
	}
	remote, err := remoteStdioConfigWithOperator("node", "/opt/mithril", "operator", "", operator)
	if err != nil {
		t.Fatal(err)
	}
	for _, wantPart := range []string{"'--enable-control'", "'/etc/mithril/mcp-approval.key'", "'mithril-mainnet.service'", "'45'"} {
		if !strings.Contains(remote.Args[len(remote.Args)-1], wantPart) {
			t.Errorf("remote operator command is missing %s: %s", wantPart, remote.Args[len(remote.Args)-1])
		}
	}
}

func TestGeneratedOperatorConfigFailsClosed(t *testing.T) {
	if _, err := portableStdioConfigWithOperator("/opt/mithril", "operator", "", operatorLaunchOptions{}); err == nil || !strings.Contains(err.Error(), "requires --enable-control") {
		t.Fatalf("incomplete operator config error = %v", err)
	}
	for _, operator := range []operatorLaunchOptions{
		{Enabled: true, ApprovalKeyFile: "relative.key"},
		{Enabled: true, ApprovalKeyFile: "/secure/key", TTLSeconds: 10},
		{Enabled: true, ApprovalKeyFile: "/secure/key", TTLSet: true},
		{Enabled: true, ApprovalKeyFile: "/secure/key", SystemdScope: "global"},
		{Enabled: true, ApprovalKeyFile: "/secure/key", SystemdUnit: "mithril.target"},
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
