package replay

import (
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/rootedevents"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/txverify"
	"github.com/gagliardetto/solana-go"
)

// PrepareRootedTransactionObservation captures the transaction's stable wire
// identity before execution. Runtime fields are filled by the matching capture
// and finish helpers below.
func PrepareRootedTransactionObservation(tx *solana.Transaction, index uint32) (rootedevents.TransactionObservation, error) {
	if tx == nil || len(tx.Signatures) == 0 {
		return rootedevents.TransactionObservation{}, fmt.Errorf("rooted transaction observation %d: transaction or signature is missing", index)
	}
	message, err := txverify.MessageBytes(tx)
	if err != nil {
		return rootedevents.TransactionObservation{}, fmt.Errorf("rooted transaction observation %d: marshal signed message: %w", index, err)
	}
	observation := rootedevents.TransactionObservation{
		Index:     index,
		Signature: tx.Signatures[0].String(),
		Message:   append([]byte(nil), message...),
	}
	observation.AccountKeys = publicKeyStrings(tx.Message.AccountKeys)
	return observation, nil
}

func publicKeyStrings(keys []solana.PublicKey) []string {
	out := make([]string, len(keys))
	for i, key := range keys {
		out[i] = key.String()
	}
	return out
}

// CaptureRootedTransactionExecution copies execution evidence from the exact
// bank execution shared by follower replay and local block production.
func CaptureRootedTransactionExecution(observation *rootedevents.TransactionObservation, execCtx *sealevel.ExecutionCtx) {
	if observation == nil || execCtx == nil || execCtx.TransactionContext == nil {
		return
	}
	if len(execCtx.TransactionContext.AccountKeys) > 0 {
		observation.AccountKeys = publicKeyStrings(execCtx.TransactionContext.AccountKeys)
	}
	if recorder, ok := execCtx.Log.(*sealevel.LogRecorder); ok {
		observation.Logs = append([]string(nil), recorder.Logs...)
		observation.LogsTruncated = recorder.Truncated
	}
	inner := AssembleInnerInstructions(execCtx)
	observation.Inner = make([]rootedevents.InnerInstructions, len(inner))
	for i, group := range inner {
		observation.Inner[i].Index = group.Index
		observation.Inner[i].Instructions = make([]rootedevents.CompiledInstruction, len(group.Instructions))
		for j, instruction := range group.Instructions {
			accounts := make([]uint16, len(instruction.Accounts))
			for k, account := range instruction.Accounts {
				accounts[k] = uint16(account)
			}
			observation.Inner[i].Instructions[j] = rootedevents.CompiledInstruction{
				ProgramIDIndex: uint16(instruction.ProgramIdIndex),
				Accounts:       accounts,
				Data:           append([]byte(nil), instruction.Data...),
			}
		}
	}
	programID, data := execCtx.TransactionContext.ReturnData()
	if programID != (solana.PublicKey{}) || len(data) > 0 {
		observation.ReturnData = &rootedevents.ReturnData{
			ProgramID: programID.String(),
			Data:      append([]byte(nil), data...),
		}
	}
}

// FinishRootedTransactionObservation records the local outcome after the
// execution result has been interpreted by the normal replay/producer path.
func FinishRootedTransactionObservation(
	observation *rootedevents.TransactionObservation,
	computeUnits uint64,
	processingErr error,
	transactionFailure string,
) {
	if observation == nil {
		return
	}
	observation.ComputeUnits = computeUnits
	observation.Succeeded = false
	switch {
	case transactionFailure != "":
		observation.Failure = transactionFailure
	case processingErr != nil:
		observation.Failure = "ReplayError"
	default:
		observation.Failure = ""
		observation.Succeeded = true
	}
}
