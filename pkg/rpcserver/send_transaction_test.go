package rpcserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/leaderschedule"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/txstatus"
	"github.com/filecoin-project/go-jsonrpc"
	"github.com/gagliardetto/solana-go"
	solanarpc "github.com/gagliardetto/solana-go/rpc"
	"github.com/mr-tron/base58"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSendTransactionConfig_Defaults(t *testing.T) {
	conf, err := parseSendTransactionConfig([]interface{}{"tx"})
	require.NoError(t, err)
	assert.Equal(t, "base58", conf.encoding)
	assert.False(t, conf.skipPreflight)
	assert.Nil(t, conf.minContextSlot)
}

func TestParseSendTransactionConfig_ValidatesAdvertisedOptions(t *testing.T) {
	t.Parallel()

	validMinSlot := float64(417_500_000)
	tests := []struct {
		name    string
		params  []interface{}
		wantErr string
		check   func(*testing.T, sendTransactionConfig)
	}{
		{
			name:   "supported options",
			params: []interface{}{"tx", map[string]interface{}{"encoding": "base64", "skipPreflight": true, "preflightCommitment": "processed", "minContextSlot": validMinSlot}},
			check: func(t *testing.T, conf sendTransactionConfig) {
				assert.Equal(t, "base64", conf.encoding)
				assert.True(t, conf.skipPreflight)
				require.NotNil(t, conf.minContextSlot)
				assert.Equal(t, uint64(validMinSlot), *conf.minContextSlot)
			},
		},
		{name: "too many parameters", params: []interface{}{"tx", map[string]interface{}{}, "extra"}, wantErr: "accepts at most two parameters"},
		{name: "config is not object", params: []interface{}{"tx", "base64"}, wantErr: "config must be an object"},
		{name: "encoding type", params: []interface{}{"tx", map[string]interface{}{"encoding": true}}, wantErr: "encoding: must be a string"},
		{name: "skip preflight type", params: []interface{}{"tx", map[string]interface{}{"skipPreflight": "true"}}, wantErr: "skipPreflight: must be a boolean"},
		{name: "commitment type", params: []interface{}{"tx", map[string]interface{}{"preflightCommitment": true}}, wantErr: "preflightCommitment: must be a string"},
		{name: "unsupported commitment", params: []interface{}{"tx", map[string]interface{}{"preflightCommitment": "confirmed"}}, wantErr: `preflightCommitment: only "processed" is supported`},
		{name: "retry type", params: []interface{}{"tx", map[string]interface{}{"maxRetries": "1"}}, wantErr: "maxRetries: must be a non-negative exact integer"},
		{name: "negative retries", params: []interface{}{"tx", map[string]interface{}{"maxRetries": float64(-1)}}, wantErr: "maxRetries: must be a non-negative exact integer"},
		{name: "fractional retries", params: []interface{}{"tx", map[string]interface{}{"maxRetries": 1.5}}, wantErr: "maxRetries: must be a non-negative exact integer"},
		{
			name:   "zero retries",
			params: []interface{}{"tx", map[string]interface{}{"maxRetries": float64(0)}},
			check:  func(t *testing.T, conf sendTransactionConfig) {},
		},
		{name: "nonzero retries unsupported", params: []interface{}{"tx", map[string]interface{}{"maxRetries": float64(1)}}, wantErr: "maxRetries: only 0 is supported"},
		{name: "slot type", params: []interface{}{"tx", map[string]interface{}{"minContextSlot": "1"}}, wantErr: "minContextSlot: must be a non-negative exact integer"},
		{name: "negative slot", params: []interface{}{"tx", map[string]interface{}{"minContextSlot": float64(-1)}}, wantErr: "minContextSlot: must be a non-negative exact integer"},
		{name: "fractional slot", params: []interface{}{"tx", map[string]interface{}{"minContextSlot": 1.5}}, wantErr: "minContextSlot: must be a non-negative exact integer"},
		{name: "inexact slot", params: []interface{}{"tx", map[string]interface{}{"minContextSlot": float64(1 << 53)}}, wantErr: "minContextSlot: must be a non-negative exact integer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			conf, err := parseSendTransactionConfig(tt.params)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				var invalidParams *InvalidParamsError
				assert.ErrorAs(t, err, &invalidParams)
				return
			}

			require.NoError(t, err)
			tt.check(t, conf)
		})
	}
}

func TestSendTransaction_RejectsSanitizeFailure(t *testing.T) {
	rpcServer := &RpcServer{}
	tx := &solana.Transaction{
		Message: solana.Message{
			Header:      solana.MessageHeader{NumRequiredSignatures: 1, NumReadonlySignedAccounts: 1},
			AccountKeys: []solana.PublicKey{{1}},
		},
	}
	wire, err := tx.MarshalBinary()
	require.NoError(t, err)

	_, err = rpcServer.SendTransaction(
		context.Background(),
		mustRawParams(t, []interface{}{
			base64.StdEncoding.EncodeToString(wire),
			map[string]interface{}{"encoding": "base64", "skipPreflight": true},
		}),
	)
	require.Error(t, err)

	invalidParams, ok := err.(*InvalidParamsError)
	require.True(t, ok)
	assert.Equal(t, "invalid transaction: Transaction failed to sanitize accounts offsets correctly", invalidParams.Message)
}

func TestDecodeSendTransactionSupportsLargeV1OnlyAsBase64(t *testing.T) {
	wire := testV1TransactionWire(t, packetDataSize+1)
	tx, gotWire, err := decodeSendTransaction(base64.StdEncoding.EncodeToString(wire), "base64")
	require.NoError(t, err)
	require.Equal(t, solana.MessageVersionV1, tx.Message.GetVersion())
	require.Equal(t, wire, gotWire)

	_, _, err = decodeSendTransaction(strings.Repeat("1", maxBase58TxSize+1), "base58")
	require.ErrorContains(t, err, "base58 encoded solana_transaction too large")
}

func TestDecodeSendTransactionV1SizeBoundaries(t *testing.T) {
	maxWire := testV1TransactionWire(t, v1PacketDataSize)
	_, _, err := decodeSendTransaction(base64.StdEncoding.EncodeToString(maxWire), "base64")
	require.NoError(t, err)

	overWire := testV1TransactionWire(t, v1PacketDataSize+1)
	_, _, err = decodeSendTransaction(base64.StdEncoding.EncodeToString(overWire), "base64")
	require.ErrorContains(t, err, "decoded solana_transaction too large")

	smallWire := testV1TransactionWire(t, 300)
	_, _, err = decodeSendTransaction(base58.Encode(smallWire), "base58")
	require.NoError(t, err)

	legacyWire := testTransactionWire(t, packetDataSize+1, solana.MessageVersionLegacy)
	_, _, err = decodeSendTransaction(base64.StdEncoding.EncodeToString(legacyWire), "base64")
	require.ErrorContains(t, err, "decoded solana_transaction too large")
}

func TestDecodeSendTransactionRejectsTrailingV1Bytes(t *testing.T) {
	wire := append(testV1TransactionWire(t, packetDataSize+1), 0)
	_, _, err := decodeSendTransaction(base64.StdEncoding.EncodeToString(wire), "base64")
	require.ErrorContains(t, err, "trailing bytes after transaction v1")
}

func TestSendTransactionSkipPreflightForwardsInactiveV1(t *testing.T) {
	wire := testV1TransactionWire(t, packetDataSize+1)
	leader := solana.PublicKey{0x41}
	global.SetLeaderSchedule(leaderschedule.NewLeaderScheduleFromKeyedSlots(
		map[solana.PublicKey][]uint64{leader: {0}}, 1,
	))
	global.SetSlot(0)
	defer global.SetLeaderSchedule(nil)
	defer global.SetSlot(0)
	sent := false
	receipts, err := txstatus.NewIndex(txstatus.Config{MaxReceipts: 8, Retention: time.Hour})
	require.NoError(t, err)
	rpcServer := &RpcServer{
		slotCtx:                           &sealevel.SlotCtx{Features: features.NewFeaturesDefault()},
		sendTransactionLeaderForwardCount: 0,
		clusterNodesFetcher: func(context.Context) ([]*solanarpc.GetClusterNodesResult, error) {
			return []*solanarpc.GetClusterNodesResult{{
				Pubkey:  leader,
				TPUQUIC: stringPtr("127.0.0.1:10401"),
			}}, nil
		},
		transactionSender: func(context.Context, []byte, tpuEndpoint) error {
			sent = true
			return nil
		},
	}
	rpcServer.SetTransactionReceipts(receipts)
	_, err = rpcServer.SendTransaction(context.Background(), mustRawParams(t, []interface{}{
		base64.StdEncoding.EncodeToString(wire),
		map[string]interface{}{"encoding": "base64", "skipPreflight": true},
	}))
	require.NoError(t, err)
	require.True(t, sent)
}

func TestSendTransactionRejectsMalformedV1BeforeForwarding(t *testing.T) {
	tx, err := solana.TransactionFromBytes(testV1TransactionWire(t, 300))
	require.NoError(t, err)
	tx.Message.AccountKeys = append(tx.Message.AccountKeys, tx.Message.AccountKeys[0])
	wire, err := tx.MarshalBinary()
	require.NoError(t, err)
	active := features.NewFeaturesDefault()
	active.EnableFeature(features.EnableTransactionV1, 0)
	sent := false
	rpcServer := &RpcServer{
		slotCtx: &sealevel.SlotCtx{Features: active},
		transactionSender: func(context.Context, []byte, tpuEndpoint) error {
			sent = true
			return nil
		},
	}
	_, err = rpcServer.SendTransaction(context.Background(), mustRawParams(t, []interface{}{
		base64.StdEncoding.EncodeToString(wire),
		map[string]interface{}{"encoding": "base64", "skipPreflight": true},
	}))
	require.ErrorContains(t, err, "failed to sanitize")
	require.False(t, sent)
}

func TestSendTransaction_PreflightSignatureFailure(t *testing.T) {
	rpcServer := &RpcServer{
		slotCtx: &sealevel.SlotCtx{
			Slot:     123,
			Features: features.NewFeaturesDefault(),
		},
	}

	tx, wire := testLegacyTransaction(t)
	_, err := rpcServer.SendTransaction(
		context.Background(),
		mustRawParams(t, []interface{}{
			base64.StdEncoding.EncodeToString(wire),
			map[string]interface{}{"encoding": "base64"},
		}),
	)
	require.Error(t, err)

	preflightErr, ok := err.(*SendTransactionPreflightFailureError)
	require.True(t, ok)
	assert.Equal(t, "Transaction simulation failed: Transaction did not pass signature verification", preflightErr.Message)
	assert.Equal(t, "SignatureFailure", preflightErr.Result.Err)
	require.NotNil(t, preflightErr.Result.Logs)
	assert.Equal(t, []string{}, *preflightErr.Result.Logs)
	require.NotNil(t, preflightErr.Result.UnitsConsumed)
	assert.Equal(t, uint64(0), *preflightErr.Result.UnitsConsumed)
	require.NotNil(t, preflightErr.Result.LoadedAccountsDataSize)
	assert.Equal(t, uint32(0), *preflightErr.Result.LoadedAccountsDataSize)
	assert.Nil(t, preflightErr.Result.LoadedAddresses)

	sig, sigErr := firstSignature(tx)
	require.NoError(t, sigErr)
	assert.Equal(t, tx.Signatures[0], sig)
}

func TestSendTransaction_SkipPreflight_FansOutToUpcomingLeaders(t *testing.T) {
	listenerA := mustListenUDP(t)
	defer listenerA.Close()
	listenerB := mustListenUDP(t)
	defer listenerB.Close()
	listenerC := mustListenUDP(t)
	defer listenerC.Close()
	listenerD := mustListenUDP(t)
	defer listenerD.Close()
	listenerE := mustListenUDP(t)
	defer listenerE.Close()
	listenerF := mustListenUDP(t)
	defer listenerF.Close()

	leaderA := solana.PublicKey{0xA1}
	leaderB := solana.PublicKey{0xB2}
	leaderC := solana.PublicKey{0xC3}
	leaderD := solana.PublicKey{0xD4}
	leaderE := solana.PublicKey{0xE5}
	leaderF := solana.PublicKey{0xF6}

	global.SetLeaderSchedule(leaderschedule.NewLeaderScheduleFromKeyedSlots(
		map[solana.PublicKey][]uint64{
			leaderA: {0, 1, 2, 3},
			leaderB: {4, 5, 6, 7},
			leaderC: {8, 9, 10, 11},
			leaderD: {12, 13, 14, 15},
			leaderE: {16, 17, 18, 19},
			leaderF: {20, 21, 22, 23},
		},
		100,
	))
	global.SetSlot(100)
	defer global.SetLeaderSchedule(nil)
	defer global.SetSlot(0)

	tx, wire := testLegacyTransaction(t)
	fetchCount := 0
	rpcServer := &RpcServer{
		transactionSender:                 defaultTransactionSender,
		clusterNodesRefreshEvery:          sendTransactionClusterNodesRefreshEvery,
		sendTransactionLeaderForwardCount: sendTransactionLeaderForwardCount,
		clusterNodesFetcher: func(context.Context) ([]*solanarpc.GetClusterNodesResult, error) {
			fetchCount++
			return []*solanarpc.GetClusterNodesResult{
				{Pubkey: leaderA, TPU: stringPtr(listenerA.LocalAddr().String())},
				{Pubkey: leaderB, TPU: stringPtr(listenerB.LocalAddr().String())},
				{Pubkey: leaderC, TPU: stringPtr(listenerC.LocalAddr().String())},
				{Pubkey: leaderD, TPU: stringPtr(listenerD.LocalAddr().String())},
				{Pubkey: leaderE, TPU: stringPtr(listenerE.LocalAddr().String())},
				{Pubkey: leaderF, TPU: stringPtr(listenerF.LocalAddr().String())},
			}, nil
		},
	}
	receipts, err := txstatus.NewIndex(txstatus.Config{MaxReceipts: 8, Retention: time.Hour})
	require.NoError(t, err)
	rpcServer.SetTransactionReceipts(receipts)

	gotSig, err := rpcServer.SendTransaction(
		context.Background(),
		mustRawParams(t, []interface{}{
			base64.StdEncoding.EncodeToString(wire),
			map[string]interface{}{"encoding": "base64", "skipPreflight": true},
		}),
	)
	require.NoError(t, err)
	assert.Equal(t, tx.Signatures[0].String(), gotSig)
	assert.Equal(t, 1, fetchCount, "first send should populate the TPU cache once")
	receipt, known := receipts.Lookup(tx.Signatures[0])
	require.True(t, known)
	assert.Equal(t, txstatus.StatusSubmitted, receipt.Status)

	assert.Equal(t, wire, mustReadUDP(t, listenerA))
	assert.Equal(t, wire, mustReadUDP(t, listenerB))
	assert.Equal(t, wire, mustReadUDP(t, listenerC))
	assert.Equal(t, wire, mustReadUDP(t, listenerD))
	assert.Equal(t, wire, mustReadUDP(t, listenerE))
	assert.Equal(t, wire, mustReadUDP(t, listenerF))
}

func TestSendTransaction_PreservesAmbiguityWhenForwardingFails(t *testing.T) {
	leader := solana.PublicKey{0xA1}
	global.SetLeaderSchedule(leaderschedule.NewLeaderScheduleFromKeyedSlots(
		map[solana.PublicKey][]uint64{leader: {0}},
		100,
	))
	global.SetSlot(100)
	defer global.SetLeaderSchedule(nil)
	defer global.SetSlot(0)

	tx, wire := testLegacyTransaction(t)
	receipts, err := txstatus.NewIndex(txstatus.Config{MaxReceipts: 8, Retention: time.Hour})
	require.NoError(t, err)
	rpcServer := &RpcServer{
		clusterNodesRefreshEvery:          sendTransactionClusterNodesRefreshEvery,
		sendTransactionLeaderForwardCount: 0,
		clusterNodesFetcher: func(context.Context) ([]*solanarpc.GetClusterNodesResult, error) {
			return []*solanarpc.GetClusterNodesResult{{
				Pubkey: leader,
				TPU:    stringPtr("127.0.0.1:9001"),
			}}, nil
		},
		transactionSender: func(context.Context, []byte, tpuEndpoint) error {
			return errors.New("ambiguous transport failure")
		},
	}
	rpcServer.SetTransactionReceipts(receipts)

	_, err = rpcServer.SendTransaction(
		context.Background(),
		mustRawParams(t, []interface{}{
			base64.StdEncoding.EncodeToString(wire),
			map[string]interface{}{"encoding": "base64", "skipPreflight": true},
		}),
	)
	require.Error(t, err)
	receipt, known := receipts.Lookup(tx.Signatures[0])
	require.True(t, known)
	assert.Equal(t, txstatus.StatusSubmissionUnknown, receipt.Status)
	assert.False(t, receipt.Status.Terminal())
}

func TestResolveUpcomingLeaderTPUEndpoints_UsesFreshCacheWithoutRefetch(t *testing.T) {
	leaderA := solana.PublicKey{0x01}
	leaderB := solana.PublicKey{0x02}
	leaderC := solana.PublicKey{0x03}
	leaderD := solana.PublicKey{0x04}
	leaderE := solana.PublicKey{0x05}
	leaderF := solana.PublicKey{0x06}

	global.SetLeaderSchedule(leaderschedule.NewLeaderScheduleFromKeyedSlots(
		map[solana.PublicKey][]uint64{
			leaderA: {0},
			leaderB: {1},
			leaderC: {2},
			leaderD: {3},
			leaderE: {4},
			leaderF: {5},
		},
		500,
	))
	global.SetSlot(500)
	defer global.SetLeaderSchedule(nil)
	defer global.SetSlot(0)

	fetchCount := 0
	rpcServer := &RpcServer{
		clusterNodesRefreshEvery:          sendTransactionClusterNodesRefreshEvery,
		sendTransactionLeaderForwardCount: sendTransactionLeaderForwardCount,
		clusterNodesFetcher: func(context.Context) ([]*solanarpc.GetClusterNodesResult, error) {
			fetchCount++
			return []*solanarpc.GetClusterNodesResult{
				{Pubkey: leaderA, TPU: stringPtr("127.0.0.1:9001")},
				{Pubkey: leaderB, TPU: stringPtr("127.0.0.1:9002")},
				{Pubkey: leaderC, TPU: stringPtr("127.0.0.1:9003")},
				{Pubkey: leaderD, TPU: stringPtr("127.0.0.1:9004")},
				{Pubkey: leaderE, TPU: stringPtr("127.0.0.1:9005")},
				{Pubkey: leaderF, TPU: stringPtr("127.0.0.1:9006")},
			}, nil
		},
	}

	require.NoError(t, rpcServer.refreshLeaderTPUCache(context.Background()))
	targets, err := rpcServer.resolveUpcomingLeaderTPUEndpoints(context.Background(), 6)
	require.NoError(t, err)
	require.Len(t, targets, 6)
	assert.Equal(t, 1, fetchCount, "fresh cache should satisfy resolution without another RPC poll")
}

func TestResolveUpcomingLeaderTPUEndpoints_RefreshesStaleCache(t *testing.T) {
	leaderA := solana.PublicKey{0x11}
	leaderB := solana.PublicKey{0x12}
	leaderC := solana.PublicKey{0x13}
	leaderD := solana.PublicKey{0x14}
	leaderE := solana.PublicKey{0x15}
	leaderF := solana.PublicKey{0x16}

	global.SetLeaderSchedule(leaderschedule.NewLeaderScheduleFromKeyedSlots(
		map[solana.PublicKey][]uint64{
			leaderA: {0},
			leaderB: {1},
			leaderC: {2},
			leaderD: {3},
			leaderE: {4},
			leaderF: {5},
		},
		900,
	))
	global.SetSlot(900)
	defer global.SetLeaderSchedule(nil)
	defer global.SetSlot(0)

	fetchCount := 0
	rpcServer := &RpcServer{
		clusterNodesRefreshEvery:          10 * time.Minute,
		sendTransactionLeaderForwardCount: sendTransactionLeaderForwardCount,
		clusterNodesFetcher: func(context.Context) ([]*solanarpc.GetClusterNodesResult, error) {
			fetchCount++
			return []*solanarpc.GetClusterNodesResult{
				{Pubkey: leaderA, TPU: stringPtr("127.0.0.1:9101")},
				{Pubkey: leaderB, TPU: stringPtr("127.0.0.1:9102")},
				{Pubkey: leaderC, TPU: stringPtr("127.0.0.1:9103")},
				{Pubkey: leaderD, TPU: stringPtr("127.0.0.1:9104")},
				{Pubkey: leaderE, TPU: stringPtr("127.0.0.1:9105")},
				{Pubkey: leaderF, TPU: stringPtr("127.0.0.1:9106")},
			}, nil
		},
	}

	require.NoError(t, rpcServer.refreshLeaderTPUCache(context.Background()))
	rpcServer.leaderTPUCacheMu.Lock()
	rpcServer.leaderTPUCacheUpdatedAt = time.Now().Add(-11 * time.Minute)
	rpcServer.leaderTPUCacheMu.Unlock()

	targets, err := rpcServer.resolveUpcomingLeaderTPUEndpoints(context.Background(), 6)
	require.NoError(t, err)
	require.Len(t, targets, 6)
	assert.Equal(t, 2, fetchCount, "stale cache should trigger a refresh before resolving targets")
}

func TestResolveUpcomingLeaderTPUEndpointsBoundsOnPathRefresh(t *testing.T) {
	rpcServer := &RpcServer{
		clusterNodesRefreshTimeout: 20 * time.Millisecond,
		clusterNodesFetcher: func(ctx context.Context) ([]*solanarpc.GetClusterNodesResult, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	started := time.Now()
	_, err := rpcServer.resolveUpcomingLeaderTPUEndpoints(ctx, 1)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("resolve error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("on-path refresh took %v", elapsed)
	}
}

func TestRefreshLeaderTPUCache_PrefersQUICEndpoints(t *testing.T) {
	leaderA := solana.PublicKey{0x21}
	leaderB := solana.PublicKey{0x22}
	leaderC := solana.PublicKey{0x23}

	rpcServer := &RpcServer{
		clusterNodesFetcher: func(context.Context) ([]*solanarpc.GetClusterNodesResult, error) {
			return []*solanarpc.GetClusterNodesResult{
				{
					Pubkey:  leaderA,
					TPU:     stringPtr("127.0.0.1:9001"),
					TPUQUIC: stringPtr("127.0.0.1:10001"),
				},
				{
					Pubkey:  leaderB,
					TPUQUIC: stringPtr("127.0.0.1:10002"),
				},
				{
					Pubkey: leaderC,
					TPU:    stringPtr("127.0.0.1:9003"),
				},
			}, nil
		},
	}

	require.NoError(t, rpcServer.refreshLeaderTPUCache(context.Background()))

	rpcServer.leaderTPUCacheMu.RLock()
	defer rpcServer.leaderTPUCacheMu.RUnlock()

	assert.Equal(t, tpuEndpoint{Addr: netip.MustParseAddrPort("127.0.0.1:10001"), Transport: tpuTransportQUIC}, rpcServer.leaderTPUByIdentity[leaderA])
	assert.Equal(t, tpuEndpoint{Addr: netip.MustParseAddrPort("127.0.0.1:10002"), Transport: tpuTransportQUIC}, rpcServer.leaderTPUByIdentity[leaderB])
	assert.Equal(t, tpuEndpoint{Addr: netip.MustParseAddrPort("127.0.0.1:9003"), Transport: tpuTransportUDP}, rpcServer.leaderTPUByIdentity[leaderC])
}

func TestResolveV1LeaderEndpointsSkipsUDPOnlyLeaders(t *testing.T) {
	udpLeader := solana.PublicKey{0x31}
	quicLeader := solana.PublicKey{0x32}
	global.SetLeaderSchedule(leaderschedule.NewLeaderScheduleFromKeyedSlots(
		map[solana.PublicKey][]uint64{udpLeader: {0}, quicLeader: {1}}, 700,
	))
	global.SetSlot(700)
	defer global.SetLeaderSchedule(nil)
	defer global.SetSlot(0)

	rpcServer := &RpcServer{clusterNodesFetcher: func(context.Context) ([]*solanarpc.GetClusterNodesResult, error) {
		return []*solanarpc.GetClusterNodesResult{
			{Pubkey: udpLeader, TPU: stringPtr("127.0.0.1:9301")},
			{Pubkey: quicLeader, TPUQUIC: stringPtr("127.0.0.1:10302")},
		}, nil
	}}
	require.NoError(t, rpcServer.refreshLeaderTPUCache(context.Background()))
	targets, err := rpcServer.resolveUpcomingLeaderTPUEndpointsForTransport(context.Background(), 1, true)
	require.NoError(t, err)
	require.Equal(t, []tpuEndpoint{{Addr: netip.MustParseAddrPort("127.0.0.1:10302"), Transport: tpuTransportQUIC}}, targets)

	udpOnly := &RpcServer{clusterNodesFetcher: func(context.Context) ([]*solanarpc.GetClusterNodesResult, error) {
		return []*solanarpc.GetClusterNodesResult{{Pubkey: udpLeader, TPU: stringPtr("127.0.0.1:9301")}}, nil
	}}
	_, err = udpOnly.resolveUpcomingLeaderTPUEndpointsForTransport(context.Background(), 1, true)
	require.ErrorContains(t, err, "unable to resolve TPU addresses")
}

func mustRawParams(t *testing.T, params []interface{}) jsonrpc.RawParams {
	t.Helper()
	raw, err := json.Marshal(params)
	require.NoError(t, err)
	return jsonrpc.RawParams(raw)
}

func testLegacyTransaction(t *testing.T) (*solana.Transaction, []byte) {
	t.Helper()

	tx := &solana.Transaction{
		Signatures: []solana.Signature{{}},
		Message: solana.Message{
			Header:      solana.MessageHeader{NumRequiredSignatures: 1},
			AccountKeys: []solana.PublicKey{{0x11}},
		},
	}

	wire, err := tx.MarshalBinary()
	require.NoError(t, err)
	return tx, wire
}

func testV1TransactionWire(t *testing.T, target int) []byte {
	return testTransactionWire(t, target, solana.MessageVersionV1)
}

func testTransactionWire(t *testing.T, target int, version solana.MessageVersion) []byte {
	t.Helper()
	tx := &solana.Transaction{
		Signatures: []solana.Signature{{}},
		Message: solana.Message{
			Header:       solana.MessageHeader{NumRequiredSignatures: 1, NumReadonlyUnsignedAccounts: 1},
			AccountKeys:  []solana.PublicKey{{1}, {2}},
			Instructions: []solana.CompiledInstruction{{ProgramIDIndex: 1, Accounts: []uint16{0}}},
		},
	}
	if version != solana.MessageVersionLegacy {
		_, err := tx.Message.SetVersion(version)
		require.NoError(t, err)
	}
	for low, high := 0, target; low <= high; {
		dataLen := low + (high-low)/2
		tx.Message.Instructions[0].Data = make([]byte, dataLen)
		wire, marshalErr := tx.MarshalBinary()
		require.NoError(t, marshalErr)
		switch {
		case len(wire) < target:
			low = dataLen + 1
		case len(wire) > target:
			high = dataLen - 1
		default:
			return wire
		}
	}
	t.Fatalf("could not construct a version %v transaction with %d-byte wire size", version, target)
	return nil
}

func mustListenUDP(t *testing.T) *net.UDPConn {
	t.Helper()

	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)
	return conn
}

func mustReadUDP(t *testing.T, conn *net.UDPConn) []byte {
	t.Helper()

	buf := make([]byte, v1PacketDataSize)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	n, _, err := conn.ReadFromUDP(buf)
	require.NoError(t, err)
	out := make([]byte, n)
	copy(out, buf[:n])
	return out
}

func stringPtr(v string) *string {
	return &v
}
