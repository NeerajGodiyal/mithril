package statsd

import (
	"runtime/metrics"
	"strings"
	"time"

	"github.com/DataDog/datadog-go/statsd"
	mithrilmetrics "github.com/Overclock-Validator/mithril/pkg/metrics"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
)

var statsdClient *statsd.Client

func init() {
	var err error
	statsdClient, err = statsd.New("127.0.0.1:8125")
	if err != nil {
		mlog.Log.Errorf("couldn't start statsdClient: %v", err)
	}
	statsdClient.Namespace = "mithril."
	periodicallySendRuntimeMetrics()
}

func Count(name string, value int64, tags []string, rate float64) error {
	if statsdClient == nil {
		return nil
	}
	return statsdClient.Count(name, value, tags, rate)
}

func Distribution(name string, value float64, tags []string, rate float64) error {
	if statsdClient == nil {
		return nil
	}
	return statsdClient.Distribution(name, value, tags, rate)
}

func Gauge(name string, value float64, tags []string, rate float64) error {
	if statsdClient == nil {
		return nil
	}
	return statsdClient.Gauge(name, value, tags, rate)
}

func Timing(name string, value time.Duration, tags []string, rate float64) error {
	if statsdClient == nil {
		return nil
	}
	return statsdClient.Timing(name, value, tags, rate)
}

func periodicallySendRuntimeMetrics() {
	descs := metrics.All()
	var samples []metrics.Sample
	for _, desc := range descs {
		if strings.Contains(desc.Name, "/memory/classes") {
			samples = append(samples, metrics.Sample{Name: desc.Name})
		}
	}
	ticker := time.NewTicker(5 * time.Second)

	go func() {
		for {
			select {
			case <-ticker.C:
				metrics.Read(samples)
				for _, sample := range samples {
					metricName := strings.TrimPrefix(strings.Map(func(r rune) rune {
						if r == '/' || r == ':' {
							return '.'
						}
						return r
					}, sample.Name), ".")
					switch sample.Value.Kind() {
					case metrics.KindUint64:
						Gauge(metricName, float64(sample.Value.Uint64()), nil, 1)
					case metrics.KindFloat64:
						Gauge(metricName, float64(sample.Value.Float64()), nil, 1)
					default:
						mlog.Log.Errorf("unknown metric kind: metric=%s kind=%d", sample.Name, sample.Value.Kind())
					}
				}
			}
		}
	}()
}

const (
	blockLatency = "replay.block.sum_latency"
	blockCount   = "replay.block.count"
	txLatency    = "replay.tx.sum_latency"
	txCount      = "replay.tx.count"
	ixLatency    = "replay.ix.sum_latency"
	ixCount      = "replay.ix.count"
	sbpfLatency  = "replay.sbpf.sum_latency"
	sbpfCount    = "replay.sbpf.count"
)

func sendPhaseMetrics(phaseLatencyKey, phaseCountKey, phase string, timing mithrilmetrics.Timing) {
	Timing(phaseLatencyKey, time.Duration(timing.SumNanoseconds), []string{"phase:" + phase}, 1)
	Count(phaseCountKey, int64(timing.Count), []string{"phase:" + phase}, 1)
}

func SendBlockReplayMetrics(r mithrilmetrics.BlockReplay) {
	sendPhaseMetrics(blockLatency, blockCount, "preprocess_block", r.PreprocessBlock)
	sendPhaseMetrics(blockLatency, blockCount, "load_block_accounts", r.LoadBlockAccounts)
	sendPhaseMetrics(blockLatency, blockCount, "tx_loop", r.TxLoop)
	sendPhaseMetrics(blockLatency, blockCount, "reward", r.Reward)
	sendPhaseMetrics(blockLatency, blockCount, "rent", r.Rent)
	sendPhaseMetrics(blockLatency, blockCount, "run_incinerator", r.RunIncinerator)
	sendPhaseMetrics(blockLatency, blockCount, "block_update_accounts", r.BlockUpdateAccounts)
	sendPhaseMetrics(blockLatency, blockCount, "accounts_delta_hash", r.AccountsDeltaHash)
	sendPhaseMetrics(blockLatency, blockCount, "bank_hash", r.BankHash)

	sendPhaseMetrics(txLatency, txCount, "instructions_and_account_metas_from_tx", r.InstructionsAndAccountMetasFromTx)
	sendPhaseMetrics(txLatency, txCount, "compute_budget_execution_instructions", r.ComputeBudgetExecutionInstructions)
	sendPhaseMetrics(txLatency, txCount, "accounts_from_tx", r.AccountsFromTx)
	sendPhaseMetrics(txLatency, txCount, "pre_balance_divergence_check", r.PreBalanceDivergenceCheck)
	sendPhaseMetrics(txLatency, txCount, "calc_and_deduct_fees", r.CalcAndDeductFees)
	sendPhaseMetrics(txLatency, txCount, "read_rent_sysvar", r.ReadRentSysvar)
	sendPhaseMetrics(txLatency, txCount, "pre_tx_rent_states", r.PreTxRentStates)
	sendPhaseMetrics(txLatency, txCount, "ix_loop", r.IxLoop)
	sendPhaseMetrics(txLatency, txCount, "post_tx_rent_states", r.PostTxRentStates)
	sendPhaseMetrics(txLatency, txCount, "post_balance_divergence_check", r.PostBalanceDivergenceCheck)
	sendPhaseMetrics(txLatency, txCount, "tx_update_accounts", r.TxUpdateAccounts)

	sendPhaseMetrics(ixLatency, ixCount, "get_next_ix_ctx", r.GetNextIxCtx)
	sendPhaseMetrics(ixLatency, ixCount, "next_ix_ctx_configure", r.NextIxCtxConfigure)
	sendPhaseMetrics(ixLatency, ixCount, "ix_push", r.IxPush)
	sendPhaseMetrics(ixLatency, ixCount, "ix_pop", r.IxPop)
	sendPhaseMetrics(ixLatency, ixCount, "exec_ix_resolve_native_program", r.ExecIxResolveNativeProgram)
	sendPhaseMetrics(ixLatency, ixCount, "exec_ix_native_program_system", r.ExecIxNativeProgramSystem)
	sendPhaseMetrics(ixLatency, ixCount, "exec_ix_native_program_stake", r.ExecIxNativeProgramStake)
	sendPhaseMetrics(ixLatency, ixCount, "exec_ix_native_program_vote", r.ExecIxNativeProgramVote)
	sendPhaseMetrics(ixLatency, ixCount, "exec_ix_native_program_compute_budget", r.ExecIxNativeProgramComputeBudget)
	sendPhaseMetrics(ixLatency, ixCount, "exec_ix_native_program_bpf_loader2", r.ExecIxNativeProgramBpfLoader2)
	sendPhaseMetrics(ixLatency, ixCount, "exec_ix_native_program_bpf_loader_deprecated", r.ExecIxNativeProgramBpfLoaderDeprecated)
	sendPhaseMetrics(ixLatency, ixCount, "exec_ix_native_program_bpf_loader_upgradeable", r.ExecIxNativeProgramBpfLoaderUpgradeable)
	sendPhaseMetrics(ixLatency, ixCount, "exec_ix_native_program_zk_elgamal_proof", r.ExecIxNativeProgramZkElgamalProof)
	sendPhaseMetrics(ixLatency, ixCount, "exec_ix_native_program_ed25519_precompile", r.ExecIxNativeProgramEd25519Precompile)
	sendPhaseMetrics(ixLatency, ixCount, "exec_ix_native_program_secp256k_precompile", r.ExecIxNativeProgramSecp256kPrecompile)
	sendPhaseMetrics(ixLatency, ixCount, "fixup_instructions_sysvar_account", r.FixupInstructionsSysvarAccount)
	sendPhaseMetrics(ixLatency, ixCount, "instruction_accounts_from_account_metas", r.InstructionAccountsFromAccountMetas)

	sendPhaseMetrics(sbpfLatency, sbpfCount, "sbpf_interpreter_new", r.SbpfInterpreterNew)
	sendPhaseMetrics(sbpfLatency, sbpfCount, "sbpf_interpreter_run", r.SbpfInterpreterRun)
	sendPhaseMetrics(sbpfLatency, sbpfCount, "add_program_to_cache", r.AddProgramToCache)
	sendPhaseMetrics(sbpfLatency, sbpfCount, "get_program_account", r.GetProgramAccount)
	sendPhaseMetrics(sbpfLatency, sbpfCount, "get_program_data_cached", r.GetProgramDataCached)
	sendPhaseMetrics(sbpfLatency, sbpfCount, "get_program_data_uncached_accounts_db", r.GetProgramDataUncachedAccountsDb)
	sendPhaseMetrics(sbpfLatency, sbpfCount, "get_program_data_uncached_accounts", r.GetProgramDataUncachedAccounts)
	sendPhaseMetrics(sbpfLatency, sbpfCount, "get_program_data_uncached_marshal", r.GetProgramDataUncachedMarshal)
}
