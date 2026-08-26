package block

import (
	"testing"

	"github.com/gagliardetto/solana-go/rpc"
	"github.com/stretchr/testify/require"
)

func TestFromBlockResultRejectsMalformedProviderData(t *testing.T) {
	require.NotPanics(t, func() {
		block, err := FromBlockResult(nil, 41, nil)
		require.Nil(t, block)
		require.ErrorContains(t, err, "slot 41: nil getBlock result")
	})

	for _, transaction := range []*rpc.DataBytesOrJSON{
		nil,
		rpc.DataBytesOrJSONFromBytes([]byte{0xff}),
	} {
		t.Run("transaction", func(t *testing.T) {
			result := &rpc.GetBlockResult{Transactions: []rpc.TransactionWithMeta{{Transaction: transaction}}}
			require.NotPanics(t, func() {
				block, err := FromBlockResult(result, 42, nil)
				require.Nil(t, block)
				require.ErrorContains(t, err, "slot 42 transaction 0")
			})
		})
	}
}
