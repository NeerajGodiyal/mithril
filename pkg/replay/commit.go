package replay

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/metrics"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
)

func applySuccessfulTransactionState(slotCtx *sealevel.SlotCtx, execCtx *sealevel.ExecutionCtx, executionResult *TransactionExecutionResult) error {
	if execCtx == nil {
		return fmt.Errorf("missing execution context")
	}
	if executionResult == nil && !accountsDeltaHashRemoved(slotCtx) {
		return fmt.Errorf("missing execution result while accounts delta hash is enabled")
	}

	recordMetrics := slotCtx != nil && slotCtx.Replay
	if executionResult != nil && !accountsDeltaHashRemoved(slotCtx) {
		var writableStart time.Time
		if recordMetrics {
			writableStart = time.Now()
		}
		slotCtx.RecordWritableAccts(executionResult.WritableAccounts)
		if recordMetrics {
			metrics.GlobalBlockReplay.TxPublishRecordWritableAcct.AddTimingSince(writableStart)
		}
	}

	var touchedStart time.Time
	if recordMetrics {
		touchedStart = time.Now()
	}
	stats := handleModifiedAccounts(slotCtx, execCtx)
	if recordMetrics {
		metrics.GlobalBlockReplay.TxPublishTouchedAccountState.AddTimingSince(touchedStart)
		atomic.AddUint64(&metrics.GlobalBlockReplay.TxPublicationTouchedAccounts, stats.touchedAccounts)
		atomic.AddUint64(&metrics.GlobalBlockReplay.TxPublicationTouchedAccountBytes, stats.touchedAccountBytes)
	}

	var stakeVoteStart time.Time
	if recordMetrics {
		stakeVoteStart = time.Now()
	}
	if executionResult == nil {
		recordStakeAndVoteAccountsFromMetas(slotCtx, execCtx)
	} else {
		recordStakeAndVoteAccounts(slotCtx, execCtx, executionResult.WritableAccountSet)
	}
	if recordMetrics {
		metrics.GlobalBlockReplay.TxPublishStakeVoteBookkeeping.AddTimingSince(stakeVoteStart)
	}
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
