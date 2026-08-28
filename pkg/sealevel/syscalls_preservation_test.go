package sealevel

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestGetSysvarSyscallReadsPublishedBankSnapshot(t *testing.T) {
	vm := newBls12_381SyscallTestVM(128)
	clock := SysvarClock{Slot: 42, EpochStartTimestamp: -17, Epoch: 3}
	snapshot, err := NewBankSysvars(42, clockSysvarTestAccount(clock))
	require.NoError(t, err)
	slotCtx := &SlotCtx{Slot: 42, Accounts: accounts.NewMemAccounts()}
	require.NoError(t, slotCtx.PublishBankSysvars(snapshot))
	vm.ctx.SlotCtx = slotCtx

	copy(vm.mem[:32], SysvarClockAddr[:])
	result, err := SyscallGetSysvarImpl(vm, 0, 64, 8, 8)
	require.NoError(t, err)
	require.Zero(t, result)
	require.Equal(t, uint64(clock.EpochStartTimestamp), binary.LittleEndian.Uint64(vm.mem[64:72]))
}

func TestGetProcessedSiblingInstructionSyscall(t *testing.T) {
	vm := newBls12_381SyscallTestVM(256)
	program := solana.PublicKey{1}
	account := solana.PublicKey{2}
	txCtx := NewTransactionCtx(*NewTransactionAccounts([]accounts.Account{{Key: program}, {Key: account}}), 1, 2)
	txCtx.InstructionTrace = []InstructionCtx{
		{
			ProgramAccounts: []uint64{0},
			InstructionAccounts: []InstructionAccount{{
				IndexInTransaction: 1,
				IndexInCaller:      0,
				IndexInCallee:      0,
				IsSigner:           true,
				IsWritable:         true,
			}},
			Data:         []byte{3, 4, 5},
			NestingLevel: 0,
		},
		{NestingLevel: 0}, // current instruction
		{},                // trace builder's next-instruction placeholder
	}
	txCtx.InstructionStack = []uint64{1}
	vm.ctx.TransactionContext = txCtx

	const (
		metaAddr     = 0
		programAddr  = 32
		dataAddr     = 64
		accountsAddr = 96
	)
	copy(vm.mem[metaAddr:], (&ProcessedSiblingInstruction{DataLen: 3, AccountsLen: 1}).Marshal())
	result, err := SyscallGetProcessedSiblingInstructionImpl(vm, 0, metaAddr, programAddr, dataAddr, accountsAddr)
	require.NoError(t, err)
	require.Equal(t, uint64(1), result)
	require.Equal(t, program[:], vm.mem[programAddr:programAddr+solana.PublicKeyLength])
	require.Equal(t, []byte{3, 4, 5}, vm.mem[dataAddr:dataAddr+3])

	var meta AccountMeta
	require.NoError(t, meta.Unmarshal(bytes.NewReader(vm.mem[accountsAddr:accountsAddr+AccountMetaSize])))
	require.Equal(t, account, meta.Pubkey)
	require.True(t, meta.IsSigner)
	require.True(t, meta.IsWritable)
}
