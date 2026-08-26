package rpcserver

import (
	"context"
	"errors"

	"github.com/filecoin-project/go-jsonrpc"
)

type rpcAPI struct {
	server *RpcServer
}

var errRPCServerBusy = errors.New("RPC server is busy")

func callRPC[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	result, err := fn()
	if err != nil {
		return result, err
	}
	return result, nil
}

func callExpensiveRPC[T any](ctx context.Context, server *RpcServer, fn func() (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	release, ok := acquireRPCSlots(
		server.expensiveSlots,
		server.remoteExpensiveSlots,
		rpcRequestIsLocal(ctx),
	)
	if !ok {
		return zero, errRPCServerBusy
	}
	defer release()
	return callRPC(ctx, fn)
}

func (api *rpcAPI) GetAccountInfo(ctx context.Context, p jsonrpc.RawParams) (GetAccountInfoResp, error) {
	return callRPC(ctx, func() (GetAccountInfoResp, error) {
		return api.server.GetAccountInfo(ctx, p)
	})
}

func (api *rpcAPI) GetSubmittedTransactionStatus(ctx context.Context, p jsonrpc.RawParams) (GetSubmittedTransactionStatusResp, error) {
	return callRPC(ctx, func() (GetSubmittedTransactionStatusResp, error) {
		return api.server.GetSubmittedTransactionStatus(ctx, p)
	})
}

func (api *rpcAPI) GetBankHash(ctx context.Context, p jsonrpc.RawParams) (string, error) {
	return callRPC(ctx, func() (string, error) {
		return api.server.GetBankHash(ctx, p)
	})
}

func (api *rpcAPI) GetBlockHeight(ctx context.Context, p jsonrpc.RawParams) (uint64, error) {
	return callRPC(ctx, func() (uint64, error) {
		return api.server.GetBlockHeight(ctx, p)
	})
}

func (api *rpcAPI) GetEpochInfo(ctx context.Context, p jsonrpc.RawParams) (GetEpochInfoResp, error) {
	return callRPC(ctx, func() (GetEpochInfoResp, error) {
		return api.server.GetEpochInfo(ctx, p)
	})
}

func (api *rpcAPI) GetGenesisHash(ctx context.Context, p jsonrpc.RawParams) (string, error) {
	return callRPC(ctx, func() (string, error) {
		return api.server.GetGenesisHash(ctx, p)
	})
}

func (api *rpcAPI) GetVerificationStatus(
	ctx context.Context, p jsonrpc.RawParams,
) (GetVerificationStatusResp, error) {
	return callRPC(ctx, func() (GetVerificationStatusResp, error) {
		return api.server.GetVerificationStatus(ctx, p)
	})
}

func (api *rpcAPI) GetHealth(ctx context.Context, p jsonrpc.RawParams) (string, error) {
	return callRPC(ctx, func() (string, error) {
		return api.server.GetHealth(ctx, p)
	})
}

func (api *rpcAPI) GetLatestBlockhash(ctx context.Context, p jsonrpc.RawParams) (GetLatestBlockhashResp, error) {
	return callRPC(ctx, func() (GetLatestBlockhashResp, error) {
		return api.server.GetLatestBlockhash(ctx, p)
	})
}

func (api *rpcAPI) GetRootedFeedStatus(ctx context.Context, p jsonrpc.RawParams) (GetRootedFeedStatusResp, error) {
	return callRPC(ctx, func() (GetRootedFeedStatusResp, error) {
		return api.server.GetRootedFeedStatus(ctx, p)
	})
}

func (api *rpcAPI) SendTransaction(ctx context.Context, p jsonrpc.RawParams) (string, error) {
	return callExpensiveRPC(ctx, api.server, func() (string, error) {
		return api.server.SendTransaction(ctx, p)
	})
}

func (api *rpcAPI) SimulateTransaction(ctx context.Context, p jsonrpc.RawParams) (SimulateTransactionResp, error) {
	return callExpensiveRPC(ctx, api.server, func() (SimulateTransactionResp, error) {
		return api.server.SimulateTransaction(ctx, p)
	})
}
