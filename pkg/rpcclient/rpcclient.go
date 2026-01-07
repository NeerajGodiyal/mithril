package rpcclient

import (
	"context"

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

// GetClient returns the underlying RPC client
func (c *RpcClient) GetClient() *rpc.Client {
	return c.client
}

// GetContext returns a background context for RPC calls
func (c *RpcClient) GetContext() context.Context {
	return context.Background()
}
