package replay

import (
	"bytes"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/metrics"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
)

func sameAccountState(a, b *accounts.Account) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Key == b.Key && a.Lamports == b.Lamports && a.Owner == b.Owner &&
		a.Executable == b.Executable && a.RentEpoch == b.RentEpoch &&
		a.IsDummy == b.IsDummy && bytes.Equal(a.Data, b.Data)
}

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

// ApplyFailedTransaction commits only the rollback view of an executed
// failure: deducted fees and an advanced durable nonce. Instruction writes
// remain diagnostic-only in ExecCtx.
func ApplyFailedTransaction(slotCtx *sealevel.SlotCtx, output LoadAndExecuteTransactionOutput) error {
	if output.ProcessingResult.TransactionError == nil {
		return fmt.Errorf("cannot apply successful transaction as failed")
	}
	if len(output.PostAccountSnapshots) == 0 || len(output.PostAccountSnapshots) != len(output.PreAccountSnapshots) {
		return fmt.Errorf("missing failed transaction rollback accounts")
	}
	touched := make([]bool, len(output.PostAccountSnapshots))
	for i := range touched {
		touched[i] = !sameAccountState(output.PreAccountSnapshots[i], output.PostAccountSnapshots[i])
	}
	if err := accounts.SetTransactionAccounts(slotCtx.Accounts, output.PostAccountSnapshots, touched); err != nil {
		return err
	}
	for i, changed := range touched {
		if changed {
			slotCtx.RecordModifiedAcct(output.PostAccountSnapshots[i].Key)
		}
	}
	return nil
}
