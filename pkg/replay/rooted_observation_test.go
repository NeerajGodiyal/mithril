package replay

import (
	"sync"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/rootedevents"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/tpu/txfixture"
	"github.com/Overclock-Validator/mithril/pkg/txstatus"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestRootedTransactionObservationUsesRuntimeEvidenceAndOwnsBytes(t *testing.T) {
	tx, err := solana.TransactionFromBytes(txfixture.MustSignedTransferWire(0))
	require.NoError(t, err)
	observation, err := PrepareRootedTransactionObservation(tx, 3)
	require.NoError(t, err)
	require.Equal(t, uint32(3), observation.Index)
	wire, err := tx.MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, wire, observation.Transaction)
	messageHash, err := txstatus.TransactionMessageHash(tx)
	require.NoError(t, err)
	require.Equal(t, solana.Hash(messageHash).String(), observation.MessageHash)

	runtimeKey := solana.PublicKey{0x77}
	returnProgram := solana.PublicKey{0x88}
	returnBytes := []byte{4, 5, 6}
	txCtx := &sealevel.TransactionCtx{AccountKeys: []solana.PublicKey{runtimeKey}}
	txCtx.SetReturnData(returnProgram, returnBytes)
	recorder := &sealevel.LogRecorder{Logs: []string{"program log"}, Truncated: true}
	execCtx := &sealevel.ExecutionCtx{
		Log:                recorder,
		TransactionContext: txCtx,
		InnerInstrs: []sealevel.RecordedInnerInstr{{
			TopLevelIdx: 2, ProgramIdIndex: 1, Accounts: []uint8{3}, Data: []byte{7, 8},
		}},
	}

	CaptureRootedTransactionExecution(&observation, execCtx)
	FinishRootedTransactionObservation(&observation, 99, nil, "")
	require.Equal(t, []string{runtimeKey.String()}, observation.AccountKeys)
	require.Equal(t, []string{"program log"}, observation.Logs)
	require.True(t, observation.LogsTruncated)
	require.True(t, observation.Succeeded)
	require.Equal(t, uint64(99), observation.ComputeUnits)
	require.Len(t, observation.Inner, 1)
	require.Equal(t, []byte{7, 8}, observation.Inner[0].Instructions[0].Data)
	require.Equal(t, returnProgram.String(), observation.ReturnData.ProgramID)
	require.Equal(t, returnBytes, observation.ReturnData.Data)

	execCtx.InnerInstrs[0].Data[0] = 0xff
	returnBytes[0] = 0xff
	recorder.Logs[0] = "changed"
	require.Equal(t, []byte{7, 8}, observation.Inner[0].Instructions[0].Data)
	require.Equal(t, []byte{4, 5, 6}, observation.ReturnData.Data)
	require.Equal(t, []string{"program log"}, observation.Logs)
}

func TestRootedTransactionObservationPreservesEmptyReturnData(t *testing.T) {
	programID := solana.PublicKey{0x55}
	txCtx := &sealevel.TransactionCtx{}
	txCtx.SetReturnData(programID, nil)
	observation := rootedevents.TransactionObservation{}
	CaptureRootedTransactionExecution(&observation, &sealevel.ExecutionCtx{TransactionContext: txCtx})
	require.NotNil(t, observation.ReturnData)
	require.Equal(t, programID.String(), observation.ReturnData.ProgramID)
	require.Empty(t, observation.ReturnData.Data)
}

func TestRootedTransactionObservationTypesEarlySanitizeFailure(t *testing.T) {
	tx, err := solana.TransactionFromBytes(txfixture.MustSignedTransferWire(0))
	require.NoError(t, err)
	tx.Message.Instructions = make([]solana.CompiledInstruction, maxInstrTraceCapacity+1)
	observation, err := PrepareRootedTransactionObservation(tx, 0)
	require.NoError(t, err)
	feats := features.NewFeaturesDefault()
	feats.EnableFeature(features.StaticInstructionLimit, 0)

	_, _, err = processTransactionForReplay(
		&sealevel.SlotCtx{Features: feats},
		&sync.WaitGroup{},
		tx,
		nil,
		nil,
		nil,
		&observation,
		nil,
		false,
	)
	require.ErrorIs(t, err, TxErrSanitizeFailure)
	require.False(t, observation.Succeeded)
	require.Equal(t, "SanitizeFailure", observation.Failure)
}
