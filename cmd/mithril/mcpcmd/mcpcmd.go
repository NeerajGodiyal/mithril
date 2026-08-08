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
	"golang.org/x/term"
)

// MCPCmd runs the MCP server or one of its setup commands.
var MCPCmd = cobra.Command{
	Use:           "mcp",
	Short:         "Expose Mithril node operations over MCP",
	Args:          cobra.NoArgs,
	SilenceErrors: true,
	SilenceUsage:  true,
	RunE:          runServe,
	Long: `Run MCP over stdio so a compatible client can inspect Mithril's RPC,
metrics, logs, state, and replay data.

An MCP client launches the stdio server as a child process. It is not a daemon
or listening service. For remote use, make "ssh -T NODE mithril mcp" the
client's SSH remote command.

The default monitor profile is read-only. Diagnostic adds profiling and
simulation tools. Operator adds approved lifecycle controls for one fixed
service; it does not expose ledger or account writes.

Any stdio-capable MCP client can use it. Run "mithril mcp config" to print a
client-neutral command-and-arguments entry. File paths must be absolute because
clients may launch the server from another directory.

MCP stdio has no authentication of its own, so local process access or SSH
identity is the authorization boundary.`,
	Example: `  # Local node
  mithril mcp config

  # Remote node
  mithril mcp config --ssh node-alias --remote-binary /absolute/path/to/mithril`,
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

var interactiveStdio = func() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

func runServe(cmd *cobra.Command, _ []string) error {
	cfg, err := resolvedServeConfig(cmd)
	if err != nil {
		return err
	}
	if cmd.Name() == "mcp" && interactiveStdio() {
		if err := mcp.ValidateServeConfig(cfg); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), `Mithril MCP is a stdio server launched by an MCP client; it is not an interactive shell.
Run "mithril mcp config" to print a client-neutral configuration entry.`)
		return nil
	}
	return mcp.Serve(cmd.Context(), cfg)
}

type operatorLaunchOptions struct {
	Enabled                bool
	AllowedActions         []string
	ApproverKeysDir        string
	ApproverHistoryKeysDir string
	ControlTargetID        string
	SystemdUnit            string
	SystemdScope           string
	SystemctlPath          string
	TTLSeconds             uint64
	TTLSet                 bool
	ControlStateDir        string
	AuditConfigPath        string
}

func (o operatorLaunchOptions) hasValues() bool {
	return o.Enabled || len(o.AllowedActions) > 0 || o.ApproverKeysDir != "" ||
		o.ApproverHistoryKeysDir != "" ||
		o.ControlTargetID != "" || o.SystemdUnit != "" || o.SystemdScope != "" ||
		o.SystemctlPath != "" || o.TTLSeconds != 0 || o.TTLSet ||
		o.ControlStateDir != "" || o.AuditConfigPath != ""
}

func applyServeOperatorFlags(cmd *cobra.Command, cfg mcp.Config) (mcp.Config, error) {
	options, err := serveOperatorOptions(cmd)
	if err != nil {
		return mcp.Config{}, err
	}
	changed := false
	for _, name := range []string{"enable-control", "allow-action", "approver-keys-dir", "approver-history-keys-dir", "control-target-id", "systemd-unit", "systemd-scope", "systemctl-path", "approval-ttl-seconds", "control-state-dir", "audit-client-config"} {
		changed = changed || cmd.Flags().Changed(name)
	}
	if !changed {
		return cfg, nil
	}
	if cfg.Profile != mcp.ProfileOperator {
		return mcp.Config{}, errors.New("operator control flags require --profile operator")
	}
	if cmd.Flags().Changed("enable-control") {
		cfg.ControlEnabled = options.Enabled
	}
	if cmd.Flags().Changed("allow-action") {
		actions, err := mcp.NormalizeServiceActions(options.AllowedActions)
		if err != nil {
			return mcp.Config{}, fmt.Errorf("--allow-action: %w", err)
		}
		cfg.AllowedServiceActions = actions
	}
	activeKeysChanged := cmd.Flags().Changed("approver-keys-dir")
	historyKeysChanged := cmd.Flags().Changed("approver-history-keys-dir")
	historyKeysFollowActive := cfg.ApproverHistoryKeysDir == "" ||
		cfg.ApproverHistoryKeysDir == cfg.ApproverKeysDir
	if raw, configured := os.LookupEnv(
		"MITHRIL_MCP_APPROVER_HISTORY_KEYS_DIR",
	); configured && raw != "" {
		historyKeysFollowActive = false
	}
	if activeKeysChanged {
		cfg.ApproverKeysDir = options.ApproverKeysDir
		if !historyKeysChanged && historyKeysFollowActive {
			cfg.ApproverHistoryKeysDir = cfg.ApproverKeysDir
		}
	}
	if historyKeysChanged {
		cfg.ApproverHistoryKeysDir = options.ApproverHistoryKeysDir
	}
	if cmd.Flags().Changed("control-target-id") {
		cfg.ControlTargetID = options.ControlTargetID
	}
	if cmd.Flags().Changed("systemd-unit") {
		cfg.SystemdUnit = options.SystemdUnit
	}
	if cmd.Flags().Changed("systemd-scope") {
		cfg.SystemdScope = options.SystemdScope
	}
	if cmd.Flags().Changed("systemctl-path") {
		cfg.SystemctlPath = options.SystemctlPath
	}
	if cmd.Flags().Changed("control-state-dir") {
		cfg.ControlStateDir = options.ControlStateDir
	}
	if cmd.Flags().Changed("audit-client-config") {
		cfg.AuditClientConfigPath = options.AuditConfigPath
	}
	return cfg, nil
}

func serveOperatorOptions(cmd *cobra.Command) (operatorLaunchOptions, error) {
	var options operatorLaunchOptions
	var err error
	if options.Enabled, err = cmd.Flags().GetBool("enable-control"); err != nil {
		return operatorLaunchOptions{}, err
	}
	if options.AllowedActions, err = cmd.Flags().GetStringArray("allow-action"); err != nil {
		return operatorLaunchOptions{}, err
	}
	if options.ApproverKeysDir, err = cmd.Flags().GetString("approver-keys-dir"); err != nil {
		return operatorLaunchOptions{}, err
	}
	if options.ApproverHistoryKeysDir, err = cmd.Flags().GetString("approver-history-keys-dir"); err != nil {
		return operatorLaunchOptions{}, err
	}
	if options.ControlTargetID, err = cmd.Flags().GetString("control-target-id"); err != nil {
		return operatorLaunchOptions{}, err
	}
	if options.SystemdUnit, err = cmd.Flags().GetString("systemd-unit"); err != nil {
		return operatorLaunchOptions{}, err
	}
	if options.SystemdScope, err = cmd.Flags().GetString("systemd-scope"); err != nil {
		return operatorLaunchOptions{}, err
	}
	if options.SystemctlPath, err = cmd.Flags().GetString("systemctl-path"); err != nil {
		return operatorLaunchOptions{}, err
	}
	if options.TTLSeconds, err = cmd.Flags().GetUint64("approval-ttl-seconds"); err != nil {
		return operatorLaunchOptions{}, err
	}
	options.TTLSet = cmd.Flags().Changed("approval-ttl-seconds")
	if options.ControlStateDir, err = cmd.Flags().GetString("control-state-dir"); err != nil {
		return operatorLaunchOptions{}, err
	}
	if options.AuditConfigPath, err = cmd.Flags().GetString("audit-client-config"); err != nil {
		return operatorLaunchOptions{}, err
	}
	return options, nil
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
node; the root --config flag always refers to a local file. Before adding the
entry to a client, verify noninteractive key authentication and known_hosts
with "ssh NODE true"; MCP cannot answer password or host-key prompts.

An operator entry must include --profile operator, --enable-control, at least
one --allow-action, an absolute --approver-keys-dir on the MCP server host, and
a stable --control-target-id. The server directory contains public keys only.
User scope targets that user's service manager. System scope requires existing
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
			"-o", "ConnectTimeout=10",
			"-o", "ServerAliveInterval=15",
			"-o", "ServerAliveCountMax=2",
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
	if !operator.Enabled || operator.ApproverKeysDir == "" || operator.ControlTargetID == "" {
		return nil, errors.New("operator config requires --enable-control, --approver-keys-dir, and --control-target-id")
	}
	actions, err := mcp.NormalizeServiceActions(operator.AllowedActions)
	if err != nil {
		return nil, fmt.Errorf("operator config requires at least one valid --allow-action: %w", err)
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
	if err := validatePath("--approver-keys-dir", operator.ApproverKeysDir); err != nil {
		return nil, err
	}
	if operator.ApproverHistoryKeysDir != "" {
		if err := validatePath(
			"--approver-history-keys-dir",
			operator.ApproverHistoryKeysDir,
		); err != nil {
			return nil, err
		}
	}
	if operator.AuditConfigPath != "" {
		if err := validatePath("--audit-client-config", operator.AuditConfigPath); err != nil {
			return nil, err
		}
	}
	if operator.ControlStateDir != "" {
		if err := validatePath("--control-state-dir", operator.ControlStateDir); err != nil {
			return nil, err
		}
	}
	if err := mcp.ValidateControlTargetID(operator.ControlTargetID); err != nil {
		return nil, fmt.Errorf("--control-target-id: %w", err)
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
	args := []string{
		"--enable-control",
		"--approver-keys-dir", operator.ApproverKeysDir,
		"--control-target-id", operator.ControlTargetID,
	}
	if operator.ApproverHistoryKeysDir != "" {
		args = append(
			args,
			"--approver-history-keys-dir",
			operator.ApproverHistoryKeysDir,
		)
	}
	for _, action := range actions {
		args = append(args, "--allow-action", action)
	}
	if operator.SystemdUnit != "" {
		args = append(args, "--systemd-unit", operator.SystemdUnit)
	}
	if operator.SystemdScope != "" {
		args = append(args, "--systemd-scope", operator.SystemdScope)
	}
	if operator.SystemctlPath != "" {
		args = append(args, "--systemctl-path", operator.SystemctlPath)
	}
	if operator.ControlStateDir != "" {
		args = append(args, "--control-state-dir", operator.ControlStateDir)
	}
	if operator.AuditConfigPath != "" {
		args = append(args, "--audit-client-config", operator.AuditConfigPath)
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
	profileName, err := cmd.Flags().GetString("profile")
	if err != nil {
		return mcp.Config{}, err
	}
	if profileName != "" {
		profile, err := mcp.ParseProfile(profileName)
		if err != nil {
			return mcp.Config{}, err
		}
		overrides.Profile = &profile
	}
	if cmd.Flags().Changed("approval-ttl-seconds") {
		ttlSeconds, err := cmd.Flags().GetUint64("approval-ttl-seconds")
		if err != nil {
			return mcp.Config{}, err
		}
		if ttlSeconds < mcp.MinApprovalTTLSeconds || ttlSeconds > mcp.MaxApprovalTTLSeconds {
			return mcp.Config{}, fmt.Errorf("--approval-ttl-seconds must be between %d and %d", mcp.MinApprovalTTLSeconds, mcp.MaxApprovalTTLSeconds)
		}
		overrides.ApprovalTTLSeconds = &ttlSeconds
	}
	cfg, err := resolvedConfigWithOverrides(overrides)
	if err != nil {
		return mcp.Config{}, err
	}
	return applyServeOperatorFlags(cmd, cfg)
}

func bindServeFlags(cmd *cobra.Command) {
	cmd.Flags().String("profile", "", "override MCP profile (monitor, diagnostic, or operator)")
	cmd.Flags().Bool("enable-control", false, "enable fixed-unit lifecycle controls (operator profile only)")
	cmd.Flags().StringArray("allow-action", nil, "allow one lifecycle action; repeat for start, stop, or restart")
	cmd.Flags().String("approver-keys-dir", "", "absolute directory of trusted approver public keys")
	cmd.Flags().String("approver-history-keys-dir", "", "absolute directory of retained historical public keys")
	cmd.Flags().String("control-target-id", "", "stable identifier for this controlled node")
	cmd.Flags().String("systemd-unit", "", "fixed Mithril .service unit")
	cmd.Flags().String("systemd-scope", "", "systemd scope: system or user (default system)")
	cmd.Flags().String("systemctl-path", "", "absolute systemctl executable path")
	cmd.Flags().Uint64("approval-ttl-seconds", 0, "approval lifetime in seconds (15-300)")
	cmd.Flags().String("control-state-dir", "", "absolute private directory for control state and local audit")
	cmd.Flags().String("audit-client-config", "", "absolute off-host audit client configuration file")
}

func init() {
	bindServeFlags(&MCPCmd)
	bindServeFlags(&serveCmd)
	approveCmd.Flags().StringVar(
		&approverPrivateKeyFile,
		"approval-key-file",
		"",
		"absolute approver private-key path (or MITHRIL_MCP_APPROVER_PRIVATE_KEY_FILE)",
	)
	configCmd.Flags().StringVar(&configProfile, "profile", "", "include an explicit MCP profile (monitor, diagnostic, or operator)")
	configCmd.Flags().StringVar(&configSSHTarget, "ssh", "", "emit an SSH-backed entry for this host or SSH alias")
	configCmd.Flags().StringVar(&configRemoteBinary, "remote-binary", "", "absolute path to the Mithril binary used with --ssh")
	configCmd.Flags().StringVar(&configRemoteConfig, "remote-config", "", "absolute node config path used with --ssh")
	configCmd.Flags().BoolVar(&configOperator.Enabled, "enable-control", false, "include fixed-unit lifecycle controls (operator profile only)")
	configCmd.Flags().StringArrayVar(&configOperator.AllowedActions, "allow-action", nil, "include one allowed lifecycle action; repeat for start, stop, or restart")
	configCmd.Flags().StringVar(&configOperator.ApproverKeysDir, "approver-keys-dir", "", "absolute public-key directory on the MCP server host")
	configCmd.Flags().StringVar(&configOperator.ApproverHistoryKeysDir, "approver-history-keys-dir", "", "absolute historical public-key directory on the MCP server host")
	configCmd.Flags().StringVar(&configOperator.ControlTargetID, "control-target-id", "", "stable identifier for the controlled node")
	configCmd.Flags().StringVar(&configOperator.SystemdUnit, "systemd-unit", "", "fixed Mithril .service unit")
	configCmd.Flags().StringVar(&configOperator.SystemdScope, "systemd-scope", "", "systemd scope: system or user (default system)")
	configCmd.Flags().StringVar(&configOperator.SystemctlPath, "systemctl-path", "", "absolute systemctl executable path on the MCP server host")
	configCmd.Flags().Uint64Var(&configOperator.TTLSeconds, "approval-ttl-seconds", 0, "approval lifetime in seconds (15-300)")
	configCmd.Flags().StringVar(&configOperator.ControlStateDir, "control-state-dir", "", "absolute private control state directory on the MCP server host")
	configCmd.Flags().StringVar(&configOperator.AuditConfigPath, "audit-client-config", "", "absolute off-host audit client configuration file on the MCP server host")
	MCPCmd.AddCommand(&serveCmd)
	MCPCmd.AddCommand(&approveCmd)
	MCPCmd.AddCommand(&initApprovalKeyCmd)
	MCPCmd.AddCommand(&configCmd)
}
