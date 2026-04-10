package dashboardcmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/tui"
	"github.com/spf13/viper"
)

// ── State file reading ──────────────────────────────────────────────────

type nodeState struct {
	LastSlot           uint64 `json:"last_slot"`
	LastEpoch          uint64 `json:"last_epoch"`
	LastBankhash       string `json:"last_bankhash"`
	SnapshotSlot       uint64 `json:"snapshot_slot"`
	Stage              string `json:"stage"`
	LastShutdownReason string `json:"last_shutdown_reason"`
	LastShutdownAt     string `json:"last_shutdown_at"`
	CurrentRunID       string `json:"current_run_id"`
	LastWriterVersion  string `json:"last_writer_version"`
	LastWriterCommit   string `json:"last_writer_commit"`
	Cluster            string `json:"cluster"`
}

func readState(accountsPath string) *nodeState {
	stateFile := filepath.Join(accountsPath, "mithril_state.json")
	f, err := os.Open(stateFile)
	if err != nil {
		return nil
	}
	defer f.Close()

	// Cap at 1MB to prevent OOM from corrupted state files
	data := make([]byte, 1<<20)
	n, err := f.Read(data)
	if err != nil && n == 0 {
		return nil
	}

	var s nodeState
	if err := json.Unmarshal(data[:n], &s); err != nil {
		return nil
	}
	return &s
}

// ── Config reading ──────────────────────────────────────────────────────

type configData struct {
	cluster       string
	rpcEndpoints  []string
	blockSource   string
	lbEnabled     bool
	lbGossip      string
	lbGrpcAddr    string
	lbRpcAddr     string
	accountsPath  string
	snapshotsPath string
	shredstorePath string
	logsPath      string
	txpar         string
	blockMaxRPS   string
	blockInflight string
	rpcPort       string
	logLevel      string
	bootstrapMode string
}

func readConfig(configFile string) *configData {
	v := viper.New()
	v.SetConfigFile(configFile)
	if err := v.ReadInConfig(); err != nil {
		return nil
	}

	cluster := v.GetString("network.cluster")
	if cluster == "" {
		cluster = "unknown"
	}

	txpar := v.GetString("tuning.txpar")
	if txpar == "" {
		txpar = v.GetString("replay.txpar")
	}
	// Don't fill in a computed default — let the UI show "auto" when empty

	logsPath := v.GetString("storage.logs")
	if logsPath == "" {
		logsPath = v.GetString("log.dir")
	}

	return &configData{
		cluster:        cluster,
		rpcEndpoints:   v.GetStringSlice("network.rpc"),
		blockSource:    v.GetString("block.source"),
		lbEnabled:      v.GetBool("lightbringer.enabled"),
		lbGossip:       v.GetString("lightbringer.gossip_entrypoint"),
		lbGrpcAddr:     v.GetString("lightbringer.grpc_addr"),
		lbRpcAddr:      v.GetString("lightbringer.rpc_addr"),
		accountsPath:   v.GetString("storage.accounts"),
		snapshotsPath:  v.GetString("storage.snapshots"),
		shredstorePath: v.GetString("storage.shredstore"),
		logsPath:       logsPath,
		txpar:          txpar,
		blockMaxRPS:    v.GetString("block.max_rps"),
		blockInflight:  v.GetString("block.max_inflight"),
		rpcPort:        v.GetString("rpc.port"),
		logLevel:       v.GetString("log.level"),
		bootstrapMode:  v.GetString("bootstrap.mode"),
	}
}

// ── Service probing ─────────────────────────────────────────────────────

type serviceStatus struct {
	name    string
	addr    string
	up      bool
}

// Default service addresses (must match config template defaults).
const (
	defaultRPCPort  = "8899"
	defaultLBGRPC   = "127.0.0.1:3001"
	defaultLBHTTP   = "127.0.0.1:3000"
)

func probeServices(cfg *configData) []serviceStatus {
	rpcPort := defaultRPCPort
	grpcAddr := defaultLBGRPC
	httpAddr := defaultLBHTTP

	if cfg != nil {
		if cfg.rpcPort != "" && cfg.rpcPort != "0" {
			rpcPort = cfg.rpcPort
		}
		if cfg.lbGrpcAddr != "" {
			grpcAddr = cfg.lbGrpcAddr
		}
		if cfg.lbRpcAddr != "" {
			httpAddr = cfg.lbRpcAddr
		}
	}

	services := []serviceStatus{
		{name: "Mithril RPC", addr: "127.0.0.1:" + rpcPort},
	}
	// Only probe lightbringer services when enabled
	if cfg != nil && cfg.lbEnabled {
		services = append(services,
			serviceStatus{name: "LB gRPC", addr: grpcAddr},
			serviceStatus{name: "LB HTTP", addr: httpAddr},
		)
	}

	// Probe all services concurrently to avoid 6s blocking when endpoints are down
	var wg sync.WaitGroup
	for i := range services {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			conn, err := net.DialTimeout("tcp", services[idx].addr, 2*time.Second)
			if err == nil {
				conn.Close()
				services[idx].up = true
			}
		}(i)
	}
	wg.Wait()

	return services
}

// ── Disk usage ──────────────────────────────────────────────────────────

type diskUsage struct {
	label string
	path  string
	used  uint64 // GB
	total uint64 // GB
	pct   int
}

func getDiskUsage(cfg *configData) []diskUsage {
	if cfg == nil {
		return nil
	}

	paths := []struct {
		label string
		path  string
	}{
		{"accounts", cfg.accountsPath},
		{"snapshots", cfg.snapshotsPath},
		{"shredstore", cfg.shredstorePath},
		{"logs", cfg.logsPath},
	}

	var results []diskUsage
	for _, p := range paths {
		if p.path == "" {
			continue
		}
		du := getDiskUsageForPath(p.label, p.path)
		if du != nil {
			results = append(results, *du)
		}
	}
	return results
}

func getDiskUsageForPath(label, path string) *diskUsage {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return nil
	}

	out, err := exec.Command("df", "-BG", "--", path).Output()
	if err != nil {
		// Try without -BG for macOS
		out, err = exec.Command("df", "-g", "--", path).Output()
		if err != nil {
			return nil
		}
	}

	lines := strings.Split(string(out), "\n")
	if len(lines) < 2 {
		return nil
	}

	fields := strings.Fields(lines[1])
	if len(fields) < 5 {
		return nil
	}

	total := parseGB(fields[1])
	used := parseGB(fields[2])
	pct := 0
	if total > 0 {
		pct = int(float64(used) / float64(total) * 100)
	}

	return &diskUsage{
		label: label,
		path:  path,
		used:  used,
		total: total,
		pct:   pct,
	}
}

func parseGB(s string) uint64 {
	s = strings.TrimSuffix(s, "G")
	s = strings.TrimSpace(s)
	n, _ := strconv.ParseUint(s, 10, 64)
	return n
}

// ── Doctor checks (structured) ──────────────────────────────────────────

type checkResult struct {
	name   string
	status string // "pass", "warn", "fail"
	msg    string
}

func runDoctorChecks(configFile string, cfg *configData) []checkResult {
	var results []checkResult

	// Config file
	if _, err := os.Stat(configFile); err != nil {
		results = append(results, checkResult{"Config file", "fail", "not found: " + configFile})
		return results
	}
	results = append(results, checkResult{"Config file", "pass", "found (" + configFile + ")"})

	if cfg == nil {
		results = append(results, checkResult{"Config parse", "fail", "could not parse config"})
		return results
	}

	// Cluster
	if cfg.cluster != "" && cfg.cluster != "unknown" {
		results = append(results, checkResult{"Network", "pass", cfg.cluster})
	} else {
		results = append(results, checkResult{"Network", "fail", "cluster not set"})
	}

	// RPC
	if len(cfg.rpcEndpoints) > 0 {
		results = append(results, checkResult{"RPC endpoint", "pass", cfg.rpcEndpoints[0]})
	} else {
		results = append(results, checkResult{"RPC endpoint", "fail", "no RPC endpoints configured"})
	}

	// AccountsDB path
	if cfg.accountsPath != "" {
		if _, err := os.Stat(cfg.accountsPath); err == nil {
			results = append(results, checkResult{"AccountsDB path", "pass", cfg.accountsPath})
		} else {
			results = append(results, checkResult{"AccountsDB path", "warn", cfg.accountsPath + " (will be created)"})
		}
	} else {
		results = append(results, checkResult{"AccountsDB path", "fail", "not set"})
	}

	// Lightbringer
	if cfg.lbEnabled {
		// Check binary
		binaryPath := "./lightbringer"
		if _, err := os.Stat(binaryPath); err == nil {
			results = append(results, checkResult{"Lightbringer binary", "pass", binaryPath})
		} else {
			results = append(results, checkResult{"Lightbringer binary", "fail", "not found at " + binaryPath})
		}

		// Check gossip format
		if cfg.lbGossip != "" {
			if _, _, err := net.SplitHostPort(cfg.lbGossip); err != nil {
				results = append(results, checkResult{"Gossip entrypoint", "fail", "invalid format: " + cfg.lbGossip})
			} else {
				results = append(results, checkResult{"Gossip entrypoint", "pass", cfg.lbGossip})
			}
		} else {
			results = append(results, checkResult{"Gossip entrypoint", "fail", "not set"})
		}
	} else {
		results = append(results, checkResult{"Lightbringer", "pass", "disabled"})
	}

	// Logs
	if cfg.logsPath != "" {
		results = append(results, checkResult{"Log directory", "pass", cfg.logsPath})
	} else {
		results = append(results, checkResult{"Log directory", "warn", "not configured (logs to stderr)"})
	}

	return results
}

// ── Log tailing ─────────────────────────────────────────────────────────

func readLogTail(logsPath string, filename string, maxLines int) []string {
	if logsPath == "" {
		return []string{"(no log directory configured)"}
	}

	// Validate filename is a simple base name (no path separators)
	if filename != filepath.Base(filename) || filename == "" {
		return []string{"(invalid log filename)"}
	}

	latestDir := filepath.Join(logsPath, "latest")
	target, err := os.Readlink(latestDir)
	if err != nil {
		return []string{"(no latest run found in " + logsPath + ")"}
	}

	// Validate symlink target — reject traversal and absolute paths
	if strings.Contains(target, "..") || filepath.IsAbs(target) {
		return []string{"(invalid latest symlink)"}
	}

	logFile := filepath.Clean(filepath.Join(logsPath, target, filename))

	// Verify resolved path is still under logsPath
	cleanLogs := filepath.Clean(logsPath) + string(os.PathSeparator)
	if !strings.HasPrefix(logFile, cleanLogs) {
		return []string{"(log path escapes directory)"}
	}

	// Read only the tail of the file to avoid loading huge logs into memory
	f, err := os.Open(logFile)
	if err != nil {
		return []string{"(could not read " + logFile + ")"}
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return []string{"(could not stat " + logFile + ")"}
	}

	// Read last 64KB — enough for ~500 log lines
	const tailSize = 64 * 1024
	offset := stat.Size() - tailSize
	if offset < 0 {
		offset = 0
	}

	buf := make([]byte, stat.Size()-offset)
	_, err = f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return []string{"(read error: " + err.Error() + ")"}
	}

	lines := strings.Split(string(buf), "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return lines
}

// ── Config saving ───────────────────────────────────────────────────────

// saveConfigValue writes a single config field to the TOML file.
func saveConfigValue(configFile, section, key, value string) error {
	data, err := os.ReadFile(configFile)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	content := string(data)

	// Determine TOML value format based on field type
	fullKey := section + "." + key
	var tomlValue string
	switch {
	case fullKey == "block.max_rps" || fullKey == "block.max_inflight" ||
		fullKey == "tuning.txpar" || fullKey == "rpc.port":
		tomlValue = value // numeric — no quoting
	case fullKey == "lightbringer.enabled":
		tomlValue = value // boolean — no quoting
	case fullKey == "network.rpc":
		tomlValue = fmt.Sprintf("[%q]", value) // string array
	default:
		tomlValue = fmt.Sprintf("%q", value) // quoted string
	}

	content = setTomlValueInline(content, section, key, tomlValue)
	if err := tui.AtomicWriteFile(configFile, []byte(content), 0600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// setTomlValueInline replaces a value in a TOML file, preserving structure.
func setTomlValueInline(content, section, key, value string) string {
	lines := strings.Split(content, "\n")
	inSection := false
	sectionFound := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && !strings.HasPrefix(trimmed, "[[") {
			if inSection {
				// Section found but key missing — insert before next section header
				result := make([]string, 0, len(lines)+1)
				result = append(result, lines[:i]...)
				result = append(result, key+" = "+value)
				result = append(result, lines[i:]...)
				return strings.Join(result, "\n")
			}
			sectionName := strings.Trim(trimmed, "[] ")
			inSection = sectionName == section
			if inSection {
				sectionFound = true
			}
			continue
		}
		if inSection && (strings.HasPrefix(trimmed, key+" ") || strings.HasPrefix(trimmed, key+"=")) {
			indent := ""
			for _, c := range line {
				if c == ' ' || c == '\t' {
					indent += string(c)
				} else {
					break
				}
			}
			lines[i] = fmt.Sprintf("%s%s = %s", indent, key, value)
			return strings.Join(lines, "\n")
		}
	}
	// If section was the last one (no next header), append key
	if inSection {
		lines = append(lines, key+" = "+value)
		return strings.Join(lines, "\n")
	}
	// Section not found at all — append new section with key
	if !sectionFound {
		lines = append(lines, "", "["+section+"]", key+" = "+value)
		return strings.Join(lines, "\n")
	}
	return content
}
