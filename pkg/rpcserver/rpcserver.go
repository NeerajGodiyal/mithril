package rpcserver

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/filecoin-project/go-jsonrpc"
	bin "github.com/gagliardetto/binary"
)

type RpcServer struct {
	isReady       bool
	rpcService    *jsonrpc.RPCServer
	serv          *httptest.Server
	listener      net.Listener
	acctsDb       *accountsdb.AccountsDb
	epochSchedule *sealevel.SysvarEpochSchedule
}

func NewRpcServer(acctsDb *accountsdb.AccountsDb, port uint16) *RpcServer {
	var err error
	rpcServer := &RpcServer{}

	addrStr := fmt.Sprintf("0.0.0.0:%d", port)
	rpcServer.listener, err = net.Listen("tcp", addrStr)
	if err != nil {
		panic(err)
	}

	rpcServer.rpcService = jsonrpc.NewServer(jsonrpc.WithServerMethodNameFormatter(
		func(namespace, method string) string {
			return strings.ToLower(string(method[0])) + method[1:]
		}))

	rpcServer.rpcService.Register("MithrilRpc", rpcServer)
	rpcServer.acctsDb = acctsDb
	rpcServer.epochSchedule = fetchAndUnmarshalEpochScheduleSysvar(acctsDb)

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

func (rpcServer *RpcServer) Start() {
	go http.Serve(rpcServer.listener, rpcServer.rpcService)
}
