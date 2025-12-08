package statsd

import (
	"fmt"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	mithrilmetrics "github.com/Overclock-Validator/mithril/pkg/metrics"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
)

type metricType int

const (
	DistributionT metricType = iota
	CountT
	GaugeT
)

type metric string

// To regiter a new metric, add it to the metricToType and metricToLabels maps below.
const (
	PreprocessBlock     metric = "preprocess_block"
	TxLoop              metric = "tx_loop"
	LoadBlockAccounts   metric = "load_block_accounts"
	RunIncinerator      metric = "run_incinerator"
	Reward              metric = "reward"
	Rent                metric = "rent"
	BlockUpdateAccounts metric = "block_update_accounts"
	AccountsDeltaHash   metric = "accounts_delta_hash"
	BankHash            metric = "bank_hash"

	InstructionsAndAccountMetasFromTx  metric = "instructions_and_account_metas_from_tx"
	ComputeBudgetExecutionInstructions metric = "compute_budget_execution_instructions"
	AccountsFromTx                     metric = "accounts_from_tx"
	PreBalanceDivergenceCheck          metric = "pre_balance_divergence_check"
	CalcAndDeductFees                  metric = "calc_and_deduct_fees"
	ReadRentSysvar                     metric = "read_rent_sysvar"
	PreTxRentStates                    metric = "pre_tx_rent_states"
	IxLoop                             metric = "ix_loop"
	PostTxRentStates                   metric = "post_tx_rent_states"
	PostBalanceDivergenceCheck         metric = "post_balance_divergence_check"
	TxUpdateAccounts                   metric = "tx_update_accounts"

	GetNextIxCtx                            metric = "get_next_ix_ctx"
	NextIxCtxConfigure                      metric = "next_ix_ctx_configure"
	IxPush                                  metric = "ix_push"
	IxPop                                   metric = "ix_pop"
	ExecIxResolveNativeProgram              metric = "exec_ix_resolve_native_program"
	ExecIxNativeProgramSystem               metric = "exec_ix_native_program_system"
	ExecIxNativeProgramStake                metric = "exec_ix_native_program_stake"
	ExecIxNativeProgramVote                 metric = "exec_ix_native_program_vote"
	ExecIxNativeProgramComputeBudget        metric = "exec_ix_native_program_compute_budget"
	ExecIxNativeProgramBpfLoader2           metric = "exec_ix_native_program_bpf_loader2"
	ExecIxNativeProgramBpfLoaderDeprecated  metric = "exec_ix_native_program_bpf_loader_deprecated"
	ExecIxNativeProgramBpfLoaderUpgradeable metric = "exec_ix_native_program_bpf_loader_upgradeable"
	ExecIxNativeProgramZkElgamalProof       metric = "exec_ix_native_program_zk_elgamal_proof"
	ExecIxNativeProgramEd25519Precompile    metric = "exec_ix_native_program_ed25519precompile"
	ExecIxNativeProgramSecp256kPrecompile   metric = "exec_ix_native_program_secp256k_precompile"
	FixupInstructionsSysvarAccount          metric = "fixup_instructions_sysvar_account"
	InstructionAccountsFromAccountMetas     metric = "instruction_accounts_from_account_metas"

	SbpfInterpreterNew metric = "sbpf_interpreter_new"
	SbpfInterpreterRun metric = "sbpf_interpreter_run"

	AddProgramToCache                metric = "add_program_to_cache"
	GetProgramAccount                metric = "get_program_account"
	GetProgramDataCached             metric = "get_program_data_cached"
	GetProgramDataUncachedAccountsDb metric = "get_program_data_uncached_accounts_db"
	GetProgramDataUncachedAccounts   metric = "get_program_data_uncached_accounts"
	GetProgramDataUncachedMarshal    metric = "get_program_data_uncached_marshal"

	TaskIndexEntryCommitterLatency metric = "tasks_index_entry_committer_latency"
	TaskSetIfSlotHigherLatency     metric = "tasks_set_if_slot_higher_latency"
	TasksIndexEntryBuilderLatency  metric = "tasks_index_entry_builder_latency"
	TasksAppendVecCopyingLatency   metric = "tasks_append_vec_copying_latency"
	SlotReplayDurationMs           metric = "slot_replay_duration_ms"
	TxsPerBlock                    metric = "txs_per_block"
	SnapshotTarBytesRead           metric = "snapshot_tar_bytes_read"
	SlotReplays                    metric = "slot_replays"

	SnapshotWorkerPoolUtilization metric = "snapshot_worker_pool_utilization"
	TasksSetIfSlotHigherQueueSize metric = "tasks_set_if_slot_higher_queue_size"
	Epoch                         metric = "epoch"
	Slot                          metric = "slot"
)

var metricToType = map[metric]metricType{
	PreprocessBlock:     DistributionT,
	TxLoop:              DistributionT,
	LoadBlockAccounts:   DistributionT,
	RunIncinerator:      DistributionT,
	Reward:              DistributionT,
	Rent:                DistributionT,
	BlockUpdateAccounts: DistributionT,
	AccountsDeltaHash:   DistributionT,
	BankHash:            DistributionT,

	InstructionsAndAccountMetasFromTx:  DistributionT,
	ComputeBudgetExecutionInstructions: DistributionT,
	AccountsFromTx:                     DistributionT,
	PreBalanceDivergenceCheck:          DistributionT,
	CalcAndDeductFees:                  DistributionT,
	ReadRentSysvar:                     DistributionT,
	PreTxRentStates:                    DistributionT,
	IxLoop:                             DistributionT,
	PostTxRentStates:                   DistributionT,
	PostBalanceDivergenceCheck:         DistributionT,
	TxUpdateAccounts:                   DistributionT,

	GetNextIxCtx:                            DistributionT,
	NextIxCtxConfigure:                      DistributionT,
	IxPush:                                  DistributionT,
	IxPop:                                   DistributionT,
	ExecIxResolveNativeProgram:              DistributionT,
	ExecIxNativeProgramSystem:               DistributionT,
	ExecIxNativeProgramStake:                DistributionT,
	ExecIxNativeProgramVote:                 DistributionT,
	ExecIxNativeProgramComputeBudget:        DistributionT,
	ExecIxNativeProgramBpfLoader2:           DistributionT,
	ExecIxNativeProgramBpfLoaderDeprecated:  DistributionT,
	ExecIxNativeProgramBpfLoaderUpgradeable: DistributionT,
	ExecIxNativeProgramZkElgamalProof:       DistributionT,
	ExecIxNativeProgramEd25519Precompile:    DistributionT,
	ExecIxNativeProgramSecp256kPrecompile:   DistributionT,
	FixupInstructionsSysvarAccount:          DistributionT,
	InstructionAccountsFromAccountMetas:     DistributionT,

	SbpfInterpreterNew:               DistributionT,
	SbpfInterpreterRun:               DistributionT,
	AddProgramToCache:                DistributionT,
	GetProgramAccount:                DistributionT,
	GetProgramDataCached:             DistributionT,
	GetProgramDataUncachedAccountsDb: DistributionT,
	GetProgramDataUncachedAccounts:   DistributionT,
	GetProgramDataUncachedMarshal:    DistributionT,

	TaskIndexEntryCommitterLatency: DistributionT,
	TaskSetIfSlotHigherLatency:     DistributionT,
	TasksIndexEntryBuilderLatency:  DistributionT,
	TasksAppendVecCopyingLatency:   DistributionT,

	SlotReplayDurationMs: DistributionT,
	TxsPerBlock:          DistributionT,

	SnapshotTarBytesRead: CountT,
	SlotReplays:          CountT,

	SnapshotWorkerPoolUtilization: GaugeT,
	TasksSetIfSlotHigherQueueSize: GaugeT,
	Epoch:                         GaugeT,
	Slot:                          GaugeT,
}

var metricToLabels = map[metric][]string{
	PreprocessBlock:     {"phase"},
	TxLoop:              {"phase"},
	LoadBlockAccounts:   {"phase"},
	RunIncinerator:      {"phase"},
	Reward:              {"phase"},
	Rent:                {"phase"},
	BlockUpdateAccounts: {"phase"},
	AccountsDeltaHash:   {"phase"},
	BankHash:            {"phase"},

	InstructionsAndAccountMetasFromTx:  {"phase"},
	ComputeBudgetExecutionInstructions: {"phase"},
	AccountsFromTx:                     {"phase"},
	PreBalanceDivergenceCheck:          {"phase"},
	CalcAndDeductFees:                  {"phase"},
	ReadRentSysvar:                     {"phase"},
	PreTxRentStates:                    {"phase"},
	IxLoop:                             {"phase"},
	PostTxRentStates:                   {"phase"},
	PostBalanceDivergenceCheck:         {"phase"},
	TxUpdateAccounts:                   {"phase"},

	GetNextIxCtx:                            {"phase"},
	NextIxCtxConfigure:                      {"phase"},
	IxPush:                                  {"phase"},
	IxPop:                                   {"phase"},
	ExecIxResolveNativeProgram:              {"phase"},
	ExecIxNativeProgramSystem:               {"phase"},
	ExecIxNativeProgramStake:                {"phase"},
	ExecIxNativeProgramVote:                 {"phase"},
	ExecIxNativeProgramComputeBudget:        {"phase"},
	ExecIxNativeProgramBpfLoader2:           {"phase"},
	ExecIxNativeProgramBpfLoaderDeprecated:  {"phase"},
	ExecIxNativeProgramBpfLoaderUpgradeable: {"phase"},
	ExecIxNativeProgramZkElgamalProof:       {"phase"},
	ExecIxNativeProgramEd25519Precompile:    {"phase"},
	ExecIxNativeProgramSecp256kPrecompile:   {"phase"},
	FixupInstructionsSysvarAccount:          {"phase"},
	InstructionAccountsFromAccountMetas:     {"phase"},

	SbpfInterpreterNew:               {"phase"},
	SbpfInterpreterRun:               {"phase"},
	AddProgramToCache:                {"phase"},
	GetProgramAccount:                {"phase"},
	GetProgramDataCached:             {"phase"},
	GetProgramDataUncachedAccountsDb: {"phase"},
	GetProgramDataUncachedAccounts:   {"phase"},
	GetProgramDataUncachedMarshal:    {"phase"},

	TaskIndexEntryCommitterLatency: {},
	TaskSetIfSlotHigherLatency:     {},
	TasksIndexEntryBuilderLatency:  {},
	TasksAppendVecCopyingLatency:   {},

	SlotReplayDurationMs: {},
	TxsPerBlock:          {},
	SnapshotTarBytesRead: {},
	SlotReplays:          {},

	SnapshotWorkerPoolUtilization: {"task"},
	TasksSetIfSlotHigherQueueSize: {},
	Epoch:                         {},
	Slot:                          {},
}

type prometheusmetrics struct {
	// Counters (for Count calls)
	counters map[metric]*prometheus.CounterVec
	// Gauges (for Gauge and runtime metrics)
	gauges map[metric]*prometheus.GaugeVec
	// Histograms (best fit for Distribution/Timing)
	histograms map[metric]*prometheus.HistogramVec
}

var metricsCollection *prometheusmetrics

func init() {
	metricsCollection = &prometheusmetrics{
		counters:   make(map[metric]*prometheus.CounterVec),
		gauges:     make(map[metric]*prometheus.GaugeVec),
		histograms: make(map[metric]*prometheus.HistogramVec),
	}
	var metricsPort = 9090 // Default Prometheus metrics port
	http.Handle("/metrics", promhttp.Handler())
	go func() {
		addr := fmt.Sprintf(":%d", metricsPort)
		mlog.Log.Infof("Prometheus metrics server starting on %s/metrics", addr)
		if err := http.ListenAndServe(addr, nil); err != nil {
			mlog.Log.Errorf("Prometheus metrics server failed: %v", err)
		}
	}()
	// register Prometheus metrics based on metricToType and metricToLabels maps
	for metric, metricType := range metricToType {
		labelNames := metricToLabels[metric]
		switch metricType {
		case DistributionT:
			histogramvec := promauto.NewHistogramVec(prometheus.HistogramOpts{
				Name:    string(metric),
				Help:    fmt.Sprintf("A histogram for the metric %s (Timing)", metric),
				Buckets: prometheus.DefBuckets,
			}, labelNames)
			metricsCollection.histograms[metric] = histogramvec
		case CountT:
			counterVec := promauto.NewCounterVec(prometheus.CounterOpts{
				Name: string(metric),
				Help: fmt.Sprintf("A counter for the metric %s (Count)", metric),
			}, labelNames)
			metricsCollection.counters[metric] = counterVec
		case GaugeT:
			gaugeVec := promauto.NewGaugeVec(prometheus.GaugeOpts{
				Name: string(metric),
				Help: fmt.Sprintf("A gauge for the metric %s (Gauge)", metric),
			}, labelNames)
			metricsCollection.gauges[metric] = gaugeVec
		}
	}

}

func Count(metric metric, value int64, labelValues []string) error {
	if labelValues == nil {
		labelValues = []string{}
	}
	counterVec := metricsCollection.counters[metric]

	counterVec.WithLabelValues(labelValues...).Add(float64(value))

	return nil
}

func Gauge(metric metric, value float64, labelValues []string) error {
	if labelValues == nil {
		labelValues = []string{}
	}
	gaugeVec := metricsCollection.gauges[metric]
	gaugeVec.WithLabelValues(labelValues...).Set(value)

	return nil
}

func Distribution(name metric, value float64, labelValues []string) error {
	if labelValues == nil {
		labelValues = []string{}
	}
	histogramVec := metricsCollection.histograms[metric(name)]
	histogramVec.WithLabelValues(labelValues...).Observe(value)
	return nil
}

func Timing(name metric, nanoSeconds uint64, labelValues []string) error {
	if labelValues == nil {
		labelValues = []string{}
	}
	histogramVec := metricsCollection.histograms[metric(name)]
	histogramVec.WithLabelValues(labelValues...).Observe(float64(nanoSeconds))
	return nil
}

func SendBlockReplayMetrics(r mithrilmetrics.BlockReplay) {
	blockLatency := "replay_block"
	txLatency := "replay_tx_sum"
	ixLatency := "replay_ix_sum"
	sbpfLatency := "replay_sbpf"

	Timing(PreprocessBlock, r.PreprocessBlock.SumNanoseconds, []string{blockLatency}, 1)
	Timing(LoadBlockAccounts, r.LoadBlockAccounts.SumNanoseconds, []string{blockLatency}, 1)
	Timing(TxLoop, r.TxLoop.SumNanoseconds, []string{blockLatency}, 1)
	Timing(Reward, r.Reward.SumNanoseconds, []string{blockLatency}, 1)
	Timing(Rent, r.Rent.SumNanoseconds, []string{blockLatency}, 1)
	Timing(RunIncinerator, r.RunIncinerator.SumNanoseconds, []string{blockLatency}, 1)
	Timing(BlockUpdateAccounts, r.BlockUpdateAccounts.SumNanoseconds, []string{blockLatency}, 1)
	Timing(AccountsDeltaHash, r.AccountsDeltaHash.SumNanoseconds, []string{blockLatency}, 1)
	Timing(BankHash, r.BankHash.SumNanoseconds, []string{blockLatency}, 1)
	Timing(InstructionsAndAccountMetasFromTx, r.InstructionsAndAccountMetasFromTx.SumNanoseconds, []string{txLatency}, 1)
	Timing(ComputeBudgetExecutionInstructions, r.ComputeBudgetExecutionInstructions.SumNanoseconds, []string{txLatency}, 1)
	Timing(AccountsFromTx, r.AccountsFromTx.SumNanoseconds, []string{txLatency}, 1)
	Timing(PreBalanceDivergenceCheck, r.PreBalanceDivergenceCheck.SumNanoseconds, []string{txLatency}, 1)
	Timing(CalcAndDeductFees, r.CalcAndDeductFees.SumNanoseconds, []string{txLatency}, 1)
	Timing(ReadRentSysvar, r.ReadRentSysvar.SumNanoseconds, []string{txLatency}, 1)
	Timing(PreTxRentStates, r.PreTxRentStates.SumNanoseconds, []string{txLatency}, 1)
	Timing(IxLoop, r.IxLoop.SumNanoseconds, []string{txLatency}, 1)
	Timing(PostTxRentStates, r.PostTxRentStates.SumNanoseconds, []string{txLatency}, 1)
	Timing(PostBalanceDivergenceCheck, r.PostBalanceDivergenceCheck.SumNanoseconds, []string{txLatency}, 1)
	Timing(TxUpdateAccounts, r.TxUpdateAccounts.SumNanoseconds, []string{txLatency}, 1)
	Timing(GetNextIxCtx, r.GetNextIxCtx.SumNanoseconds, []string{ixLatency}, 1)
	Timing(NextIxCtxConfigure, r.NextIxCtxConfigure.SumNanoseconds, []string{ixLatency}, 1)
	Timing(IxPush, r.IxPush.SumNanoseconds, []string{ixLatency}, 1)
	Timing(IxPop, r.IxPop.SumNanoseconds, []string{ixLatency}, 1)
	Timing(ExecIxResolveNativeProgram, r.ExecIxResolveNativeProgram.SumNanoseconds, []string{ixLatency}, 1)
	Timing(ExecIxNativeProgramSystem, r.ExecIxNativeProgramSystem.SumNanoseconds, []string{ixLatency}, 1)
	Timing(ExecIxNativeProgramStake, r.ExecIxNativeProgramStake.SumNanoseconds, []string{ixLatency}, 1)
	Timing(ExecIxNativeProgramVote, r.ExecIxNativeProgramVote.SumNanoseconds, []string{ixLatency}, 1)
	Timing(ExecIxNativeProgramComputeBudget, r.ExecIxNativeProgramComputeBudget.SumNanoseconds, []string{ixLatency}, 1)
	Timing(ExecIxNativeProgramBpfLoader2, r.ExecIxNativeProgramBpfLoader2.SumNanoseconds, []string{ixLatency}, 1)
	Timing(ExecIxNativeProgramBpfLoaderDeprecated, r.ExecIxNativeProgramBpfLoaderDeprecated.SumNanoseconds, []string{ixLatency}, 1)
	Timing(ExecIxNativeProgramBpfLoaderUpgradeable, r.ExecIxNativeProgramBpfLoaderUpgradeable.SumNanoseconds, []string{ixLatency}, 1)
	Timing(ExecIxNativeProgramZkElgamalProof, r.ExecIxNativeProgramZkElgamalProof.SumNanoseconds, []string{ixLatency}, 1)
	Timing(ExecIxNativeProgramEd25519Precompile, r.ExecIxNativeProgramEd25519Precompile.SumNanoseconds, []string{ixLatency}, 1)
	Timing(ExecIxNativeProgramSecp256kPrecompile, r.ExecIxNativeProgramSecp256kPrecompile.SumNanoseconds, []string{ixLatency}, 1)
	Timing(FixupInstructionsSysvarAccount, r.FixupInstructionsSysvarAccount.SumNanoseconds, []string{ixLatency}, 1)
	Timing(InstructionAccountsFromAccountMetas, r.InstructionAccountsFromAccountMetas.SumNanoseconds, []string{ixLatency}, 1)
	Timing(SbpfInterpreterNew, r.SbpfInterpreterNew.SumNanoseconds, []string{sbpfLatency}, 1)
	Timing(SbpfInterpreterRun, r.SbpfInterpreterRun.SumNanoseconds, []string{sbpfLatency}, 1)
	Timing(AddProgramToCache, r.AddProgramToCache.SumNanoseconds, []string{sbpfLatency}, 1)
	Timing(GetProgramAccount, r.GetProgramAccount.SumNanoseconds, []string{sbpfLatency}, 1)
	Timing(GetProgramDataCached, r.GetProgramDataCached.SumNanoseconds, []string{sbpfLatency}, 1)
	Timing(GetProgramDataUncachedAccountsDb, r.GetProgramDataUncachedAccountsDb.SumNanoseconds, []string{sbpfLatency}, 1)
	Timing(GetProgramDataUncachedAccounts, r.GetProgramDataUncachedAccounts.SumNanoseconds, []string{sbpfLatency}, 1)
	Timing(GetProgramDataUncachedMarshal, r.GetProgramDataUncachedMarshal.SumNanoseconds, []string{sbpfLatency}, 1)
}
