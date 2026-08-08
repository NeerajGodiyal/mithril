package rpcserver

import (
	"context"
	"fmt"

	"github.com/filecoin-project/go-jsonrpc"
)

func (rpcServer *RpcServer) GetBlockHeight(ctx context.Context, p jsonrpc.RawParams) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	params, err := jsonrpc.DecodeParams[[]interface{}](p)
	if err != nil {
		return 0, &InvalidParamsError{Message: fmt.Sprintf("decoding params: %v", err)}
	}
	conf, err := parseProcessedCommitmentConfig(params, "getBlockHeight")
	if err != nil {
		return 0, err
	}
	slotCtx := rpcServer.getSlotCtx()
	if slotCtx == nil {
		return 0, fmt.Errorf("node is not ready to provide block height")
	}
	if conf.minContextSlot != nil && slotCtx.Slot < *conf.minContextSlot {
		return 0, &MinContextSlotNotReachedError{ContextSlot: *conf.minContextSlot}
	}
	return slotCtx.BlockHeight, nil
}
