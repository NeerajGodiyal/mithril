package rpcserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/txstatus"
	"github.com/filecoin-project/go-jsonrpc"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"golang.org/x/time/rate"
)

type RpcServer struct {
	rpcService           *jsonrpc.RPCServer
	listeners            []rpcBoundListener
	httpServers          []*http.Server
	startOnce            sync.Once
	bindIP               net.IP
	requestRate          *rate.Limiter
	requestSlots         chan struct{}
	remoteRate           *rate.Limiter
	remoteSlots          chan struct{}
	expensiveSlots       chan struct{}
	remoteExpensiveSlots chan struct{}
	acctsDb              *accountsdb.AccountsDb
	// readAccount is the account-read seam. It defaults to acctsDb.GetAccount;
	// tests replace it to exercise the missing-versus-failed distinction, which
	// a real AccountsDb cannot express without a corrupted store on disk.
	readAccount func(slot uint64, pubkey solana.PublicKey) (*accounts.Account, error)
	// txReceipts records the fate of transactions this node submitted. A nil
	// store makes submission and status calls fail closed.
	txReceipts txstatus.Store
	// verificationSnapshot is the evidence-gate seam. It defaults to
	// replay.VerificationSnapshot; tests replace it to drive every verification
	// state without running a replay.
	verificationSnapshot verificationSnapshotFunc
	epochSchedule        *sealevel.SysvarEpochSchedule
	genesisHashMu        sync.RWMutex
	genesisHash          string
	slotCtx              *sealevel.SlotCtx
	slotCtxMu            sync.RWMutex
	slotCtxLifecycle     uint64

	leaderTPUCacheMu           sync.RWMutex
	leaderTPUByIdentity        map[solana.PublicKey]tpuEndpoint
	leaderTPUCacheUpdatedAt    time.Time
	clusterNodesRefreshEvery   time.Duration
	clusterNodesRefreshTimeout time.Duration
	clusterNodesRefreshOnce    sync.Once
	clusterNodesRefreshMu      sync.Mutex
	clusterNodesRefreshCancel  context.CancelFunc
	clusterNodesRefreshDone    chan struct{}
	clusterNodesRefreshStopped bool

	clusterRPCEndpoints []string
	clusterNodesFetcher clusterNodesFetcher
	// transactionSender is injectable for tests; production supports QUIC with UDP fallback.
	transactionSender                 transactionSender
	sendTransactionLeaderForwardCount uint64
}

type rpcBoundListener struct {
	listener net.Listener
	bindIP   net.IP
}

type rpcListenerHandler struct {
	server *RpcServer
	bindIP net.IP
}

const (
	maxRPCRequestBytes                = 1 << 20
	maxRPCBatchRequests               = 32
	maxRPCHeaderBytes                 = 64 << 10
	maxRPCConcurrentRequests          = 32
	maxRPCRemoteConcurrentRequests    = 24
	maxRPCConcurrentExpensiveRequests = 4
	maxRPCRemoteExpensiveRequests     = 3
	rpcRequestsPerSecond              = 100
	rpcRequestBurst                   = 200
	rpcRemoteRequestsPerSecond        = 50
	rpcRemoteRequestBurst             = 100
	rpcReadHeaderTimeout              = 5 * time.Second
	rpcReadTimeout                    = 15 * time.Second
	rpcRequestContextTimeout          = 10 * time.Second
	rpcWriteTimeout                   = 30 * time.Second
	rpcIdleTimeout                    = 60 * time.Second
	defaultClusterNodesRefreshTimeout = 10 * time.Second
)

// DefaultRPCBindAddress keeps the RPC listener local unless configured otherwise.
const DefaultRPCBindAddress = "127.0.0.1"

var supportedRPCMethods = map[string]struct{}{
	"getAccountInfo": {},
	// Additive, and deliberately NOT named getSignatureStatuses: this node can
	// only answer from retained receipts for transactions it submitted itself.
	"getSubmittedTransactionStatus": {},
	"getBankHash":                   {},
	"getBlockHeight":                {},
	"getEpochInfo":                  {},
	"getGenesisHash":                {},
	// Mithril-specific: this node verifies its own replayed state, so it can
	// answer whether that state checks out. No provider can.
	"getVerificationStatus": {},
	"getHealth":             {},
	"getLatestBlockhash":    {},
	"sendTransaction":       {},
	"simulateTransaction":   {},
}

func (rpcServer *RpcServer) SetGenesisHash(value string) error {
	hash, err := solana.HashFromBase58(value)
	if err != nil || hash == (solana.Hash{}) {
		return errors.New("genesis hash is invalid")
	}
	rpcServer.genesisHashMu.Lock()
	defer rpcServer.genesisHashMu.Unlock()
	if rpcServer.genesisHash != "" && rpcServer.genesisHash != value {
		return errors.New("genesis hash cannot change while the RPC server is running")
	}
	rpcServer.genesisHash = value
	return nil
}

func NewRpcServer(acctsDb *accountsdb.AccountsDb, port uint16, epochSchedule *sealevel.SysvarEpochSchedule) *RpcServer {
	return NewRpcServerWithBindAddress(acctsDb, DefaultRPCBindAddress, port, epochSchedule)
}

func NewRpcServerWithBindAddress(acctsDb *accountsdb.AccountsDb, bindAddress string, port uint16, epochSchedule *sealevel.SysvarEpochSchedule) *RpcServer {
	rpcServer, err := NewRpcServerWithBindAddressE(acctsDb, bindAddress, port, epochSchedule)
	if err != nil {
		panic(err)
	}
	return rpcServer
}

// NewRpcServerWithBindAddressE constructs the RPC server without panicking on
// operator input, state decode, or listener failures.
func NewRpcServerWithBindAddressE(acctsDb *accountsdb.AccountsDb, bindAddress string, port uint16, epochSchedule *sealevel.SysvarEpochSchedule) (*RpcServer, error) {
	return NewRpcServerWithClusterRPCEndpointsE(
		acctsDb,
		bindAddress,
		port,
		epochSchedule,
		configuredSendTransactionRPCEndpoints(),
	)
}

// NewRpcServerWithClusterRPCEndpointsE uses the node's resolved RPC endpoint
// list for transaction leader discovery.
func NewRpcServerWithClusterRPCEndpointsE(
	acctsDb *accountsdb.AccountsDb,
	bindAddress string,
	port uint16,
	epochSchedule *sealevel.SysvarEpochSchedule,
	clusterRPCEndpoints []string,
) (*RpcServer, error) {
	rpcServer := &RpcServer{}

	rpcServer.bindIP = net.ParseIP(bindAddress)
	if rpcServer.bindIP == nil {
		return nil, errors.New("RPC bind address must be an IP address")
	}

	rpcServer.rpcService = newRPCService(rpcServer)
	rpcServer.requestRate = rate.NewLimiter(rpcRequestsPerSecond, rpcRequestBurst)
	rpcServer.requestSlots = make(chan struct{}, maxRPCConcurrentRequests)
	rpcServer.remoteRate = rate.NewLimiter(rpcRemoteRequestsPerSecond, rpcRemoteRequestBurst)
	rpcServer.remoteSlots = make(chan struct{}, maxRPCRemoteConcurrentRequests)
	rpcServer.expensiveSlots = make(chan struct{}, maxRPCConcurrentExpensiveRequests)
	rpcServer.remoteExpensiveSlots = make(chan struct{}, maxRPCRemoteExpensiveRequests)
	rpcServer.acctsDb = acctsDb
	if epochSchedule != nil {
		rpcServer.epochSchedule = epochSchedule
	} else {
		loaded, loadErr := fetchAndUnmarshalEpochScheduleSysvarE(acctsDb)
		if loadErr != nil {
			return nil, loadErr
		}
		rpcServer.epochSchedule = loaded
	}
	rpcServer.leaderTPUByIdentity = make(map[solana.PublicKey]tpuEndpoint)
	rpcServer.clusterNodesRefreshEvery = sendTransactionClusterNodesRefreshEvery
	rpcServer.clusterNodesRefreshTimeout = defaultClusterNodesRefreshTimeout
	rpcServer.clusterRPCEndpoints = append([]string(nil), clusterRPCEndpoints...)
	rpcServer.transactionSender = defaultTransactionSender
	rpcServer.sendTransactionLeaderForwardCount = sendTransactionLeaderForwardCount

	var err error
	rpcServer.listeners, err = openRPCListeners(bindAddress, rpcServer.bindIP, port, net.Listen)
	if err != nil {
		return nil, fmt.Errorf("open RPC listener: %w", err)
	}
	for _, bound := range rpcServer.listeners {
		handler := rpcListenerHandler{server: rpcServer, bindIP: bound.bindIP}
		rpcServer.httpServers = append(rpcServer.httpServers, newRPCHTTPServer(handler))
	}

	return rpcServer, nil
}

func rpcListenAddress(bindAddress string, port uint16) string {
	return net.JoinHostPort(bindAddress, fmt.Sprintf("%d", port))
}

func openRPCListeners(
	bindAddress string,
	bindIP net.IP,
	port uint16,
	listen func(network, address string) (net.Listener, error),
) ([]rpcBoundListener, error) {
	primary, err := listen("tcp", rpcListenAddress(bindAddress, port))
	if err != nil {
		return nil, err
	}
	bound := []rpcBoundListener{{listener: primary, bindIP: bindIP}}

	companionIP := companionLoopbackIP(bindIP)
	if companionIP == nil {
		return bound, nil
	}
	_, actualPort, err := net.SplitHostPort(primary.Addr().String())
	if err != nil {
		closeErr := primary.Close()
		return nil, errors.Join(fmt.Errorf("resolve RPC listener port: %w", err), closeErr)
	}
	companionAddress := net.JoinHostPort(companionIP.String(), actualPort)
	companion, err := listen("tcp", companionAddress)
	if err != nil {
		closeErr := primary.Close()
		return nil, errors.Join(fmt.Errorf("listen for local RPC companion on %s: %w", companionAddress, err), closeErr)
	}
	return append(bound, rpcBoundListener{listener: companion, bindIP: companionIP}), nil
}

func companionLoopbackIP(bindIP net.IP) net.IP {
	if bindIP == nil || bindIP.IsLoopback() || bindIP.IsUnspecified() {
		return nil
	}
	if bindIP.To4() != nil {
		return net.ParseIP(DefaultRPCBindAddress)
	}
	return net.IPv6loopback
}

// LocalRPCAddress returns the listener address a process on the node should use.
// Wildcard and exact non-loopback listeners are also served on loopback.
func LocalRPCAddress(bindAddress, port string) string {
	ip := net.ParseIP(strings.TrimSpace(bindAddress))
	if ip == nil {
		ip = net.ParseIP(DefaultRPCBindAddress)
	}
	if !ip.IsLoopback() {
		if ip.To4() == nil {
			ip = net.IPv6loopback
		} else {
			ip = net.ParseIP(DefaultRPCBindAddress)
		}
	}
	return net.JoinHostPort(ip.String(), port)
}

// LocalRPCURL is LocalRPCAddress formatted as an HTTP URL.
func LocalRPCURL(bindAddress string, port int) string {
	if port <= 0 {
		return ""
	}
	return "http://" + LocalRPCAddress(bindAddress, strconv.Itoa(port))
}

func newRPCHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: rpcReadHeaderTimeout,
		ReadTimeout:       rpcReadTimeout,
		WriteTimeout:      rpcWriteTimeout,
		IdleTimeout:       rpcIdleTimeout,
		MaxHeaderBytes:    maxRPCHeaderBytes,
	}
}

func newRPCService(rpcServer *RpcServer) *jsonrpc.RPCServer {
	service := jsonrpc.NewServer(
		jsonrpc.WithServerMethodNameFormatter(formatRPCMethodName),
		jsonrpc.WithServerErrors(rpcErrorRegistry()),
		jsonrpc.WithMaxRequestSize(maxRPCRequestBytes),
	)
	service.Register("MithrilRpc", &rpcAPI{server: rpcServer})
	return service
}

func formatRPCMethodName(_, method string) string {
	return strings.ToLower(string(method[0])) + method[1:]
}

func fetchAndUnmarshalEpochScheduleSysvarE(acctsDb *accountsdb.AccountsDb) (*sealevel.SysvarEpochSchedule, error) {
	if acctsDb == nil {
		return nil, errors.New("AccountsDB is required to load the epoch schedule")
	}
	epochScheduleAcct, err := acctsDb.GetAccount(0, sealevel.SysvarEpochScheduleAddr)
	if err != nil {
		return nil, fmt.Errorf("load epoch schedule for RPC server: %w", err)
	}

	decoder := bin.NewBinDecoder(epochScheduleAcct.Data)
	var epochSchedule sealevel.SysvarEpochSchedule
	if err := epochSchedule.UnmarshalWithDecoder(decoder); err != nil {
		return nil, fmt.Errorf("decode epoch schedule for RPC server: %w", err)
	}

	return &epochSchedule, nil
}

func (rpcServer *RpcServer) SetSlotCtx(slotCtx *sealevel.SlotCtx) {
	rpcServer.slotCtxMu.Lock()
	if slotCtx == nil {
		rpcServer.slotCtxLifecycle++
	}
	rpcServer.slotCtx = slotCtx
	rpcServer.slotCtxMu.Unlock()
}

func (rpcServer *RpcServer) getSlotCtxWithLifecycle() (*sealevel.SlotCtx, uint64) {
	rpcServer.slotCtxMu.RLock()
	defer rpcServer.slotCtxMu.RUnlock()
	return rpcServer.slotCtx, rpcServer.slotCtxLifecycle
}

func (rpcServer *RpcServer) validateSlotCtxLifecycle(generation uint64, method string) error {
	rpcServer.slotCtxMu.RLock()
	defer rpcServer.slotCtxMu.RUnlock()
	if rpcServer.slotCtxLifecycle != generation {
		return fmt.Errorf("processed bank was invalidated during %s", method)
	}
	return nil
}

func (rpcServer *RpcServer) validateProcessedBankPublication(slotCtx *sealevel.SlotCtx, lifecycle uint64, method string) error {
	if slotCtx != nil {
		if err := slotCtx.ValidateAccountRead(); err != nil {
			return fmt.Errorf("processed account bank changed during %s: %w", method, err)
		}
	}
	return rpcServer.validateSlotCtxLifecycle(lifecycle, method)
}

func (rpcServer *RpcServer) Start() {
	rpcServer.startOnce.Do(func() {
		rpcServer.startClusterNodesRefreshLoop()
		for i := range rpcServer.httpServers {
			server := rpcServer.httpServers[i]
			listener := rpcServer.listeners[i].listener
			go func() {
				if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
					mlog.Log.Errorf("RPC server stopped: %v", err)
				}
			}()
		}
	})
}

func (rpcServer *RpcServer) Shutdown(ctx context.Context) error {
	if rpcServer == nil {
		return nil
	}
	errs := []error{rpcServer.stopClusterNodesRefresh(ctx)}
	serverErrs := make([]error, len(rpcServer.httpServers))
	var wg sync.WaitGroup
	for i := range rpcServer.httpServers {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			serverErrs[index] = rpcServer.httpServers[index].Shutdown(ctx)
		}(i)
	}
	wg.Wait()
	errs = append(errs, serverErrs...)
	for _, bound := range rpcServer.listeners {
		if err := bound.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (rpcServer *RpcServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rpcServer.serveHTTPForBind(w, r, rpcServer.bindIP)
}

func (handler rpcListenerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	handler.server.serveHTTPForBind(w, r, handler.bindIP)
}

type rpcLocalRequestContextKey struct{}

func (rpcServer *RpcServer) serveHTTPForBind(w http.ResponseWriter, r *http.Request, bindIP net.IP) {
	if !requestHostAllowed(bindIP, r.Host) {
		http.Error(w, "invalid request host", http.StatusMisdirectedRequest)
		return
	}
	if r.Header.Get("Origin") != "" {
		http.Error(w, "browser-originated requests are not accepted", http.StatusForbidden)
		return
	}
	if strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") ||
		r.Header.Get("Upgrade") != "" {
		http.Error(w, "protocol upgrades are not supported", http.StatusBadRequest)
		return
	}
	if quietNonRPCProbe(w, r) {
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		http.Error(w, "content type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	if r.ContentLength > maxRPCRequestBytes {
		writeRPCProbeError(w, nil, http.StatusRequestEntityTooLarge, -32700, "request body exceeds 1 MiB")
		return
	}
	local := isLocalRPCRequest(bindIP, r.RemoteAddr)
	if !rpcServer.allowRequests(local, 1) {
		writeRPCRateError(w)
		return
	}
	body, ok := readRPCRequestBody(w, r)
	if !ok {
		return
	}
	release, ok := rpcServer.acquireRequestSlots(local)
	if !ok {
		http.Error(w, "too many active requests", http.StatusServiceUnavailable)
		return
	}
	defer release()

	ctx := context.WithValue(r.Context(), rpcLocalRequestContextKey{}, local)
	ctx, cancel := context.WithTimeout(ctx, rpcRequestContextTimeout)
	defer cancel()
	rpcServer.handleRPCRequest(ctx, w, body, local)
}

func isLocalRPCRequest(bindIP net.IP, remoteAddr string) bool {
	if bindIP != nil && bindIP.IsLoopback() {
		return true
	}
	if bindIP == nil || !bindIP.IsUnspecified() {
		return false
	}
	host := strings.TrimSpace(remoteAddr)
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}
	host = strings.Trim(host, "[]")
	remoteIP := net.ParseIP(host)
	return remoteIP != nil && remoteIP.IsLoopback()
}

func rpcRequestIsLocal(ctx context.Context) bool {
	local, _ := ctx.Value(rpcLocalRequestContextKey{}).(bool)
	return local
}

func (rpcServer *RpcServer) allowRequests(local bool, count int) bool {
	if count <= 0 {
		return true
	}
	now := time.Now()
	if !local && rpcServer.remoteRate != nil && !rpcServer.remoteRate.AllowN(now, count) {
		return false
	}
	return rpcServer.requestRate == nil || rpcServer.requestRate.AllowN(now, count)
}

func tryAcquireRPCSlot(slots chan struct{}) bool {
	if slots == nil {
		return true
	}
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func releaseRPCSlot(slots chan struct{}) {
	if slots != nil {
		<-slots
	}
}

func acquireRPCSlots(global, remote chan struct{}, local bool) (func(), bool) {
	remoteHeld := false
	if !local && remote != nil {
		if !tryAcquireRPCSlot(remote) {
			return nil, false
		}
		remoteHeld = true
	}
	if !tryAcquireRPCSlot(global) {
		if remoteHeld {
			releaseRPCSlot(remote)
		}
		return nil, false
	}
	return func() {
		releaseRPCSlot(global)
		if remoteHeld {
			releaseRPCSlot(remote)
		}
	}, true
}

func (rpcServer *RpcServer) acquireRequestSlots(local bool) (func(), bool) {
	return acquireRPCSlots(rpcServer.requestSlots, rpcServer.remoteSlots, local)
}

func requestHostAllowed(bindIP net.IP, hostport string) bool {
	if bindIP == nil {
		return false
	}
	host := strings.TrimSpace(hostport)
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}
	host = strings.Trim(strings.TrimSuffix(strings.ToLower(host), "."), "[]")
	if host == "localhost" {
		return bindIP.IsLoopback()
	}
	requestIP := net.ParseIP(host)
	if requestIP == nil {
		return false
	}
	if bindIP.IsUnspecified() {
		return !requestIP.IsUnspecified()
	}
	return requestIP.Equal(bindIP)
}

type rpcMethodProbe struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	invalid bool
}

var errRPCBatchTooLarge = errors.New("RPC batch is too large")

type rpcProbeErrorResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   rpcProbeError   `json:"error"`
}

type rpcProbeError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func quietNonRPCProbe(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodPost {
		return false
	}
	http.NotFound(w, r)
	return true
}

func (rpcServer *RpcServer) handleRPCRequest(ctx context.Context, w http.ResponseWriter, body []byte, local bool) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		writeRPCProbeError(w, nil, http.StatusBadRequest, -32600, "Invalid request")
		return
	}
	if body[0] == '[' {
		rpcServer.handleRPCBatch(ctx, w, body, local)
		return
	}

	if !json.Valid(body) {
		writeRPCProbeError(w, nil, http.StatusInternalServerError, -32700, "Parse error")
		return
	}
	var req rpcMethodProbe
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPCProbeError(w, nil, http.StatusBadRequest, -32600, "Invalid request")
		return
	}
	if req.JSONRPC != "2.0" {
		writeRPCProbeError(w, nil, http.StatusBadRequest, -32600, "Invalid request")
		return
	}
	if !validRPCID(req.ID) {
		writeRPCProbeError(w, nil, http.StatusBadRequest, -32600, "Invalid request")
		return
	}
	if req.Method == "" {
		writeRPCProbeError(w, req.ID, http.StatusBadRequest, -32600, "Invalid request")
		return
	}
	if req.isNotification() {
		if _, ok := supportedRPCMethods[req.Method]; ok {
			rpcServer.executeRPCNotification(ctx, req)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if _, ok := supportedRPCMethods[req.Method]; ok {
		rpcServer.serveRPCRequestWithID(ctx, w, req)
		return
	}

	writeRPCProbeError(w, req.ID, http.StatusInternalServerError, -32601, fmt.Sprintf("method '%s' not found", req.Method))
}

func scanRPCBatch(body []byte) ([]rpcMethodProbe, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return nil, errors.New("invalid RPC batch")
	}
	reqs := make([]rpcMethodProbe, 0, maxRPCBatchRequests)
	for decoder.More() {
		if len(reqs) == maxRPCBatchRequests {
			return nil, errRPCBatchTooLarge
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, errors.New("invalid RPC batch")
		}
		var req rpcMethodProbe
		if err := json.Unmarshal(raw, &req); err != nil {
			req.invalid = true
		}
		reqs = append(reqs, req)
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim(']') {
		return nil, errors.New("invalid RPC batch")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("invalid RPC batch")
	}
	return reqs, nil
}

func isExpensiveRPCMethod(method string) bool {
	return method == "sendTransaction" || method == "simulateTransaction"
}

func (req rpcMethodProbe) isNotification() bool {
	return len(req.ID) == 0 && req.Method != ""
}

func validRPCID(id json.RawMessage) bool {
	if len(id) == 0 {
		return true
	}
	decoder := json.NewDecoder(bytes.NewReader(id))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return false
	}
	switch value.(type) {
	case nil, string, json.Number:
		return true
	default:
		return false
	}
}

func (rpcServer *RpcServer) handleRPCBatch(ctx context.Context, w http.ResponseWriter, body []byte, local bool) {
	reqs, err := scanRPCBatch(body)
	if errors.Is(err, errRPCBatchTooLarge) {
		writeRPCProbeError(
			w,
			nil,
			http.StatusRequestEntityTooLarge,
			-32600,
			fmt.Sprintf("batch exceeds maximum %d requests", maxRPCBatchRequests),
		)
		return
	}
	if err != nil {
		writeRPCProbeError(w, nil, http.StatusInternalServerError, -32700, "Parse error")
		return
	}
	if len(reqs) == 0 {
		writeRPCProbeError(w, nil, http.StatusBadRequest, -32600, "Invalid request")
		return
	}
	if !rpcServer.allowRequests(local, len(reqs)-1) {
		writeRPCRateError(w)
		return
	}
	rpcServer.serveRPCBatch(ctx, w, reqs)
}

func (rpcServer *RpcServer) serveRPCBatch(
	ctx context.Context,
	w http.ResponseWriter,
	reqs []rpcMethodProbe,
) {
	responses := make([]json.RawMessage, 0, len(reqs))
	for _, req := range reqs {
		if req.invalid {
			responses = append(responses, marshalRPCProbeError(nil, -32600, "Invalid request"))
			continue
		}
		if req.JSONRPC != "2.0" {
			responses = append(responses, marshalRPCProbeError(nil, -32600, "Invalid request"))
			continue
		}
		if !validRPCID(req.ID) {
			responses = append(responses, marshalRPCProbeError(nil, -32600, "Invalid request"))
			continue
		}
		if len(reqs) > 1 && isExpensiveRPCMethod(req.Method) {
			if !req.isNotification() {
				responses = append(responses, marshalRPCProbeError(
					req.ID,
					-32600,
					fmt.Sprintf("%s does not support batch requests", req.Method),
				))
			}
			continue
		}
		if req.isNotification() {
			if _, supported := supportedRPCMethods[req.Method]; supported {
				rpcServer.executeRPCNotification(ctx, req)
			}
			continue
		}

		if req.Method == "" {
			responses = append(responses, marshalRPCProbeError(req.ID, -32600, "Invalid request"))
			continue
		}
		if _, supported := supportedRPCMethods[req.Method]; !supported {
			responses = append(responses, marshalRPCProbeError(
				req.ID,
				-32601,
				fmt.Sprintf("method '%s' not found", req.Method),
			))
			continue
		}

		payload, err := rpcServer.executeRPCRequestWithID(ctx, req)
		if err != nil {
			writeRPCProbeError(w, nil, http.StatusInternalServerError, -32603, "Internal error")
			return
		}
		responses = append(responses, payload)
	}

	if len(responses) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(responses)
}

func (rpcServer *RpcServer) serveRPCRequestWithID(ctx context.Context, w http.ResponseWriter, req rpcMethodProbe) {
	payload, err := rpcServer.executeRPCRequestWithID(ctx, req)
	if err != nil {
		writeRPCProbeError(w, nil, http.StatusInternalServerError, -32603, "Internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(payload)
}

func (rpcServer *RpcServer) executeRPCRequestWithID(ctx context.Context, req rpcMethodProbe) (json.RawMessage, error) {
	if reason, verified, eligible := rpcServer.evidenceGate(req.Method); reason != evidenceGateOpen {
		return marshalNodeUnhealthyError(req.ID, reason, verified, eligible), nil
	}
	raw, err := rpcRequestWithTemporaryID(req)
	if err != nil {
		return nil, err
	}
	var response bytes.Buffer
	rpcServer.rpcService.HandleRequest(ctx, bytes.NewReader(raw), &response)
	payload := bytes.TrimSpace(response.Bytes())
	payload, err = rpcResponseWithID(payload, req.ID)
	if err != nil || !json.Valid(payload) {
		return nil, errors.New("invalid RPC response")
	}
	return append(json.RawMessage(nil), payload...), nil
}

// executeRPCNotification runs a request that produces no response. The gate
// still applies: a notification cannot be told the node refused, but
// sendTransaction arrives here too, and an unhealthy node must not submit
// merely because the caller omitted an id.
func (rpcServer *RpcServer) executeRPCNotification(ctx context.Context, req rpcMethodProbe) {
	if reason, _, _ := rpcServer.evidenceGate(req.Method); reason != evidenceGateOpen {
		return
	}
	raw, err := rpcRequestForDispatch(req, nil)
	if err == nil {
		rpcServer.rpcService.HandleRequest(ctx, bytes.NewReader(raw), io.Discard)
	}
}

func marshalRPCProbeError(id json.RawMessage, code int, message string) json.RawMessage {
	payload, _ := json.Marshal(rpcProbeErrorResponse{
		JSONRPC: "2.0",
		ID:      normalizedRPCProbeID(id),
		Error: rpcProbeError{
			Code:    code,
			Message: message,
		},
	})
	return payload
}

func rpcRequestWithTemporaryID(req rpcMethodProbe) (json.RawMessage, error) {
	return rpcRequestForDispatch(req, json.RawMessage("0"))
}

func rpcRequestForDispatch(req rpcMethodProbe, id json.RawMessage) (json.RawMessage, error) {
	request := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id,omitempty"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}{
		JSONRPC: req.JSONRPC,
		ID:      id,
		Method:  req.Method,
		Params:  req.Params,
	}
	return json.Marshal(request)
}

func rpcResponseWithID(raw, id json.RawMessage) (json.RawMessage, error) {
	var response map[string]json.RawMessage
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, err
	}
	response["id"] = normalizedRPCProbeID(id)
	payload, err := json.Marshal(response)
	if err != nil {
		return nil, err
	}
	return payload, nil
}

func writeRPCRateError(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	http.Error(w, "request rate exceeded", http.StatusTooManyRequests)
}

func writeRPCProbeError(w http.ResponseWriter, id json.RawMessage, status int, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(marshalRPCProbeError(id, code, message), '\n'))
}

func normalizedRPCProbeID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func readRPCRequestBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.Body == nil {
		r.Body = http.NoBody
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRPCRequestBytes+1))
	if err != nil {
		writeRPCProbeError(w, nil, http.StatusBadRequest, -32700, "unable to read request body")
		return nil, false
	}
	if len(body) > maxRPCRequestBytes {
		writeRPCProbeError(w, nil, http.StatusRequestEntityTooLarge, -32700, "request body exceeds 1 MiB")
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	return body, true
}

func (rpcServer *RpcServer) startClusterNodesRefreshLoop() {
	rpcServer.clusterNodesRefreshOnce.Do(func() {
		if rpcServer.clusterNodesFetcher == nil && len(rpcServer.clusterRPCEndpoints) == 0 {
			return
		}

		rpcServer.clusterNodesRefreshMu.Lock()
		if rpcServer.clusterNodesRefreshStopped {
			rpcServer.clusterNodesRefreshMu.Unlock()
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		rpcServer.clusterNodesRefreshCancel = cancel
		rpcServer.clusterNodesRefreshDone = done
		rpcServer.clusterNodesRefreshMu.Unlock()

		go rpcServer.runClusterNodesRefreshLoop(ctx, done)
	})
}

func (rpcServer *RpcServer) runClusterNodesRefreshLoop(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	if err := rpcServer.refreshLeaderTPUCacheBounded(ctx); err != nil && ctx.Err() == nil {
		mlog.Log.Warnf("sendTransaction: initial cluster node refresh failed: %v", err)
	}

	ticker := time.NewTicker(rpcServer.clusterNodesRefreshInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := rpcServer.refreshLeaderTPUCacheBounded(ctx); err != nil && ctx.Err() == nil {
				mlog.Log.Warnf("sendTransaction: periodic cluster node refresh failed: %v", err)
			}
		}
	}
}

func (rpcServer *RpcServer) refreshLeaderTPUCacheBounded(ctx context.Context) error {
	timeout := rpcServer.clusterNodesRefreshTimeout
	if timeout <= 0 {
		timeout = defaultClusterNodesRefreshTimeout
	}
	refreshCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return rpcServer.refreshLeaderTPUCache(refreshCtx)
}

func (rpcServer *RpcServer) stopClusterNodesRefresh(ctx context.Context) error {
	rpcServer.clusterNodesRefreshMu.Lock()
	rpcServer.clusterNodesRefreshStopped = true
	cancel := rpcServer.clusterNodesRefreshCancel
	done := rpcServer.clusterNodesRefreshDone
	rpcServer.clusterNodesRefreshMu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (rpcServer *RpcServer) clusterNodesRefreshInterval() time.Duration {
	if rpcServer.clusterNodesRefreshEvery > 0 {
		return rpcServer.clusterNodesRefreshEvery
	}
	return sendTransactionClusterNodesRefreshEvery
}
