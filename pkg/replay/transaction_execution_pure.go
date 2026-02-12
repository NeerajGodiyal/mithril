package replay

import (
	"time"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/cu"
	"github.com/Overclock-Validator/mithril/pkg/metrics"
	"github.com/Overclock-Validator/mithril/pkg/migration"
	"github.com/Overclock-Validator/mithril/pkg/rent"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
)

// ExecuteLoadedTransaction is a pure state transition function. It takes pre-loaded,
// pre-validated, fee-deducted accounts and executes the transaction's instructions.
// All dependencies are explicit in the input struct — no SlotCtx, no I/O.
// Given the same input, it produces the same output.
func ExecuteLoadedTransaction(input ExecuteLoadedTransactionInput) ExecuteLoadedTransactionOutput {
	// Create execution context from input fields — no SlotCtx
	var log sealevel.LogRecorder
	txCtx := sealevel.NewTransactionCtx(*input.TransactionAccts, maxStackCapacity, maxInstrTraceCapacity)
	txCtx.AllInstructions = input.Instrs
	txCtx.Signature = input.Tx.Signatures[0]
	txCtx.BorrowedAccountArena = input.Arena
	txCtx.ComputeBudgetLimits = input.ComputeBudgetLimits

	execCtx := &sealevel.ExecutionCtx{
		Log:                      &log,
		Accounts:                 accounts.NewMemAccounts(),
		TransactionContext:       txCtx,
		Features:                 input.Features,
		ComputeMeter:             cu.NewComputeMeter(uint64(input.ComputeBudgetLimits.ComputeUnitLimit)),
		PrevLamportsPerSignature: input.PrevLamportsPerSignature,
		ModifiedVoteStates:       make(map[solana.PublicKey]*sealevel.VoteStateVersions, 8),

		// Execution fields — replace SlotCtx access during instruction execution
		Slot:                     input.Slot,
		LastBlockhash:            input.LastBlockhash,
		TotalEpochStake:          input.TotalEpochStake,
		VoteAccts:                input.VoteAccts,
		SerializedParameterArena: input.SerializedParameterArena,
		ProgramLoader:            input.ProgramLoader,
		AccountsForLookup:        input.AccountsForLookup,
	}

	// Set rent-exempt rent epoch max and compute pre-tx rent states
	start := time.Now()
	rent.MaybeSetRentExemptRentEpochMax(input.Epoch, &input.RentSysvar, &execCtx.Features, &execCtx.TransactionContext.Accounts)
	preTxRentStates := rent.NewRentStateInfo(&input.RentSysvar, execCtx.TransactionContext, &execCtx.Features, input.ProgramIDSet)
	metrics.GlobalBlockReplay.PreTxRentStates.AddTimingSince(start)

	// Execute all instructions
	var instrErr error
	writablePubkeys := make([]solana.PublicKey, 0, 64)

	start = time.Now()
	for instrIdx, instr := range input.Tx.Message.Instructions {
		ixStart := time.Now()
		err := fixupInstructionsSysvarAcct(execCtx, uint16(instrIdx))
		if err != nil {
			instrErr = err
			break
		}
		metrics.GlobalBlockReplay.FixupInstructionsSysvarAccount.AddTimingSince(ixStart)

		ixStart = time.Now()
		acctMetas := input.AcctMetasPerInstr[instrIdx]
		instructionAccts := sealevel.InstructionAcctsFromAccountMetas(acctMetas, *input.TransactionAccts)
		metrics.GlobalBlockReplay.InstructionAccountsFromAccountMetas.AddTimingSince(ixStart)

		programId := input.Tx.Message.AccountKeys[instr.ProgramIDIndex]
		migratingCus, isMigrating := migration.IsMigratingProgramAndGetCUs(programId)
		if isMigrating {
			err = execCtx.ComputeMeter.Consume(migratingCus)
			if err != nil {
				instrErr = err
				break
			}
			execCtx.ComputeMeter.Disable()
		}

		err = execCtx.ProcessInstruction(instr.Data, instructionAccts, programIndices(input.Tx, instrIdx))
		if err == nil {
			for _, am := range acctMetas {
				if am.IsWritable {
					writablePubkeys = append(writablePubkeys, am.Pubkey)
				}
			}
			if isMigrating {
				execCtx.ComputeMeter.Enable()
			}
		} else {
			instrErr = err
			break
		}
	}
	metrics.GlobalBlockReplay.IxLoop.AddTimingSince(start)

	// Check rent state transitions
	start = time.Now()
	postTxRentStates := rent.NewRentStateInfo(&input.RentSysvar, execCtx.TransactionContext, &execCtx.Features, input.ProgramIDSet)
	rentStateErr := rent.VerifyRentStateChanges(preTxRentStates, postTxRentStates, execCtx.TransactionContext)
	metrics.GlobalBlockReplay.PostTxRentStates.AddTimingSince(start)

	// On success: collect transaction-level writable accounts + payer
	var writablePubkeySet map[solana.PublicKey]struct{}
	if instrErr == nil && rentStateErr == nil {
		writablePubkeySet = make(map[solana.PublicKey]struct{}, len(input.TxAcctMetas))
		for _, txAcctMeta := range input.TxAcctMetas {
			if isWritable(txAcctMeta, &execCtx.Features, input.ProgramIDSet) {
				writablePubkeys = append(writablePubkeys, txAcctMeta.PublicKey)
				writablePubkeySet[txAcctMeta.PublicKey] = struct{}{}
			}
		}
		// Add payer
		payerAcct := execCtx.TransactionContext.Accounts.Accounts[0]
		writablePubkeys = append(writablePubkeys, payerAcct.Key)
		writablePubkeySet[payerAcct.Key] = struct{}{}
	}

	return ExecuteLoadedTransactionOutput{
		ExecCtx:           execCtx,
		InstrErr:          instrErr,
		RentStateErr:      rentStateErr,
		WritablePubkeys:   writablePubkeys,
		WritablePubkeySet: writablePubkeySet,
	}
}
