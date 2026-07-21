// Package mcpcmd defines the `mithril mcp` commands.
package mcpcmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Overclock-Validator/mithril/pkg/config"
	"github.com/Overclock-Validator/mithril/pkg/mcp"
	"github.com/spf13/cobra"
)

// MCPCmd runs the MCP server or one of its setup commands.
var MCPCmd = cobra.Command{
	Use:           "mcp",
	Short:         "Expose Mithril node operations over MCP",
	Args:          cobra.NoArgs,
	SilenceErrors: true,
	SilenceUsage:  true,
	RunE:          runServe,
	Long: `Run a Model Context Protocol (MCP) server that exposes this node's
metrics, RPC, logs, state, replay timings, pprof, and the Lightbringer sidecar
as typed tools.

An MCP client launches the stdio server as a child process. It is not a daemon
or listening service. For remote use, launch "ssh -T NODE mithril mcp" as the
client's SSH remote command.

Any stdio-capable MCP client can use it. Run "mithril mcp config" for a
common command-and-arguments entry.

The default monitor profile exposes bounded observation tools. Diagnostic adds
raw metrics, pprof, account inspection, and transaction simulation. Operator
replaces those diagnostic tools with fixed-unit service lifecycle controls.
Operator controls must be explicitly enabled and each action requires a
separate short-lived, interactive approval. No tool changes ledger or account
state.

Operator mode runs systemctl directly. Create the approval key as the same OS
user that launches MCP. User scope controls that user's service manager. System
scope requires that user to already have noninteractive permission for the
fixed unit; MCP never invokes sudo or handles authorization prompts.

MCP stdio has no authentication. Process launch or SSH identity is the
authorization boundary.

Defaults match a standard node and use read-only path discovery. Override them
with --config or environment variables. File paths must be absolute because MCP
clients may launch from another directory. Environment variables take precedence:
  MITHRIL_METRICS_URL   (default http://127.0.0.1:9090/metrics)
  MITHRIL_RPC_URL       (default http://127.0.0.1:8899)
  MITHRIL_PPROF_URL     (default http://127.0.0.1:6060)
  MITHRIL_LOG_DIR, MITHRIL_ACCOUNTS_PATH, MITHRIL_SNAPSHOTS_PATH
  MITHRIL_SHREDSTORE_PATH, MITHRIL_STATE_PATH, MITHRIL_REPLAY_PATH
    (default: effective storage layout)
  MITHRIL_NODE_CGROUP_PATH (optional cgroup-v2 directory)
  MITHRIL_BLOCK_SOURCE, MITHRIL_LIGHTBRINGER_QUIET
  MITHRIL_REFERENCE_RPC_URL (optional trusted external Solana RPC)
  MITHRIL_LIGHTBRINGER_GRPC_ADDR (default 127.0.0.1:3001; insecure loopback only)
  MITHRIL_LIGHTBRINGER_INFLUXDB_URL, MITHRIL_LIGHTBRINGER_INFLUXDB_DATABASE
  MITHRIL_LIGHTBRINGER_INFLUXDB_TOKEN (secret; never reported by MCP)
  MITHRIL_REPLAY_P99_WARN_MS, MITHRIL_SLOTS_BEHIND_WARN
  MITHRIL_DISK_WARN_PERCENT, MITHRIL_DISK_CRITICAL_PERCENT
  MITHRIL_MCP_PROFILE   (monitor, diagnostic, or operator; default monitor)
  MITHRIL_MCP_CONTROL_ENABLED, MITHRIL_MCP_SYSTEMD_UNIT, MITHRIL_MCP_SYSTEMD_SCOPE
  MITHRIL_MCP_SYSTEMCTL_PATH, MITHRIL_MCP_APPROVAL_KEY_FILE
  MITHRIL_MCP_APPROVAL_TTL_SECONDS
  MITHRIL_MCP_MAX_CONCURRENT, MITHRIL_MCP_RATE_PER_SECOND, MITHRIL_MCP_RATE_BURST
  MITHRIL_MCP_OUTPUT_BUDGET_BYTES`,
	Example: `  # Print a common entry for any stdio MCP client:
  mithril mcp config

  # Print an SSH-backed entry for a remote node:
  mithril mcp config --ssh node-alias --remote-binary /absolute/path/to/mithril

  # Run the stdio server directly (normally the client launches this):
  mithril mcp

  # Create a key, then print a complete operator-profile entry:
  mithril mcp init-approval-key /absolute/path/to/approval.key
  mithril mcp config --profile operator --enable-control \
    --approval-key-file /absolute/path/to/approval.key`,
}

var serveCmd = cobra.Command{
	Use:           "serve",
	Short:         "Run the MCP server over stdio",
	Long:          "Serve MCP over stdio until the launching client disconnects. For remote use, run this as the SSH remote command. --profile overrides MITHRIL_MCP_PROFILE.",
	Hidden:        true,
	Args:          cobra.NoArgs,
	SilenceErrors: true,
	SilenceUsage:  true,
	RunE:          runServe,
}

func runServe(cmd *cobra.Command, _ []string) error {
	cfg, err := resolvedServeConfig(cmd)
	if err != nil {
		return err
	}
	return mcp.Serve(cmd.Context(), cfg)
}

var serveProfile string

type operatorLaunchOptions struct {
	Enabled         bool
	ApprovalKeyFile string
	SystemdUnit     string
	SystemdScope    string
	SystemctlPath   string
	TTLSeconds      uint64
	TTLSet          bool
}

func (o operatorLaunchOptions) hasValues() bool {
	return o.Enabled || o.ApprovalKeyFile != "" || o.SystemdUnit != "" || o.SystemdScope != "" || o.SystemctlPath != "" || o.TTLSeconds != 0 || o.TTLSet
}

var serveOperator operatorLaunchOptions

func applyServeOperatorFlags(cmd *cobra.Command, cfg mcp.Config) (mcp.Config, error) {
	changed := false
	for _, name := range []string{"enable-control", "approval-key-file", "systemd-unit", "systemd-scope", "systemctl-path", "approval-ttl-seconds"} {
		changed = changed || cmd.Flags().Changed(name)
	}
	if !changed {
		return cfg, nil
	}
	if cfg.Profile != mcp.ProfileOperator {
		return mcp.Config{}, errors.New("operator control flags require --profile operator")
	}
	if cmd.Flags().Changed("enable-control") {
		cfg.ControlEnabled = serveOperator.Enabled
	}
	if cmd.Flags().Changed("approval-key-file") {
		cfg.ApprovalKeyPath = serveOperator.ApprovalKeyFile
	}
	if cmd.Flags().Changed("systemd-unit") {
		cfg.SystemdUnit = serveOperator.SystemdUnit
	}
	if cmd.Flags().Changed("systemd-scope") {
		cfg.SystemdScope = serveOperator.SystemdScope
	}
	if cmd.Flags().Changed("systemctl-path") {
		cfg.SystemctlPath = serveOperator.SystemctlPath
	}
	return cfg, nil
}

type stdioConfigEntry struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

var configCmd = cobra.Command{
	Use:           "config",
	Short:         "Print a common stdio MCP server entry",
	SilenceErrors: true,
	SilenceUsage:  true,
	Long: `Print a common client-neutral JSON server entry for MCP hosts that support
stdio. MCP does not standardize host configuration files, so wrap its command
and args fields as required by your client. This command only prints JSON; it
does not modify client configuration.

With --ssh, the entry launches Mithril on a remote node through SSH. The remote
binary must be an absolute path. Use --remote-config for a config file on that
node; the root --config flag always refers to a local file.

An operator entry must include --profile operator, --enable-control, and an
absolute --approval-key-file on the MCP server host. Create that key first with
"mithril mcp init-approval-key" as the same OS user that launches MCP. User
scope targets that user's service manager. System scope requires existing
noninteractive permission for the fixed unit.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		operator := configOperator
		operator.TTLSet = cmd.Flags().Changed("approval-ttl-seconds")
		var entry stdioConfigEntry
		if configSSHTarget != "" || configRemoteBinary != "" || configRemoteConfig != "" {
			if configSSHTarget == "" {
				return errors.New("--remote-binary and --remote-config require --ssh")
			}
			if config.ConfigFile != "" {
				return errors.New("local --config cannot be combined with --ssh; use --remote-config")
			}
			var err error
			entry, err = remoteStdioConfigWithOperator(configSSHTarget, configRemoteBinary, configProfile, configRemoteConfig, operator)
			if err != nil {
				return err
			}
		} else {
			rawExecutable, err := currentExecutable()
			if err != nil {
				return fmt.Errorf("locate Mithril executable: %w", err)
			}
			entry, err = portableStdioConfigWithOperator(rawExecutable, configProfile, config.ConfigFile, operator)
			if err != nil {
				return err
			}
		}
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(entry)
	},
}

var (
	configProfile      string
	configSSHTarget    string
	configRemoteBinary string
	configRemoteConfig string
	configOperator     operatorLaunchOptions
)

var currentExecutable = os.Executable

func durableExecutablePath(raw string) (string, error) {
	executable, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("resolve Mithril executable path: %w", err)
	}
	if strings.Contains(filepath.ToSlash(executable), "/go-build") || pathWithinConfiguredGoCache(executable) {
		return "", errors.New("cannot configure an ephemeral 'go run' binary; build or install Mithril first")
	}
	return executable, nil
}

func pathWithinConfiguredGoCache(path string) bool {
	cache := os.Getenv("GOCACHE")
	if cache == "" || cache == "off" {
		return false
	}
	cache, err := filepath.Abs(cache)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(cache, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func portableStdioConfigWithOperator(rawExecutable, rawProfile, rawConfigPath string, operator operatorLaunchOptions) (stdioConfigEntry, error) {
	executable, err := durableExecutablePath(rawExecutable)
	if err != nil {
		return stdioConfigEntry{}, err
	}
	args := []string{"mcp"}
	if rawConfigPath != "" {
		configPath, err := filepath.Abs(rawConfigPath)
		if err != nil {
			return stdioConfigEntry{}, fmt.Errorf("resolve config path: %w", err)
		}
		args = append(args, "--config", configPath)
	}
	if rawProfile != "" {
		profile, err := mcp.ParseProfile(rawProfile)
		if err != nil {
			return stdioConfigEntry{}, err
		}
		args = append(args, "--profile", string(profile))
	}
	operatorArgs, err := operatorServeArgs(rawProfile, operator, false)
	if err != nil {
		return stdioConfigEntry{}, err
	}
	args = append(args, operatorArgs...)
	return stdioConfigEntry{Command: executable, Args: args}, nil
}

func remoteStdioConfigWithOperator(target, remoteBinary, rawProfile, remoteConfig string, operator operatorLaunchOptions) (stdioConfigEntry, error) {
	if err := validateSSHTarget(target); err != nil {
		return stdioConfigEntry{}, err
	}
	if err := validateRemoteAbsolutePath("--remote-binary", remoteBinary); err != nil {
		return stdioConfigEntry{}, err
	}
	if remoteConfig != "" {
		if err := validateRemoteAbsolutePath("--remote-config", remoteConfig); err != nil {
			return stdioConfigEntry{}, err
		}
	}

	remoteArgs := []string{remoteBinary, "mcp"}
	if remoteConfig != "" {
		remoteArgs = append(remoteArgs, "--config", remoteConfig)
	}
	if rawProfile != "" {
		profile, err := mcp.ParseProfile(rawProfile)
		if err != nil {
			return stdioConfigEntry{}, err
		}
		remoteArgs = append(remoteArgs, "--profile", string(profile))
	}
	operatorArgs, err := operatorServeArgs(rawProfile, operator, true)
	if err != nil {
		return stdioConfigEntry{}, err
	}
	remoteArgs = append(remoteArgs, operatorArgs...)

	quoted := make([]string, len(remoteArgs))
	for i, arg := range remoteArgs {
		quoted[i] = posixShellQuote(arg)
	}
	remoteCommand := "exec " + strings.Join(quoted, " ")
	// Preserve host aliases and identity settings, but isolate this stdio session
	// from inherited forwarding, multiplexing, and command/lifecycle overrides.
	return stdioConfigEntry{
		Command: "ssh",
		Args: []string{
			"-T", "-a", "-x",
			"-o", "IgnoreUnknown=ForkAfterAuthentication,SessionType,StdinNull,RemoteCommand",
			"-o", "BatchMode=yes",
			"-o", "ClearAllForwardings=yes",
			"-o", "Tunnel=no",
			"-o", "PermitLocalCommand=no",
			"-o", "StdinNull=no",
			"-o", "ForkAfterAuthentication=no",
			"-o", "SessionType=default",
			"-o", "RemoteCommand=none",
			"-o", "ControlPath=none",
			"--", target, remoteCommand,
		},
	}, nil
}

func operatorServeArgs(rawProfile string, operator operatorLaunchOptions, remote bool) ([]string, error) {
	profile := mcp.ProfileMonitor
	if rawProfile != "" {
		parsed, err := mcp.ParseProfile(rawProfile)
		if err != nil {
			return nil, err
		}
		profile = parsed
	}
	if profile != mcp.ProfileOperator {
		if operator.hasValues() {
			return nil, errors.New("operator control options require --profile operator")
		}
		return nil, nil
	}
	if !operator.Enabled || operator.ApprovalKeyFile == "" {
		return nil, errors.New("operator config requires --enable-control and --approval-key-file")
	}
	validatePath := func(name, value string) error {
		if remote {
			return validateRemoteAbsolutePath(name, value)
		}
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return fmt.Errorf("%s must be a clean absolute path", name)
		}
		return nil
	}
	if err := validatePath("--approval-key-file", operator.ApprovalKeyFile); err != nil {
		return nil, err
	}
	if operator.SystemdScope != "" && operator.SystemdScope != "system" && operator.SystemdScope != "user" {
		return nil, errors.New("--systemd-scope must be system or user")
	}
	if operator.SystemdUnit != "" {
		if err := mcp.ValidateSystemdServiceUnit(operator.SystemdUnit); err != nil {
			return nil, fmt.Errorf("--systemd-unit: %w", err)
		}
	}
	if operator.SystemctlPath != "" {
		if err := validatePath("--systemctl-path", operator.SystemctlPath); err != nil {
			return nil, err
		}
	}
	if (operator.TTLSet || operator.TTLSeconds != 0) && (operator.TTLSeconds < mcp.MinApprovalTTLSeconds || operator.TTLSeconds > mcp.MaxApprovalTTLSeconds) {
		return nil, fmt.Errorf("--approval-ttl-seconds must be between %d and %d", mcp.MinApprovalTTLSeconds, mcp.MaxApprovalTTLSeconds)
	}
	args := []string{"--enable-control", "--approval-key-file", operator.ApprovalKeyFile}
	if operator.SystemdUnit != "" {
		args = append(args, "--systemd-unit", operator.SystemdUnit)
	}
	if operator.SystemdScope != "" {
		args = append(args, "--systemd-scope", operator.SystemdScope)
	}
	if operator.SystemctlPath != "" {
		args = append(args, "--systemctl-path", operator.SystemctlPath)
	}
	if operator.TTLSeconds != 0 {
		args = append(args, "--approval-ttl-seconds", strconv.FormatUint(operator.TTLSeconds, 10))
	}
	return args, nil
}

func validateSSHTarget(target string) error {
	if target == "" || strings.HasPrefix(target, "-") {
		return errors.New("--ssh must be a non-empty SSH host or alias")
	}
	if strings.Count(target, "@") > 1 {
		return errors.New("--ssh must contain at most one user separator")
	}
	host := target
	if separator := strings.IndexByte(target, '@'); separator >= 0 {
		if separator == 0 || separator == len(target)-1 {
			return errors.New("--ssh user and host must both be non-empty")
		}
		host = target[separator+1:]
	}
	if strings.ContainsAny(host, "[]") &&
		!(strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") && strings.Count(host, "[") == 1 && strings.Count(host, "]") == 1) {
		return errors.New("--ssh has invalid host brackets")
	}
	if strings.HasPrefix(host, "-") {
		return errors.New("--ssh host cannot start with '-'")
	}
	for _, r := range target {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("._-@:+%[]", r):
		default:
			return errors.New("--ssh contains an unsupported character")
		}
	}
	if strings.IndexFunc(host, func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
	}) < 0 {
		return errors.New("--ssh must contain a host or alias name")
	}
	return nil
}

func validateRemoteAbsolutePath(flagName, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required and must be an absolute path", flagName)
	}
	if !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("%s contains an unsupported character", flagName)
	}
	if !path.IsAbs(value) || path.Clean(value) != value || value == "/" {
		return fmt.Errorf("%s must be a clean absolute path", flagName)
	}
	return nil
}

func posixShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func resolvedServeConfig(cmd *cobra.Command) (mcp.Config, error) {
	var overrides resolvedConfigOverrides
	if serveProfile != "" {
		profile, err := mcp.ParseProfile(serveProfile)
		if err != nil {
			return mcp.Config{}, err
		}
		overrides.Profile = &profile
	}
	if cmd.Flags().Changed("approval-ttl-seconds") {
		if serveOperator.TTLSeconds < mcp.MinApprovalTTLSeconds || serveOperator.TTLSeconds > mcp.MaxApprovalTTLSeconds {
			return mcp.Config{}, fmt.Errorf("--approval-ttl-seconds must be between %d and %d", mcp.MinApprovalTTLSeconds, mcp.MaxApprovalTTLSeconds)
		}
		overrides.ApprovalTTLSeconds = &serveOperator.TTLSeconds
	}
	cfg, err := resolvedConfigWithOverrides(overrides)
	if err != nil {
		return mcp.Config{}, err
	}
	return applyServeOperatorFlags(cmd, cfg)
}

func bindServeFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&serveProfile, "profile", "", "override MCP profile (monitor, diagnostic, or operator)")
	cmd.Flags().BoolVar(&serveOperator.Enabled, "enable-control", false, "enable fixed-unit lifecycle controls (operator profile only)")
	cmd.Flags().StringVar(&serveOperator.ApprovalKeyFile, "approval-key-file", "", "absolute approval key path")
	cmd.Flags().StringVar(&serveOperator.SystemdUnit, "systemd-unit", "", "fixed Mithril .service unit")
	cmd.Flags().StringVar(&serveOperator.SystemdScope, "systemd-scope", "", "systemd scope: system or user (default system)")
	cmd.Flags().StringVar(&serveOperator.SystemctlPath, "systemctl-path", "", "absolute systemctl executable path")
	cmd.Flags().Uint64Var(&serveOperator.TTLSeconds, "approval-ttl-seconds", 0, "approval lifetime in seconds (15-300)")
}

func init() {
	bindServeFlags(&MCPCmd)
	bindServeFlags(&serveCmd)
	approveCmd.Flags().StringVar(&approvalKeyFile, "approval-key-file", "", "absolute approval key path (or MITHRIL_MCP_APPROVAL_KEY_FILE)")
	configCmd.Flags().StringVar(&configProfile, "profile", "", "include an explicit MCP profile (monitor, diagnostic, or operator)")
	configCmd.Flags().StringVar(&configSSHTarget, "ssh", "", "emit an SSH-backed entry for this host or SSH alias")
	configCmd.Flags().StringVar(&configRemoteBinary, "remote-binary", "", "absolute path to the Mithril binary used with --ssh")
	configCmd.Flags().StringVar(&configRemoteConfig, "remote-config", "", "absolute node config path used with --ssh")
	configCmd.Flags().BoolVar(&configOperator.Enabled, "enable-control", false, "include fixed-unit lifecycle controls (operator profile only)")
	configCmd.Flags().StringVar(&configOperator.ApprovalKeyFile, "approval-key-file", "", "absolute approval key path on the MCP server host")
	configCmd.Flags().StringVar(&configOperator.SystemdUnit, "systemd-unit", "", "fixed Mithril .service unit")
	configCmd.Flags().StringVar(&configOperator.SystemdScope, "systemd-scope", "", "systemd scope: system or user (default system)")
	configCmd.Flags().StringVar(&configOperator.SystemctlPath, "systemctl-path", "", "absolute systemctl executable path on the MCP server host")
	configCmd.Flags().Uint64Var(&configOperator.TTLSeconds, "approval-ttl-seconds", 0, "approval lifetime in seconds (15-300)")
	MCPCmd.AddCommand(&serveCmd)
	MCPCmd.AddCommand(&approveCmd)
	MCPCmd.AddCommand(&initApprovalKeyCmd)
	MCPCmd.AddCommand(&configCmd)
}
