package replay

import (
	"errors"
	"math"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/arena"
	"github.com/Overclock-Validator/mithril/pkg/cu"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/fees"
	"github.com/Overclock-Validator/mithril/pkg/metrics"
	"github.com/Overclock-Validator/mithril/pkg/migration"
	"github.com/Overclock-Validator/mithril/pkg/rent"
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
	// IsSimulation indicates this is a simulation (no side effects on shared state)
	IsSimulation bool
	// RecordInnerInstructions enables CPI recording. The simulate handler
	// sets this when the RPC request asks for innerInstructions.
	RecordInnerInstructions bool
	// LeanResult skips response-only result materialization. Validator replay
	// and block production consume ExecCtx and FeeInfo directly. ADH-disabled
	// banks also skip writable-account result materialization; RPC simulation
	// leaves this false to retain the rich result.
	LeanResult bool
	// CapturePreBalances retains pre-fee balances in lean mode. Rich mode
	// always captures them for RPC compatibility.
	CapturePreBalances bool
	// RecordLogs retains execution logs in lean mode. Rich mode always records
	// them for RPC compatibility.
	RecordLogs bool
	// LogMessagesBytesLimit bounds retained logs. Zero preserves the existing
	// unbounded recorder used by rich simulation responses.
	LogMessagesBytesLimit uint64
	// CaptureRollbackAccounts retains the committed failure view needed by
	// local block production. Simulation enables it implicitly.
	CaptureRollbackAccounts bool
}

// LoadAndExecuteTransaction is a pure function that loads and executes a transaction.
// It takes all required state as input and returns all state changes as output without
// modifying the input SlotCtx. This function can be used for both actual execution
// and simulation.
// transactionFailureOutput returns a canonical unit-variant transaction
// error. InstructionError stays nil so the RPC renderer uses ErrorType.
func transactionFailureOutput(errorType TransactionErrorType) LoadAndExecuteTransactionOutput {
	return LoadAndExecuteTransactionOutput{
		ProcessingResult: TransactionProcessingResult{
			TransactionError: &TransactionError{
				ErrorType: errorType,
			},
		},
	}
}

// sanitizeFailureOutput returns the canonical SanitizeFailure response.
func sanitizeFailureOutput() LoadAndExecuteTransactionOutput {
	return transactionFailureOutput(TransactionErrorSanitizeFailure)
}

func cloneTransactionAccounts(accts []*accounts.Account) []*accounts.Account {
	out := make([]*accounts.Account, len(accts))
	for i, acct := range accts {
		if acct != nil {
			out[i] = acct.Clone()
		}
	}
	return out
}

func executionReturnData(execCtx *sealevel.ExecutionCtx) *TransactionReturnData {
	if execCtx == nil || execCtx.TransactionContext == nil {
		return nil
	}
	programID, data := execCtx.TransactionContext.ReturnData()
	if len(data) == 0 {
		return nil
	}
	return &TransactionReturnData{ProgramId: programID, Data: append([]byte(nil), data...)}
}

func mergeRollbackNonce(post []*accounts.Account, keys []solana.PublicKey, nonceKey solana.PublicKey, nonceAccount *accounts.Account, advanced bool) []*accounts.Account {
	if !advanced || nonceAccount == nil {
		return post
	}
	for i, key := range keys {
		if key != nonceKey || i >= len(post) {
			continue
		}
		nonceAccount = nonceAccount.Clone()
		if i == 0 && post[0] != nil {
			nonceAccount.Lamports = post[0].Lamports
		}
		post[i] = nonceAccount
		break
	}
	return post
}

func committedFailureAccounts(postFee []*accounts.Account, execCtx *sealevel.ExecutionCtx, nonceKey solana.PublicKey, nonceAccount *accounts.Account, nonceAdvanced bool) []*accounts.Account {
	if postFee == nil {
		return nil
	}
	committed := cloneTransactionAccounts(postFee)
	if execCtx == nil || execCtx.TransactionContext == nil {
		return committed
	}
	return mergeRollbackNonce(committed, execCtx.TransactionContext.AccountKeys, nonceKey, nonceAccount, nonceAdvanced)
}

func feePayerFailure(err error) *TransactionError {
	errorType := TransactionErrorInsufficientFundsForFee
	var accountIndex *uint8
	switch {
	case errors.Is(err, fees.ErrFeePayerNotFound):
		errorType = TransactionErrorAccountNotFound
	case errors.Is(err, fees.ErrInvalidAccountForFee):
		errorType = TransactionErrorInvalidAccountForFee
	case errors.Is(err, fees.ErrInsufficientFundsForRent):
		errorType = TransactionErrorInsufficientFundsForRent
		zero := uint8(0)
		accountIndex = &zero
	}
	return &TransactionError{ErrorType: errorType, InstructionError: err, AccountIndex: accountIndex}
}

func loadAccountSnapshot(slotCtx *sealevel.SlotCtx, key solana.PublicKey) (*accounts.Account, error) {
	acct, err := slotCtx.GetAccountShared(key)
	if err == nil {
		return acct.Clone(), nil
	}
	if slotCtx.AccountsDb != nil || slotCtx.UnrootedRead != nil {
		acct, err = slotCtx.GetAccountFromAccountsDb(key)
		if err == nil {
			return acct.Clone(), nil
		}
		if !errors.Is(err, accountsdb.ErrNoAccount) {
			return nil, newAccountSourceError("load fee-only account", err)
		}
	}
	return &accounts.Account{Key: key, Owner: addresses.SystemProgramAddr, RentEpoch: math.MaxUint64}, nil
}

func prepareFeeOnlyState(slotCtx *sealevel.SlotCtx, tx *solana.Transaction, instrs []sealevel.Instruction, limits *sealevel.ComputeBudgetLimits, nonceKey solana.PublicKey, nonceAccount *accounts.Account, nonceAdvanced bool) (*fees.TxFeeInfo, []*accounts.Account, []*accounts.Account, []uint64, *TransactionError, error) {
	pre := make([]*accounts.Account, len(tx.Message.AccountKeys))
	balances := make([]uint64, len(pre))
	for i, key := range tx.Message.AccountKeys {
		acct, err := loadAccountSnapshot(slotCtx, key)
		if err != nil {
			return nil, nil, nil, nil, nil, err
		}
		pre[i] = acct
		balances[i] = acct.Lamports
	}
	feeInfo := fees.CalculateTxFees(tx, instrs, limits, slotCtx.Features)
	if len(pre) == 0 {
		return feeInfo, pre, cloneTransactionAccounts(pre), balances, feePayerFailure(fees.ErrFeePayerNotFound), nil
	}
	if err := fees.ValidateFeePayer(pre[0], feeInfo.TotalFee, fees.RentForSlot(slotCtx)); err != nil {
		return feeInfo, pre, cloneTransactionAccounts(pre), balances, feePayerFailure(err), nil
	}
	post := cloneTransactionAccounts(pre)
	post[0].Lamports -= feeInfo.TotalFee
	post = mergeRollbackNonce(post, tx.Message.AccountKeys, nonceKey, nonceAccount, nonceAdvanced)
	return feeInfo, pre, post, balances, nil, nil
}

func LoadAndExecuteTransaction(input LoadAndExecuteTransactionInput) LoadAndExecuteTransactionOutput {
	tx := input.Transaction
	slotCtx := input.SlotCtx

	if input.Arena != nil {
		input.Arena.Reset()
	}

	if err := ValidateTransactionShape(tx, slotCtx.Features); err != nil {
		return sanitizeFailureOutput()
	}
	if tx.Message.IsVersioned() && !tx.Message.IsResolved() && tx.Message.AddressTableLookups.NumLookups() != 0 {
		return sanitizeFailureOutput()
	}
	if errorType, failed := transactionAccountLockError(tx, slotCtx.Features); failed {
		return transactionFailureOutput(errorType)
	}

	// Parse instructions and account metas
	start := time.Now()
	instrs, instructionAcctsPerInstr, txAcctMetas, err := instrsAndAcctMetasFromTx(tx, slotCtx.Features)
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
	computeBudgetLimits, err := sealevel.ComputeBudgetForTransaction(tx, instrs, slotCtx.Features)
	if err != nil {
		txErr := &TransactionError{ErrorType: TransactionErrorInstructionError, InstructionError: err}
		var computeBudgetErr *sealevel.ComputeBudgetError
		if errors.As(err, &computeBudgetErr) {
			idx := computeBudgetErr.InstructionIndex
			switch computeBudgetErr.Kind {
			case sealevel.ComputeBudgetErrorDuplicateInstruction:
				txErr = &TransactionError{ErrorType: TransactionErrorDuplicateInstruction, InstructionIndex: &idx}
			case sealevel.ComputeBudgetErrorInvalidLoadedAccountsDataSizeLimit:
				txErr = &TransactionError{ErrorType: TransactionErrorInvalidLoadedAccountsDataSizeLimit}
			default:
				txErr.InstructionIndex = &idx
				txErr.InstructionError = sealevel.InstrErrInvalidInstructionData
			}
		}
		return LoadAndExecuteTransactionOutput{
			ProcessingResult: TransactionProcessingResult{
				TransactionError: txErr,
			},
			Instrs: instrs,
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
		}
	}
	var rollbackNonceKey solana.PublicKey
	var rollbackNonceAccount *accounts.Account
	var rollbackNonceAdvanced bool
	if len(instrs) != 0 {
		rollbackNonceKey, rollbackNonceAccount, rollbackNonceAdvanced, err = sealevel.AdvancedNonceAccountForFailedTx(slotCtx, tx, instrs[0])
		if err != nil {
			return LoadAndExecuteTransactionOutput{LoadError: newAccountSourceError("prepare rollback nonce", err)}
		}
	}

	// Load and validate accounts
	start = time.Now()
	instructionsSysvarIdx := instructionsSysvarAccountIndex(tx)
	var instrsAcct *accounts.Account
	if instructionsSysvarIdx >= 0 {
		instrsAcct = sealevel.MakeInstructionsSysvarAccount(instrs)
	}
	var transactionAccts *sealevel.TransactionAccounts

	if slotCtx.Features.IsActive(features.FormalizeLoadedTransactionDataSize) {
		transactionAccts, txAcctMetas, err = loadAndValidateTxAcctsSimd186(slotCtx, txAcctMetas, tx, instrs, instrsAcct, computeBudgetLimits.LoadedAccountBytes)
	} else {
		transactionAccts, txAcctMetas, err = loadAndValidateTxAccts(slotCtx, txAcctMetas, tx, instrs, instrsAcct, computeBudgetLimits.LoadedAccountBytes)
	}

	// Base output fields for all paths after parsing
	baseFields := func(out *LoadAndExecuteTransactionOutput) {
		out.Instrs = instrs
		out.ComputeBudgetLimits = computeBudgetLimits
		if transactionAccts != nil {
			out.LoadedAccountsDataSize = transactionAccts.LoadedAccountsDataSize
		} else {
			out.LoadedAccountsDataSize = loadedAccountsSizeOnError(err)
		}
	}
	var sourceErr *accountSourceError
	if errors.As(err, &sourceErr) {
		out := LoadAndExecuteTransactionOutput{LoadError: sourceErr}
		baseFields(&out)
		return out
	}

	if errors.Is(err, TxErrMaxLoadedAccountsDataSizeExceeded) || errors.Is(err, TxErrInvalidProgramForExecution) || errors.Is(err, TxErrProgramAccountNotFound) {
		errType := mapLoadErrorType(err)
		feeInfo, pre, post, preBalances, payerErr, prepErr := prepareFeeOnlyState(slotCtx, tx, instrs, computeBudgetLimits, rollbackNonceKey, rollbackNonceAccount, rollbackNonceAdvanced)
		if prepErr != nil {
			out := LoadAndExecuteTransactionOutput{LoadError: prepErr}
			baseFields(&out)
			return out
		}
		if payerErr != nil {
			out := LoadAndExecuteTransactionOutput{
				ProcessingResult: TransactionProcessingResult{TransactionError: payerErr},
				FeeInfo:          feeInfo, PreBalances: preBalances,
				PreAccountSnapshots: pre, PostAccountSnapshots: post,
			}
			baseFields(&out)
			return out
		}
		out := LoadAndExecuteTransactionOutput{
			ProcessingResult: TransactionProcessingResult{
				TransactionError: &TransactionError{
					ErrorType:        errType,
					InstructionError: err,
				},
			},
			FeesOnly:             true,
			FeeInfo:              feeInfo,
			PreBalances:          preBalances,
			PreAccountSnapshots:  pre,
			PostAccountSnapshots: post,
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

	// Create execution context
	var logRecorder *sealevel.LogRecorder
	var logger sealevel.Logger = discardLogger{}
	if !input.LeanResult || input.RecordLogs {
		logRecorder = &sealevel.LogRecorder{BytesLimit: input.LogMessagesBytesLimit}
		logger = logRecorder
	}
	execCtx := newExecCtx(slotCtx, transactionAccts, computeBudgetLimits, logger)
	execCtx.TransactionContext.AllInstructions = instrs
	execCtx.TransactionContext.Signature = tx.Signatures[0]
	execCtx.TransactionContext.BorrowedAccountArena = input.Arena
	execCtx.IsSimulation = input.IsSimulation
	execCtx.RecordInnerInstructions = input.RecordInnerInstructions

	// Capture pre-balance lamports (before fee deduction)
	var preBalances []uint64
	if !input.LeanResult || input.CapturePreBalances {
		preBalances = make([]uint64, len(tx.Message.AccountKeys))
		for i := range tx.Message.AccountKeys {
			acct := execCtx.TransactionContext.Accounts.Accounts[i]
			preBalances[i] = acct.Lamports
		}
	}

	// Snapshot accounts so the simulate response can decode pre-state
	// token balances. Cloning is required because execution mutates
	// transactionAccts in place. Only the simulate handler reads this;
	// block-replay callers leave it nil and ignore.
	var preAccountSnapshots []*accounts.Account
	if input.IsSimulation || input.CaptureRollbackAccounts {
		preAccountSnapshots = make([]*accounts.Account, len(execCtx.TransactionContext.Accounts.Accounts))
		for i, acct := range execCtx.TransactionContext.Accounts.Accounts {
			if acct == nil {
				continue
			}
			preAccountSnapshots[i] = acct.Clone()
		}
	}

	// Calculate and deduct fees. RentForSlot supplies the exemption
	// minimum so a rent-exempt payer is rejected before instructions run.
	start = time.Now()
	txFeeInfo, _, err := fees.CalculateAndDeductTxFees(tx, input.TxMeta, instrs, &execCtx.TransactionContext.Accounts, computeBudgetLimits, slotCtx.Features, fees.RentForSlot(slotCtx), input.IsSimulation)
	var postFeeAccountSnapshots []*accounts.Account
	if input.IsSimulation || input.CaptureRollbackAccounts {
		postFeeAccountSnapshots = cloneTransactionAccounts(execCtx.TransactionContext.Accounts.Accounts)
	}
	if err != nil {
		out := LoadAndExecuteTransactionOutput{
			ProcessingResult: TransactionProcessingResult{
				TransactionError: feePayerFailure(err),
			},
			ExecCtx:              execCtx,
			PreBalances:          preBalances,
			PreAccountSnapshots:  preAccountSnapshots,
			PostAccountSnapshots: postFeeAccountSnapshots,
			FeeInfo:              txFeeInfo,
		}
		baseFields(&out)
		return out
	}
	metrics.GlobalBlockReplay.CalcAndDeductFees.AddTimingSince(start)

	// Read rent sysvar
	start = time.Now()
	rentSysvar, err := sealevel.ReadRentSysvar(execCtx)
	if err != nil {
		out := LoadAndExecuteTransactionOutput{
			LoadError:            newAccountSourceError("read rent sysvar", err),
			ExecCtx:              execCtx,
			PreBalances:          preBalances,
			PreAccountSnapshots:  preAccountSnapshots,
			PostAccountSnapshots: postFeeAccountSnapshots,
			FeeInfo:              txFeeInfo,
		}
		baseFields(&out)
		return out
	}
	metrics.GlobalBlockReplay.ReadRentSysvar.AddTimingSince(start)

	// Set rent-exempt rent epoch max and compute pre-tx rent states
	start = time.Now()
	rent.MaybeSetRentExemptRentEpochMax(slotCtx, &rentSysvar, &execCtx.Features, &execCtx.TransactionContext.Accounts)
	preTxRentStates := rent.NewRentStateInfo(&rentSysvar, execCtx.TransactionContext, &execCtx.Features)
	metrics.GlobalBlockReplay.PreTxRentStates.AddTimingSince(start)

	// Execute all instructions
	var instrErr error
	var failedInstructionIndex *uint8
	start = time.Now()
	for instrIdx, instr := range tx.Message.Instructions {
		setInstructionFailure := func(err error) {
			index := uint8(instrIdx)
			failedInstructionIndex = &index
			instrErr = err
		}
		execCtx.SetCurrentTopLevelInstr(uint8(instrIdx))
		if instructionsSysvarIdx >= 0 {
			ixStart := time.Now()
			err = fixupInstructionsSysvarAcct(execCtx, instructionsSysvarIdx, uint16(instrIdx))
			if err != nil {
				setInstructionFailure(err)
				break
			}
			metrics.GlobalBlockReplay.FixupInstructionsSysvarAccount.AddTimingSince(ixStart)
		}

		instructionAccts := instructionAcctsPerInstr[instrIdx]

		programId := tx.Message.AccountKeys[instr.ProgramIDIndex]
		migratingCus, isMigrating := migration.IsMigratingProgramAndGetCUs(programId)
		if isMigrating {
			err = execCtx.ComputeMeter.Consume(migratingCus)
			if err != nil {
				setInstructionFailure(err)
				break
			}
			execCtx.ComputeMeter.Disable()
		}

		programIndices := [1]uint64{uint64(instr.ProgramIDIndex)}
		err = execCtx.ProcessInstruction(instr.Data, instructionAccts, programIndices[:])
		if err == nil {
			if isMigrating {
				execCtx.ComputeMeter.Enable()
			}
		} else {
			setInstructionFailure(err)
			break
		}
	}
	metrics.GlobalBlockReplay.IxLoop.AddTimingSince(start)

	// Check rent state transitions
	start = time.Now()
	postTxRentStates := rent.NewRentStateInfo(&rentSysvar, execCtx.TransactionContext, &execCtx.Features)
	rentStateAccountIndex, rentStateErr := rent.VerifyRentStateChanges(preTxRentStates, postTxRentStates, execCtx.TransactionContext)
	metrics.GlobalBlockReplay.PostTxRentStates.AddTimingSince(start)

	// If there was an error, return failed transaction result
	if instrErr != nil || rentStateErr != nil {
		var relevantErr error
		var errType TransactionErrorType
		if instrErr != nil {
			relevantErr = instrErr
			errType = TransactionErrorInstructionError
		} else {
			relevantErr = rentStateErr
			errType = TransactionErrorInsufficientFundsForRent
		}
		var rentAccountIndex *uint8
		if rentStateErr != nil {
			rentAccountIndex = &rentStateAccountIndex
		}

		out := LoadAndExecuteTransactionOutput{
			ProcessingResult: TransactionProcessingResult{
				TransactionError: &TransactionError{
					ErrorType:        errType,
					InstructionIndex: failedInstructionIndex,
					InstructionError: relevantErr,
					AccountIndex:     rentAccountIndex,
				},
			},
			ExecutionStarted:     true,
			ExecCtx:              execCtx,
			PreBalances:          preBalances,
			PreAccountSnapshots:  preAccountSnapshots,
			PostAccountSnapshots: committedFailureAccounts(postFeeAccountSnapshots, execCtx, rollbackNonceKey, rollbackNonceAccount, rollbackNonceAdvanced),
			ReturnData:           executionReturnData(execCtx),
			FeeInfo:              txFeeInfo,
		}
		baseFields(&out)
		return out
	}

	// ADH-disabled validator replay commits directly from the transaction
	// context's touched accounts. It does not consume the response-shaped
	// writable slice/set, so avoid materializing either on this hot path.
	// ApplySuccessfulTransaction and ProcessTransaction re-derive effective
	// writability from AcctMetas only for vote/stake cache bookkeeping, which
	// must retain the legacy all-writable semantics.
	if input.LeanResult && accountsDeltaHashRemoved(slotCtx) {
		return LoadAndExecuteTransactionOutput{
			ExecutionStarted:       true,
			ExecCtx:                execCtx,
			PreBalances:            preBalances,
			PreAccountSnapshots:    preAccountSnapshots,
			FeeInfo:                txFeeInfo,
			Instrs:                 instrs,
			ComputeBudgetLimits:    computeBudgetLimits,
			LoadedAccountsDataSize: transactionAccts.LoadedAccountsDataSize,
		}
	}

	// Rich results and legacy ADH-enabled lean results still need the complete
	// writable account list.
	writablePubkeys := make([]solana.PublicKey, 0, len(txAcctMetas))
	writablePubkeySet := make(map[solana.PublicKey]struct{}, len(txAcctMetas))
	for _, txAcctMeta := range txAcctMetas {
		if isWritable(txAcctMeta, &execCtx.Features) {
			if _, exists := writablePubkeySet[txAcctMeta.PublicKey]; exists {
				continue
			}
			writablePubkeys = append(writablePubkeys, txAcctMeta.PublicKey)
			writablePubkeySet[txAcctMeta.PublicKey] = struct{}{}
		}
	}

	// Add payer to writable sets
	payerAcct := execCtx.TransactionContext.Accounts.Accounts[0]
	if _, exists := writablePubkeySet[payerAcct.Key]; !exists {
		writablePubkeys = append(writablePubkeys, payerAcct.Key)
		writablePubkeySet[payerAcct.Key] = struct{}{}
	}

	// Replay and block production commit directly from ExecCtx. Avoid building
	// response-only account copies and nested processed-transaction structures.
	if input.LeanResult {
		return LoadAndExecuteTransactionOutput{
			ExecutionStarted: true,
			ExecutionResult: &TransactionExecutionResult{
				WritableAccounts:   writablePubkeys,
				WritableAccountSet: writablePubkeySet,
			},
			ExecCtx:                execCtx,
			PreBalances:            preBalances,
			PreAccountSnapshots:    preAccountSnapshots,
			FeeInfo:                txFeeInfo,
			Instrs:                 instrs,
			ComputeBudgetLimits:    computeBudgetLimits,
			LoadedAccountsDataSize: transactionAccts.LoadedAccountsDataSize,
		}
	}

	// Collect modified accounts for simulation output
	accountUpdates := collectAccountUpdates(execCtx)

	// Collect modified vote accounts
	modifiedVoteAccounts := make(map[solana.PublicKey]*sealevel.VoteStateVersions)
	for pk, voteState := range execCtx.ModifiedVoteStates {
		modifiedVoteAccounts[pk] = voteState
	}

	returnData := executionReturnData(execCtx)

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
		LoadedAccountsDataSize: transactionAccts.LoadedAccountsDataSize,
	}

	// Build execution details
	executionDetails := TransactionExecutionDetails{
		Status:               nil,
		LogMessages:          logRecorder.Logs,
		InnerInstructions:    AssembleInnerInstructions(execCtx),
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
		ExecutionResult:     executionResult,
		ExecutionStarted:    true,
		ExecCtx:             execCtx,
		PreBalances:         preBalances,
		PreAccountSnapshots: preAccountSnapshots,
		FeeInfo:             txFeeInfo,
	}
	baseFields(&out)
	return out
}

// mapLoadErrorType maps account loading errors to TransactionErrorType
func mapLoadErrorType(err error) TransactionErrorType {
	switch {
	case errors.Is(err, TxErrMaxLoadedAccountsDataSizeExceeded):
		return TransactionErrorMaxLoadedAccountsDataSizeExceeded
	case errors.Is(err, TxErrInvalidProgramForExecution):
		return TransactionErrorInvalidProgramForExecution
	case errors.Is(err, TxErrProgramAccountNotFound):
		return TransactionErrorProgramAccountNotFound
	default:
		return TransactionErrorAccountNotFound
	}
}

// AssembleInnerInstructions groups CPI captures from execCtx by their
// top-level instruction index and converts them into the response shape.
// Returns nil when no captures were recorded (e.g. simulate without
// innerInstructions=true, or block-replay).
//
// Exported so the simulate RPC handler can reach into a partially-executed
// execCtx on the InstructionError path — Agave wire format keeps captured
// CPIs on tx failure even though Mithril's bifurcated processing-result
// type discards them via the TransactionError early-return.
func AssembleInnerInstructions(execCtx *sealevel.ExecutionCtx) []InnerInstructionsList {
	if len(execCtx.InnerInstrs) == 0 {
		return nil
	}

	byTopLevel := make(map[uint8][]CompiledInstruction)
	order := make([]uint8, 0)
	for _, r := range execCtx.InnerInstrs {
		if _, seen := byTopLevel[r.TopLevelIdx]; !seen {
			order = append(order, r.TopLevelIdx)
		}
		byTopLevel[r.TopLevelIdx] = append(byTopLevel[r.TopLevelIdx], CompiledInstruction{
			ProgramIdIndex: r.ProgramIdIndex,
			Accounts:       append([]uint8{}, r.Accounts...),
			Data:           append([]byte{}, r.Data...),
			StackHeight:    r.StackHeight,
		})
	}

	result := make([]InnerInstructionsList, 0, len(order))
	for _, idx := range order {
		result = append(result, InnerInstructionsList{
			Index:        idx,
			Instructions: byTopLevel[idx],
		})
	}
	return result
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
