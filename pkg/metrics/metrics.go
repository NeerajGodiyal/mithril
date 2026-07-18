package metrics

import (
	"sync/atomic"
	"time"
)

type Timing struct {
	Count          uint64
	SumNanoseconds uint64
}

func (t *Timing) AddTiming(d time.Duration) {
	atomic.AddUint64(&t.Count, 1)
	atomic.AddUint64(&t.SumNanoseconds, uint64(d.Nanoseconds()))
}

func (t *Timing) AddTimingSince(start time.Time) {
	t.AddTiming(time.Since(start))
}

// AccountLoader is the per-slot decomposition of LoadBlockAccounts. Counters
// describe logical loader work; allocation counters cover objects/data created
// directly by the batch loader rather than runtime or Pebble internals.
type AccountLoader struct {
	AddressTableLookups Timing
	DedupeBlockAccounts Timing
	SourceBatch         Timing
	ParentMapBuild      Timing
	SysvarUpdates       Timing

	WorkingSetLookup Timing
	InProgressLookup Timing
	CacheLookup      Timing
	IndexLookup      Timing
	ReadPlanning     Timing
	AppendVecRead    Timing

	RequestedKeys  uint64
	DurableKeys    uint64
	ParentAccounts uint64

	WorkingSetHits    uint64
	InProgressHits    uint64
	CacheHits         uint64
	IndexHits         uint64
	IndexMisses       uint64
	UniqueAppendVecs  uint64
	AppendVecChunks   uint64
	AppendVecAccounts uint64
	OpenFailures      uint64
	ReadFailures      uint64
	RetryAccounts     uint64

	CommonCacheAdmissions        uint64
	CommonCacheAdmissionsSkipped uint64
	VoteCacheAdmissions          uint64

	DecodedAccountObjects uint64
	DecodedAccountBytes   uint64
	PlaceholderObjects    uint64
}

// Metrics for replaying a single block
type BlockReplay struct {
	Slot          uint64
	AccountLoader AccountLoader

	// Block-level latencies.
	PreprocessBlock     Timing
	LoadBlockAccounts   Timing
	TxLoop              Timing
	Reward              Timing
	Rent                Timing
	RunIncinerator      Timing
	BlockUpdateAccounts Timing
	AccountsDeltaHash   Timing
	BankHash            Timing

	// Tx-level latencies summed for all the txs in a block.
	InstructionsAndAccountMetasFromTx  Timing
	ComputeBudgetExecutionInstructions Timing
	AccountsFromTx                     Timing
	PreBalanceDivergenceCheck          Timing
	CalcAndDeductFees                  Timing
	ReadRentSysvar                     Timing
	PreTxRentStates                    Timing
	IxLoop                             Timing
	PostTxRentStates                   Timing
	PostBalanceDivergenceCheck         Timing
	TxUpdateAccounts                   Timing

	// Async part of tx latency
	Sigverify Timing

	// Ix-level latencies summed across all the instructions in a block.
	GetNextIxCtx                            Timing
	NextIxCtxConfigure                      Timing
	IxPush                                  Timing
	IxPop                                   Timing
	ExecIxResolveNativeProgram              Timing
	ExecIxNativeProgramSystem               Timing
	ExecIxNativeProgramStake                Timing
	ExecIxNativeProgramVote                 Timing
	ExecIxNativeProgramComputeBudget        Timing
	ExecIxNativeProgramBpfLoader2           Timing
	ExecIxNativeProgramBpfLoaderDeprecated  Timing
	ExecIxNativeProgramBpfLoaderUpgradeable Timing
	ExecIxNativeProgramZkElgamalProof       Timing
	ExecIxNativeProgramEd25519Precompile    Timing
	ExecIxNativeProgramSecp256kPrecompile   Timing
	FixupInstructionsSysvarAccount          Timing
	InstructionAccountsFromAccountMetas     Timing

	// BPF Loader
	SbpfInterpreterNew               Timing
	SbpfInterpreterRun               Timing
	AddProgramToCache                Timing
	GetProgramAccount                Timing
	GetProgramDataCached             Timing
	GetProgramDataUncachedAccountsDb Timing
	GetProgramDataUncachedAccounts   Timing
	GetProgramDataUncachedMarshal    Timing
}

var GlobalBlockReplay = BlockReplay{}

// Counter is a monotonic atomic counter for events. Operations are
// lock-free; use it for high-frequency increments where Timing's
// nanosecond accumulator is unnecessary.
type Counter struct {
	value uint64
}

func (c *Counter) Inc() {
	atomic.AddUint64(&c.value, 1)
}

func (c *Counter) Get() uint64 {
	return atomic.LoadUint64(&c.value)
}

// Simulate tracks RPC simulateTransaction handler events. Counters are
// surfaced through the standard metrics endpoint; latency uses the same
// Timing type as block replay so dashboards can reuse existing rendering.
type Simulate struct {
	TotalCalls         Counter
	SanitizeFailures   Counter
	AddressLookupFails Counter
	NonceFallbackHits  Counter
	Errors             Counter
	Successes          Counter
	HandlerLatency     Timing
}

var GlobalSimulate = Simulate{}
