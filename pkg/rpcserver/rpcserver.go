package rpcserver

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/filecoin-project/go-jsonrpc"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
)

type RpcServer struct {
	isReady       bool
	rpcService    *jsonrpc.RPCServer
	serv          *httptest.Server
	listener      net.Listener
	acctsDb       *accountsdb.AccountsDb
	epochSchedule *sealevel.SysvarEpochSchedule
	slotCtx       *sealevel.SlotCtx
	slotCtxMu     sync.RWMutex

	leaderTPUCacheMu         sync.RWMutex
	leaderTPUByIdentity      map[solana.PublicKey]netip.AddrPort
	leaderTPUCacheUpdatedAt  time.Time
	clusterNodesRefreshEvery time.Duration
	clusterNodesRefreshOnce  sync.Once

	clusterRPCEndpoints []string
	clusterNodesFetcher clusterNodesFetcher
	// packetSender is injectable for tests; production defaults to UDP.
	packetSender                      packetSender
	sendTransactionLeaderForwardCount uint64
}

func NewRpcServer(acctsDb *accountsdb.AccountsDb, port uint16) *RpcServer {
	var err error
	rpcServer := &RpcServer{}

	addrStr := fmt.Sprintf("0.0.0.0:%d", port)
	rpcServer.listener, err = net.Listen("tcp", addrStr)
	if err != nil {
		panic(err)
	}

	rpcErrors := rpcErrorRegistry()
	rpcServer.rpcService = jsonrpc.NewServer(
		jsonrpc.WithServerMethodNameFormatter(
			func(namespace, method string) string {
				return strings.ToLower(string(method[0])) + method[1:]
			}),
		jsonrpc.WithServerErrors(rpcErrors),
	)

	rpcServer.rpcService.Register("MithrilRpc", rpcServer)
	rpcServer.acctsDb = acctsDb
	rpcServer.epochSchedule = fetchAndUnmarshalEpochScheduleSysvar(acctsDb)
	rpcServer.leaderTPUByIdentity = make(map[solana.PublicKey]netip.AddrPort)
	rpcServer.clusterNodesRefreshEvery = sendTransactionClusterNodesRefreshEvery
	rpcServer.clusterRPCEndpoints = configuredSendTransactionRPCEndpoints()
	rpcServer.packetSender = defaultPacketSender
	rpcServer.sendTransactionLeaderForwardCount = sendTransactionLeaderForwardCount

	return rpcServer
}

func fetchAndUnmarshalEpochScheduleSysvar(acctsDb *accountsdb.AccountsDb) *sealevel.SysvarEpochSchedule {
	epochScheduleAcct, err := acctsDb.GetAccount(0, sealevel.SysvarEpochScheduleAddr)
	if err != nil {
		panic("unable to get epochschedule when creating RPC server")
	}

	decoder := bin.NewBinDecoder(epochScheduleAcct.Data)
	var epochSchedule sealevel.SysvarEpochSchedule
	epochSchedule.MustUnmarshalWithDecoder(decoder)

	return &epochSchedule
}

func (rpcServer *RpcServer) SetSlotCtx(slotCtx *sealevel.SlotCtx) {
	rpcServer.slotCtxMu.Lock()
	rpcServer.slotCtx = slotCtx
	rpcServer.slotCtxMu.Unlock()
}

func (rpcServer *RpcServer) getSlotCtx() *sealevel.SlotCtx {
	rpcServer.slotCtxMu.RLock()
	defer rpcServer.slotCtxMu.RUnlock()
	return rpcServer.slotCtx
}

func (rpcServer *RpcServer) Start() {
	rpcServer.startClusterNodesRefreshLoop()
	go http.Serve(rpcServer.listener, rpcServer.rpcService)
}

func (rpcServer *RpcServer) startClusterNodesRefreshLoop() {
	rpcServer.clusterNodesRefreshOnce.Do(func() {
		if rpcServer.clusterNodesFetcher == nil && len(rpcServer.clusterRPCEndpoints) == 0 {
			return
		}

		go func() {
			if err := rpcServer.refreshLeaderTPUCache(context.Background()); err != nil {
				mlog.Log.Warnf("sendTransaction: initial cluster node refresh failed: %v", err)
			}

			ticker := time.NewTicker(rpcServer.clusterNodesRefreshInterval())
			defer ticker.Stop()

			for range ticker.C {
				if err := rpcServer.refreshLeaderTPUCache(context.Background()); err != nil {
					mlog.Log.Warnf("sendTransaction: periodic cluster node refresh failed: %v", err)
				}
			}
		}()
	})
}

func (rpcServer *RpcServer) clusterNodesRefreshInterval() time.Duration {
	if rpcServer.clusterNodesRefreshEvery > 0 {
		return rpcServer.clusterNodesRefreshEvery
	}
	return sendTransactionClusterNodesRefreshEvery
}
