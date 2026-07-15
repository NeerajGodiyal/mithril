package replay

import (
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/sealevel"
)

// ApplySuccessfulTransaction commits account updates from a successful
// LoadAndExecuteTransaction result into slotCtx. Divergence checks are
// skipped; callers must only pass outputs with no TransactionError.
func ApplySuccessfulTransaction(slotCtx *sealevel.SlotCtx, output LoadAndExecuteTransactionOutput) error {
	if output.ProcessingResult.TransactionError != nil {
		return fmt.Errorf("cannot apply failed transaction: %s", output.ProcessingResult.TransactionError.ErrorType.String())
	}
	if output.ExecCtx == nil || output.ExecutionResult == nil {
		return fmt.Errorf("missing execution state")
	}

	for _, pk := range output.ExecutionResult.WritableAccounts {
		slotCtx.RecordWritableAcct(pk)
	}
	handleModifiedAccounts(slotCtx, output.ExecCtx)
	recordStakeAndVoteAccounts(slotCtx, output.ExecCtx, output.ExecutionResult.WritableAccountSet)
	return nil
}
