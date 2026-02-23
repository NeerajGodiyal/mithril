package replay

import (
	"math"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/arena"
	"github.com/Overclock-Validator/mithril/pkg/cu"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/fees"
	"github.com/Overclock-Validator/mithril/pkg/metrics"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

// LoadAndExecuteTransactionInput contains all the input state needed for pure transaction processing
type LoadAndExecuteTransactionInput struct {
	// SlotCtx provides access to accounts and slot state (used read-only)
	SlotCtx *sealevel.SlotCtx
	// Transaction is the transaction to process
	Transaction *solana.Transaction
	// Arena for borrowed accounts (optional)
	Arena *arena.Arena[sealevel.BorrowedAccount]
	// TxMeta is the on-chain transaction metadata (optional, used for fee calculation)
	TxMeta *rpc.TransactionMeta
}

// LoadAndExecuteTransaction is a function that loads and executes a transaction.
// It handles the loading phase (parsing, validation, account loading, fee deduction)
// and delegates execution to ExecuteLoadedTransaction.
func LoadAndExecuteTransaction(input LoadAndExecuteTransactionInput) LoadAndExecuteTransactionOutput {
	tx := input.Transaction
	slotCtx := input.SlotCtx

	if input.Arena != nil {
		input.Arena.Reset()
	}

	if slotCtx.Features.IsActive(features.StaticInstructionLimit) {
		if len(tx.Message.Instructions) > maxInstrTraceCapacity {
			return LoadAndExecuteTransactionOutput{
				ProcessingResult: TransactionProcessingResult{
					TransactionError: &TransactionError{
						ErrorType: TransactionErrorSanitizeFailure,
					},
				},
			}
		}
	}

	// Parse instructions and account metas
	start := time.Now()
	instrs, acctMetasPerInstr, programIDSet, err := instrsAndAcctMetasFromTx(tx, slotCtx.Features)
	if err != nil {
		return LoadAndExecuteTransactionOutput{
			ProcessingResult: TransactionProcessingResult{
				TransactionError: &TransactionError{
					ErrorType:        TransactionErrorSanitizeFailure,
					InstructionError: err,
				},
			},
		}
	}
	metrics.GlobalBlockReplay.InstructionsAndAccountMetasFromTx.AddTimingSince(start)

	// Compute budget limits
	start = time.Now()
	computeBudgetLimits, err := sealevel.ComputeBudgetExecuteInstructions(instrs, slotCtx.Features)
	if err != nil {
		return LoadAndExecuteTransactionOutput{
			ProcessingResult: TransactionProcessingResult{
				TransactionError: &TransactionError{
					ErrorType:        TransactionErrorInstructionError,
					InstructionError: err,
				},
			},
			Instrs:       instrs,
			ProgramIDSet: programIDSet,
		}
	}
	metrics.GlobalBlockReplay.ComputeBudgetExecutionInstructions.AddTimingSince(start)

	// Validate transaction age
	if !sealevel.IsTransactionAgeValid(tx, instrs, slotCtx) {
		return LoadAndExecuteTransactionOutput{
			ProcessingResult: TransactionProcessingResult{
				TransactionError: &TransactionError{
					ErrorType: TransactionErrorBlockhashNotFound,
				},
			},
			Instrs:              instrs,
			ComputeBudgetLimits: computeBudgetLimits,
			ProgramIDSet:        programIDSet,
		}
	}

	// Load and validate accounts
	start = time.Now()
	instrsAcct := sealevel.MakeInstructionsSysvarAccount(instrs)
	var transactionAccts *sealevel.TransactionAccounts
	var txAcctMetas []*solana.AccountMeta

	if slotCtx.Features.IsActive(features.FormalizeLoadedTransactionDataSize) {
		transactionAccts, txAcctMetas, err = loadAndValidateTxAcctsSimd186(slotCtx, acctMetasPerInstr, tx, instrs, instrsAcct, computeBudgetLimits.LoadedAccountBytes)
	} else {
		transactionAccts, txAcctMetas, err = loadAndValidateTxAccts(slotCtx, acctMetasPerInstr, tx, instrs, instrsAcct, computeBudgetLimits.LoadedAccountBytes)
	}

	// Base output fields for all paths after parsing
	baseFields := func(out *LoadAndExecuteTransactionOutput) {
		out.Instrs = instrs
		out.ComputeBudgetLimits = computeBudgetLimits
		out.ProgramIDSet = programIDSet
	}

	if err == TxErrMaxLoadedAccountsDataSizeExceeded || err == TxErrInvalidProgramForExecution || err == TxErrProgramAccountNotFound {
		errType := mapLoadErrorType(err)
		out := LoadAndExecuteTransactionOutput{
			ProcessingResult: TransactionProcessingResult{
				TransactionError: &TransactionError{
					ErrorType:        errType,
					InstructionError: err,
				},
			},
		}
		baseFields(&out)
		return out
	} else if err != nil {
		out := LoadAndExecuteTransactionOutput{
			ProcessingResult: TransactionProcessingResult{
				TransactionError: &TransactionError{
					ErrorType:        TransactionErrorAccountNotFound,
					InstructionError: err,
				},
			},
		}
		baseFields(&out)
		return out
	}
	metrics.GlobalBlockReplay.AccountsFromTx.AddTimingSince(start)

	// Capture pre-balance lamports (before fee deduction)
	preBalances := make([]uint64, len(tx.Message.AccountKeys))
	for i := range tx.Message.AccountKeys {
		preBalances[i] = transactionAccts.Accounts[i].Lamports
	}

	// Calculate and deduct fees (directly on transactionAccts — pointers share Account objects)
	start = time.Now()
	txFeeInfo, _, err := fees.CalculateAndDeductTxFees(tx, input.TxMeta, instrs, transactionAccts, computeBudgetLimits, slotCtx.Features)
	if err != nil {
		// Create a temporary ExecCtx for the InsufficientFundsForFee error path
		// (ProcessTransaction needs ExecCtx for divergence checks even on fee failure)
		var log sealevel.LogRecorder
		execCtx := newExecCtx(slotCtx, transactionAccts, computeBudgetLimits, &log)

		out := LoadAndExecuteTransactionOutput{
			ProcessingResult: TransactionProcessingResult{
				TransactionError: &TransactionError{
					ErrorType:        TransactionErrorInsufficientFundsForFee,
					InstructionError: err,
				},
			},
			ExecCtx:     execCtx,
			PreBalances: preBalances,
			FeeInfo:     txFeeInfo,
		}
		baseFields(&out)
		return out
	}
	metrics.GlobalBlockReplay.CalcAndDeductFees.AddTimingSince(start)

	// Read rent sysvar from cache
	start = time.Now()
	if sealevel.SysvarCache.Rent.Sysvar == nil {
		panic("rent sysvar not in cache")
	}
	rentSysvar := *sealevel.SysvarCache.Rent.Sysvar
	metrics.GlobalBlockReplay.ReadRentSysvar.AddTimingSince(start)

	// Delegate to the pure execution function
	execOutput := ExecuteLoadedTransaction(ExecuteLoadedTransactionInput{
		Tx:                       tx,
		TransactionAccts:         transactionAccts,
		TxAcctMetas:              txAcctMetas,
		AcctMetasPerInstr:        acctMetasPerInstr,
		Instrs:                   instrs,
		ProgramIDSet:             programIDSet,
		ComputeBudgetLimits:      computeBudgetLimits,
		Features:                 *slotCtx.Features,
		RentSysvar:               rentSysvar,
		Epoch:                    slotCtx.Epoch,
		Slot:                     slotCtx.Slot,
		PrevLamportsPerSignature: slotCtx.FeeRateGovernor.PrevLamportsPerSignature,
		LastBlockhash:            slotCtx.LastBlockhash,
		TotalEpochStake:          slotCtx.TotalEpochStake,
		VoteAccts:                slotCtx.VoteAccts,
		AccountsForLookup:        slotCtx.Accounts,
		ProgramLoader:            slotCtx,
		SerializedParameterArena: slotCtx.SerializedParameterArena,
		Arena:                    input.Arena,
	})

	execCtx := execOutput.ExecCtx

	// If there was an execution error, return failed transaction result
	if execOutput.InstrErr != nil || execOutput.RentStateErr != nil {
		var relevantErr error
		var errType TransactionErrorType
		if execOutput.InstrErr != nil {
			relevantErr = execOutput.InstrErr
			errType = TransactionErrorInstructionError
		} else {
			relevantErr = execOutput.RentStateErr
			errType = TransactionErrorInsufficientFundsForRent
		}

		out := LoadAndExecuteTransactionOutput{
			ProcessingResult: TransactionProcessingResult{
				TransactionError: &TransactionError{
					ErrorType:        errType,
					InstructionError: relevantErr,
				},
			},
			ExecCtx:     execCtx,
			PreBalances: preBalances,
			FeeInfo:     txFeeInfo,
		}
		baseFields(&out)
		return out
	}

	// Success path: build full output from execution result
	writablePubkeys := execOutput.WritablePubkeys
	writablePubkeySet := execOutput.WritablePubkeySet

	// Collect modified accounts
	accountUpdates := collectAccountUpdates(execCtx)

	// Collect modified vote accounts
	modifiedVoteAccounts := make(map[solana.PublicKey]*sealevel.VoteStateVersions)
	for pk, voteState := range execCtx.ModifiedVoteStates {
		modifiedVoteAccounts[pk] = voteState
	}

	// Calculate loaded accounts data size
	var loadedAccountsDataSize uint32
	for _, acct := range transactionAccts.Accounts {
		if !acct.IsDummy {
			loadedAccountsDataSize += uint32(len(acct.Data))
		}
	}

	// Collect return data
	var returnData *TransactionReturnData
	programId, data := execCtx.TransactionContext.ReturnData()
	if len(data) > 0 {
		returnData = &TransactionReturnData{
			ProgramId: programId,
			Data:      data,
		}
	}

	// Calculate accounts data len delta
	var accountsDataLenDelta int64
	for idx, acct := range execCtx.TransactionContext.Accounts.Accounts {
		if execCtx.TransactionContext.Accounts.Touched[idx] {
			originalAcct := transactionAccts.Accounts[idx]
			accountsDataLenDelta += int64(len(acct.Data)) - int64(len(originalAcct.Data))
		}
	}

	// Build compute budget
	computeBudget := SVMTransactionExecutionBudget{
		ComputeUnitLimit:          uint64(computeBudgetLimits.ComputeUnitLimit),
		MaxInstructionStackDepth:  maxStackCapacity,
		MaxInstructionTraceLength: maxInstrTraceCapacity,
		Sha256MaxSlices:           cu.CUSha256MaxSlices,
		MaxCallDepth:              64,
		StackFrameSize:            4096,
		HeapSize:                  computeBudgetLimits.UpdatedHeapBytes,
	}

	// Build loaded transaction
	loadedTransaction := LoadedTransaction{
		Accounts:               convertToKeyedAccountSharedData(transactionAccts.Accounts),
		ProgramIndices:         []uint16{},
		FeeDetails:             *txFeeInfo,
		ComputeBudget:          computeBudget,
		LoadedAccountsDataSize: loadedAccountsDataSize,
	}

	// Get logs from the execution context
	var logMessages []string
	if logRecorder, ok := execCtx.Log.(*sealevel.LogRecorder); ok && logRecorder != nil {
		logMessages = logRecorder.Logs
	}

	// Build execution details
	executionDetails := TransactionExecutionDetails{
		Status:               nil,
		LogMessages:          logMessages,
		ReturnData:           returnData,
		ExecutedUnits:        execCtx.ComputeMeter.Used(),
		AccountsDataLenDelta: accountsDataLenDelta,
	}

	// Build processed transaction
	processedTx := ProcessedTransaction{
		TransactionType: ProcessedTransactionTypeExecuted,
		Executed: &ExecutedTransaction{
			LoadedTransaction: loadedTransaction,
			ExecutionDetails:  executionDetails,
			ProgramsModified:  make(map[solana.PublicKey]bool),
		},
	}

	// Build execution result
	executionResult := &TransactionExecutionResult{
		AccountUpdates:        accountUpdates,
		WritableAccounts:      writablePubkeys,
		WritableAccountSet:    writablePubkeySet,
		ModifiedVoteAccounts:  modifiedVoteAccounts,
		ModifiedStakeAccounts: []solana.PublicKey{},
	}

	out := LoadAndExecuteTransactionOutput{
		ProcessingResult: TransactionProcessingResult{
			ProcessedTransaction: &processedTx,
		},
		ExecutionResult: executionResult,
		ExecCtx:         execCtx,
		PreBalances:     preBalances,
		FeeInfo:         txFeeInfo,
	}
	baseFields(&out)
	return out
}

// mapLoadErrorType maps account loading errors to TransactionErrorType
func mapLoadErrorType(err error) TransactionErrorType {
	switch err {
	case TxErrMaxLoadedAccountsDataSizeExceeded:
		return TransactionErrorMaxLoadedAccountsDataSizeExceeded
	case TxErrInvalidProgramForExecution:
		return TransactionErrorInvalidProgramForExecution
	case TxErrProgramAccountNotFound:
		return TransactionErrorProgramAccountNotFound
	default:
		return TransactionErrorAccountNotFound
	}
}

// collectAccountUpdates collects all modified accounts from the execution context
func collectAccountUpdates(execCtx *sealevel.ExecutionCtx) []AccountUpdate {
	updates := make([]AccountUpdate, 0)

	for idx, newAcctState := range execCtx.TransactionContext.Accounts.Accounts {
		if execCtx.TransactionContext.Accounts.Touched[idx] {
			acct := *newAcctState
			if acct.Lamports == 0 {
				acct = accounts.Account{Key: acct.Key, RentEpoch: math.MaxUint64}
			}

			updates = append(updates, AccountUpdate{
				Pubkey:  acct.Key,
				Account: acct,
			})
		}
	}

	return updates
}

// convertToKeyedAccountSharedData converts accounts to KeyedAccountSharedData
func convertToKeyedAccountSharedData(accts []*accounts.Account) []KeyedAccountSharedData {
	result := make([]KeyedAccountSharedData, len(accts))
	for i, acct := range accts {
		result[i] = KeyedAccountSharedData{
			Pubkey:      acct.Key,
			AccountData: accountToSharedData(acct),
		}
	}
	return result
}

// accountToSharedData converts an Account to AccountSharedData
func accountToSharedData(acct *accounts.Account) AccountSharedData {
	data := make([]byte, len(acct.Data))
	copy(data, acct.Data)
	return AccountSharedData{
		Lamports:   acct.Lamports,
		Data:       data,
		Owner:      acct.Owner,
		Executable: acct.Executable,
		RentEpoch:  acct.RentEpoch,
	}
}
