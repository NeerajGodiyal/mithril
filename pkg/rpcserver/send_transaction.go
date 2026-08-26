package rpcserver

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/config"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/replay"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/filecoin-project/go-jsonrpc"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	solanarpc "github.com/gagliardetto/solana-go/rpc"
	"github.com/mr-tron/base58"
)

type clusterNodesFetcher func(context.Context) ([]*solanarpc.GetClusterNodesResult, error)
type transactionSender func(context.Context, []byte, tpuEndpoint) error

type tpuTransport string

const (
	tpuTransportUDP  tpuTransport = "udp"
	tpuTransportQUIC tpuTransport = "quic"
)

type tpuEndpoint struct {
	Addr      netip.AddrPort
	Transport tpuTransport
}

func (endpoint tpuEndpoint) String() string {
	return fmt.Sprintf("%s/%s", endpoint.Addr.String(), endpoint.Transport)
}

type sendTransactionConfig struct {
	encoding       string
	skipPreflight  bool
	minContextSlot *uint64
}

const (
	maxBase58TxSize                         = 1683
	packetDataSize                          = 1232
	v1PacketDataSize                        = solana.MaxTransactionSizeV1
	maxBase64TxSize                         = (v1PacketDataSize + 2) / 3 * 4
	sendTransactionLeaderForwardCount       = 10
	sendTransactionTargetCount              = sendTransactionLeaderForwardCount + 1
	sendTransactionLeaderLookahead          = 64
	sendTransactionClusterNodesRefreshEvery = 10 * time.Minute
	sendTransactionLeaderRefreshTimeout     = 2 * time.Second
	sendTransactionTPUSendTimeout           = 3 * time.Second
	maxSanitizedInstructionCount            = 64
)

var errInvalidSanitizedTransaction = &InvalidParamsError{
	Message: "invalid transaction: Transaction failed to sanitize accounts offsets correctly",
}

func (rpcServer *RpcServer) SendTransaction(ctx context.Context, p jsonrpc.RawParams) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	params, err := jsonrpc.DecodeParams[[]interface{}](p)
	if err != nil {
		return "", &InvalidParamsError{Message: fmt.Sprintf("decoding params: %v", err)}
	}
	if len(params) < 1 {
		return "", &InvalidParamsError{Message: "sendTransaction requires a transaction string as first parameter"}
	}

	txStr, ok := params[0].(string)
	if !ok {
		return "", &InvalidParamsError{Message: "sendTransaction requires a transaction string as first parameter"}
	}

	conf, err := parseSendTransactionConfig(params)
	if err != nil {
		return "", err
	}

	tx, wire, err := decodeSendTransaction(txStr, conf.encoding)
	if err != nil {
		return "", err
	}

	slotCtx := rpcServer.getSlotCtx()
	// skipPreflight still performs bank-independent structural sanitization.
	// Feature-gated execution remains the bank's job, matching Agave's RPC.
	if err := validateSendTransactionSanitize(tx, featuresForSendValidation(slotCtx)); err != nil {
		return "", err
	}

	if conf.minContextSlot != nil {
		if slotCtx == nil || slotCtx.Slot < *conf.minContextSlot {
			return "", &MinContextSlotNotReachedError{ContextSlot: *conf.minContextSlot}
		}
	}

	if !conf.skipPreflight {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if slotCtx == nil {
			return "", fmt.Errorf("node is not ready for transaction preflight")
		}
		if err := rpcServer.preflightSendTransaction(ctx, tx, slotCtx); err != nil {
			return "", err
		}
	}

	signature, err := firstSignature(tx)
	if err != nil {
		return "", err
	}

	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Record the attempt immediately before forwarding. If forwarding fails
	// partway the transaction may still have reached a leader, so that outcome
	// remains explicitly ambiguous until replay supplies evidence.
	if err := rpcServer.recordSubmissionAttempt(tx, signature); err != nil {
		return "", err
	}
	if err := rpcServer.forwardTransactionToUpcomingLeaders(ctx, wire, tx.Message.GetVersion() == solana.MessageVersionV1); err != nil {
		return "", err
	}
	rpcServer.recordSubmissionForwarded(signature)

	return signature.String(), nil
}

func parseSendTransactionConfig(params []interface{}) (sendTransactionConfig, error) {
	conf := sendTransactionConfig{
		encoding: "base58",
	}
	if len(params) > 2 {
		return conf, &InvalidParamsError{Message: "sendTransaction accepts at most two parameters"}
	}
	if len(params) < 2 {
		return conf, nil
	}

	confMap, ok := params[1].(map[string]interface{})
	if !ok {
		return conf, &InvalidParamsError{Message: "sendTransaction config must be an object"}
	}

	if value, exists := confMap["encoding"]; exists {
		encoding, ok := value.(string)
		if !ok {
			return conf, invalidRPCOption("sendTransaction", "encoding", "must be a string")
		}
		conf.encoding = encoding
	}
	if value, exists := confMap["skipPreflight"]; exists {
		skipPreflight, ok := value.(bool)
		if !ok {
			return conf, invalidRPCOption("sendTransaction", "skipPreflight", "must be a boolean")
		}
		conf.skipPreflight = skipPreflight
	}
	if value, exists := confMap["preflightCommitment"]; exists {
		preflightCommitment, ok := value.(string)
		if !ok {
			return conf, invalidRPCOption("sendTransaction", "preflightCommitment", "must be a string")
		}
		if preflightCommitment != "processed" {
			return conf, invalidRPCOption("sendTransaction", "preflightCommitment", `only "processed" is supported`)
		}
	}
	if value, exists := confMap["maxRetries"]; exists {
		maxRetries, err := parseExactJSONUint(value, "sendTransaction", "maxRetries")
		if err != nil {
			return conf, err
		}
		if maxRetries != 0 {
			return conf, invalidRPCOption("sendTransaction", "maxRetries", "only 0 is supported")
		}
	}
	if value, exists := confMap["minContextSlot"]; exists {
		minContextSlot, err := parseExactJSONUint(value, "sendTransaction", "minContextSlot")
		if err != nil {
			return conf, err
		}
		conf.minContextSlot = &minContextSlot
	}

	return conf, nil
}

func parseExactJSONUint(value interface{}, method, name string) (uint64, error) {
	number, ok := value.(float64)
	if !ok || number < 0 || number != math.Trunc(number) || number > 1<<53-1 {
		return 0, invalidRPCOption(method, name, "must be a non-negative exact integer")
	}
	return uint64(number), nil
}

func invalidRPCOption(method, name, reason string) error {
	return &InvalidParamsError{Message: fmt.Sprintf("invalid %s %s: %s", method, name, reason)}
}

func decodeSendTransaction(txStr string, encoding string) (*solana.Transaction, []byte, error) {
	if encoding != "base58" && encoding != "base64" {
		return nil, nil, &InvalidParamsError{
			Message: fmt.Sprintf("unsupported encoding: %s. Supported encodings: base58, base64", encoding),
		}
	}

	if encoding == "base58" && len(txStr) > maxBase58TxSize {
		return nil, nil, &InvalidParamsError{
			Message: fmt.Sprintf("base58 encoded solana_transaction too large: %d bytes (max: encoded/raw %d/%d)", len(txStr), maxBase58TxSize, packetDataSize),
		}
	}
	if encoding == "base64" && len(txStr) > maxBase64TxSize {
		return nil, nil, &InvalidParamsError{
			Message: fmt.Sprintf("base64 encoded solana_transaction too large: %d bytes (max: encoded/raw %d/%d)", len(txStr), maxBase64TxSize, v1PacketDataSize),
		}
	}

	var (
		wire []byte
		err  error
	)
	switch encoding {
	case "base58":
		wire, err = base58.Decode(txStr)
		if err != nil {
			return nil, nil, &InvalidParamsError{Message: fmt.Sprintf("invalid base58 encoding: %v", err)}
		}
	default:
		wire, err = base64.StdEncoding.DecodeString(txStr)
		if err != nil {
			return nil, nil, &InvalidParamsError{Message: fmt.Sprintf("invalid base64 encoding: %v", err)}
		}
	}

	rawLimit := packetDataSize
	if encoding == "base64" {
		rawLimit = v1PacketDataSize
	}
	if len(wire) > rawLimit {
		return nil, nil, &InvalidParamsError{
			Message: fmt.Sprintf("decoded solana_transaction too large: %d bytes (max: %d bytes)", len(wire), rawLimit),
		}
	}

	decoder := bin.NewBinDecoder(wire)
	tx, err := solana.TransactionFromDecoder(decoder)
	if err != nil {
		return nil, nil, &InvalidParamsError{Message: fmt.Sprintf("failed to deserialize solana_transaction: %v", err)}
	}
	if tx.Message.GetVersion() != solana.MessageVersionV1 && len(wire) > packetDataSize {
		return nil, nil, &InvalidParamsError{
			Message: fmt.Sprintf("decoded solana_transaction too large: %d bytes (max: %d bytes)", len(wire), packetDataSize),
		}
	}
	// Agave's RPC decoder is lenient, but TPU rejects trailing v1 bytes. Refuse
	// them here rather than returning a receipt for a packet no leader accepts.
	if tx.Message.GetVersion() == solana.MessageVersionV1 && decoder.HasRemaining() {
		return nil, nil, &InvalidParamsError{Message: "failed to deserialize solana_transaction: trailing bytes after transaction v1"}
	}

	return tx, wire, nil
}

func featuresForSendValidation(slotCtx *sealevel.SlotCtx) *features.Features {
	if slotCtx != nil && slotCtx.Features != nil {
		return slotCtx.Features
	}
	return nil
}

func validateSendTransactionSanitize(tx *solana.Transaction, feats *features.Features) error {
	if err := replay.ValidateTransactionShape(tx, nil); err != nil {
		return errInvalidSanitizedTransaction
	}
	if feats != nil &&
		feats.IsActive(features.StaticInstructionLimit) &&
		len(tx.Message.Instructions) > maxSanitizedInstructionCount {
		return errInvalidSanitizedTransaction
	}
	return nil
}

func firstSignature(tx *solana.Transaction) (solana.Signature, error) {
	if len(tx.Signatures) == 0 {
		return solana.Signature{}, errInvalidSanitizedTransaction
	}
	return tx.Signatures[0], nil
}

func (rpcServer *RpcServer) preflightSendTransaction(ctx context.Context, tx *solana.Transaction, slotCtx *sealevel.SlotCtx) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := rpcServer.resolveAddressTablesForPreflight(ctx, tx, slotCtx); err != nil {
		return err
	}

	if err := validateSendTransactionSanitize(tx, slotCtx.Features); err != nil {
		return err
	}

	if err := tx.VerifySignatures(); err != nil {
		return signaturePreflightFailure()
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	output := replay.LoadAndExecuteTransaction(replay.LoadAndExecuteTransactionInput{
		SlotCtx:                 slotCtx,
		Transaction:             tx,
		TxMeta:                  nil,
		IsSimulation:            true,
		RecordInnerInstructions: false,
	})
	if err := ctx.Err(); err != nil {
		return err
	}

	if output.ProcessingResult.TransactionError == nil {
		return nil
	}
	if output.ProcessingResult.TransactionError.ErrorType == replay.TransactionErrorSanitizeFailure {
		return errInvalidSanitizedTransaction
	}

	txErr := output.ProcessingResult.TransactionError
	return &SendTransactionPreflightFailureError{
		Message: fmt.Sprintf("Transaction simulation failed: %s", sendTransactionErrorMessage(txErr)),
		Result:  sendTransactionFailureResultFromOutput(output),
	}
}

func (rpcServer *RpcServer) resolveAddressTablesForPreflight(ctx context.Context, tx *solana.Transaction, slotCtx *sealevel.SlotCtx) error {
	if !tx.Message.IsVersioned() || tx.Message.AddressTableLookups.NumLookups() == 0 {
		return nil
	}
	if rpcServer.acctsDb == nil {
		return &InvalidParamsError{Message: "invalid transaction: address lookup table resolution unavailable"}
	}
	if err := replay.ResolveAddrTableLookupsForTx(ctx, rpcServer.acctsDb, slotCtx.Slot, tx); err != nil {
		return &InvalidParamsError{Message: fmt.Sprintf("invalid transaction: %s", err.Error())}
	}
	return nil
}

func signaturePreflightFailure() *SendTransactionPreflightFailureError {
	return &SendTransactionPreflightFailureError{
		Message: "Transaction simulation failed: Transaction did not pass signature verification",
		Result: sendTransactionFailureResult(
			"SignatureFailure",
			[]string{},
			0,
			0,
			nil,
		),
	}
}

func sendTransactionFailureResultFromOutput(output replay.LoadAndExecuteTransactionOutput) SimulateTransactionRespValue {
	logs := []string{}
	if output.ExecCtx != nil {
		if logRecorder, ok := output.ExecCtx.Log.(*sealevel.LogRecorder); ok && logRecorder != nil && logRecorder.Logs != nil {
			logs = clampLogs(logRecorder.Logs)
		}
	}

	var (
		unitsConsumed uint64
		dataSize      uint32
		returnData    *ReturnDataPayload
	)

	if processedTx := output.ProcessingResult.ProcessedTransaction; processedTx != nil && processedTx.Executed != nil {
		executed := processedTx.Executed
		unitsConsumed = executed.ExecutionDetails.ExecutedUnits
		dataSize = executed.LoadedTransaction.LoadedAccountsDataSize
		if executed.ExecutionDetails.ReturnData != nil {
			rd := executed.ExecutionDetails.ReturnData
			clamped := clampReturnData(rd.Data)
			returnData = &ReturnDataPayload{
				ProgramId: rd.ProgramId.String(),
				Data:      []string{base64.StdEncoding.EncodeToString(clamped), "base64"},
			}
		}
	} else if output.ExecCtx != nil {
		unitsConsumed = output.ExecCtx.ComputeMeter.Used()
		dataSize = loadedAccountsDataSizeFromExecCtx(output.ExecCtx)
	}

	return sendTransactionFailureResult(
		output.ProcessingResult.TransactionError,
		logs,
		unitsConsumed,
		dataSize,
		returnData,
	).withFee(output)
}

func sendTransactionFailureResult(errValue interface{}, logs []string, unitsConsumed uint64, dataSize uint32, returnData *ReturnDataPayload) SimulateTransactionRespValue {
	return SimulateTransactionRespValue{
		Err:                    errValue,
		Logs:                   ptrSlice(logs),
		UnitsConsumed:          &unitsConsumed,
		ReturnData:             returnData,
		InnerInstructions:      nil,
		LoadedAccountsDataSize: &dataSize,
		Fee:                    nil,
		PreBalances:            nil,
		PostBalances:           nil,
		PreTokenBalances:       nil,
		PostTokenBalances:      nil,
		LoadedAddresses:        nil,
	}
}

func (v SimulateTransactionRespValue) withFee(output replay.LoadAndExecuteTransactionOutput) SimulateTransactionRespValue {
	if output.FeeInfo != nil {
		fee := output.FeeInfo.TotalFee
		v.Fee = &fee
	}
	return v
}

func sendTransactionErrorMessage(txErr *replay.TransactionError) string {
	if txErr == nil {
		return "unknown error"
	}

	switch txErr.ErrorType {
	case replay.TransactionErrorBlockhashNotFound:
		return "Blockhash not found"
	case replay.TransactionErrorSignatureFailure:
		return "Transaction did not pass signature verification"
	case replay.TransactionErrorSanitizeFailure:
		return "Transaction failed to sanitize accounts offsets correctly"
	case replay.TransactionErrorInstructionError:
		if txErr.InstructionError != nil {
			return normalizeSendTransactionErrorName(txErr.InstructionError.Error())
		}
	}

	return normalizeSendTransactionErrorName(txErr.ErrorType.String())
}

func normalizeSendTransactionErrorName(name string) string {
	switch {
	case strings.HasPrefix(name, "InstrErr"):
		return strings.TrimPrefix(name, "InstrErr")
	case strings.HasPrefix(name, "TxErr"):
		return strings.TrimPrefix(name, "TxErr")
	default:
		return name
	}
}

func (rpcServer *RpcServer) forwardTransactionToUpcomingLeaders(ctx context.Context, wire []byte, requireQUIC bool) error {
	targetCount := int(rpcServer.sendTransactionLeaderForwardCount) + 1
	if targetCount <= 1 {
		targetCount = sendTransactionTargetCount
	}

	targets, err := rpcServer.resolveUpcomingLeaderTPUEndpointsForTransport(ctx, targetCount, requireQUIC)
	if err != nil {
		return err
	}

	send := rpcServer.transactionSender
	if send == nil {
		send = defaultTransactionSender
	}

	var sendErrs []error
	sentCount := 0
	var sendMu sync.Mutex
	var sendWg sync.WaitGroup
	for _, target := range targets {
		target := target
		sendWg.Add(1)
		go func() {
			defer sendWg.Done()
			sendCtx, cancel := context.WithTimeout(ctx, sendTransactionTPUSendTimeout)
			defer cancel()

			err := send(sendCtx, wire, target)
			sendMu.Lock()
			defer sendMu.Unlock()
			if err != nil {
				sendErrs = append(sendErrs, fmt.Errorf("%s: %w", target.String(), err))
				return
			}
			sentCount++
		}()
	}
	sendWg.Wait()

	if sentCount == 0 {
		return fmt.Errorf("failed to forward transaction to any leader TPU: %w", errors.Join(sendErrs...))
	}
	if len(sendErrs) > 0 {
		mlog.Log.Warnf("sendTransaction: forwarded to %d/%d leader TPUs; partial failures: %v", sentCount, len(targets), errors.Join(sendErrs...))
	}
	return nil
}

func (rpcServer *RpcServer) resolveUpcomingLeaderTPUEndpoints(ctx context.Context, want int) ([]tpuEndpoint, error) {
	return rpcServer.resolveUpcomingLeaderTPUEndpointsForTransport(ctx, want, false)
}

func (rpcServer *RpcServer) resolveUpcomingLeaderTPUEndpointsForTransport(ctx context.Context, want int, requireQUIC bool) ([]tpuEndpoint, error) {
	if want <= 0 {
		want = 1
	}

	targets, updatedAt := rpcServer.collectUpcomingLeaderTPUEndpointsFromCache(want, requireQUIC)
	cacheStale := updatedAt.IsZero() || time.Since(updatedAt) >= rpcServer.clusterNodesRefreshInterval()
	if cacheStale || len(targets) < want {
		if err := rpcServer.refreshLeaderTPUCacheForSend(ctx); err != nil {
			if len(targets) == 0 {
				return nil, err
			}
			mlog.Log.Warnf("sendTransaction: using partial cached TPU target set after refresh failure: %v", err)
			return targets, nil
		}
		targets, _ = rpcServer.collectUpcomingLeaderTPUEndpointsFromCache(want, requireQUIC)
	}

	if len(targets) == 0 {
		return nil, fmt.Errorf("unable to resolve TPU addresses for upcoming leaders from current leader schedule")
	}
	return targets, nil
}

func (rpcServer *RpcServer) refreshLeaderTPUCacheForSend(ctx context.Context) error {
	timeout := sendTransactionLeaderRefreshTimeout
	if configured := rpcServer.clusterNodesRefreshTimeout; configured > 0 && configured < timeout {
		timeout = configured
	}
	refreshCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return rpcServer.refreshLeaderTPUCache(refreshCtx)
}

func (rpcServer *RpcServer) fetchClusterNodes(ctx context.Context) ([]*solanarpc.GetClusterNodesResult, error) {
	if rpcServer.clusterNodesFetcher != nil {
		return rpcServer.clusterNodesFetcher(ctx)
	}

	endpoints := rpcServer.clusterRPCEndpoints
	if len(endpoints) == 0 {
		endpoints = configuredSendTransactionRPCEndpoints()
	}
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("no cluster RPC endpoints configured for sendTransaction leader resolution")
	}

	var lastErr error
	for _, endpoint := range endpoints {
		client := solanarpc.New(endpoint)
		nodes, err := client.GetClusterNodes(ctx)
		if err == nil {
			return nodes, nil
		}
		lastErr = err
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("unable to fetch cluster nodes")
	}
	return nil, lastErr
}

func (rpcServer *RpcServer) refreshLeaderTPUCache(ctx context.Context) error {
	nodes, err := rpcServer.fetchClusterNodes(ctx)
	if err != nil {
		return err
	}

	leaderTPUs := make(map[solana.PublicKey]tpuEndpoint, len(nodes))
	for _, node := range nodes {
		if node == nil {
			continue
		}

		endpoint, ok := leaderTPUEndpointFromClusterNode(node)
		if !ok {
			continue
		}
		leaderTPUs[node.Pubkey] = endpoint
	}

	if len(leaderTPUs) == 0 {
		return fmt.Errorf("cluster did not advertise any TPU endpoints")
	}

	rpcServer.leaderTPUCacheMu.Lock()
	rpcServer.leaderTPUByIdentity = leaderTPUs
	rpcServer.leaderTPUCacheUpdatedAt = time.Now()
	rpcServer.leaderTPUCacheMu.Unlock()
	return nil
}

func leaderTPUEndpointFromClusterNode(node *solanarpc.GetClusterNodesResult) (tpuEndpoint, bool) {
	if endpoint, ok := parseLeaderTPUEndpoint(node.TPUQUIC, tpuTransportQUIC); ok {
		return endpoint, true
	}
	return parseLeaderTPUEndpoint(node.TPU, tpuTransportUDP)
}

func parseLeaderTPUEndpoint(raw *string, transport tpuTransport) (tpuEndpoint, bool) {
	if raw == nil || *raw == "" {
		return tpuEndpoint{}, false
	}
	addr, err := netip.ParseAddrPort(*raw)
	if err != nil {
		return tpuEndpoint{}, false
	}
	return tpuEndpoint{Addr: addr, Transport: transport}, true
}

func (rpcServer *RpcServer) collectUpcomingLeaderTPUEndpointsFromCache(want int, requireQUIC bool) ([]tpuEndpoint, time.Time) {
	rpcServer.leaderTPUCacheMu.RLock()
	nodeTPUs := make(map[solana.PublicKey]tpuEndpoint, len(rpcServer.leaderTPUByIdentity))
	for leader, endpoint := range rpcServer.leaderTPUByIdentity {
		nodeTPUs[leader] = endpoint
	}
	updatedAt := rpcServer.leaderTPUCacheUpdatedAt
	rpcServer.leaderTPUCacheMu.RUnlock()

	currentSlot := global.Slot()
	targets := make([]tpuEndpoint, 0, want)
	seenLeaders := make(map[solana.PublicKey]struct{}, want)
	seenTargets := make(map[tpuEndpoint]struct{}, want)

	for offset := uint64(0); offset < sendTransactionLeaderLookahead && len(targets) < want; offset++ {
		leader, ok := global.LeaderForSlot(currentSlot + offset)
		if !ok {
			continue
		}
		if _, exists := seenLeaders[leader]; exists {
			continue
		}
		seenLeaders[leader] = struct{}{}

		target, ok := nodeTPUs[leader]
		if !ok || requireQUIC && target.Transport != tpuTransportQUIC {
			continue
		}
		if _, exists := seenTargets[target]; exists {
			continue
		}
		seenTargets[target] = struct{}{}
		targets = append(targets, target)
	}

	return targets, updatedAt
}

func configuredSendTransactionRPCEndpoints() []string {
	endpoints := config.GetStringSlice("network.rpc")
	if len(endpoints) == 0 {
		endpoints = config.GetStringSlice("rpc.rpc")
	}
	return endpoints
}

func defaultTransactionSender(ctx context.Context, payload []byte, target tpuEndpoint) error {
	switch target.Transport {
	case tpuTransportQUIC:
		return defaultTPUQUICSender.Send(ctx, payload, target.Addr)
	case tpuTransportUDP:
		return defaultUDPPacketSender(payload, target.Addr)
	default:
		return fmt.Errorf("unsupported TPU transport %q", target.Transport)
	}
}

func defaultUDPPacketSender(payload []byte, target netip.AddrPort) error {
	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	written, err := conn.WriteToUDPAddrPort(payload, target)
	if err != nil {
		return err
	}
	if written != len(payload) {
		return fmt.Errorf("short write: wrote %d of %d bytes", written, len(payload))
	}
	return nil
}
