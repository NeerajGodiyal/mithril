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

	if len(params) != 1 {
		return "", invalidRPCOption("getBankHash", "parameters", "expected exactly one slot")
	}

	slot, err := parseExactJSONUint(params[0], "getBankHash", "slot")
	if err != nil {
		return "", err
	}

	bankHash, err := rpcServer.acctsDb.GetBankHashForSlot(slot)
	if err != nil {
		return "", fmt.Errorf("unable to retrieve bankhash for slot %d", slot)
	}

	return base58.Encode(bankHash), nil
}
