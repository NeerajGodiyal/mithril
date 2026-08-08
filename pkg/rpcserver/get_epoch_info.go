package rpcserver

import (
	"context"
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/filecoin-project/go-jsonrpc"
)

type GetEpochInfoResp struct {
	AbsoluteSlot     uint64 `json:"absoluteSlot"`
	BlockHeight      uint64 `json:"blockHeight"`
	Epoch            uint64 `json:"epoch"`
	SlotIndex        uint64 `json:"slotIndex"`
	SlotsInEpoch     uint64 `json:"slotsInEpoch"`
	TransactionCount uint64 `json:"transactionCount"`
}

func (rpcServer *RpcServer) GetEpochInfo(ctx context.Context, p jsonrpc.RawParams) (GetEpochInfoResp, error) {
	if err := ctx.Err(); err != nil {
		return GetEpochInfoResp{}, err
	}
	params, err := jsonrpc.DecodeParams[[]interface{}](p)
	if err != nil {
		return GetEpochInfoResp{}, &InvalidParamsError{Message: fmt.Sprintf("decoding params: %v", err)}
	}
	conf, err := parseProcessedCommitmentConfig(params, "getEpochInfo")
	if err != nil {
		return GetEpochInfoResp{}, err
	}
	slotCtx := rpcServer.getSlotCtx()
	if slotCtx == nil || rpcServer.epochSchedule == nil {
		return GetEpochInfoResp{}, fmt.Errorf("node is not ready to provide epoch information")
	}
	if conf.minContextSlot != nil && slotCtx.Slot < *conf.minContextSlot {
		return GetEpochInfoResp{}, &MinContextSlotNotReachedError{ContextSlot: *conf.minContextSlot}
	}
	epoch := slotCtx.Epoch
	slot := slotCtx.Slot
	firstSlotInEpoch := rpcServer.epochSchedule.FirstSlotInEpoch(epoch)
	slotIndex := slot - firstSlotInEpoch

	resp := GetEpochInfoResp{
		AbsoluteSlot:     slot,
		BlockHeight:      slotCtx.BlockHeight,
		Epoch:            epoch,
		SlotIndex:        slotIndex,
		SlotsInEpoch:     rpcServer.epochSchedule.SlotsInEpoch(epoch),
		TransactionCount: global.TransactionCount(),
	}

	return resp, nil
}
