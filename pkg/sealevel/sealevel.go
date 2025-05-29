package sealevel

import (
	"bytes"
	"slices"

	a "github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/sbpf"
	"github.com/gagliardetto/solana-go"
)

func executionCtx(vm sbpf.VM) *ExecutionCtx {
	return vm.VMContext().(*ExecutionCtx)
}

func transactionCtx(vm sbpf.VM) *TransactionCtx {
	return vm.VMContext().(*ExecutionCtx).TransactionContext
}

func (t *TransactionCtx) newVMOpts(params *Params) *sbpf.VMOpts {
	execution := &ExecutionCtx{
		Log: new(LogRecorder),
	}
	var buf bytes.Buffer
	params.Serialize(&buf)
	return &sbpf.VMOpts{
		HeapMax:  32 * 1024,
		Syscalls: Syscalls(&params.Features, false),
		Context:  execution,
		MaxCU:    1_400_000,
		Input:    buf.Bytes(),
	}
}

func IsWritable(tx *solana.Transaction, am *AccountMeta, f *features.Features) bool {
	if !am.IsWritable {
		return false
	}

	if IsNativeProgram(am.Pubkey) || IsSysvar(am.Pubkey) {
		return false
	}

	if f.IsActive(features.AddNewReservedAccountKeys) {
		if slices.Contains(newReservedAccts, am.Pubkey) {
			return false
		}
	}

	if f.IsActive(features.EnableSecp256r1Precompile) {
		if am.Pubkey == a.Secp256r1PrecompileAddr {
			return false
		}
	}

	programIds, err := tx.GetProgramIDs()
	if err != nil {
		panic(err)
	}

	for _, programId := range programIds {
		if am.Pubkey == programId {
			return false
		}
	}

	return true
}

var newReservedAccts = []solana.PublicKey{a.AddressLookupTableAddr, a.ComputeBudgetProgramAddr,
	a.Ed25519PrecompileAddr, a.LoaderV4Addr, a.Secp256kPrecompileAddr, a.ZkElgamalProofProgramAddr,
	a.ZkTokenProofProgramAddr, SysvarEpochRewardsAddr, SysvarLastRestartSlotAddr, a.SysvarOwnerAddr}

func IsSysvar(pubkey solana.PublicKey) bool {
	if pubkey == SysvarClockAddr || pubkey == SysvarEpochScheduleAddr ||
		pubkey == SysvarFeesAddr || pubkey == SysvarInstructionsAddr ||
		pubkey == SysvarRecentBlockHashesAddr || pubkey == SysvarRentAddr ||
		pubkey == a.SysvarRewardsAddr || pubkey == SysvarSlotHashesAddr ||
		pubkey == SysvarSlotHistoryAddr || pubkey == SysvarStakeHistoryAddr {
		return true
	} else {
		return false
	}
}

func IsNativeProgram(pubkey solana.PublicKey) bool {
	if pubkey == a.SystemProgramAddr || pubkey == a.BpfLoaderUpgradeableAddr ||
		pubkey == a.BpfLoader2Addr || pubkey == a.BpfLoaderDeprecatedAddr ||
		pubkey == a.VoteProgramAddr || pubkey == a.StakeProgramAddr ||
		pubkey == a.ConfigProgramAddr || pubkey == a.StakeProgramConfigAddr ||
		pubkey == a.NativeLoaderAddr {
		return true
	} else {
		return false
	}
}
