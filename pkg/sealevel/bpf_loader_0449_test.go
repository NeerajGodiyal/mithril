package sealevel

import (
	"encoding/binary"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	feat "github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/sbpf"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestSerializeParametersAlignedAppendsDirectAccountPointers(t *testing.T) {
	features := feat.NewFeaturesDefault()
	features.EnableFeature(feat.VirtualAddressSpaceAdjustments, 0)
	features.EnableFeature(feat.AccountDataDirectMapping, 0)
	features.EnableFeature(feat.DirectAccountPointersInProgramInput, 0)

	account := &accounts.Account{Key: solana.PublicKey{1}, Data: []byte{1, 2, 3}, Lamports: 1}
	program := &accounts.Account{Key: solana.PublicKey{2}, Executable: true, Lamports: 1}
	txAccounts := NewTransactionAccountsFromRefs([]*accounts.Account{account, program}, []bool{false, false})
	txCtx := NewTransactionCtx(*txAccounts, 4, 4)
	instrCtx := &InstructionCtx{}
	instrCtx.Configure(
		[]uint64{1},
		[]InstructionAccount{
			{IndexInTransaction: 0, IndexInCallee: 0},
			{IndexInTransaction: 0, IndexInCallee: 0},
		},
		[]byte{7, 8, 9},
	)
	txCtx.InstructionTrace = []InstructionCtx{*instrCtx}
	txCtx.InstructionStack = []uint64{0}
	execCtx := &ExecutionCtx{Features: *features, TransactionContext: txCtx, IsSimulation: true}

	parameterBytes, _, _, _, regions, err := serializeParametersAligned(execCtx)
	require.NoError(t, err)
	require.Len(t, parameterBytes, 184)

	pointers := parameterBytes[len(parameterBytes)-16:]
	want := sbpf.VaddrInput + 8
	require.Equal(t, want, binary.LittleEndian.Uint64(pointers[:8]))
	require.Equal(t, want, binary.LittleEndian.Uint64(pointers[8:]))

	last := regions[len(regions)-1]
	pointerVaddr := sbpf.VaddrInput + last.Offset + last.RegionSize - 16
	require.Zero(t, pointerVaddr%8)
	require.Equal(t, pointers, parameterBytes[last.HostOffset+last.RegionSize-16:last.HostOffset+last.RegionSize])
}
