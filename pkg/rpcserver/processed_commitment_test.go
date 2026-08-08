package rpcserver

import (
	"context"
	"errors"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessedCommitmentMethodsUseOnePublishedBank(t *testing.T) {
	schedule := &sealevel.SysvarEpochSchedule{
		SlotsPerEpoch:    100,
		FirstNormalEpoch: 0,
		FirstNormalSlot:  0,
	}
	server := &RpcServer{epochSchedule: schedule}
	blockhash := solana.Hash{9}
	server.SetSlotCtx(&sealevel.SlotCtx{
		Slot:        250,
		BlockHeight: 200,
		Epoch:       2,
		Blockhash:   blockhash,
	})
	global.SetTransactionCount(321)
	defer global.SetTransactionCount(0)

	config := map[string]interface{}{
		"commitment":     "processed",
		"minContextSlot": float64(250),
	}
	params := mustRawParams(t, []interface{}{config})

	latest, err := server.GetLatestBlockhash(context.Background(), params)
	require.NoError(t, err)
	assert.Equal(t, uint64(250), latest.Context.Slot)
	require.NotNil(t, latest.Value)
	assert.Equal(t, blockhash.String(), latest.Value.Blockhash)
	assert.Equal(t, uint64(350), latest.Value.LastValidBlockHeight)

	height, err := server.GetBlockHeight(context.Background(), params)
	require.NoError(t, err)
	assert.Equal(t, uint64(200), height)

	epoch, err := server.GetEpochInfo(context.Background(), params)
	require.NoError(t, err)
	assert.Equal(t, uint64(250), epoch.AbsoluteSlot)
	assert.Equal(t, uint64(200), epoch.BlockHeight)
	assert.Equal(t, uint64(2), epoch.Epoch)
	assert.Equal(t, uint64(50), epoch.SlotIndex)
	assert.Equal(t, uint64(100), epoch.SlotsInEpoch)
	assert.Equal(t, uint64(321), epoch.TransactionCount)
}

func TestProcessedCommitmentMethodsRejectUnsupportedOrStaleRequests(t *testing.T) {
	server := &RpcServer{
		epochSchedule: &sealevel.SysvarEpochSchedule{SlotsPerEpoch: 100},
	}
	server.SetSlotCtx(&sealevel.SlotCtx{
		Slot:        250,
		BlockHeight: 200,
		Epoch:       2,
		Blockhash:   solana.Hash{9},
	})

	for _, call := range []struct {
		name string
		run  func(params map[string]interface{}) error
	}{
		{
			name: "latest blockhash",
			run: func(config map[string]interface{}) error {
				_, err := server.GetLatestBlockhash(context.Background(), mustRawParams(t, []interface{}{config}))
				return err
			},
		},
		{
			name: "block height",
			run: func(config map[string]interface{}) error {
				_, err := server.GetBlockHeight(context.Background(), mustRawParams(t, []interface{}{config}))
				return err
			},
		},
		{
			name: "epoch info",
			run: func(config map[string]interface{}) error {
				_, err := server.GetEpochInfo(context.Background(), mustRawParams(t, []interface{}{config}))
				return err
			},
		},
	} {
		t.Run(call.name+" commitment", func(t *testing.T) {
			err := call.run(map[string]interface{}{"commitment": "finalized"})
			var invalid *InvalidParamsError
			require.ErrorAs(t, err, &invalid)
		})
		t.Run(call.name+" min context", func(t *testing.T) {
			err := call.run(map[string]interface{}{"minContextSlot": float64(251)})
			var stale *MinContextSlotNotReachedError
			require.True(t, errors.As(err, &stale))
			assert.Equal(t, uint64(251), stale.ContextSlot)
		})
	}
}

func TestProcessedCommitmentMethodsFailWhileNodeIsNotReady(t *testing.T) {
	server := &RpcServer{epochSchedule: &sealevel.SysvarEpochSchedule{SlotsPerEpoch: 100}}
	params := mustRawParams(t, []interface{}{})

	if _, err := server.GetLatestBlockhash(context.Background(), params); err == nil {
		t.Fatal("getLatestBlockhash succeeded without a published bank")
	}
	if _, err := server.GetBlockHeight(context.Background(), params); err == nil {
		t.Fatal("getBlockHeight succeeded without a published bank")
	}
	if _, err := server.GetEpochInfo(context.Background(), params); err == nil {
		t.Fatal("getEpochInfo succeeded without a published bank")
	}
}

func TestParseProcessedCommitmentConfigRejectsMalformedValues(t *testing.T) {
	tests := []struct {
		name   string
		params []interface{}
	}{
		{name: "too many", params: []interface{}{map[string]interface{}{}, map[string]interface{}{}}},
		{name: "not object", params: []interface{}{"processed"}},
		{name: "commitment type", params: []interface{}{map[string]interface{}{"commitment": true}}},
		{name: "unsupported commitment", params: []interface{}{map[string]interface{}{"commitment": "confirmed"}}},
		{name: "slot type", params: []interface{}{map[string]interface{}{"minContextSlot": "250"}}},
		{name: "fractional slot", params: []interface{}{map[string]interface{}{"minContextSlot": 250.5}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseProcessedCommitmentConfig(test.params, "getLatestBlockhash")
			var invalid *InvalidParamsError
			require.ErrorAs(t, err, &invalid)
		})
	}
}
