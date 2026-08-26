package sealevel

import (
	"encoding/binary"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/stretchr/testify/require"
)

func secp256k1OffsetsData(offsets SecppSignatureOffsets) []byte {
	data := make([]byte, Secp256k1DataStart)
	data[0] = 1
	binary.LittleEndian.PutUint16(data[1:3], offsets.SignatureOffset)
	data[3] = offsets.SignatureInstructionIndex
	binary.LittleEndian.PutUint16(data[4:6], offsets.EthAddressOffset)
	data[6] = offsets.EthAddressInstructionIndex
	binary.LittleEndian.PutUint16(data[7:9], offsets.MessageDataOffset)
	binary.LittleEndian.PutUint16(data[9:11], offsets.MessageDataSize)
	data[11] = offsets.MessageInstructionIndex
	return data
}

func runSecp256k1Offsets(t *testing.T, offsets SecppSignatureOffsets, instructions ...[]byte) error {
	t.Helper()
	data := secp256k1OffsetsData(offsets)
	txCtx := secp256r1TestTxCtx(data, instructions...)
	return Secp256k1ProgramExecute(&ExecutionCtx{
		TransactionContext: txCtx,
		Features:           *features.NewFeaturesDefault(),
	})
}

func TestSecp256k1OffsetErrorsMatchAgave(t *testing.T) {
	validOffsets := SecppSignatureOffsets{
		SignatureInstructionIndex:  0,
		EthAddressOffset:           65,
		EthAddressInstructionIndex: 0,
		MessageDataOffset:          85,
		MessageDataSize:            1,
		MessageInstructionIndex:    0,
	}
	for _, test := range []struct {
		name   string
		mutate func(*SecppSignatureOffsets)
		want   error
	}{
		{"signature instruction index", func(o *SecppSignatureOffsets) { o.SignatureInstructionIndex = 1 }, PrecompileErrInstrDataSize},
		{"signature range", func(o *SecppSignatureOffsets) { o.SignatureOffset = 36 }, PrecompileErrSignature},
		{"eth instruction index", func(o *SecppSignatureOffsets) { o.EthAddressInstructionIndex = 1 }, PrecompileErrDataOffset},
		{"eth range", func(o *SecppSignatureOffsets) { o.EthAddressOffset = 81 }, PrecompileErrSignature},
		{"message instruction index", func(o *SecppSignatureOffsets) { o.MessageInstructionIndex = 1 }, PrecompileErrDataOffset},
		{"message range", func(o *SecppSignatureOffsets) { o.MessageDataOffset = 100 }, PrecompileErrSignature},
	} {
		t.Run(test.name, func(t *testing.T) {
			offsets := validOffsets
			test.mutate(&offsets)
			err := runSecp256k1Offsets(t, offsets, make([]byte, 100))
			require.ErrorIs(t, err, test.want)
		})
	}
}

func TestSecp256k1RecoveryIDClassification(t *testing.T) {
	offsets := SecppSignatureOffsets{
		SignatureInstructionIndex:  0,
		EthAddressOffset:           65,
		EthAddressInstructionIndex: 0,
		MessageDataOffset:          85,
		MessageDataSize:            1,
		MessageInstructionIndex:    0,
	}
	for _, recoveryID := range []byte{4, 255} {
		data := make([]byte, 100)
		data[Secp256k1SignatureSerializedSize] = recoveryID
		require.ErrorIs(t, runSecp256k1Offsets(t, offsets, data), PrecompileErrRecoveryId)
	}
	for _, recoveryID := range []byte{0, 1, 2, 3} {
		data := make([]byte, 100)
		data[Secp256k1SignatureSerializedSize] = recoveryID
		err := runSecp256k1Offsets(t, offsets, data)
		require.ErrorIs(t, err, PrecompileErrSignature)
		require.NotErrorIs(t, err, PrecompileErrRecoveryId)
	}
}
