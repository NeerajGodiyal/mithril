package rpcserver

import (
	"context"
	"fmt"

	"github.com/filecoin-project/go-jsonrpc"
	"github.com/mr-tron/base58"
)

func (rpcServer *RpcServer) GetBankHash(ctx context.Context, p jsonrpc.RawParams) (string, error) {
	params, err := jsonrpc.DecodeParams[[]interface{}](p)
	if err != nil {
		return "", fmt.Errorf("decoding params: %w", err)
	}

	if len(params) < 1 {
		return "", fmt.Errorf("getBankHash requires a slot number as first parameter")
	}

	slotFloat, ok := params[0].(float64)
	if !ok {
		return "", fmt.Errorf("getBankHash requires a slot number as first parameter")
	}

	slot := uint64(slotFloat)
	bankHash, err := rpcServer.acctsDb.GetBankHashForSlot(slot)
	if err != nil {
		return "", fmt.Errorf("unable to retrieve bankhash for slot %d", slot)
	}

	return base58.Encode(bankHash), nil
}
