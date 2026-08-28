package costmodel

import (
	"github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/safemath"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
)

// TransactionCost is the estimated block-cost budget a transaction consumes.
type TransactionCost struct {
	SignatureCost              uint64
	WriteLockCost              uint64
	DataBytesCost              uint64
	ProgramsExecutionCost      uint64
	LoadedAccountsDataSizeCost uint64
	AllocatedAccountsDataSize  uint64
	WritableAccounts           []solana.PublicKey
	WireSize                   int
}

func (c TransactionCost) Sum() uint64 {
	return c.SignatureCost +
		c.WriteLockCost +
		c.DataBytesCost +
		c.ProgramsExecutionCost +
		c.LoadedAccountsDataSizeCost
}

// EstimateTransactionCost applies the protocol cost model to a parsed transaction.
func EstimateTransactionCost(tx *solana.Transaction, feats *features.Features) (TransactionCost, error) {
	if tx == nil {
		return TransactionCost{}, nil
	}

	instrs, _, err := replayInstrsAndAcctMetas(tx, feats)
	if err != nil {
		return TransactionCost{}, err
	}
	metas, err := tx.AccountMetaList()
	if err != nil {
		return TransactionCost{}, err
	}
	writable, err := writableAccounts(tx, metas, feats)
	if err != nil {
		return TransactionCost{}, err
	}
	writeLocks := countWriteLocks(metas)

	limits, err := sealevel.ComputeBudgetForTransaction(tx, instrs, feats)
	if err != nil {
		// A compute-budget parse failure yields zero execution cost; the transaction will not execute.
		return TransactionCost{
			SignatureCost:    signatureCost(tx, feats),
			WriteLockCost:    writeLockCost(writeLocks),
			DataBytesCost:    instructionDataCost(tx),
			WritableAccounts: writable,
		}, nil
	}

	loadedDataCost := loadedAccountsDataSizeCost(limits.LoadedAccountBytes)
	if tx.Message.GetVersion() == solana.MessageVersionV1 && limits.LoadedAccountBytes == 0 {
		loadedDataCost = HeapCost
	}
	return TransactionCost{
		SignatureCost:              signatureCost(tx, feats),
		WriteLockCost:              writeLockCost(writeLocks),
		DataBytesCost:              instructionDataCost(tx),
		ProgramsExecutionCost:      uint64(limits.ComputeUnitLimit),
		LoadedAccountsDataSizeCost: loadedDataCost,
		AllocatedAccountsDataSize:  estimateAllocDelta(instrs, feats),
		WritableAccounts:           writable,
	}, nil
}

func signatureCost(tx *solana.Transaction, feats *features.Features) uint64 {
	cost := safemath.SaturatingMulU64(uint64(len(tx.Signatures)), SignatureCost)
	for _, instruction := range tx.Message.Instructions {
		if len(instruction.Data) == 0 {
			continue
		}
		programID, err := tx.ResolveProgramIDIndex(instruction.ProgramIDIndex)
		if err != nil {
			continue
		}
		var perSignature uint64
		secp256k := solana.PublicKey(addresses.Secp256kPrecompileAddr)
		ed25519 := solana.PublicKey(addresses.Ed25519PrecompileAddr)
		secp256r1 := solana.PublicKey(addresses.Secp256r1PrecompileAddr)
		switch {
		case programID == secp256k:
			perSignature = Secp256k1VerifyCost
		case programID == ed25519:
			perSignature = Ed25519VerifyStrictCost
		case programID == secp256r1:
			if feats != nil && feats.IsActive(features.EnableSecp256r1Precompile) {
				perSignature = Secp256r1VerifyCost
			}
		}
		cost = safemath.SaturatingAddU64(cost, safemath.SaturatingMulU64(uint64(instruction.Data[0]), perSignature))
	}
	return cost
}

func writeLockCost(num uint64) uint64 {
	return num * WriteLockUnits
}

func instructionDataCost(tx *solana.Transaction) uint64 {
	var bytes uint64
	for _, ix := range tx.Message.Instructions {
		bytes += uint64(len(ix.Data))
	}
	if InstructionDataBytesCost == 0 {
		return 0
	}
	return bytes / InstructionDataBytesCost
}

func loadedAccountsDataSizeCost(bytes uint32) uint64 {
	pages := (uint64(bytes) + AccountDataCostPageSize - 1) / AccountDataCostPageSize
	return pages * HeapCost
}

// LoadedAccountsDataSizeCost is the protocol page charge for loaded account bytes.
func LoadedAccountsDataSizeCost(bytes uint32) uint64 {
	return loadedAccountsDataSizeCost(bytes)
}

func countWriteLocks(metas []*solana.AccountMeta) uint64 {
	var count uint64
	for _, meta := range metas {
		if meta.IsWritable {
			count++
		}
	}
	return count
}

func writableAccounts(tx *solana.Transaction, metas []*solana.AccountMeta, feats *features.Features) ([]solana.PublicKey, error) {
	if tx == nil {
		return nil, nil
	}
	programIDs, err := tx.GetProgramIDs()
	if err != nil {
		return nil, err
	}
	programSet := make(map[solana.PublicKey]struct{}, len(programIDs))
	for _, programID := range programIDs {
		programSet[programID] = struct{}{}
	}
	demoteProgramIDs := true
	for _, key := range tx.Message.AccountKeys {
		if key == addresses.BpfLoaderUpgradeableAddr {
			demoteProgramIDs = false
			break
		}
	}
	out := make([]solana.PublicKey, 0, len(metas))
	for _, meta := range metas {
		if isWritableForCost(meta, programSet, demoteProgramIDs, feats) {
			out = append(out, meta.PublicKey)
		}
	}
	return out, nil
}

func estimateAllocDelta(instrs []sealevel.Instruction, feats *features.Features) uint64 {
	var total uint64
	for _, instruction := range instrs {
		if instruction.ProgramId != addresses.SystemProgramAddr {
			continue
		}
		decoder := bin.NewBinDecoder(instruction.Data)
		kind, err := decoder.ReadUint32(bin.LE)
		if err != nil {
			return 0
		}
		var space uint64
		switch kind {
		case sealevel.SystemProgramInstrTypeCreateAccount:
			var value sealevel.SystemInstrCreateAccount
			if value.UnmarshalWithDecoder(decoder) != nil {
				return 0
			}
			space = value.Space
		case sealevel.SystemProgramInstrTypeCreateAccountWithSeed:
			var value sealevel.SystemInstrCreateAccountWithSeed
			if value.UnmarshalWithDecoder(decoder) != nil {
				return 0
			}
			space = value.Space
		case sealevel.SystemProgramInstrTypeAllocate:
			var value sealevel.SystemInstrAllocate
			if value.UnmarshalWithDecoder(decoder) != nil {
				return 0
			}
			space = value.Space
		case sealevel.SystemProgramInstrTypeAllocateWithSeed:
			var value sealevel.SystemInstrAllocateWithSeed
			if value.UnmarshalWithDecoder(decoder) != nil {
				return 0
			}
			space = value.Space
		case sealevel.SystemProgramInstrTypeCreateAccountAllowPrefund:
			if feats == nil || !feats.IsActive(features.CreateAccountAllowPrefund) {
				continue
			}
			var value sealevel.SystemInstrCreateAccountAllowPrefund
			if value.UnmarshalWithDecoder(decoder) != nil {
				return 0
			}
			space = value.Space
		default:
			continue
		}
		if space > sealevel.SystemProgMaxPermittedDataLen {
			return 0
		}
		total = safemath.SaturatingAddU64(total, space)
	}
	if total > sealevel.SystemProgMaxPermittedDataLen {
		return sealevel.SystemProgMaxPermittedDataLen
	}
	return total
}

func replayInstrsAndAcctMetas(tx *solana.Transaction, feats *features.Features) ([]sealevel.Instruction, [][]sealevel.AccountMeta, error) {
	programIDs, err := tx.GetProgramIDs()
	if err != nil {
		return nil, nil, err
	}
	programIDSet := make(map[solana.PublicKey]struct{}, len(programIDs))
	for _, pid := range programIDs {
		programIDSet[pid] = struct{}{}
	}
	upgradeableLoaderPresent := false
	for _, key := range tx.Message.AccountKeys {
		if key.String() == "BPFLoaderUpgradeab1e11111111111111111111111" {
			upgradeableLoaderPresent = true
			break
		}
	}
	demoteProgramIDs := !upgradeableLoaderPresent

	instrs := make([]sealevel.Instruction, 0, len(tx.Message.Instructions))
	acctMetasPerInstr := make([][]sealevel.AccountMeta, 0, len(tx.Message.Instructions))
	for _, compiled := range tx.Message.Instructions {
		programID, err := tx.ResolveProgramIDIndex(compiled.ProgramIDIndex)
		if err != nil {
			return nil, nil, err
		}
		ams, err := compiled.ResolveInstructionAccounts(&tx.Message)
		if err != nil {
			return nil, nil, err
		}
		metas := make([]sealevel.AccountMeta, 0, len(ams))
		for _, am := range ams {
			metas = append(metas, sealevel.AccountMeta{
				Pubkey:     am.PublicKey,
				IsSigner:   am.IsSigner,
				IsWritable: isWritableForCost(am, programIDSet, demoteProgramIDs, feats),
			})
		}
		instrs = append(instrs, sealevel.Instruction{
			Accounts:  metas,
			ProgramId: programID,
			Data:      compiled.Data,
		})
		acctMetasPerInstr = append(acctMetasPerInstr, metas)
	}
	return instrs, acctMetasPerInstr, nil
}

func isWritableForCost(am *solana.AccountMeta, programIDSet map[solana.PublicKey]struct{}, demote bool, feats *features.Features) bool {
	if feats == nil {
		feats = features.NewFeaturesDefault()
	}
	meta := &sealevel.AccountMeta{Pubkey: am.PublicKey, IsSigner: am.IsSigner, IsWritable: am.IsWritable}
	if !sealevel.IsWritable(meta, feats) {
		return false
	}
	if demote {
		if _, ok := programIDSet[am.PublicKey]; ok {
			return false
		}
	}
	_ = feats
	return true
}
