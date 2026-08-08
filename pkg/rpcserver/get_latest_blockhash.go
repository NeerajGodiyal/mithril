package rpcserver

import (
	"context"
	"fmt"
	"math"

	"github.com/filecoin-project/go-jsonrpc"
	"github.com/mr-tron/base58"
)

type GetLatestBlockhashRespContext struct {
	Slot uint64 `json:"slot"`
}

type GetLatestBlockhashRespValue struct {
	Blockhash            string `json:"blockhash"`
	LastValidBlockHeight uint64 `json:"lastValidBlockHeight"`
}

type GetLatestBlockhashResp struct {
	Context GetLatestBlockhashRespContext `json:"context"`
	Value   *GetLatestBlockhashRespValue  `json:"value"`
}

const recentBlockhashValidityHeight = 150

func (rpcServer *RpcServer) GetLatestBlockhash(ctx context.Context, p jsonrpc.RawParams) (GetLatestBlockhashResp, error) {
	if err := ctx.Err(); err != nil {
		return GetLatestBlockhashResp{}, err
	}
	params, err := jsonrpc.DecodeParams[[]interface{}](p)
	if err != nil {
		return GetLatestBlockhashResp{}, &InvalidParamsError{Message: fmt.Sprintf("decoding params: %v", err)}
	}
	conf, err := parseProcessedCommitmentConfig(params, "getLatestBlockhash")
	if err != nil {
		return GetLatestBlockhashResp{}, err
	}
	slotCtx := rpcServer.getSlotCtx()
	if slotCtx == nil || slotCtx.Blockhash == [32]byte{} {
		return GetLatestBlockhashResp{}, fmt.Errorf("node is not ready to provide a recent blockhash")
	}
	if conf.minContextSlot != nil && slotCtx.Slot < *conf.minContextSlot {
		return GetLatestBlockhashResp{}, &MinContextSlotNotReachedError{ContextSlot: *conf.minContextSlot}
	}
	lastValidBlockHeight, ok := recentBlockhashLastValidHeight(slotCtx.BlockHeight)
	if !ok {
		return GetLatestBlockhashResp{}, fmt.Errorf("block height is too large to calculate blockhash validity")
	}

	val := &GetLatestBlockhashRespValue{
		Blockhash:            base58.Encode(slotCtx.Blockhash[:]),
		LastValidBlockHeight: lastValidBlockHeight,
	}
	return GetLatestBlockhashResp{
		Context: GetLatestBlockhashRespContext{Slot: slotCtx.Slot},
		Value:   val,
	}, nil
}

func recentBlockhashLastValidHeight(blockHeight uint64) (uint64, bool) {
	if blockHeight > math.MaxUint64-recentBlockhashValidityHeight {
		return 0, false
	}
	return blockHeight + recentBlockhashValidityHeight, true
}

type processedCommitmentConfig struct {
	minContextSlot *uint64
}

func parseProcessedCommitmentConfig(params []interface{}, method string) (processedCommitmentConfig, error) {
	var conf processedCommitmentConfig
	if len(params) > 1 {
		return conf, &InvalidParamsError{Message: fmt.Sprintf("%s accepts at most one parameter", method)}
	}
	if len(params) == 0 {
		return conf, nil
	}
	confMap, ok := params[0].(map[string]interface{})
	if !ok {
		return conf, &InvalidParamsError{Message: fmt.Sprintf("%s config must be an object", method)}
	}
	if value, exists := confMap["commitment"]; exists {
		commitment, ok := value.(string)
		if !ok {
			return conf, invalidRPCOption(method, "commitment", "must be a string")
		}
		if commitment != "processed" {
			return conf, invalidRPCOption(method, "commitment", `only "processed" is supported`)
		}
	}
	if value, exists := confMap["minContextSlot"]; exists {
		minContextSlot, err := parseExactJSONUint(value, method, "minContextSlot")
		if err != nil {
			return conf, err
		}
		conf.minContextSlot = &minContextSlot
	}
	return conf, nil
}
