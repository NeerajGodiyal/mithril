package blockstream

import (
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
)

type rpcConnPool struct {
	pool       chan *rpcclient.RpcClient
	numClients int
}

func newRpcConnPool(addrs []string) *rpcConnPool {
	clientsPerAddr := 3
	numClients := len(addrs) * clientsPerAddr

	pool := make(chan *rpcclient.RpcClient, numClients)

	for _, addr := range addrs {
		for range clientsPerAddr {
			c := rpcclient.NewRpcClient(addr)
			pool <- c
		}
	}

	return &rpcConnPool{pool: pool, numClients: numClients}
}

func (pool *rpcConnPool) Take() *rpcclient.RpcClient {
	for {
		c := <-pool.pool
		if c != nil {
			return c
		} else {
			mlog.Log.Infof("waiting for client")
		}
	}
}

func (pool *rpcConnPool) Release(client *rpcclient.RpcClient) {
	pool.pool <- client
}

func (pool *rpcConnPool) NumClients() int {
	return pool.numClients
}
