package node

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "net/http/pprof"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/arena"
	"github.com/Overclock-Validator/mithril/pkg/config"
	"github.com/Overclock-Validator/mithril/pkg/lthash"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/progress"
	"github.com/Overclock-Validator/mithril/pkg/replay"
	"github.com/Overclock-Validator/mithril/pkg/rpcserver"
	"github.com/Overclock-Validator/mithril/pkg/sbpf"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/snapshot"
	"github.com/Overclock-Validator/mithril/pkg/snapshotdl"
	"github.com/Overclock-Validator/mithril/pkg/state"
	"github.com/Overclock-Validator/mithril/pkg/statsd"
	solrpc "github.com/gagliardetto/solana-go/rpc"
	"github.com/mr-tron/base58"
	"github.com/spf13/cobra"
	"k8s.io/klog/v2"
)

var (
	VerifyRange = cobra.Command{
		Use:   "verify-range",
		Short: "Verify a range of slots from snapshot",
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return initConfigAndBindFlags(cmd)
		},
		Run: func(cmd *cobra.Command, args []string) {
			runVerifyRange(cmd, args)
		},
	}

	// Run is the main command for running Mithril as a live verifier.
	// This is the primary way most users will run Mithril.
	Run = cobra.Command{
		Use:   "run",
		Short: "Run Mithril as a live verifier (downloads snapshot, builds AccountsDB, verifies blocks)",
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return initConfigAndBindFlags(cmd)
		},
		Run: func(cmd *cobra.Command, args []string) {
			runLive(cmd, args)
		},
	}

	// VerifyLive is an alias for Run (kept for backwards compatibility)
	VerifyLive = cobra.Command{
		Use:    "verify-live",
		Short:  "Alias for 'run' (deprecated, use 'mithril run' instead)",
		Hidden: true, // Hide from help but still works
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return initConfigAndBindFlags(cmd)
		},
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("Note: 'verify-live' is deprecated. Use 'mithril run' instead.")
			runLive(cmd, args)
		},
	}

	loadFromSnapshot            bool
	loadFromAccountsDb          bool
	bootstrapMode               string // "auto", "snapshot", or "accountsdb"
	snapshotArchivePath         string
	incrementalSnapshotFilename string
	accountsPath                string
	scratchDirectory            string
	rpcEndpoints                []string
	blockSource                 string // "rpc" or "overcast"
	overcastEndpoint            string
	snapshotDlPath              string
	numReplaySlots              int64
	endSlot                     int64
	pprofPort                   int64
	blockstorePath              string
	txParallelism               int64

	debugTxs        []string
	debugAcctWrites []string
	metricsPath     string
	cpuprofPath     string

	paramArenaSizeMB         uint64
	borrowedAccountArenaSize uint64

	rpcPort int
)

func init() {
	// flags for verifier mode
	// [replay] section flags
	VerifyRange.Flags().BoolVarP(&loadFromSnapshot, "load-from-snapshot", "s", false, "Load from a full snapshot")
	VerifyRange.Flags().BoolVarP(&loadFromAccountsDb, "load-from-accounts-db", "a", false, "Load from AccountsDB")
	VerifyRange.Flags().Int64Var(&numReplaySlots, "num-slots", 0, "Number of slots to replay.")
	VerifyRange.Flags().Int64VarP(&endSlot, "end-slot", "e", -1, "Block at which to stop replaying, inclusive")
	VerifyRange.Flags().Int64Var(&txParallelism, "txpar", 0, "Set to 0 to use sequential execution, or >0 to execute a topsort tx plan with the given number of workers")

	// [ledger] section flags
	VerifyRange.Flags().StringVarP(&snapshotArchivePath, "snapshot-archive-path", "p", "", "Path of full snapshot or AccountsDB to load from")
	VerifyRange.Flags().StringVar(&incrementalSnapshotFilename, "incremental-snapshot", "", "Filename containing incremental snapshot")
	VerifyRange.Flags().StringVarP(&accountsPath, "accounts-path", "o", "", "Output path for writing AccountsDB data to")
	VerifyRange.Flags().StringVar(&blockstorePath, "ledger-path", "/tmp/blocks", "Path containing slot.json files")

	// [rpc] section flags
	VerifyRange.Flags().StringSliceVarP(&rpcEndpoints, "rpc", "r", []string{}, "URL(s) for RPC endpoint(s) - can specify multiple")
	VerifyRange.Flags().IntVar(&rpcPort, "rpc-port", 0, "RPC server port. Default off.")

	// [development] section flags
	VerifyRange.Flags().Uint64Var(&paramArenaSizeMB, "param-arena-size-mb", 512, "Size in MB for serialized parameter arena (0 to disable)")
	VerifyRange.Flags().Uint64Var(&borrowedAccountArenaSize, "borrowed-account-arena-size", 1024, "Number of borrowed accounts to preallocate in arena (0 to disable)")
	VerifyRange.Flags().IntVar(&snapshot.ZstdDecoderConcurrency, "zstd-decoder-concurrency", runtime.NumCPU(), "Zstd decoder concurrency")
	VerifyRange.Flags().IntVar(&snapshot.MaxConcurrentFlushers, "max-concurrent-flushers", 16, "Bound for number of log shards to flush to Accounts DB Index at once.")
	VerifyRange.Flags().BoolVar(&sbpf.UsePool, "use-pool", true, "Disable to allocate fresh slices")

	// [tuning.pprof] section flags
	VerifyRange.Flags().Int64Var(&pprofPort, "pprof-port", -1, "Port to serve HTTP pprof endpoint")
	VerifyRange.Flags().StringVar(&cpuprofPath, "cpu-profile-path", "", "Filename to write CPU profile")

	// [debug] section flags
	VerifyRange.Flags().StringSliceVar(&debugTxs, "transaction-signatures", []string{}, "Pass tx signature strings to enable debug logging during that transaction's execution")
	VerifyRange.Flags().StringSliceVar(&debugAcctWrites, "account-writes", []string{}, "Pass account pubkeys to enable debug logging of transactions that modify the account")

	// [reporting] section flags
	VerifyRange.Flags().StringVar(&metricsPath, "metrics-path", "", "Filename to write JSONL records of latencies")

	// [overcast] section flags
	VerifyRange.Flags().StringVar(&snapshotDlPath, "download-snapshot-path", "", "Path to download snapshot to")

	// flags for 'mithril run' (live verifier mode)
	// [bootstrap] section flags
	Run.Flags().StringVar(&bootstrapMode, "bootstrap-mode", "auto", "Bootstrap mode: 'auto' (use AccountsDB if exists, else snapshot), 'accountsdb' (require existing), 'snapshot' (rebuild from snapshot), 'new-snapshot' (always download fresh)")

	// [ledger] section flags
	Run.Flags().StringVarP(&accountsPath, "accounts-path", "o", "", "Output path for writing AccountsDB data to")
	Run.Flags().StringVar(&blockstorePath, "ledger-path", "/tmp/blocks", "Path containing slot.json files")

	// [rpc] section flags
	Run.Flags().StringSliceVarP(&rpcEndpoints, "rpc", "r", []string{}, "URL(s) for RPC endpoint(s) - can specify multiple")
	Run.Flags().IntVar(&rpcPort, "rpc-port", 0, "RPC server port. Default off.")

	// [replay] section flags
	Run.Flags().Int64Var(&txParallelism, "txpar", 0, "Set to 0 to use sequential execution, or >0 to execute a topsort tx plan with the given number of workers")

	// [tuning] section flags
	Run.Flags().Uint64Var(&paramArenaSizeMB, "param-arena-size-mb", 512, "Size in MB for serialized parameter arena (0 to disable)")
	Run.Flags().Uint64Var(&borrowedAccountArenaSize, "borrowed-account-arena-size", 1024, "Number of borrowed accounts to preallocate in arena (0 to disable)")
	Run.Flags().IntVar(&snapshot.ZstdDecoderConcurrency, "zstd-decoder-concurrency", runtime.NumCPU(), "Zstd decoder concurrency")
	Run.Flags().IntVar(&snapshot.MaxConcurrentFlushers, "max-concurrent-flushers", 16, "Bound for number of log shards to flush to Accounts DB Index at once.")
	Run.Flags().BoolVar(&sbpf.UsePool, "use-pool", true, "Disable to allocate fresh slices")

	// [tuning.pprof] section flags
	Run.Flags().StringVar(&cpuprofPath, "cpu-profile-path", "", "Filename to write CPU profile")

	// [debug] section flags
	Run.Flags().StringSliceVar(&debugTxs, "transaction-signatures", []string{}, "Pass tx signature strings to enable debug logging during that transaction's execution")
	Run.Flags().StringSliceVar(&debugAcctWrites, "account-writes", []string{}, "Pass account pubkeys to enable debug logging of transactions that modify the account")

	// [reporting] section flags
	Run.Flags().StringVar(&metricsPath, "metrics-path", "", "Filename to write JSONL records of latencies")

	// Top-level flags
	Run.Flags().StringVar(&scratchDirectory, "scratch-directory", "/tmp", "Path for downloads (e.g. snapshots) and other temp state")

	// [block] section flags
	Run.Flags().StringVar(&blockSource, "block-source", "rpc", "Block source: 'rpc' or 'overcast'")
	Run.Flags().StringVar(&overcastEndpoint, "overcast-endpoint", "", "Address for Overcast endpoint (only used when block-source=overcast)")

	// Copy all Run flags to VerifyLive for backwards compatibility
	VerifyLive.Flags().AddFlagSet(Run.Flags())
}

// initConfigAndBindFlags loads TOML config file (if specified) and binds flags to viper.
// After this runs, config values can be read from either CLI flags or config file,
// with CLI flags taking precedence.
func initConfigAndBindFlags(cmd *cobra.Command) error {
	// Initialize config from file (do NOT bind flags - we handle precedence manually)
	if err := config.InitConfig(); err != nil {
		return err
	}

	// Check if a CLI flag was explicitly set by the user
	flagChanged := func(name string) bool {
		if f := cmd.Flags().Lookup(name); f != nil {
			return f.Changed
		}
		return false
	}

	// Helper to get string: CLI flag if explicitly set, otherwise TOML config
	getString := func(cliKey, tomlKey string) string {
		if flagChanged(cliKey) {
			if f := cmd.Flags().Lookup(cliKey); f != nil {
				return f.Value.String()
			}
		}
		return config.GetString(tomlKey)
	}

	// Helper to get int: CLI flag if explicitly set, then TOML config, then flag default
	getInt := func(cliKey, tomlKey string) int {
		if flagChanged(cliKey) {
			if f := cmd.Flags().Lookup(cliKey); f != nil {
				if v, err := strconv.Atoi(f.Value.String()); err == nil {
					return v
				}
			}
		}
		if config.IsSet(tomlKey) {
			return config.GetInt(tomlKey)
		}
		// Fall back to flag default value
		if f := cmd.Flags().Lookup(cliKey); f != nil {
			if v, err := strconv.Atoi(f.DefValue); err == nil {
				return v
			}
		}
		return 0
	}

	// Helper to get int64: CLI flag if explicitly set, then TOML config, then flag default
	getInt64 := func(cliKey, tomlKey string) int64 {
		if flagChanged(cliKey) {
			if f := cmd.Flags().Lookup(cliKey); f != nil {
				if v, err := strconv.ParseInt(f.Value.String(), 10, 64); err == nil {
					return v
				}
			}
		}
		if config.IsSet(tomlKey) {
			return config.GetInt64(tomlKey)
		}
		// Fall back to flag default value
		if f := cmd.Flags().Lookup(cliKey); f != nil {
			if v, err := strconv.ParseInt(f.DefValue, 10, 64); err == nil {
				return v
			}
		}
		return 0
	}

	// Helper to get uint64: CLI flag if explicitly set, then TOML config, then flag default
	getUint64 := func(cliKey, tomlKey string) uint64 {
		if flagChanged(cliKey) {
			if f := cmd.Flags().Lookup(cliKey); f != nil {
				if v, err := strconv.ParseUint(f.Value.String(), 10, 64); err == nil {
					return v
				}
			}
		}
		if config.IsSet(tomlKey) {
			return config.GetUint64(tomlKey)
		}
		// Fall back to flag default value
		if f := cmd.Flags().Lookup(cliKey); f != nil {
			if v, err := strconv.ParseUint(f.DefValue, 10, 64); err == nil {
				return v
			}
		}
		return 0
	}

	// Helper to get bool: CLI flag if explicitly set, otherwise TOML config
	getBool := func(cliKey, tomlKey string) bool {
		if flagChanged(cliKey) {
			if f := cmd.Flags().Lookup(cliKey); f != nil {
				return f.Value.String() == "true"
			}
		}
		return config.GetBool(tomlKey)
	}

	// Helper to get string slice: CLI flag if explicitly set, otherwise TOML config
	getStringSlice := func(cliKey, tomlKey string) []string {
		if flagChanged(cliKey) {
			// Get value directly from the flag, not viper (flags aren't bound)
			if f := cmd.Flags().Lookup(cliKey); f != nil {
				if ss, ok := f.Value.(interface{ GetSlice() []string }); ok {
					return ss.GetSlice()
				}
			}
		}
		return config.GetStringSlice(tomlKey)
	}

	// Update variables (CLI flags take precedence over TOML config when explicitly set)
	// CLI flag names -> TOML nested keys (Firedancer-style)

	// [bootstrap] section (new unified mode replacing two booleans)
	bootstrapMode = getString("bootstrap-mode", "bootstrap.mode")
	if bootstrapMode == "" {
		bootstrapMode = "snapshot" // default: always download fresh snapshot
	}

	// [replay] section (legacy booleans for verify-range)
	loadFromSnapshot = getBool("load-from-snapshot", "replay.load_from_snapshot")
	loadFromAccountsDb = getBool("load-from-accounts-db", "replay.load_from_accounts_db")
	numReplaySlots = getInt64("num-slots", "replay.num_slots")
	endSlot = getInt64("end-slot", "replay.end_slot")
	txParallelism = getInt64("txpar", "replay.txpar")

	// [storage] section (with fallback to legacy [ledger] keys for backwards compatibility)
	snapshotArchivePath = getString("snapshot-archive-path", "storage.snapshots")
	if snapshotArchivePath == "" {
		snapshotArchivePath = getString("snapshot-archive-path", "ledger.snapshot_archive_path")
	}
	incrementalSnapshotFilename = getString("incremental-snapshot", "storage.incremental_snapshot")
	if incrementalSnapshotFilename == "" {
		incrementalSnapshotFilename = getString("incremental-snapshot", "ledger.incremental_snapshot")
	}
	accountsPath = getString("accounts-path", "storage.accounts")
	if accountsPath == "" {
		accountsPath = getString("accounts-path", "ledger.accounts_path")
	}
	blockstorePath = getString("ledger-path", "storage.blockstore")
	if blockstorePath == "" {
		blockstorePath = getString("ledger-path", "ledger.path")
	}

	// [network] section (with fallback to legacy [rpc] keys)
	rpcEndpoints = getStringSlice("rpc", "network.rpc")
	if len(rpcEndpoints) == 0 {
		rpcEndpoints = getStringSlice("rpc", "rpc.rpc")
	}

	// [rpc] section - Mithril's RPC server
	rpcPort = getInt("rpc-port", "rpc.port")

	// Top-level
	scratchDirectory = getString("scratch-directory", "scratch_directory")

	// [block] section
	blockSource = getString("block-source", "block.source")
	if blockSource == "" {
		blockSource = "rpc" // default
	}
	if blockSource == "rpc" && len(rpcEndpoints) == 0 {
		return fmt.Errorf("blockSource=rpc but no endpoints were provided")
	}
	overcastEndpoint = getString("overcast-endpoint", "block.overcast_endpoint")

	// Snapshot download path - defaults to storage.snapshots, can be overridden
	snapshotDlPath = getString("download-snapshot-path", "snapshot.download_path")
	if snapshotDlPath == "" {
		snapshotDlPath = snapshotArchivePath // Use storage.snapshots as default
	}

	// [tuning.pprof] section (with fallback to legacy [development.pprof])
	pprofPort = getInt64("pprof-port", "tuning.pprof.port")
	if pprofPort == 0 {
		pprofPort = getInt64("pprof-port", "development.pprof.port")
	}
	cpuprofPath = getString("cpu-profile-path", "tuning.pprof.cpu_profile_path")
	if cpuprofPath == "" {
		cpuprofPath = getString("cpu-profile-path", "development.pprof.cpu_profile_path")
	}

	// [debug] section (with fallback to legacy [development.debug])
	debugTxs = getStringSlice("transaction-signatures", "debug.transaction_signatures")
	if len(debugTxs) == 0 {
		debugTxs = getStringSlice("transaction-signatures", "development.debug.transaction_signatures")
	}
	debugAcctWrites = getStringSlice("account-writes", "debug.account_writes")
	if len(debugAcctWrites) == 0 {
		debugAcctWrites = getStringSlice("account-writes", "development.debug.account_writes")
	}

	// [tuning] section (with fallback to legacy [development])
	paramArenaSizeMB = getUint64("param-arena-size-mb", "tuning.param_arena_size_mb")
	if paramArenaSizeMB == 0 {
		paramArenaSizeMB = getUint64("param-arena-size-mb", "development.param_arena_size_mb")
	}
	borrowedAccountArenaSize = getUint64("borrowed-account-arena-size", "tuning.borrowed_account_arena_size")
	if borrowedAccountArenaSize == 0 {
		borrowedAccountArenaSize = getUint64("borrowed-account-arena-size", "development.borrowed_account_arena_size")
	}

	// [reporting] section
	metricsPath = getString("metrics-path", "reporting.metrics_path")

	// Handle external package variables (try tuning.* first, fallback to development.*)
	if flagChanged("zstd-decoder-concurrency") {
		snapshot.ZstdDecoderConcurrency = config.GetInt("zstd-decoder-concurrency")
	} else if config.IsSet("tuning.zstd_decoder_concurrency") {
		snapshot.ZstdDecoderConcurrency = config.GetInt("tuning.zstd_decoder_concurrency")
	} else if config.IsSet("development.zstd_decoder_concurrency") {
		snapshot.ZstdDecoderConcurrency = config.GetInt("development.zstd_decoder_concurrency")
	}
	if flagChanged("max-concurrent-flushers") {
		snapshot.MaxConcurrentFlushers = config.GetInt("max-concurrent-flushers")
	} else if config.IsSet("tuning.max_concurrent_flushers") {
		snapshot.MaxConcurrentFlushers = config.GetInt("tuning.max_concurrent_flushers")
	} else if config.IsSet("development.max_concurrent_flushers") {
		snapshot.MaxConcurrentFlushers = config.GetInt("development.max_concurrent_flushers")
	}
	if flagChanged("use-pool") {
		sbpf.UsePool = config.GetBool("use-pool")
	} else if config.IsSet("tuning.use_pool") {
		sbpf.UsePool = config.GetBool("tuning.use_pool")
	} else if config.IsSet("development.use_pool") {
		sbpf.UsePool = config.GetBool("development.use_pool")
	}

	return nil
}

// buildSnapshotConfig creates a snapshotdl.SnapshotConfig from the mithril config.
// It starts with defaults and overrides with any values set in the TOML file.
func buildSnapshotConfig(rpcEndpoints []string) snapshotdl.SnapshotConfig {
	cfg := snapshotdl.DefaultSnapshotConfig()
	cfg.RPCAddresses = rpcEndpoints

	// Override with TOML values if set
	if config.IsSet("snapshot.verbose") {
		cfg.Verbose = config.GetBool("snapshot.verbose")
	}
	if config.IsSet("snapshot.download_path") {
		cfg.DownloadPath = config.GetString("snapshot.download_path")
	}
	if config.IsSet("snapshot.stage1_warm_kib") {
		cfg.Stage1WarmKiB = config.GetInt64("snapshot.stage1_warm_kib")
	}
	if config.IsSet("snapshot.stage1_window_kib") {
		cfg.Stage1WindowKiB = config.GetInt64("snapshot.stage1_window_kib")
	}
	if config.IsSet("snapshot.stage1_windows") {
		cfg.Stage1Windows = config.GetInt("snapshot.stage1_windows")
	}
	if config.IsSet("snapshot.stage1_timeout_ms") {
		cfg.Stage1TimeoutMS = config.GetInt64("snapshot.stage1_timeout_ms")
	}
	if config.IsSet("snapshot.stage1_concurrency") {
		cfg.Stage1Concurrency = config.GetInt("snapshot.stage1_concurrency")
	}
	if config.IsSet("snapshot.stage2_top_k") {
		cfg.Stage2TopK = config.GetInt("snapshot.stage2_top_k")
	}
	if config.IsSet("snapshot.stage2_warm_sec") {
		cfg.Stage2WarmSec = config.GetInt("snapshot.stage2_warm_sec")
	}
	if config.IsSet("snapshot.stage2_measure_sec") {
		cfg.Stage2MeasureSec = config.GetInt("snapshot.stage2_measure_sec")
	}
	if config.IsSet("snapshot.stage2_min_ratio") {
		cfg.Stage2MinRatio = config.GetFloat64("snapshot.stage2_min_ratio")
	}
	if config.IsSet("snapshot.stage2_min_abs_mbs") {
		cfg.Stage2MinAbsMBs = config.GetFloat64("snapshot.stage2_min_abs_mbs")
	}
	if config.IsSet("snapshot.max_rtt_ms") {
		cfg.MaxRTTMs = config.GetInt("snapshot.max_rtt_ms")
	}
	if config.IsSet("snapshot.tcp_timeout_ms") {
		cfg.TCPTimeoutMs = config.GetInt("snapshot.tcp_timeout_ms")
	}
	if config.IsSet("snapshot.min_node_version") {
		cfg.MinNodeVersion = config.GetString("snapshot.min_node_version")
	}
	if config.IsSet("snapshot.allowed_node_versions") {
		cfg.AllowedNodeVersions = config.GetStringSlice("snapshot.allowed_node_versions")
	}
	if config.IsSet("snapshot.full_threshold") {
		cfg.FullThreshold = config.GetInt("snapshot.full_threshold")
	}
	if config.IsSet("snapshot.incremental_threshold") {
		cfg.IncrementalThreshold = config.GetInt("snapshot.incremental_threshold")
	}
	if config.IsSet("snapshot.safety_margin_slots") {
		cfg.SafetyMarginSlots = config.GetInt("snapshot.safety_margin_slots")
	}
	if config.IsSet("snapshot.max_full_snapshots") {
		cfg.MaxFullSnapshots = config.GetInt("snapshot.max_full_snapshots")
	}
	if config.IsSet("snapshot.worker_count") {
		cfg.WorkerCount = config.GetInt("snapshot.worker_count")
	}
	if config.IsSet("snapshot.max_snapshot_url_attempts") {
		cfg.MaxSnapshotURLAttempts = config.GetInt("snapshot.max_snapshot_url_attempts")
	}
	if config.IsSet("snapshot.min_incremental_speed_mbs") {
		cfg.MinIncrementalSpeedMBs = config.GetFloat64("snapshot.min_incremental_speed_mbs")
	}

	return cfg
}

func runVerifyRange(c *cobra.Command, args []string) {
	ctx := c.Context()
	if pprofPort != -1 {
		startPprofHandlers(int(pprofPort))
	}
	if endSlot != -1 && numReplaySlots != 0 {
		klog.Fatalf("specify at most one of --end-slot and --num-slots")
	}

	if !loadFromSnapshot && !loadFromAccountsDb && snapshotDlPath == "" {
		klog.Fatalf("must specify either to load from a snapshot, or load from an existing AccountsDB, or download a snapshot.")
	}

	var err error
	var accountsDbDir string
	var accountsDb *accountsdb.AccountsDb
	var manifest *snapshot.SnapshotManifest
	dbgOpts, err := replay.NewDebugOptions(debugTxs, debugAcctWrites)
	if err != nil {
		klog.Fatalf("failed to parse --transaction-signatures or --account-writes values: %v", err)
	}

	logVCSInfo()
	cpuprofWriter, cpuprofCleanup, err := createBufWriter(cpuprofPath)
	if err != nil {
		klog.Fatalf("unable to create metrics writer to filename=%s: %v", metricsPath, err)
	}
	defer cpuprofCleanup()
	if cpuprofWriter != nil {
		pprof.StartCPUProfile(cpuprofWriter)
		defer pprof.StopCPUProfile()
	}

	if len(rpcEndpoints) == 0 {
		rpcEndpoints = []string{"https://api.mainnet-beta.solana.com"}
	}

	if loadFromSnapshot {
		if snapshotArchivePath == "" || accountsPath == "" {
			klog.Fatalf("must specify snapshot path and directory path for writing generated AccountsDB")
		}

		mlog.Log.Infof("building AccountsDB from snapshot at %s\n", snapshotArchivePath)

		// extract accountvecs from full snapshot, build accountsdb index, and write it all out to disk
		accountsDb, manifest, err = snapshot.BuildAccountsDb(ctx, snapshotArchivePath, incrementalSnapshotFilename, accountsPath)
		if err != nil {
			klog.Fatalf("failed to populate new accounts db from snapshot %s: %s", snapshotArchivePath, err)
		}

		//mlog.Log.Debugf("successfully created accounts db from snapshot %s", snapshotArchivePath)

		accountsDbDir = accountsPath
	} else if loadFromAccountsDb {
		if snapshotArchivePath == "" {
			klog.Fatalf("must specify an AccountsDB directory path to load from")
		}

		accountsDbDir = snapshotArchivePath
	} else if snapshotDlPath != "" {
		if accountsPath == "" {
			klog.Fatalf("must specify a path to download a snapshot to")
		}

		mlog.Log.Infof("downloading snapshot...")

		snapCfg := buildSnapshotConfig(rpcEndpoints)
		var dlPath string
		dlPath, _, _, err = snapshotdl.DownloadSnapshotWithConfig(ctx, snapshotDlPath, snapCfg)
		if err != nil {
			klog.Fatalf("error downloading snapshot: %s", err)
		}

		accountsDb, manifest, err = snapshot.BuildAccountsDb(ctx, dlPath, incrementalSnapshotFilename, accountsPath)
		if err != nil {
			klog.Fatalf("failed to populate new accounts db from snapshot %s: %s", dlPath, err)
		}

		accountsDbDir = accountsPath
	}

	if accountsDb == nil || manifest == nil {
		mlog.Log.Infof("loading from AccountsDB at %s", accountsDbDir)

		accountsDb, err = accountsdb.OpenDb(accountsDbDir)
		if err != nil {
			klog.Fatalf("unable to open accounts db %s\n", accountsDbDir)
		}

		manifest, err = snapshot.LoadManifestFromFile(filepath.Join(accountsDbDir, "manifest"))
		if err != nil {
			klog.Fatalf("unable to open manifest file")
		}

		// Show disk usage summary
		progress.PrintDiskUsage(accountsDbDir, blockstorePath, snapshotDlPath)
	}

	// Check for state file to resume from correct slot
	var snapshotBaseSlot = manifest.Bank.Slot
	startSlot := int64(manifest.Bank.Slot + 1)
	mithrilState, _ := state.CheckAndLoadValidState(accountsDbDir)
	if mithrilState != nil {
		// Validate state file matches current manifest
		if mithrilState.SnapshotSlot != manifest.Bank.Slot {
			mlog.Log.Infof("state file snapshot_slot (%d) doesn't match manifest (%d), ignoring state file",
				mithrilState.SnapshotSlot, manifest.Bank.Slot)
			mithrilState = nil
		} else if mithrilState.LastSlot > 0 {
			// Validate last_slot is reasonable (not in the future relative to what we could have replayed)
			if mithrilState.LastSlot < manifest.Bank.Slot {
				mlog.Log.Infof("state file last_slot (%d) is before snapshot slot (%d), ignoring",
					mithrilState.LastSlot, manifest.Bank.Slot)
				mithrilState = nil
			} else {
				startSlot = int64(mithrilState.LastSlot + 1)
				mlog.Log.Infof("resuming from state file: last_slot=%d, starting at %d", mithrilState.LastSlot, startSlot)
			}
		}
	}

	// Create ResumeState if we have resume context from a previous graceful shutdown
	var resumeState *replay.ResumeState
	if mithrilState != nil && mithrilState.HasResumeContext() {
		resumeCtx := mithrilState.GetResumeContext()
		// Decode bankhash from base58
		parentBankhash, err := base58.Decode(mithrilState.LastBankhash)
		if err != nil {
			mlog.Log.Infof("warning: failed to decode last_bankhash from state file: %v", err)
		} else {
			// Decode LtHash from base64
			ltHashBytes, err := base64.StdEncoding.DecodeString(resumeCtx.AcctsLtHash)
			if err != nil {
				mlog.Log.Infof("warning: failed to decode last_accts_lt_hash from state file: %v", err)
			} else {
				ltHash := &lthash.LtHash{}
				ltHash.InitWithHash(ltHashBytes)
				resumeState = &replay.ResumeState{
					ParentSlot:               mithrilState.LastSlot,
					ParentBankhash:           parentBankhash,
					AcctsLtHash:              ltHash,
					LamportsPerSignature:     resumeCtx.LamportsPerSignature,
					PrevLamportsPerSignature: resumeCtx.PrevLamportsPerSig,
					NumSignatures:            resumeCtx.NumSignatures,
				}

				// Decode blockhash context
				if resumeCtx.RecentBlockhashes != nil && len(resumeCtx.RecentBlockhashes) > 0 {
					recentBlockhashes := decodeRecentBlockhashes(resumeCtx.RecentBlockhashes)
					resumeState.RecentBlockhashes = &recentBlockhashes

					if resumeCtx.EvictedBlockhash != "" {
						evictedBytes, err := base58.Decode(resumeCtx.EvictedBlockhash)
						if err == nil && len(evictedBytes) == 32 {
							copy(resumeState.EvictedBlockhash[:], evictedBytes)
						}
					}

					if resumeCtx.LastBlockhash != "" {
						lastBhBytes, err := base58.Decode(resumeCtx.LastBlockhash)
						if err == nil && len(lastBhBytes) == 32 {
							copy(resumeState.LastBlockhash[:], lastBhBytes)
						}
					}
					mlog.Log.Infof("loaded resume context with %d blockhashes from state file", len(*resumeState.RecentBlockhashes))
				} else {
					mlog.Log.Infof("loaded resume context from state file (no blockhashes)")
				}
			}
		}
	}

	if mithrilState == nil {
		// Create a new state file for this session
		var snapshotEpoch uint64
		if sealevel.SysvarCache.EpochSchedule.Sysvar != nil {
			snapshotEpoch = sealevel.SysvarCache.EpochSchedule.Sysvar.GetEpoch(manifest.Bank.Slot)
		}
		mithrilState = state.NewReadyState(manifest.Bank.Slot, snapshotEpoch, "", "", 0, 0)
		if err := mithrilState.Save(accountsDbDir); err != nil {
			mlog.Log.Errorf("failed to save state file: %v", err)
		}
	}

	if endSlot != -1 {
		numReplaySlots = endSlot - startSlot
	} else if numReplaySlots != 0 {
		endSlot = startSlot + numReplaySlots
	}

	// just processing the snapshot - not executing blocks.
	if numReplaySlots == 0 {
		return
	}

	if endSlot != -1 && endSlot < startSlot {
		klog.Fatalf("end slot cannot be lower than start slot")
	}
	mlog.Log.Infof("will replay startSlot=%d endSlot=%d", startSlot, endSlot)

	mlog.Log.Infof("initializing caches")
	accountsDb.InitCaches()

	metricsWriter, metricsWriterCleanup, err := createBufWriter(metricsPath)
	if err != nil {
		klog.Fatalf("unable to create metrics writer to filename=%s: %v", metricsPath, err)
	}
	defer metricsWriterCleanup()

	if paramArenaSizeMB > 0 {
		replay.SerializedParameterArena = arena.New[byte](paramArenaSizeMB << 20)
	}
	if borrowedAccountArenaSize > 0 {
		sealevel.BorrowedAccountArenas = make([]*arena.Arena[sealevel.BorrowedAccount], txParallelism)
		for i := range txParallelism {
			sealevel.BorrowedAccountArenas[i] = arena.New[sealevel.BorrowedAccount](borrowedAccountArenaSize)
		}
	}

	var rpcServer *rpcserver.RpcServer
	if rpcPort < 0 || rpcPort > 65535 {
		klog.Fatalf("invalid port: %d", rpcPort)
	} else if rpcPort != 0 {
		rpcServer = rpcserver.NewRpcServer(accountsDb, uint16(rpcPort))
		rpcServer.Start()
		mlog.Log.Infof("started RPC server on port %d", rpcPort)
	}

	replayStartTime := time.Now()
	result := runReplayWithRecovery(ctx, accountsDb, accountsDbDir, manifest, resumeState, uint64(startSlot), uint64(endSlot), rpcEndpoints[0], blockstorePath, int(txParallelism), false, false, dbgOpts, metricsWriter, rpcServer, mithrilState)

	// Update state file with last persisted slot and resume context
	if result.LastPersistedSlot > 0 && mithrilState != nil {
		// Build resume context for graceful shutdown
		var resumeCtx *state.ResumeContext
		if result.LastAcctsLtHash != nil {
			// Calculate epoch for the last persisted slot
			var lastEpoch uint64
			if sealevel.SysvarCache.EpochSchedule.Sysvar != nil {
				lastEpoch = sealevel.SysvarCache.EpochSchedule.Sysvar.GetEpoch(result.LastPersistedSlot)
			}
			resumeCtx = &state.ResumeContext{
				AcctsLtHash:          base64.StdEncoding.EncodeToString(result.LastAcctsLtHash.Hash()),
				LamportsPerSignature: result.LastLamportsPerSignature,
				PrevLamportsPerSig:   result.LastPrevLamportsPerSig,
				NumSignatures:        result.LastNumSignatures,
				Epoch:                lastEpoch,

				// Blockhash context - required because appendvec writes are not fsynced
				RecentBlockhashes: encodeRecentBlockhashes(result.LastRecentBlockhashes),
				EvictedBlockhash:  base58.Encode(result.LastEvictedBlockhash[:]),
				LastBlockhash:     base58.Encode(result.LastBlockhash[:]),

				// Run tracking - for log correlation
				RunID:        replay.CurrentRunID,
				RunStartedAt: replayStartTime,
				Commit:       getCommitHash(),
			}
		}
		if err := mithrilState.UpdateLastSlotWithContext(accountsDbDir, result.LastPersistedSlot, result.LastPersistedBankhash, resumeCtx); err != nil {
			mlog.Log.Errorf("failed to update state file: %v", err)
		}
	}

	// Print shutdown summary if cancelled
	if result.WasCancelled && result.LastPersistedSlot > 0 {
		// Calculate epoch from slot using epoch schedule
		var epoch, snapshotEpoch uint64
		if sealevel.SysvarCache.EpochSchedule.Sysvar != nil {
			epoch = sealevel.SysvarCache.EpochSchedule.Sysvar.GetEpoch(result.LastPersistedSlot)
			snapshotEpoch = sealevel.SysvarCache.EpochSchedule.Sysvar.GetEpoch(snapshotBaseSlot)
		}
		progress.PrintShutdownSummary(progress.ShutdownInfo{
			LastSlot:         result.LastPersistedSlot,
			LastBankhash:     result.LastPersistedBankhash,
			SnapshotBaseSlot: snapshotBaseSlot,
			AccountsDBPath:   accountsDbDir,
			ReplayDuration:   time.Since(replayStartTime),
			WasCancelled:     true,
			RunID:            replay.CurrentRunID,
			Epoch:            epoch,
			SnapshotEpoch:    snapshotEpoch,
		})
	}

	mlog.Log.Infof("done replaying, closing DB")
	accountsDb.CloseDb()
}

func runLive(c *cobra.Command, args []string) {
	ctx := c.Context()

	// Print the Mithril banner first, before any other output
	progress.PrintBanner()

	// Kill any existing mithril processes to prevent zombie accumulation
	if killed := killExistingMithrilProcesses(); killed > 0 {
		fmt.Printf("  ⚠ Killed %d existing mithril process(es)\n\n", killed)
	}

	// Print consolidated startup info
	printStartupInfo("run")

	// Now start the metrics server (after banner so errors don't appear first)
	statsd.StartMetricsServer()

	// Determine if using Overcast based on block source
	useOvercast := blockSource == "overcast"
	if useOvercast && overcastEndpoint == "" {
		mlog.Log.Infof("block.source=overcast but no overcast_endpoint provided, falling back to RPC")
		useOvercast = false
	}

	dbgOpts, err := replay.NewDebugOptions(debugTxs, debugAcctWrites)
	if err != nil {
		klog.Fatalf("failed to parse --transaction-signatures or --account-writes values: %v", err)
	}

	cpuprofWriter, cpuprofCleanup, err := createBufWriter(cpuprofPath)
	if err != nil {
		klog.Fatalf("unable to create metrics writer to filename=%s: %v", metricsPath, err)
	}
	defer cpuprofCleanup()
	if cpuprofWriter != nil {
		pprof.StartCPUProfile(cpuprofWriter)
		defer pprof.StopCPUProfile()
	}

	if len(rpcEndpoints) == 0 {
		rpcEndpoints = []string{"https://api.mainnet-beta.solana.com"}
	}

	// Bootstrap: determine how to initialize AccountsDB based on mode
	var accountsDb *accountsdb.AccountsDb
	var manifest *snapshot.SnapshotManifest
	var mithrilState *state.MithrilState
	// Use configured snapshot directory (storage.snapshots / snapshot.download_path), not scratch
	snapshotDownloadPath := snapshotDlPath

	// Check for valid state file first (this is the authoritative source of truth)
	mithrilState, _ = state.CheckAndLoadValidState(accountsPath)
	hasValidState := mithrilState != nil

	// Fall back to legacy detection if no state file
	hasAccountsDB, accountsDBSlot := detectExistingAccountsDB(accountsPath)
	if hasValidState {
		hasAccountsDB = true
		accountsDBSlot = mithrilState.GetCurrentSlot() // Use current slot (LastSlot if replayed, else SnapshotSlot)
	}

	// Pass overcast endpoint if using overcast, otherwise empty string for RPC mode
	var overcastAddr string
	if useOvercast {
		overcastAddr = overcastEndpoint
	}

	switch bootstrapMode {
	case "accountsdb":
		// Mode: Require existing AccountsDB, never download
		if !hasValidState && !hasAccountsDB {
			klog.Fatalf("mode=accountsdb requires existing AccountsDB at %s", accountsPath)
		}
		if !hasValidState {
			mlog.Log.Infof("WARNING: no state file found, AccountsDB may be from incomplete build")
		}
		mlog.Log.Infof("resuming from existing AccountsDB at slot %d", accountsDBSlot)
		accountsDb, err = accountsdb.OpenDb(accountsPath)
		if err != nil {
			klog.Fatalf("failed to open AccountsDB at %s: %v", accountsPath, err)
		}
		manifest, err = snapshot.LoadManifestFromFile(filepath.Join(accountsPath, "manifest"))
		if err != nil {
			klog.Fatalf("failed to load manifest: %v", err)
		}
		// Run integrity check if we have a state file (warn only, don't fail - user chose force mode)
		if hasValidState {
			if err := mithrilState.ValidateAgainstBankhashDB(accountsDb); err != nil {
				mlog.Log.Errorf("WARNING: integrity check failed: %v", err)
				mlog.Log.Errorf("WARNING: AccountsDB may be corrupted. Consider using --bootstrap-mode=snapshot to rebuild.")
			}
		}

	case "new-snapshot":
		// Mode: Always download fresh snapshot, clean everything
		if snapshotDownloadPath == "" {
			klog.Fatalf("mode=new-snapshot requires a snapshot directory (set storage.snapshots or snapshot.download_path in config)")
		}
		mlog.Log.Infof("mode=new-snapshot: downloading fresh snapshot")
		if accountsPath != "" {
			mlog.Log.Infof("cleaning up previous AccountsDB artifacts in %s", accountsPath)
			snapshot.CleanAccountsDbDir(accountsPath)
		}
		// Clean ALL existing snapshots (force fresh download)
		if snapshotDownloadPath != "" {
			mlog.Log.Infof("cleaning up existing snapshot files in %s", snapshotDownloadPath)
			snapshot.CleanSnapshotDownloadDir(snapshotDownloadPath, 0, true) // 0 means delete all
		}
		accountsDb, manifest, err = downloadAndBuildFromSnapshot(ctx, rpcEndpoints, snapshotDownloadPath, accountsPath, blockstorePath, overcastAddr)
		if err != nil {
			klog.Fatalf("failed to build AccountsDB from snapshot: %v", err)
		}
		// Write state file to mark build as complete
		var snapshotEpoch uint64
		if sealevel.SysvarCache.EpochSchedule.Sysvar != nil {
			snapshotEpoch = sealevel.SysvarCache.EpochSchedule.Sysvar.GetEpoch(manifest.Bank.Slot)
		}
		mithrilState = state.NewReadyState(manifest.Bank.Slot, snapshotEpoch, "", "", 0, 0)
		if err := mithrilState.Save(accountsPath); err != nil {
			mlog.Log.Errorf("failed to save state file: %v", err)
		}

	case "snapshot":
		// Mode: Rebuild AccountsDB from snapshot, reuse existing snapshot file if fresh enough
		if snapshotDownloadPath == "" {
			klog.Fatalf("mode=snapshot requires a snapshot directory (set storage.snapshots or snapshot.download_path in config)")
		}
		mlog.Log.Infof("mode=snapshot: will rebuild AccountsDB from snapshot")
		if accountsPath != "" {
			mlog.Log.Infof("cleaning up previous AccountsDB artifacts in %s", accountsPath)
			snapshot.CleanAccountsDbDir(accountsPath)
		}

		// Check for existing fresh snapshot
		fullThreshold := config.GetInt("snapshot.full_threshold")
		if fullThreshold == 0 {
			fullThreshold = 100000 // default
		}
		existingSnap := detectFreshSnapshot(snapshotDownloadPath, fullThreshold, rpcEndpoints, ctx)

		if existingSnap != nil {
			// Reuse existing snapshot
			mlog.Log.Infof("reusing existing snapshot file at slot %d", existingSnap.slot)
			accountsDb, manifest, err = buildFromExistingSnapshot(ctx, existingSnap, snapshotDownloadPath, accountsPath, blockstorePath, overcastAddr, rpcEndpoints)
		} else {
			// Download fresh
			mlog.Log.Infof("no fresh snapshot file found, downloading new one")
			// Clean up old snapshot files based on retention settings
			if snapshotDownloadPath != "" {
				maxSnapshots := config.GetInt("snapshot.max_full_snapshots")
				if maxSnapshots == 0 {
					maxSnapshots = 2 // default
				}
				deleteOld := config.GetBool("snapshot.delete_old_snapshots")
				snapshot.CleanSnapshotDownloadDir(snapshotDownloadPath, maxSnapshots, deleteOld)
			}
			accountsDb, manifest, err = downloadAndBuildFromSnapshot(ctx, rpcEndpoints, snapshotDownloadPath, accountsPath, blockstorePath, overcastAddr)
		}
		if err != nil {
			klog.Fatalf("failed to build AccountsDB from snapshot: %v", err)
		}
		// Write state file to mark build as complete
		var snapshotEpoch uint64
		if sealevel.SysvarCache.EpochSchedule.Sysvar != nil {
			snapshotEpoch = sealevel.SysvarCache.EpochSchedule.Sysvar.GetEpoch(manifest.Bank.Slot)
		}
		mithrilState = state.NewReadyState(manifest.Bank.Slot, snapshotEpoch, "", "", 0, 0)
		if err := mithrilState.Save(accountsPath); err != nil {
			mlog.Log.Errorf("failed to save state file: %v", err)
		}

	case "auto":
		fallthrough
	default:
		// Mode: auto - prefer valid AccountsDB (with state file), then download fresh
		fullThreshold := config.GetInt("snapshot.full_threshold")
		if fullThreshold == 0 {
			fullThreshold = 100000 // default
		}

		if hasValidState {
			// Check if AccountsDB is stale (significantly behind chain tip)
			// Use queryCurrentSlot instead of queryLatestSnapshotSlot to avoid expensive node discovery
			currentSlot, err := queryCurrentSlot(ctx, rpcEndpoints)
			if err != nil {
				mlog.Log.Infof("could not query current slot: %v (continuing with existing AccountsDB)", err)
			} else if mithrilState.IsStale(currentSlot, uint64(fullThreshold)) {
				slotsBehind := currentSlot - mithrilState.GetCurrentSlot()
				mlog.Log.Infof("AccountsDB is %d slots behind chain tip", slotsBehind)

				// Interactive prompt
				choice := progress.PromptStaleAccountsDB(progress.StaleInfo{
					AccountsDBSlot:     mithrilState.GetCurrentSlot(),
					LatestSnapshotSlot: currentSlot, // Using current chain tip slot
					SlotsBehind:        slotsBehind,
				})

				if choice == 2 {
					// User chose to start fresh from snapshot
					if snapshotDownloadPath == "" {
						klog.Fatalf("cannot rebuild from snapshot: no snapshot directory configured (set storage.snapshots or snapshot.download_path in config)")
					}
					mlog.Log.Infof("user chose to rebuild from latest snapshot")
					if accountsPath != "" {
						snapshot.CleanAccountsDbDir(accountsPath)
					}
					// Check for existing fresh snapshot
					existingSnap := detectFreshSnapshot(snapshotDownloadPath, fullThreshold, rpcEndpoints, ctx)
					if existingSnap != nil {
						mlog.Log.Infof("reusing existing snapshot file at slot %d", existingSnap.slot)
						accountsDb, manifest, err = buildFromExistingSnapshot(ctx, existingSnap, snapshotDownloadPath, accountsPath, blockstorePath, overcastAddr, rpcEndpoints)
					} else {
						// Clean up old snapshot files
						if snapshotDownloadPath != "" {
							maxSnapshots := config.GetInt("snapshot.max_full_snapshots")
							if maxSnapshots == 0 {
								maxSnapshots = 2
							}
							deleteOld := config.GetBool("snapshot.delete_old_snapshots")
							snapshot.CleanSnapshotDownloadDir(snapshotDownloadPath, maxSnapshots, deleteOld)
						}
						accountsDb, manifest, err = downloadAndBuildFromSnapshot(ctx, rpcEndpoints, snapshotDownloadPath, accountsPath, blockstorePath, overcastAddr)
					}
					if err != nil {
						klog.Fatalf("failed to build AccountsDB from snapshot: %v", err)
					}
					var snapshotEpoch uint64
					if sealevel.SysvarCache.EpochSchedule.Sysvar != nil {
						snapshotEpoch = sealevel.SysvarCache.EpochSchedule.Sysvar.GetEpoch(manifest.Bank.Slot)
					}
					mithrilState = state.NewReadyState(manifest.Bank.Slot, snapshotEpoch, "", "", 0, 0)
					if err := mithrilState.Save(accountsPath); err != nil {
						mlog.Log.Errorf("failed to save state file: %v", err)
					}
					break // Exit the switch, continue with fresh AccountsDB
				}
				// choice == 1: continue with existing AccountsDB
			}

			mlog.Log.Infof("mode=auto: resuming from existing AccountsDB at slot %d", accountsDBSlot)
			accountsDb, err = accountsdb.OpenDb(accountsPath)
			if err != nil {
				klog.Fatalf("failed to open AccountsDB at %s: %v", accountsPath, err)
			}
			manifest, err = snapshot.LoadManifestFromFile(filepath.Join(accountsPath, "manifest"))
			if err != nil {
				klog.Fatalf("failed to load manifest: %v", err)
			}

			// Validate state file matches AccountsDB (detect Ctrl+Z / kill -9 corruption)
			if err := mithrilState.ValidateAgainstBankhashDB(accountsDb); err != nil {
				mlog.Log.Errorf("INTEGRITY CHECK FAILED: %v", err)
				mlog.Log.Infof("AccountsDB appears to have been modified beyond what state file records")
				mlog.Log.Infof("This usually happens when the process was killed with Ctrl+Z or kill -9")

				// Mark state as corrupted so next startup knows to rebuild
				if markErr := mithrilState.MarkCorrupted(accountsPath, err.Error()); markErr != nil {
					mlog.Log.Errorf("failed to mark state as corrupted: %v", markErr)
				} else {
					mlog.Log.Infof("state file updated to indicate corruption")
				}

				// Close AccountsDB before exiting
				accountsDb.CloseDb()

				mlog.Log.Infof("restart mithril to automatically rebuild from snapshot")
				klog.Fatalf("AccountsDB corrupted - restart to rebuild")
			}
		} else {
			// No valid state - need to clean and rebuild from snapshot
			if snapshotDownloadPath == "" {
				klog.Fatalf("mode=auto requires a snapshot directory to rebuild (set storage.snapshots or snapshot.download_path in config)")
			}
			if hasAccountsDB {
				mlog.Log.Infof("mode=auto: AccountsDB exists but state invalid, rebuilding from snapshot")
			} else {
				mlog.Log.Infof("mode=auto: no existing AccountsDB, will download snapshot")
			}
			if accountsPath != "" {
				mlog.Log.Infof("cleaning up previous AccountsDB artifacts in %s", accountsPath)
				snapshot.CleanAccountsDbDir(accountsPath)
			}

			// Check for existing fresh snapshot
			existingSnap := detectFreshSnapshot(snapshotDownloadPath, fullThreshold, rpcEndpoints, ctx)
			if existingSnap != nil {
				mlog.Log.Infof("reusing existing snapshot file at slot %d", existingSnap.slot)
				accountsDb, manifest, err = buildFromExistingSnapshot(ctx, existingSnap, snapshotDownloadPath, accountsPath, blockstorePath, overcastAddr, rpcEndpoints)
			} else {
				// Clean up old snapshot files based on retention settings
				maxSnapshots := config.GetInt("snapshot.max_full_snapshots")
				if maxSnapshots == 0 {
					maxSnapshots = 2 // default
				}
				deleteOld := config.GetBool("snapshot.delete_old_snapshots")
				snapshot.CleanSnapshotDownloadDir(snapshotDownloadPath, maxSnapshots, deleteOld)
				accountsDb, manifest, err = downloadAndBuildFromSnapshot(ctx, rpcEndpoints, snapshotDownloadPath, accountsPath, blockstorePath, overcastAddr)
			}
			if err != nil {
				klog.Fatalf("failed to build AccountsDB from snapshot: %v", err)
			}
			// Write state file to mark build as complete
			var snapshotEpoch uint64
			if sealevel.SysvarCache.EpochSchedule.Sysvar != nil {
				snapshotEpoch = sealevel.SysvarCache.EpochSchedule.Sysvar.GetEpoch(manifest.Bank.Slot)
			}
			mithrilState = state.NewReadyState(manifest.Bank.Slot, snapshotEpoch, "", "", 0, 0)
			if err := mithrilState.Save(accountsPath); err != nil {
				mlog.Log.Errorf("failed to save state file: %v", err)
			}
		}
	}

	mlog.Log.Infof("AccountsDB ready")

	// Show disk usage summary
	progress.PrintDiskUsage(accountsPath, blockstorePath, snapshotDlPath)

	// Determine start slot from state file or manifest
	var snapshotBaseSlot = manifest.Bank.Slot
	startSlot := int64(manifest.Bank.Slot + 1)
	if mithrilState != nil {
		// Validate state file matches current manifest
		if mithrilState.SnapshotSlot != manifest.Bank.Slot {
			mlog.Log.Infof("state file snapshot_slot (%d) doesn't match manifest (%d), ignoring state file",
				mithrilState.SnapshotSlot, manifest.Bank.Slot)
			mithrilState = nil
		} else if mithrilState.LastSlot > 0 {
			// Validate last_slot is reasonable
			if mithrilState.LastSlot < manifest.Bank.Slot {
				mlog.Log.Infof("state file last_slot (%d) is before snapshot slot (%d), ignoring",
					mithrilState.LastSlot, manifest.Bank.Slot)
				mithrilState = nil
			} else {
				startSlot = int64(mithrilState.LastSlot + 1)
				mlog.Log.Infof("resuming from state file: last_slot=%d, starting at %d", mithrilState.LastSlot, startSlot)
			}
		}
	}

	// Create ResumeState if we have resume context from state file
	var resumeState *replay.ResumeState
	if mithrilState != nil && mithrilState.HasResumeContext() {
		resumeCtx := mithrilState.GetResumeContext()

		// Decode parent bankhash
		parentBankhash, err := base58.Decode(mithrilState.LastBankhash)
		if err != nil {
			mlog.Log.Errorf("failed to decode last_bankhash from state file: %v", err)
			mlog.Log.Infof("will start fresh from snapshot")
			mithrilState = nil
		} else {
			// Decode AcctsLtHash
			ltHashBytes, err := base64.StdEncoding.DecodeString(resumeCtx.AcctsLtHash)
			if err != nil {
				mlog.Log.Errorf("failed to decode accts_lt_hash from state file: %v", err)
				mlog.Log.Infof("will start fresh from snapshot")
				mithrilState = nil
			} else {
				ltHash := &lthash.LtHash{}
				ltHash.InitWithHash(ltHashBytes)

				resumeState = &replay.ResumeState{
					ParentSlot:               mithrilState.LastSlot,
					ParentBankhash:           parentBankhash,
					AcctsLtHash:              ltHash,
					LamportsPerSignature:     resumeCtx.LamportsPerSignature,
					PrevLamportsPerSignature: resumeCtx.PrevLamportsPerSig,
					NumSignatures:            resumeCtx.NumSignatures,
				}

				// Decode blockhash context
				if resumeCtx.RecentBlockhashes != nil && len(resumeCtx.RecentBlockhashes) > 0 {
					recentBlockhashes := decodeRecentBlockhashes(resumeCtx.RecentBlockhashes)
					resumeState.RecentBlockhashes = &recentBlockhashes

					if resumeCtx.EvictedBlockhash != "" {
						evictedBytes, err := base58.Decode(resumeCtx.EvictedBlockhash)
						if err == nil && len(evictedBytes) == 32 {
							copy(resumeState.EvictedBlockhash[:], evictedBytes)
						}
					}

					if resumeCtx.LastBlockhash != "" {
						lastBhBytes, err := base58.Decode(resumeCtx.LastBlockhash)
						if err == nil && len(lastBhBytes) == 32 {
							copy(resumeState.LastBlockhash[:], lastBhBytes)
						}
					}
				}
				mlog.Log.Infof("resume context loaded: parent_slot=%d", resumeState.ParentSlot)
			}
		}
	}

	if mithrilState == nil {
		// Initialize state for this session
		var snapshotEpoch uint64
		if sealevel.SysvarCache.EpochSchedule.Sysvar != nil {
			snapshotEpoch = sealevel.SysvarCache.EpochSchedule.Sysvar.GetEpoch(manifest.Bank.Slot)
		}
		mithrilState = state.NewReadyState(manifest.Bank.Slot, snapshotEpoch, "", "", 0, 0)
		if err := mithrilState.Save(accountsPath); err != nil {
			mlog.Log.Errorf("failed to save state file: %v", err)
		}
	}

	liveEndSlot := uint64(math.MaxUint64)

	mlog.Log.Infof("starting replay from slot %d", startSlot)

	mlog.Log.Infof("initializing caches")
	accountsDb.InitCaches()

	metricsWriter, metricsWriterCleanup, err := createBufWriter(metricsPath)
	if err != nil {
		klog.Fatalf("unable to create metrics writer to filename=%s: %v", metricsPath, err)
	}
	defer metricsWriterCleanup()

	if paramArenaSizeMB > 0 {
		replay.SerializedParameterArena = arena.New[byte](paramArenaSizeMB << 20)
	}
	if borrowedAccountArenaSize > 0 {
		sealevel.BorrowedAccountArenas = make([]*arena.Arena[sealevel.BorrowedAccount], txParallelism)
		for i := range txParallelism {
			sealevel.BorrowedAccountArenas[i] = arena.New[sealevel.BorrowedAccount](borrowedAccountArenaSize)
		}
	}

	var rpcServer *rpcserver.RpcServer
	if rpcPort < 0 || rpcPort > 65535 {
		klog.Fatalf("invalid port: %d", rpcPort)
	} else if rpcPort != 0 {
		rpcServer = rpcserver.NewRpcServer(accountsDb, uint16(rpcPort))
		rpcServer.Start()
		mlog.Log.Infof("started RPC server on port %d", rpcPort)
	}

	replayStartTime := time.Now()
	result := runReplayWithRecovery(ctx, accountsDb, accountsPath, manifest, resumeState, uint64(startSlot), liveEndSlot, rpcEndpoints[0], blockstorePath, int(txParallelism), true, useOvercast, dbgOpts, metricsWriter, rpcServer, mithrilState)

	// Update state file with last persisted slot and resume context
	if result.LastPersistedSlot > 0 && mithrilState != nil {
		var resumeCtx *state.ResumeContext
		if result.LastAcctsLtHash != nil {
			// Calculate epoch for the last persisted slot
			var lastEpoch uint64
			if sealevel.SysvarCache.EpochSchedule.Sysvar != nil {
				lastEpoch = sealevel.SysvarCache.EpochSchedule.Sysvar.GetEpoch(result.LastPersistedSlot)
			}
			resumeCtx = &state.ResumeContext{
				AcctsLtHash:          base64.StdEncoding.EncodeToString(result.LastAcctsLtHash.Hash()),
				LamportsPerSignature: result.LastLamportsPerSignature,
				PrevLamportsPerSig:   result.LastPrevLamportsPerSig,
				NumSignatures:        result.LastNumSignatures,
				Epoch:                lastEpoch,

				// Blockhash context - required because appendvec writes are not fsynced
				RecentBlockhashes: encodeRecentBlockhashes(result.LastRecentBlockhashes),
				EvictedBlockhash:  base58.Encode(result.LastEvictedBlockhash[:]),
				LastBlockhash:     base58.Encode(result.LastBlockhash[:]),

				// Run tracking - for log correlation
				RunID:        replay.CurrentRunID,
				RunStartedAt: replayStartTime,
				Commit:       getCommitHash(),
			}
		}
		if err := mithrilState.UpdateLastSlotWithContext(accountsPath, result.LastPersistedSlot, result.LastPersistedBankhash, resumeCtx); err != nil {
			mlog.Log.Errorf("failed to update state file: %v", err)
		}
	}

	// Print shutdown summary if cancelled
	if result.WasCancelled && result.LastPersistedSlot > 0 {
		// Calculate epoch from slot using epoch schedule
		var epoch, snapshotEpoch uint64
		if sealevel.SysvarCache.EpochSchedule.Sysvar != nil {
			epoch = sealevel.SysvarCache.EpochSchedule.Sysvar.GetEpoch(result.LastPersistedSlot)
			snapshotEpoch = sealevel.SysvarCache.EpochSchedule.Sysvar.GetEpoch(snapshotBaseSlot)
		}
		progress.PrintShutdownSummary(progress.ShutdownInfo{
			LastSlot:         result.LastPersistedSlot,
			LastBankhash:     result.LastPersistedBankhash,
			SnapshotBaseSlot: snapshotBaseSlot,
			AccountsDBPath:   accountsPath,
			ReplayDuration:   time.Since(replayStartTime),
			WasCancelled:     true,
			RunID:            replay.CurrentRunID,
			Epoch:            epoch,
			SnapshotEpoch:    snapshotEpoch,
		})
	}

	mlog.Log.Infof("done replaying, closing DB")
	accountsDb.CloseDb()
}

func logVCSInfo() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		mlog.Log.Errorf("VCS info: not available")
		return
	}

	var revision, vcsTime, modified string

	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.time":
			vcsTime = setting.Value
		case "vcs.modified":
			modified = setting.Value
		}
	}

	mlog.Log.Infof("VCS info: revision=%s time=%s modified=%s", revision, vcsTime, modified)
}

// getCommitHash returns the short git commit hash from build info
func getCommitHash() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				if len(setting.Value) > 8 {
					return setting.Value[:8]
				}
				return setting.Value
			}
		}
	}
	return ""
}

// printStartupInfo prints consolidated startup info including version, timestamp, and configuration
func printStartupInfo(commandName string) {
	// Gold/amber color like the banner
	gold := "\x1b[38;2;217;164;65m"
	reset := "\x1b[0m"
	dim := "\x1b[2m"
	green := "\x1b[32m"
	cyan := "\x1b[36m"

	fmt.Printf("%s━━━ Startup Info ━━━%s\n", gold, reset)

	// Get version info from build
	var revision, modified string
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				revision = setting.Value
			case "vcs.modified":
				modified = setting.Value
			}
		}
	}

	// Shorten the commit hash to 8 chars
	if len(revision) > 8 {
		revision = revision[:8]
	}

	// Timestamp (current time)
	fmt.Printf("  Timestamp:    %s%s%s\n", dim, time.Now().Format(time.RFC3339), reset)

	// Commit info
	if revision != "" {
		commitStr := revision
		if modified == "true" {
			commitStr += " (modified)"
		}
		fmt.Printf("  Commit:       %s%s%s\n", dim, commitStr, reset)
	}

	// Go version
	fmt.Printf("  Go:           %s%s%s\n", dim, runtime.Version(), reset)

	// Run ID (generated fresh each run)
	runID := replay.CurrentRunID
	if runID == "" {
		// Generate one early if not yet set, and assign to global for consistency
		runID = fmt.Sprintf("%08x", time.Now().UnixNano()&0xFFFFFFFF)
		replay.CurrentRunID = runID
	}
	fmt.Printf("  Run ID:       %s%s%s\n", dim, runID, reset)

	// Command being run
	fmt.Printf("  Command:      %s%s%s\n", cyan, commandName, reset)

	// Bootstrap mode with description
	var bootstrapDesc string
	switch bootstrapMode {
	case "auto":
		bootstrapDesc = "use existing AccountsDB if valid, else download snapshot"
	case "snapshot":
		bootstrapDesc = "rebuild from local snapshot"
	case "new-snapshot":
		bootstrapDesc = "download fresh snapshot from network"
	case "accountsdb":
		bootstrapDesc = "require existing AccountsDB"
	default:
		bootstrapDesc = ""
	}
	if bootstrapDesc != "" {
		fmt.Printf("  Bootstrap:    %s%s%s %s(%s)%s\n", green, bootstrapMode, reset, dim, bootstrapDesc, reset)
	} else {
		fmt.Printf("  Bootstrap:    %s%s%s\n", green, bootstrapMode, reset)
	}

	// Load state file for detailed info
	mithrilState, _ := state.LoadState(accountsPath)

	// Show state info if available
	if mithrilState != nil && mithrilState.IsReady() {
		fmt.Printf("%s━━━ State Info ━━━%s\n", gold, reset)

		// Original snapshot info
		snapshotInfo := fmt.Sprintf("slot %d", mithrilState.SnapshotSlot)
		if mithrilState.SnapshotEpoch > 0 {
			snapshotInfo = fmt.Sprintf("slot %d (epoch %d)", mithrilState.SnapshotSlot, mithrilState.SnapshotEpoch)
		}
		fmt.Printf("  Snapshot:     %s%s%s\n", dim, snapshotInfo, reset)

		// Current/resume slot info
		if mithrilState.LastSlot > 0 {
			resumeInfo := fmt.Sprintf("slot %d", mithrilState.LastSlot)
			if mithrilState.LastEpoch > 0 {
				resumeInfo = fmt.Sprintf("slot %d (epoch %d)", mithrilState.LastSlot, mithrilState.LastEpoch)
			}
			fmt.Printf("  Resume from:  %s%s%s\n", cyan, resumeInfo, reset)

			// Slots replayed so far
			slotsReplayed := mithrilState.LastSlot - mithrilState.SnapshotSlot
			fmt.Printf("  Replayed:     %s%d slots%s\n", dim, slotsReplayed, reset)
		} else {
			fmt.Printf("  Resume from:  %ssnapshot (fresh start)%s\n", dim, reset)
		}

		// Previous run info
		if mithrilState.LastRunID != "" {
			fmt.Printf("  Last run:     %s%s%s", dim, mithrilState.LastRunID, reset)
			if mithrilState.LastCommit != "" && mithrilState.LastCommit != revision {
				fmt.Printf(" %s(commit: %s)%s", dim, mithrilState.LastCommit, reset)
			}
			fmt.Println()
		}
	}

	fmt.Printf("%s━━━ Paths ━━━%s\n", gold, reset)

	// AccountsDB path
	if accountsPath != "" {
		fmt.Printf("  AccountsDB:   %s%s%s\n", cyan, accountsPath, reset)
	}

	// Blockstore path
	if blockstorePath != "" {
		fmt.Printf("  Blockstore:   %s%s%s\n", gold, blockstorePath, reset)
	}

	// Snapshots path - show configured snapshot directory
	snapshotDir := snapshotDlPath
	if snapshotDir == "" {
		snapshotDir = snapshotArchivePath
	}
	if snapshotDir != "" {
		fmt.Printf("  Snapshots:    %s%s%s\n", gold, snapshotDir, reset)
	}

	// RPC endpoints
	if len(rpcEndpoints) > 0 {
		fmt.Printf("  RPC:          %s%s%s\n", gold, rpcEndpoints[0], reset)
		for _, ep := range rpcEndpoints[1:] {
			fmt.Printf("                %s%s%s\n", gold, ep, reset)
		}
	}

	// Block source
	fmt.Printf("  Block source: %s%s%s", gold, blockSource, reset)
	if blockSource == "overcast" && overcastEndpoint != "" {
		fmt.Printf(" %s(%s)%s\n", dim, overcastEndpoint, reset)
	} else {
		fmt.Println()
	}

	fmt.Println()
}

// snapshotInfo holds information about a detected snapshot file
type snapshotInfo struct {
	filename string
	slot     uint64
	isIncr   bool
}

// detectExistingAccountsDB checks if a valid AccountsDB exists at the given path
// Returns (exists, slot) where slot is parsed from the manifest if available
func detectExistingAccountsDB(path string) (bool, uint64) {
	if path == "" {
		return false, 0
	}

	// Check for the accounts directory
	accountsDir := filepath.Join(path, "accounts")
	if _, err := os.Stat(accountsDir); os.IsNotExist(err) {
		return false, 0
	}

	// Check for manifest file
	manifestPath := filepath.Join(path, "manifest")
	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		return false, 0
	}

	// Try to load the manifest to get the slot
	manifest, err := snapshot.LoadManifestFromFile(manifestPath)
	if err != nil {
		// AccountsDB exists but manifest is unreadable
		return true, 0
	}

	return true, manifest.Bank.Slot
}

// detectExistingSnapshots finds snapshot files in the given directory
func detectExistingSnapshots(dir string) []snapshotInfo {
	if dir == "" {
		return nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var snapshots []snapshotInfo

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()

		// Full snapshot: snapshot-{slot}-{hash}.tar.zst
		if len(name) > 9 && name[:9] == "snapshot-" && filepath.Ext(name) == ".zst" {
			slot := parseSlotFromSnapshotName(name)
			snapshots = append(snapshots, snapshotInfo{
				filename: name,
				slot:     slot,
				isIncr:   false,
			})
		}

		// Incremental snapshot: incremental-snapshot-{baseSlot}-{endSlot}-{hash}.tar.zst
		if len(name) > 21 && name[:21] == "incremental-snapshot-" && filepath.Ext(name) == ".zst" {
			slot := parseSlotFromIncrementalName(name)
			snapshots = append(snapshots, snapshotInfo{
				filename: name,
				slot:     slot,
				isIncr:   true,
			})
		}
	}

	return snapshots
}

// parseSlotFromSnapshotName extracts slot from "snapshot-{slot}-{hash}.tar.zst"
func parseSlotFromSnapshotName(name string) uint64 {
	// Remove "snapshot-" prefix and ".tar.zst" suffix
	if len(name) <= 17 {
		return 0
	}
	trimmed := name[9 : len(name)-8] // "slot-hash"
	// Find the first dash after the slot number
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] == '-' {
			slot, err := strconv.ParseUint(trimmed[:i], 10, 64)
			if err != nil {
				return 0
			}
			return slot
		}
	}
	return 0
}

// parseSlotFromIncrementalName extracts end slot from "incremental-snapshot-{baseSlot}-{endSlot}-{hash}.tar.zst"
func parseSlotFromIncrementalName(name string) uint64 {
	// Remove "incremental-snapshot-" prefix and ".tar.zst" suffix
	if len(name) <= 29 {
		return 0
	}
	trimmed := name[21 : len(name)-8] // "baseSlot-endSlot-hash"

	// Find first dash (after baseSlot)
	firstDash := -1
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] == '-' {
			firstDash = i
			break
		}
	}
	if firstDash == -1 {
		return 0
	}

	// Find second dash (after endSlot)
	remaining := trimmed[firstDash+1:]
	for i := 0; i < len(remaining); i++ {
		if remaining[i] == '-' {
			slot, err := strconv.ParseUint(remaining[:i], 10, 64)
			if err != nil {
				return 0
			}
			return slot
		}
	}
	return 0
}

// detectFreshSnapshot checks for an existing snapshot file within the freshness threshold.
// Returns the snapshotInfo if found, nil otherwise.
func detectFreshSnapshot(snapshotDir string, fullThreshold int, rpcEndpoints []string, ctx context.Context) *snapshotInfo {
	if snapshotDir == "" {
		return nil
	}

	snapshots := detectExistingSnapshots(snapshotDir)
	if len(snapshots) == 0 {
		return nil
	}

	// Get current slot estimate from RPC
	currentSlot, err := queryCurrentSlot(ctx, rpcEndpoints)
	if err != nil {
		mlog.Log.Infof("could not query current slot: %v", err)
		return nil
	}

	// Find the freshest full snapshot within threshold
	var bestSnapshot *snapshotInfo
	for i := range snapshots {
		snap := &snapshots[i]
		if snap.isIncr {
			continue // Skip incrementals
		}
		if currentSlot > snap.slot && (currentSlot-snap.slot) <= uint64(fullThreshold) {
			if bestSnapshot == nil || snap.slot > bestSnapshot.slot {
				bestSnapshot = snap
			}
		}
	}

	return bestSnapshot
}

// queryCurrentSlot gets the current slot from RPC.
func queryCurrentSlot(ctx context.Context, rpcEndpoints []string) (uint64, error) {
	if len(rpcEndpoints) == 0 {
		return 0, fmt.Errorf("no RPC endpoints configured")
	}

	// Use the first RPC endpoint
	client := solrpc.New(rpcEndpoints[0])
	slot, err := client.GetSlot(ctx, solrpc.CommitmentFinalized)
	if err != nil {
		return 0, fmt.Errorf("failed to get slot from RPC: %w", err)
	}
	return slot, nil
}

// queryLatestSnapshotSlot queries the latest available snapshot slot from the network.
func queryLatestSnapshotSlot(ctx context.Context, rpcEndpoints []string) (uint64, error) {
	if len(rpcEndpoints) == 0 {
		return 0, fmt.Errorf("no RPC endpoints configured")
	}

	snapCfg := buildSnapshotConfig(rpcEndpoints)
	info, err := snapshotdl.GetSnapshotURLWithInfo(ctx, snapCfg)
	if err != nil {
		return 0, fmt.Errorf("failed to query snapshot info: %w", err)
	}
	return uint64(info.Slot), nil
}

// buildFromExistingSnapshot builds AccountsDB from an existing downloaded snapshot file.
func buildFromExistingSnapshot(ctx context.Context, snap *snapshotInfo, snapshotDir, accountsPath, blockstorePath, overcastAddr string, rpcEndpoints []string) (*accountsdb.AccountsDb, *snapshot.SnapshotManifest, error) {
	snapCfg := buildSnapshotConfig(rpcEndpoints)

	// Construct full path to snapshot file
	fullSnapshotPath := filepath.Join(snapshotDir, snap.filename)
	mlog.Log.Infof("building AccountsDB from existing snapshot: %s", fullSnapshotPath)

	// Create progress display for extract
	dp := progress.NewDualProgress()

	accountsDb, manifest, err := snapshot.BuildAccountsDbWithIncr(ctx, fullSnapshotPath, snapshotDir, int(snap.slot), int(snap.slot), accountsPath, rpcEndpoints, blockstorePath, overcastAddr, snapCfg, dp)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to build AccountsDB from snapshot: %w", err)
	}
	mlog.Log.Infof("finished building AccountsDB from existing snapshot")

	return accountsDb, manifest, nil
}

// downloadAndBuildFromSnapshot finds, downloads, and builds AccountsDB from a snapshot
func downloadAndBuildFromSnapshot(ctx context.Context, rpcEndpoints []string, snapshotDownloadPath, accountsPath, blockstorePath, overcastAddr string) (*accountsdb.AccountsDb, *snapshot.SnapshotManifest, error) {
	snapCfg := buildSnapshotConfig(rpcEndpoints)
	fullSnapshotDlStart := time.Now()
	fullSnapshotInfo, err := snapshotdl.GetSnapshotURLWithInfo(ctx, snapCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("error getting snapshot URL: %w", err)
	}
	fullSnapshotURL := fullSnapshotInfo.URL
	fullSnapshotSlot := fullSnapshotInfo.Slot

	// Print a clean summary of the selected snapshot source
	progress.PrintSnapshotSourceSummary(
		fullSnapshotInfo.NodeIP,
		fullSnapshotInfo.Slot,
		fullSnapshotInfo.ReferenceSlot,
		fullSnapshotInfo.NodeVersion,
		fullSnapshotInfo.SpeedMBs,
		fullSnapshotInfo.RTTMs,
		time.Since(fullSnapshotDlStart),
	)

	// Create progress display for snapshot download and extract
	dp := progress.NewDualProgress()

	accountsDb, manifest, err := snapshot.BuildAccountsDbWithIncr(ctx, fullSnapshotURL, snapshotDownloadPath, fullSnapshotSlot, fullSnapshotSlot, accountsPath, rpcEndpoints, blockstorePath, overcastAddr, snapCfg, dp)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to build AccountsDB from snapshot: %w", err)
	}
	mlog.Log.Infof("finished building AccountsDB")

	return accountsDb, manifest, nil
}

// killExistingMithrilProcesses finds and kills any other running mithril processes.
// This prevents zombie processes from accumulating and holding disk space.
// Returns the number of processes killed.
func killExistingMithrilProcesses() int {
	myPID := os.Getpid()
	myPPID := os.Getppid()

	// Use pgrep to find mithril processes by executable name (not full command line)
	// This avoids matching sudo or shell processes that have "mithril" in args
	cmd := exec.Command("pgrep", "-x", "mithril")
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		// No processes found or pgrep not available
		return 0
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	killed := 0

	for _, line := range lines {
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			continue
		}

		// Don't kill ourselves or our parent (sudo)
		if pid == myPID || pid == myPPID {
			continue
		}

		// Try to kill the process
		proc, err := os.FindProcess(pid)
		if err != nil {
			continue
		}

		// Send SIGKILL
		if err := proc.Signal(syscall.SIGKILL); err == nil {
			killed++
		}
	}

	return killed
}

// decodeRecentBlockhashes converts state.BlockhashEntry list to sealevel.SysvarRecentBlockhashes
func decodeRecentBlockhashes(entries []state.BlockhashEntry) sealevel.SysvarRecentBlockhashes {
	result := make(sealevel.SysvarRecentBlockhashes, 0, len(entries))
	for _, entry := range entries {
		hashBytes, err := base58.Decode(entry.Blockhash)
		if err != nil || len(hashBytes) != 32 {
			continue
		}
		var blockhash [32]byte
		copy(blockhash[:], hashBytes)
		result = append(result, sealevel.RecentBlockHashesEntry{
			Blockhash:     blockhash,
			FeeCalculator: sealevel.FeeCalculator{LamportsPerSignature: entry.LamportsPerSignature},
		})
	}
	return result
}

// encodeRecentBlockhashes converts sealevel.SysvarRecentBlockhashes to state.BlockhashEntry list
func encodeRecentBlockhashes(sysvar *sealevel.SysvarRecentBlockhashes) []state.BlockhashEntry {
	if sysvar == nil {
		return nil
	}
	result := make([]state.BlockhashEntry, 0, len(*sysvar))
	for _, entry := range *sysvar {
		result = append(result, state.BlockhashEntry{
			Blockhash:            base58.Encode(entry.Blockhash[:]),
			LamportsPerSignature: entry.FeeCalculator.LamportsPerSignature,
		})
	}
	return result
}

func createBufWriter(filename string) (io.Writer, func(), error) {
	if filename == "" {
		return nil, func() {}, nil
	}

	file, err := os.Create(filename)
	if err != nil {
		return nil, nil, err
	}

	writer := bufio.NewWriter(file)

	cleanup := func() {
		writer.Flush()
		file.Close()
	}

	return writer, cleanup, nil
}

// runReplayWithRecovery wraps replay.ReplayBlocks with panic recovery.
// It distinguishes between:
// - Panics during commit (commitInProgress=true): AccountsDB may be corrupted, marks state as corrupted
// - Panics outside commit (divergence): AccountsDB is safe, logs helpful message
// After handling, it re-panics to propagate the error.
func runReplayWithRecovery(
	ctx context.Context,
	accountsDb *accountsdb.AccountsDb,
	accountsDbPath string,
	manifest *snapshot.SnapshotManifest,
	resumeState *replay.ResumeState,
	startSlot, endSlot uint64,
	rpcEndpoint string,
	blockDir string,
	txParallelism int,
	isLive bool,
	useOvercast bool,
	dbgOpts *replay.DebugOptions,
	metricsWriter io.Writer,
	rpcServer *rpcserver.RpcServer,
	mithrilState *state.MithrilState,
) *replay.ReplayResult {
	var result *replay.ReplayResult

	defer func() {
		if r := recover(); r != nil {
			runID := replay.CurrentRunID
			if replay.IsCommitInProgress() {
				commitSlot := replay.GetCommitSlot()
				mlog.Log.Errorf("[run:%s] FATAL: Panic during commit at slot %d - AccountsDB may be corrupted", runID, commitSlot)
				mlog.Log.Errorf("[run:%s] Panic occurred between StoreAccounts and StoreBankHashForSlot", runID)
				// Mark state as corrupted so next startup knows to rebuild
				if mithrilState != nil {
					reason := fmt.Sprintf("panic during commit at slot %d: %v", commitSlot, r)
					if err := mithrilState.MarkCorrupted(accountsDbPath, reason); err != nil {
						mlog.Log.Errorf("[run:%s] Failed to mark state as corrupted: %v", runID, err)
					}
				}
			} else {
				mlog.Log.Errorf("[run:%s] Panic during replay (outside commit window) - AccountsDB is SAFE", runID)
				mlog.Log.Errorf("[run:%s] This appears to be a divergence panic. You can resume from the last persisted slot.", runID)
			}
			// Print debug info and paths
			mlog.Log.Errorf("[run:%s] Debug info:", runID)
			if mithrilState != nil {
				mlog.Log.Errorf("[run:%s]   Snapshot slot: %d", runID, mithrilState.SnapshotSlot)
				if mithrilState.FullSnapshot != nil {
					mlog.Log.Errorf("[run:%s]   Full snapshot:  %s (slot %d)", runID, mithrilState.FullSnapshot.Path, mithrilState.FullSnapshot.Slot)
				}
				if mithrilState.IncrSnapshot != nil {
					mlog.Log.Errorf("[run:%s]   Incr snapshot:  %s (base %d -> slot %d)", runID, mithrilState.IncrSnapshot.Path, mithrilState.IncrSnapshot.BaseSlot, mithrilState.IncrSnapshot.Slot)
				}
				if mithrilState.LastSlot > 0 {
					mlog.Log.Errorf("[run:%s]   Last persisted: slot %d, bankhash %s", runID, mithrilState.LastSlot, mithrilState.LastBankhash)
				}
			}
			mlog.Log.Errorf("[run:%s] Debug files:", runID)
			mlog.Log.Errorf("[run:%s]   Bankhash log: %s/bankhash.log", runID, accountsDbPath)
			mlog.Log.Errorf("[run:%s]   State file:   %s/mithril_state.json", runID, accountsDbPath)
			// Re-panic to propagate the error
			panic(r)
		}
	}()

	result = replay.ReplayBlocks(ctx, accountsDb, accountsDbPath, manifest, resumeState, startSlot, endSlot, rpcEndpoint, blockDir, txParallelism, isLive, useOvercast, dbgOpts, metricsWriter, rpcServer)
	return result
}
