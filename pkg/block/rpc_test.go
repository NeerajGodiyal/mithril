package block

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/gagliardetto/solana-go"
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

func TestFromBlockResultPreservesV1TransactionConfig(t *testing.T) {
	config := solana.TransactionConfig{}.
		WithPriorityFee(9_001).
		WithComputeUnitLimit(300_000).
		WithLoadedAccountsDataSizeLimit(65_536).
		WithHeapSize(64 * 1024)
	tx := &solana.Transaction{
		Signatures: []solana.Signature{{1}},
		Message: solana.Message{
			Header: solana.MessageHeader{
				NumRequiredSignatures:       1,
				NumReadonlyUnsignedAccounts: 1,
			},
			AccountKeys:       []solana.PublicKey{{1}, {2}},
			Instructions:      []solana.CompiledInstruction{{ProgramIDIndex: 1, Accounts: []uint16{0}}},
			TransactionConfig: config,
		},
	}
	_, err := tx.Message.SetVersion(solana.MessageVersionV1)
	require.NoError(t, err)
	wire, err := tx.MarshalBinary()
	require.NoError(t, err)

	fixture := fmt.Sprintf(`{
		"blockhash":"11111111111111111111111111111111",
		"previousBlockhash":"11111111111111111111111111111111",
		"transactions":[{"transaction":[%q,"base64"],"meta":null,"version":1}],
		"rewards":[]
	}`, base64.StdEncoding.EncodeToString(wire))
	var result rpc.GetBlockResult
	require.NoError(t, json.Unmarshal([]byte(fixture), &result))

	block, err := FromBlockResult(&result, 42, nil)
	require.NoError(t, err)
	require.Len(t, block.Transactions, 1)
	require.Equal(t, solana.MessageVersionV1, block.Transactions[0].Message.GetVersion())
	require.Equal(t, config, block.Transactions[0].Message.TransactionConfig)
	gotWire, err := block.Transactions[0].MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, wire, gotWire)
}
