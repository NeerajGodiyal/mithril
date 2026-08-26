package sealevel

import (
	"encoding/binary"
	"encoding/hex"
	"math"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSecp256r1_Success_1(t *testing.T) {
	msg, err := hex.DecodeString("deadbeef0000")
	assert.NoError(t, err)

	sig, err := hex.DecodeString("65f479af7700ea826cdf4a2d30bbbfd5be5a8abb4dd6e8ef0bb0d5018b5f08160856e32671be561383d7eb408c6d24c28fd05141fd247dd8e67fc511d4f2ace9")
	assert.NoError(t, err)

	pub, err := hex.DecodeString("030f5183ccd84510385acc742f2d9d83771190c83cd0a36c42b0877c1666598a31")
	assert.NoError(t, err)

	err = verifySecp256r1(pub, msg, sig)
	assert.NoError(t, err)
}

func TestSecp256r1_Success_2(t *testing.T) {
	msg, err := hex.DecodeString("deadbeef0001")
	assert.NoError(t, err)

	sig, err := hex.DecodeString("dde6de58059a2edc745f3757a45b527c6a838e2f9944e7985cdbce18a9831444662257cde953020a5ba3dbd77dabc0e7ecf35dadf35754dd5c014e3197173ca7")
	assert.NoError(t, err)

	pub, err := hex.DecodeString("032a18f703b754f728b4faa2cd9e81d82647b86fb4e22bce7348ddf2a977a4e9d9")
	assert.NoError(t, err)

	err = verifySecp256r1(pub, msg, sig)
	assert.NoError(t, err)
}

func TestSecp256r1_Success_3(t *testing.T) {
	msg, err := hex.DecodeString("deadbeef0002")
	assert.NoError(t, err)

	sig, err := hex.DecodeString("d852239f6cdd19f530636fed1736f6c1fff499e988ffc14faf9098b6c359f53f24d8918494d158e562643da21939e3d8f4f733b2e135c63f205281c3cbae7cc1")
	assert.NoError(t, err)

	pub, err := hex.DecodeString("025241d2133264e7d4b0f91c0d2b08d7b8e4c015cc84d68eafe8c5dfe4b8bf6753")
	assert.NoError(t, err)

	err = verifySecp256r1(pub, msg, sig)
	assert.NoError(t, err)
}

func TestSecp256r1_Success_4(t *testing.T) {
	msg := "hello"

	sig, err := hex.DecodeString("a940d67c9560a47c5dafb45ab1f39eb68c8fac9b51fc8c4e30b1f0e63e4967d3586569a56364c3b03eefd421aa7fc750f6fa187210c3206c55602f96e0ecaa4d")
	assert.NoError(t, err)

	pub, err := hex.DecodeString("02d8c82b3791c8b51cfe44aa50226217159596ca26e6075aaf8bf8be2d351b96ae")
	assert.NoError(t, err)

	err = verifySecp256r1(pub, []byte(msg), sig)
	assert.NoError(t, err)
}

func TestSecp256r1_Failure_1(t *testing.T) {
	msg := "hello"

	sig, err := hex.DecodeString("a940d67c9560a47c5dafb45ab1f39eb68c8fac9b51fc8c4e30b1f0e63e4967d3a79a96599c9b3c50c1102bde558038aec5ece23b96547e189e599b2c1b767b04")
	assert.NoError(t, err)

	pub, err := hex.DecodeString("025241d2133264e7d4b0f91c0d2b08d7b8e4c015cc84d68eafe8c5dfe4b8bf6753")
	assert.NoError(t, err)

	err = verifySecp256r1(pub, []byte(msg), sig)
	assert.Error(t, err)
}

func secp256r1TestTxCtx(current []byte, instructions ...[]byte) *TransactionCtx {
	txCtx := NewTransactionCtx(*NewTransactionAccounts([]accounts.Account{}), 1, 2)
	for _, data := range instructions {
		txCtx.AllInstructions = append(txCtx.AllInstructions, Instruction{Data: data})
	}
	if current != nil {
		txCtx.PushInstructionCtx(InstructionCtx{Data: current})
		txCtx.InstructionStack = append(txCtx.InstructionStack, 1)
	}
	return txCtx
}

func TestSecp256r1GetDataSliceUsesInvalidDataOffsets(t *testing.T) {
	external := make([]byte, 100)
	txCtx := secp256r1TestTxCtx(make([]byte, 100), external)

	got, err := Secp256r1GetDataSlice(txCtx, 0, 99, 1)
	require.NoError(t, err)
	require.Len(t, got, 1)
	_, err = Secp256r1GetDataSlice(txCtx, 0, 100, 1)
	require.ErrorIs(t, err, PrecompileErrDataOffset)
	_, err = Secp256r1GetDataSlice(txCtx, 1, 0, 1)
	require.ErrorIs(t, err, PrecompileErrDataOffset)

	_, err = Secp256r1GetDataSlice(txCtx, math.MaxUint16, 100, 1)
	require.ErrorIs(t, err, PrecompileErrDataOffset)
	_, err = Secp256r1GetDataSlice(secp256r1TestTxCtx(nil), math.MaxUint16, 0, 1)
	require.ErrorIs(t, err, PrecompileErrDataOffset)
}

func secp256r1OffsetsData(messageOffset, messageSize uint16) []byte {
	data := make([]byte, Secp256r1DataStart)
	data[0] = 1
	binary.LittleEndian.PutUint16(data[2:4], 0)
	binary.LittleEndian.PutUint16(data[4:6], 0)
	binary.LittleEndian.PutUint16(data[6:8], 0)
	binary.LittleEndian.PutUint16(data[8:10], 0)
	binary.LittleEndian.PutUint16(data[10:12], messageOffset)
	binary.LittleEndian.PutUint16(data[12:14], messageSize)
	binary.LittleEndian.PutUint16(data[14:16], 0)
	return data
}

func TestSecp256r1ProgramRendersOffsetAndSignatureCodes(t *testing.T) {
	external := make([]byte, 100)
	for _, test := range []struct {
		name   string
		offset uint16
		want   error
		code   int
	}{
		{"exact boundary reaches signature validation", 99, PrecompileErrSignature, 2},
		{"out of bounds is invalid data offsets", 100, PrecompileErrDataOffset, 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := secp256r1OffsetsData(test.offset, 1)
			txCtx := secp256r1TestTxCtx(data, external)
			err := Secp256r1ProgramExecute(&ExecutionCtx{TransactionContext: txCtx})
			require.ErrorIs(t, err, test.want)
			require.Equal(t, test.code, TranslateErrToErrCode(err))
		})
	}
}
