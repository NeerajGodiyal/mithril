package rpcserver

import (
	"context"
	"errors"
	"fmt"

	"github.com/filecoin-project/go-jsonrpc"
)

func (rpcServer *RpcServer) GetGenesisHash(ctx context.Context, p jsonrpc.RawParams) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	params, err := jsonrpc.DecodeParams[[]interface{}](p)
	if err != nil {
		return "", &InvalidParamsError{Message: fmt.Sprintf("decoding params: %v", err)}
	}
	if len(params) != 0 {
		return "", &InvalidParamsError{Message: "getGenesisHash does not accept parameters"}
	}
	rpcServer.genesisHashMu.RLock()
	defer rpcServer.genesisHashMu.RUnlock()
	if rpcServer.genesisHash == "" {
		return "", errors.New("node is not ready to provide its genesis hash")
	}
	return rpcServer.genesisHash, nil
}
