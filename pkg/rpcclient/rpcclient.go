package rpcclient

import (
	"github.com/gagliardetto/solana-go/rpc"
)

type RpcClient struct {
	client   *rpc.Client
	endpoint string
}

func NewRpcClient(endpoint string) *RpcClient {
	client := rpc.New(endpoint)
	return &RpcClient{client: client, endpoint: endpoint}
}

// Endpoint returns the RPC endpoint URL
func (c *RpcClient) Endpoint() string {
	return c.endpoint
}
