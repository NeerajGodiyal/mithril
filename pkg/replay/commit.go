package replay

import (
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/sealevel"
)

func applySuccessfulTransactionState(slotCtx *sealevel.SlotCtx, execCtx *sealevel.ExecutionCtx, executionResult *TransactionExecutionResult) error {
	if execCtx == nil {
		return fmt.Errorf("missing execution context")
	}

	if executionResult == nil {
		if !accountsDeltaHashRemoved(slotCtx) {
			return fmt.Errorf("missing execution result while accounts delta hash is enabled")
		}
		handleModifiedAccounts(slotCtx, execCtx)
		recordStakeAndVoteAccountsFromMetas(slotCtx, execCtx)
		return nil
	}

	for _, pk := range executionResult.WritableAccounts {
		slotCtx.RecordWritableAcct(pk)
	}
	handleModifiedAccounts(slotCtx, execCtx)
	recordStakeAndVoteAccounts(slotCtx, execCtx, executionResult.WritableAccountSet)
	return nil
}

// ApplySuccessfulTransaction commits account updates from a successful
// LoadAndExecuteTransaction result into slotCtx. Divergence checks are
// skipped; callers must only pass outputs with no TransactionError.
func ApplySuccessfulTransaction(slotCtx *sealevel.SlotCtx, output LoadAndExecuteTransactionOutput) error {
	if output.ProcessingResult.TransactionError != nil {
		return fmt.Errorf("cannot apply failed transaction: %s", output.ProcessingResult.TransactionError.ErrorType.String())
	}
	return applySuccessfulTransactionState(slotCtx, output.ExecCtx, output.ExecutionResult)
}
