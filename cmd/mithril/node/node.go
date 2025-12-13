package node

import (
	"bufio"
	"context"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"syscall"
	"time"

	_ "net/http/pprof"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/arena"
	"github.com/Overclock-Validator/mithril/pkg/config"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/replay"
	"github.com/Overclock-Validator/mithril/pkg/rpcserver"
	"github.com/Overclock-Validator/mithril/pkg/sbpf"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/snapshot"
	"github.com/Overclock-Validator/mithril/pkg/snapshotdl"
	"github.com/spf13/cobra"
	"k8s.io/klog/v2"
)

var (
	Verifier = cobra.Command{
		Use:   "verifier",
		Short: "Run mithril verifier node",
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return initConfigAndBindFlags(cmd)
		},
		Run: func(cmd *cobra.Command, args []string) {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			cmd.SetContext(ctx)
			defer cancel()
			runVerifier(cmd, args)
		},
	}

	CatchupRpc = cobra.Command{
		Use:   "catchup-rpc",
		Short: "Catchup and run live from RPC",
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return initConfigAndBindFlags(cmd)
		},
		Run: func(cmd *cobra.Command, args []string) {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			cmd.SetContext(ctx)
			defer cancel()
			runRpcCatchup(cmd, args)
		},
	}

	CatchupOvercast = cobra.Command{
		Use:   "catchup-overcast",
		Short: "Catchup and run live from Overcast",
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return initConfigAndBindFlags(cmd)
		},
		Run: func(cmd *cobra.Command, args []string) {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			cmd.SetContext(ctx)
			defer cancel()
			runOvercastCatchup(cmd, args)
		},
	}

	loadFromSnapshot            bool
	loadFromAccountsDb          bool
	snapshotArchivePath         string
	incrementalSnapshotFilename string
	accountsPath     string
	scratchDirectory string
	rpcEndpoint      string
	overcastEndpoint string
	snapshotDlPath   string
	numReplaySlots              int64
	endSlot                     int64
	pprofPort                   int64
	ledgerPath                  string
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
	Verifier.Flags().BoolVarP(&loadFromSnapshot, "load-from-snapshot", "s", false, "Load from a full snapshot")
	Verifier.Flags().BoolVarP(&loadFromAccountsDb, "load-from-accounts-db", "a", false, "Load from AccountsDB")
	Verifier.Flags().Int64Var(&numReplaySlots, "num-slots", 0, "Number of slots to replay.")
	Verifier.Flags().Int64VarP(&endSlot, "end-slot", "e", -1, "Block at which to stop replaying, inclusive")
	Verifier.Flags().Int64Var(&txParallelism, "txpar", 0, "Set to 0 to use sequential execution, or >0 to execute a topsort tx plan with the given number of workers")

	// [ledger] section flags
	Verifier.Flags().StringVarP(&snapshotArchivePath, "snapshot-archive-path", "p", "", "Path of full snapshot or AccountsDB to load from")
	Verifier.Flags().StringVar(&incrementalSnapshotFilename, "incremental-snapshot", "", "Filename containing incremental snapshot")
	Verifier.Flags().StringVarP(&accountsPath, "accounts-path", "o", "", "Output path for writing AccountsDB data to")
	Verifier.Flags().StringVar(&ledgerPath, "ledger-path", "/tmp/blocks", "Path containing slot.json files")

	// [rpc] section flags
	Verifier.Flags().StringVarP(&rpcEndpoint, "rpc", "r", "", "URL for RPC endpoint")
	Verifier.Flags().IntVar(&rpcPort, "rpc-port", 0, "RPC server port. Default off.")

	// [development] section flags
	Verifier.Flags().Uint64Var(&paramArenaSizeMB, "param-arena-size-mb", 512, "Size in MB for serialized parameter arena (0 to disable)")
	Verifier.Flags().Uint64Var(&borrowedAccountArenaSize, "borrowed-account-arena-size", 1024, "Number of borrowed accounts to preallocate in arena (0 to disable)")
	Verifier.Flags().IntVar(&snapshot.ZstdDecoderConcurrency, "zstd-decoder-concurrency", runtime.NumCPU(), "Zstd decoder concurrency")
	Verifier.Flags().IntVar(&snapshot.MaxConcurrentFlushers, "max-concurrent-flushers", 16, "Bound for number of log shards to flush to Accounts DB Index at once.")
	Verifier.Flags().BoolVar(&sbpf.UsePool, "use-pool", true, "Disable to allocate fresh slices")

	// [development.pprof] section flags
	Verifier.Flags().Int64Var(&pprofPort, "pprof-port", -1, "Port to serve HTTP pprof endpoint")
	Verifier.Flags().StringVar(&cpuprofPath, "cpu-profile-path", "", "Filename to write CPU profile")

	// [development.debug] section flags
	Verifier.Flags().StringSliceVar(&debugTxs, "transaction-signatures", []string{}, "Pass tx signature strings to enable debug logging during that transaction's execution")
	Verifier.Flags().StringSliceVar(&debugAcctWrites, "account-writes", []string{}, "Pass account pubkeys to enable debug logging of transactions that modify the account")

	// [reporting] section flags
	Verifier.Flags().StringVar(&metricsPath, "metrics-path", "", "Filename to write JSONL records of latencies")

	// [overcast] section flags
	Verifier.Flags().StringVar(&snapshotDlPath, "download-snapshot-path", "", "Path to download snapshot to")

	// flags for RPC catchup mode
	// [ledger] section flags
	CatchupRpc.Flags().StringVarP(&accountsPath, "accounts-path", "o", "", "Output path for writing AccountsDB data to")
	CatchupRpc.Flags().StringVar(&ledgerPath, "ledger-path", "/tmp/blocks", "Path containing slot.json files")

	// [rpc] section flags
	CatchupRpc.Flags().StringVarP(&rpcEndpoint, "rpc", "r", "", "URL for RPC endpoint")
	CatchupRpc.Flags().IntVar(&rpcPort, "rpc-port", 0, "RPC server port. Default off.")

	// [replay] section flags
	CatchupRpc.Flags().Int64Var(&txParallelism, "txpar", 0, "Set to 0 to use sequential execution, or >0 to execute a topsort tx plan with the given number of workers")

	// [development] section flags
	CatchupRpc.Flags().Uint64Var(&paramArenaSizeMB, "param-arena-size-mb", 512, "Size in MB for serialized parameter arena (0 to disable)")
	CatchupRpc.Flags().Uint64Var(&borrowedAccountArenaSize, "borrowed-account-arena-size", 1024, "Number of borrowed accounts to preallocate in arena (0 to disable)")
	CatchupRpc.Flags().IntVar(&snapshot.ZstdDecoderConcurrency, "zstd-decoder-concurrency", runtime.NumCPU(), "Zstd decoder concurrency")
	CatchupRpc.Flags().IntVar(&snapshot.MaxConcurrentFlushers, "max-concurrent-flushers", 16, "Bound for number of log shards to flush to Accounts DB Index at once.")
	CatchupRpc.Flags().BoolVar(&sbpf.UsePool, "use-pool", true, "Disable to allocate fresh slices")

	// [development.pprof] section flags
	CatchupRpc.Flags().StringVar(&cpuprofPath, "cpu-profile-path", "", "Filename to write CPU profile")

	// [development.debug] section flags
	CatchupRpc.Flags().StringSliceVar(&debugTxs, "transaction-signatures", []string{}, "Pass tx signature strings to enable debug logging during that transaction's execution")
	CatchupRpc.Flags().StringSliceVar(&debugAcctWrites, "account-writes", []string{}, "Pass account pubkeys to enable debug logging of transactions that modify the account")

	// [reporting] section flags
	CatchupRpc.Flags().StringVar(&metricsPath, "metrics-path", "", "Filename to write JSONL records of latencies")

	// Top-level flags
	CatchupRpc.Flags().StringVar(&scratchDirectory, "scratch-directory", "/tmp", "Path for downloads (e.g. snapshots) and other temp state")

	// flags for Overcast catchup mode
	// [ledger] section flags
	CatchupOvercast.Flags().StringVarP(&accountsPath, "accounts-path", "o", "", "Output path for writing AccountsDB data to")
	CatchupOvercast.Flags().StringVar(&ledgerPath, "ledger-path", "/tmp/blocks", "Path containing slot.json files")

	// [rpc] section flags
	CatchupOvercast.Flags().StringVarP(&rpcEndpoint, "rpc", "r", "", "URL for RPC endpoint")
	CatchupOvercast.Flags().IntVar(&rpcPort, "rpc-port", 0, "RPC server port. Default off.")

	// [overcast] section flags
	CatchupOvercast.Flags().StringVarP(&overcastEndpoint, "overcast-endpoint", "", "", "Address for Overcast endpoint")

	// [replay] section flags
	CatchupOvercast.Flags().Int64Var(&txParallelism, "txpar", 0, "Set to 0 to use sequential execution, or >0 to execute a topsort tx plan with the given number of workers")

	// [development] section flags
	CatchupOvercast.Flags().Uint64Var(&paramArenaSizeMB, "param-arena-size-mb", 512, "Size in MB for serialized parameter arena (0 to disable)")
	CatchupOvercast.Flags().Uint64Var(&borrowedAccountArenaSize, "borrowed-account-arena-size", 1024, "Number of borrowed accounts to preallocate in arena (0 to disable)")
	CatchupOvercast.Flags().IntVar(&snapshot.ZstdDecoderConcurrency, "zstd-decoder-concurrency", runtime.NumCPU(), "Zstd decoder concurrency")
	CatchupOvercast.Flags().IntVar(&snapshot.MaxConcurrentFlushers, "max-concurrent-flushers", 16, "Bound for number of log shards to flush to Accounts DB Index at once.")
	CatchupOvercast.Flags().BoolVar(&sbpf.UsePool, "use-pool", true, "Disable to allocate fresh slices")

	// [development.pprof] section flags
	CatchupOvercast.Flags().StringVar(&cpuprofPath, "cpu-profile-path", "", "Filename to write CPU profile")

	// [development.debug] section flags
	CatchupOvercast.Flags().StringSliceVar(&debugTxs, "transaction-signatures", []string{}, "Pass tx signature strings to enable debug logging during that transaction's execution")
	CatchupOvercast.Flags().StringSliceVar(&debugAcctWrites, "account-writes", []string{}, "Pass account pubkeys to enable debug logging of transactions that modify the account")

	// [reporting] section flags
	CatchupOvercast.Flags().StringVar(&metricsPath, "metrics-path", "", "Filename to write JSONL records of latencies")

	// Top-level flags
	CatchupOvercast.Flags().StringVar(&scratchDirectory, "scratch-directory", "/tmp", "Path for downloads (e.g. snapshots) and other temp state")
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
			return config.GetString(cliKey)
		}
		return config.GetString(tomlKey)
	}

	// Helper to get int: CLI flag if explicitly set, otherwise TOML config
	getInt := func(cliKey, tomlKey string) int {
		if flagChanged(cliKey) {
			return config.GetInt(cliKey)
		}
		return config.GetInt(tomlKey)
	}

	// Helper to get int64: CLI flag if explicitly set, otherwise TOML config
	getInt64 := func(cliKey, tomlKey string) int64 {
		if flagChanged(cliKey) {
			return config.GetInt64(cliKey)
		}
		return config.GetInt64(tomlKey)
	}

	// Helper to get uint64: CLI flag if explicitly set, otherwise TOML config
	getUint64 := func(cliKey, tomlKey string) uint64 {
		if flagChanged(cliKey) {
			return config.GetUint64(cliKey)
		}
		return config.GetUint64(tomlKey)
	}

	// Helper to get bool: CLI flag if explicitly set, otherwise TOML config
	getBool := func(cliKey, tomlKey string) bool {
		if flagChanged(cliKey) {
			return config.GetBool(cliKey)
		}
		return config.GetBool(tomlKey)
	}

	// Helper to get string slice: CLI flag if explicitly set, otherwise TOML config
	getStringSlice := func(cliKey, tomlKey string) []string {
		if flagChanged(cliKey) {
			return config.GetStringSlice(cliKey)
		}
		return config.GetStringSlice(tomlKey)
	}

	// Update variables (CLI flags take precedence over TOML config when explicitly set)
	// CLI flag names -> TOML nested keys (Firedancer-style)

	// [replay] section
	loadFromSnapshot = getBool("load-from-snapshot", "replay.load_from_snapshot")
	loadFromAccountsDb = getBool("load-from-accounts-db", "replay.load_from_accounts_db")
	numReplaySlots = getInt64("num-slots", "replay.num_slots")
	endSlot = getInt64("end-slot", "replay.end_slot")
	txParallelism = getInt64("txpar", "replay.txpar")

	// [ledger] section
	snapshotArchivePath = getString("snapshot-archive-path", "ledger.snapshot_archive_path")
	incrementalSnapshotFilename = getString("incremental-snapshot", "ledger.incremental_snapshot")
	accountsPath = getString("accounts-path", "ledger.accounts_path")
	ledgerPath = getString("ledger-path", "ledger.path")

	// [rpc] section
	rpcEndpoint = getString("rpc", "rpc.rpc")
	rpcPort = getInt("rpc-port", "rpc.port")

	// Top-level
	scratchDirectory = getString("scratch-directory", "scratch_directory")

	// [overcast] section
	overcastEndpoint = getString("overcast-endpoint", "overcast.endpoint")
	snapshotDlPath = getString("download-snapshot-path", "overcast.download_snapshot_path")

	// [development.pprof] section
	pprofPort = getInt64("pprof-port", "development.pprof.port")
	cpuprofPath = getString("cpu-profile-path", "development.pprof.cpu_profile_path")

	// [development.debug] section
	debugTxs = getStringSlice("transaction-signatures", "development.debug.transaction_signatures")
	debugAcctWrites = getStringSlice("account-writes", "development.debug.account_writes")

	// [development] section
	paramArenaSizeMB = getUint64("param-arena-size-mb", "development.param_arena_size_mb")
	borrowedAccountArenaSize = getUint64("borrowed-account-arena-size", "development.borrowed_account_arena_size")

	// [reporting] section
	metricsPath = getString("metrics-path", "reporting.metrics_path")

	// Handle external package variables
	if flagChanged("zstd-decoder-concurrency") {
		snapshot.ZstdDecoderConcurrency = config.GetInt("zstd-decoder-concurrency")
	} else if config.IsSet("development.zstd_decoder_concurrency") {
		snapshot.ZstdDecoderConcurrency = config.GetInt("development.zstd_decoder_concurrency")
	}
	if flagChanged("max-concurrent-flushers") {
		snapshot.MaxConcurrentFlushers = config.GetInt("max-concurrent-flushers")
	} else if config.IsSet("development.max_concurrent_flushers") {
		snapshot.MaxConcurrentFlushers = config.GetInt("development.max_concurrent_flushers")
	}
	if flagChanged("use-pool") {
		sbpf.UsePool = config.GetBool("use-pool")
	} else if config.IsSet("development.use_pool") {
		sbpf.UsePool = config.GetBool("development.use_pool")
	}

	return nil
}

func runVerifier(c *cobra.Command, args []string) {
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

	if rpcEndpoint == "" {
		rpcEndpoint = "https://api.mainnet-beta.solana.com"
	}

	if loadFromSnapshot {
		if snapshotArchivePath == "" || accountsPath == "" {
			klog.Fatalf("must specify snapshot path and directory path for writing generated AccountsDB")
		}

		mlog.Log.Infof("building AccountsDB from snapshot at %s\n", snapshotArchivePath)

		// extract accountvecs from full snapshot, build accountsdb index, and write it all out to disk
		accountsDb, manifest, err = snapshot.BuildAccountsDb(snapshotArchivePath, incrementalSnapshotFilename, accountsPath)
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

		var dlPath string
		dlPath, _, _, err = snapshotdl.DownloadSnapshot("https://api.mainnet-beta.solana.com", snapshotDlPath)
		if err != nil {
			klog.Fatalf("error downloading snapshot: %s", err)
		}

		accountsDb, manifest, err = snapshot.BuildAccountsDb(dlPath, incrementalSnapshotFilename, accountsPath)
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
	}

	startSlot := int64(manifest.Bank.Slot + 1)
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

	replay.ReplayBlocks(c.Context(), accountsDb, accountsDbDir, manifest, uint64(startSlot), uint64(endSlot), rpcEndpoint, ledgerPath, int(txParallelism), false, false, dbgOpts, metricsWriter, rpcServer)
	mlog.Log.Infof("done replaying, closing DB")
	accountsDb.CloseDb()
}

func runRpcCatchup(c *cobra.Command, args []string) {
	logVCSInfo()
	snapshotDownloadPath := scratchDirectory

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

	if rpcEndpoint == "" {
		rpcEndpoint = "https://api.mainnet-beta.solana.com"
	}

	mlog.Log.Infof("downloading full snapshot...")
	fullSnapshotDlStart := time.Now()
	fullSnapshotPath, _, fullSnapshotSlot, err := snapshotdl.DownloadSnapshot("https://api.mainnet-beta.solana.com", snapshotDownloadPath)
	if err != nil {
		klog.Fatalf("error downloading snapshot: %s", err)
	}
	mlog.Log.Infof("finished downloading full snapshot in %s to %s", time.Since(fullSnapshotDlStart), fullSnapshotPath)

	accountsDb, manifest, err := snapshot.BuildAccountsDbWithIncr(fullSnapshotPath, snapshotDownloadPath, fullSnapshotSlot, fullSnapshotSlot, accountsPath, rpcEndpoint, ledgerPath, "")
	if err != nil {
		klog.Fatalf("failed to populate new accounts db from snapshot %s: %s", snapshotArchivePath, err)
	}
	mlog.Log.Infof("finished building accountsdb")

	startSlot := int64(manifest.Bank.Slot + 1)
	endSlot := uint64(math.MaxUint64)

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

	replay.ReplayBlocks(c.Context(), accountsDb, accountsPath, manifest, uint64(startSlot), uint64(endSlot), rpcEndpoint, ledgerPath, int(txParallelism), true, false, dbgOpts, metricsWriter, rpcServer)
	mlog.Log.Infof("done replaying, closing DB")
	accountsDb.CloseDb()
}

func runOvercastCatchup(c *cobra.Command, args []string) {
	logVCSInfo()
	snapshotDownloadPath := scratchDirectory

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

	if rpcEndpoint == "" {
		rpcEndpoint = "https://api.mainnet-beta.solana.com"
	}

	if overcastEndpoint == "" {
		klog.Fatalf("must provide Overcast node address")
	}

	mlog.Log.Infof("downloading full snapshot...")
	fullSnapshotDlStart := time.Now()
	fullSnapshotPath, _, fullSnapshotSlot, err := snapshotdl.DownloadSnapshot("https://api.mainnet-beta.solana.com", snapshotDownloadPath)
	if err != nil {
		klog.Fatalf("error downloading snapshot: %s", err)
	}
	mlog.Log.Infof("finished downloading full snapshot in %s to %s", time.Since(fullSnapshotDlStart), fullSnapshotPath)

	accountsDb, manifest, err := snapshot.BuildAccountsDbWithIncr(fullSnapshotPath, snapshotDownloadPath, fullSnapshotSlot, fullSnapshotSlot, accountsPath, rpcEndpoint, ledgerPath, overcastEndpoint)
	if err != nil {
		klog.Fatalf("failed to populate new accounts db from snapshot %s: %s", snapshotArchivePath, err)
	}
	mlog.Log.Infof("finished building accountsdb")

	startSlot := int64(manifest.Bank.Slot + 1)
	endSlot := uint64(math.MaxUint64)

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

	replay.ReplayBlocks(c.Context(), accountsDb, accountsPath, manifest, uint64(startSlot), uint64(endSlot), rpcEndpoint, ledgerPath, int(txParallelism), true, true, dbgOpts, metricsWriter, rpcServer)
	mlog.Log.Infof("done replaying, closing DB")
	accountsDb.CloseDb()
}

func logVCSInfo() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		mlog.Log.Errorf("VCS info: not available")
		return
	}

	var revision, time, modified string

	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.time":
			time = setting.Value
		case "vcs.modified":
			modified = setting.Value
		}
	}

	mlog.Log.Infof("VCS info: revision=%s time=%s modified=%s", revision, time, modified)
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
