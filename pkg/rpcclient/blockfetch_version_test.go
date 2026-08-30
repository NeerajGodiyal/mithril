package rpcclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/jsonrpc"
	"github.com/stretchr/testify/require"
)

type blockVersionRPC struct {
	t     *testing.T
	calls int
}

type transactionVersionRPC struct {
	t *testing.T
}

func (m *transactionVersionRPC) CallForInto(_ context.Context, out any, method string, params []any) error {
	m.t.Helper()
	require.Equal(m.t, "getTransaction", method)
	require.Len(m.t, params, 2)
	opts, ok := params[1].(rpc.M)
	require.True(m.t, ok)
	require.Equal(m.t, uint64(1), opts["maxSupportedTransactionVersion"])
	return json.Unmarshal([]byte(`{"slot":1,"meta":{}}`), out)
}

func (*transactionVersionRPC) CallWithCallback(context.Context, string, []any, func(*http.Request, *http.Response) error) error {
	return errors.New("unexpected callback request")
}

func (*transactionVersionRPC) CallBatch(context.Context, jsonrpc.RPCRequests) (jsonrpc.RPCResponses, error) {
	return nil, errors.New("unexpected batch request")
}

func (m *blockVersionRPC) CallForInto(_ context.Context, out any, method string, params []any) error {
	m.t.Helper()
	require.Equal(m.t, "getBlock", method)
	require.Len(m.t, params, 2)
	opts, ok := params[1].(rpc.M)
	require.True(m.t, ok)
	require.Equal(m.t, uint64(1), opts["maxSupportedTransactionVersion"])
	m.calls++
	return json.Unmarshal([]byte(`{"blockhash":"11111111111111111111111111111111","previousBlockhash":"11111111111111111111111111111111","parentSlot":0,"transactions":[],"rewards":[],"numRewardPartitions":1}`), out)
}

func (*blockVersionRPC) CallWithCallback(context.Context, string, []any, func(*http.Request, *http.Response) error) error {
	return errors.New("unexpected callback request")
}

func (*blockVersionRPC) CallBatch(context.Context, jsonrpc.RPCRequests) (jsonrpc.RPCResponses, error) {
	return nil, errors.New("unexpected batch request")
}

func TestAllBlockFetchesAdvertiseTransactionV1(t *testing.T) {
	mock := &blockVersionRPC{t: t}
	fetcher := &RpcClient{client: rpc.NewWithCustomRPCClient(mock)}

	_, err := fetcher.GetBlock(10)
	require.NoError(t, err)
	_, err = fetcher.GetBlockConfirmed(10)
	require.NoError(t, err)
	_, err = fetcher.GetBlockFinalizedOnce(10)
	require.NoError(t, err)
	_, err = fetcher.GetBlockConfirmedOnce(10)
	require.NoError(t, err)
	_, err = fetcher.GetBlockFinalized(10)
	require.NoError(t, err)
	_, err = fetcher.GetRewardsForSlotWithCommitment(10, rpc.CommitmentFinalized, 0)
	require.NoError(t, err)
	_, err = fetcher.GetNumRewardPartitions(10)
	require.NoError(t, err)
	_, _, err = fetcher.GetRewardSlots(10)
	require.NoError(t, err)

	require.Equal(t, 8, mock.calls)
}

func TestTransactionMetadataFetchAdvertisesTransactionV1(t *testing.T) {
	fetcher := &RpcClient{client: rpc.NewWithCustomRPCClient(&transactionVersionRPC{t: t})}
	meta, err := fetcher.GetTransactionMeta([64]byte{1})
	require.NoError(t, err)
	require.NotNil(t, meta)
}
