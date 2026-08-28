package rpcserver

import (
	"bytes"
	"testing"

	"github.com/mr-tron/base58"
	"github.com/stretchr/testify/require"
)

func TestAccountBase58EncodingUsesRequestedSliceLimit(t *testing.T) {
	const maxBase58Data = 128
	data128 := bytes.Repeat([]byte{1}, maxBase58Data)
	got, err := encodeAcctDataWithConfig(data128, &GetAccountInfoConfig{})
	require.NoError(t, err)
	require.Equal(t, base58.Encode(data128), got)

	data129 := bytes.Repeat([]byte{1}, maxBase58Data+1)
	got, err = encodeAcctDataWithConfig(data129, &GetAccountInfoConfig{})
	require.NoError(t, err)
	require.Equal(t, "error: data too large for bs58 encoding", got)
	explicitBase58 := GetAccountEncodingBase58
	got, err = encodeAcctDataWithConfig(data129, &GetAccountInfoConfig{EncodingType: &explicitBase58})
	require.NoError(t, err)
	require.Equal(t, []string{"error: data too large for bs58 encoding", "base58"}, got)

	one := uint64(1)
	zero := uint64(0)
	got, err = encodeAcctDataWithConfig(data129, &GetAccountInfoConfig{
		DataSlice: &GetAccountInfoDataSlice{Len: &one, Offset: &zero},
	})
	require.NoError(t, err)
	require.Equal(t, base58.Encode([]byte{1}), got)
}
