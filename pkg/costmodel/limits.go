package costmodel

// Block cost limits mirror agave/cost-model/src/block_cost_limits.rs defaults.
const (
	ComputeUnitToUSRatio = 30

	SignatureCost            = ComputeUnitToUSRatio * 24 // 720
	Secp256k1VerifyCost      = ComputeUnitToUSRatio * 223
	Ed25519VerifyStrictCost  = ComputeUnitToUSRatio * 80
	Secp256r1VerifyCost      = ComputeUnitToUSRatio * 160
	WriteLockUnits           = ComputeUnitToUSRatio * 10  // 300
	InstructionDataBytesCost = 140 / ComputeUnitToUSRatio // ~4 CU per byte

	MaxBlockUnitsSIMD0256 = 60_000_000
	MaxBlockUnitsSIMD0286 = 100_000_000

	MaxWritableAccountUnits = 24_000_000

	MaxBlockAccountsDataSizeDelta = 100_000_000

	// DefaultMaxDataShredsPerSlot matches agave DEFAULT_MAX_DATA_SHREDS_PER_SLOT.
	DefaultMaxDataShredsPerSlot = 32 * 1024
	// TypicalDataShredPayloadBytes is the usable data bytes per entry-batch target.
	TypicalDataShredPayloadBytes = 963
	DefaultTargetBatchBytes      = 2 * TypicalDataShredPayloadBytes
)

// Limits configures per-slot cost and size budgets.
type Limits struct {
	BlockCost              uint64
	WritableAccountCost    uint64
	AllocatedDataSizeDelta uint64
	MaxBatchBytes          uint64
}

func DefaultLimits() Limits {
	return Limits{
		BlockCost:              MaxBlockUnitsSIMD0256,
		WritableAccountCost:    MaxWritableAccountUnits,
		AllocatedDataSizeDelta: MaxBlockAccountsDataSizeDelta,
		MaxBatchBytes:          DefaultTargetBatchBytes,
	}
}
