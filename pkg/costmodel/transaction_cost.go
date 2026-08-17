package costmodel

import (
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
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

	limits, err := sealevel.ComputeBudgetExecuteInstructions(instrs, feats)
	if err != nil {
		// A compute-budget parse failure yields zero execution cost; the transaction will not execute.
		return TransactionCost{
			SignatureCost: signatureCost(tx),
			WriteLockCost: writeLockCost(countWriteLocks(tx)),
			DataBytesCost: instructionDataCost(tx),
			WritableAccounts: writableAccounts(tx),
		}, nil
	}

	loadedDataCost := loadedAccountsDataSizeCost(limits.LoadedAccountBytes)
	return TransactionCost{
		SignatureCost:              signatureCost(tx),
		WriteLockCost:              writeLockCost(countWriteLocks(tx)),
		DataBytesCost:              instructionDataCost(tx),
		ProgramsExecutionCost:      uint64(limits.ComputeUnitLimit),
		LoadedAccountsDataSizeCost: loadedDataCost,
		AllocatedAccountsDataSize:  estimateAllocDelta(instrs, feats),
		WritableAccounts:           writableAccounts(tx),
	}, nil
}

func signatureCost(tx *solana.Transaction) uint64 {
	return uint64(len(tx.Signatures)) * SignatureCost
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

func countWriteLocks(tx *solana.Transaction) uint64 {
	return uint64(len(writableAccounts(tx)))
}

func writableAccounts(tx *solana.Transaction) []solana.PublicKey {
	if tx == nil {
		return nil
	}
	hdr := tx.Message.Header
	numSigners := int(hdr.NumRequiredSignatures)
	numKeys := len(tx.Message.AccountKeys)
	if numKeys == 0 {
		return nil
	}
	numWritableSigners := numSigners - int(hdr.NumReadonlySignedAccounts)
	if numWritableSigners < 0 {
		numWritableSigners = 0
	}
	numWritableNonSigners := numKeys - numSigners - int(hdr.NumReadonlyUnsignedAccounts)
	if numWritableNonSigners < 0 {
		numWritableNonSigners = 0
	}
	out := make([]solana.PublicKey, 0, numWritableSigners+numWritableNonSigners)
	for i := 0; i < numWritableSigners && i < numKeys; i++ {
		out = append(out, tx.Message.AccountKeys[i])
	}
	start := numSigners + int(hdr.NumReadonlyUnsignedAccounts)
	for i := start; i < numKeys; i++ {
		out = append(out, tx.Message.AccountKeys[i])
	}
	return out
}

func estimateAllocDelta(instrs []sealevel.Instruction, feats *features.Features) uint64 {
	_ = instrs
	_ = feats
	return 0
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
	if !am.IsWritable {
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
