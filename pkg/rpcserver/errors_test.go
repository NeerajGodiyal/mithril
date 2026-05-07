package rpcserver

import (
	"encoding/json"
	"testing"

	"github.com/filecoin-project/go-jsonrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMinContextSlotNotReachedError_ToJSONRPCError(t *testing.T) {
	e := &MinContextSlotNotReachedError{ContextSlot: 417_500_000}
	got, err := e.ToJSONRPCError()
	require.NoError(t, err)
	assert.Equal(t, jsonrpc.ErrorCode(-32016), got.Code)
	assert.Equal(t, "Minimum context slot has not been reached", got.Message)
	require.NotNil(t, got.Data, "must populate Data (not Meta) to match Agave wire format")
	dataMap, ok := got.Data.(map[string]uint64)
	require.True(t, ok, "Data should be a map[string]uint64")
	assert.Equal(t, uint64(417_500_000), dataMap["contextSlot"])
}

// JSON-marshal round-trip: catches wire-format regressions where the
// structured payload lands in the wrong envelope field. Solana SDKs read
// `error.data.contextSlot`, so the wire MUST emit `data`, not `meta`.
func TestMinContextSlotNotReachedError_WireShape(t *testing.T) {
	e := &MinContextSlotNotReachedError{ContextSlot: 417_500_000}
	got, err := e.ToJSONRPCError()
	require.NoError(t, err)

	raw, err := json.Marshal(&got)
	require.NoError(t, err)
	rawStr := string(raw)

	assert.Contains(t, rawStr, `"code":-32016`)
	assert.Contains(t, rawStr, `"message":"Minimum context slot has not been reached"`)
	assert.Contains(t, rawStr, `"data":{"contextSlot":417500000}`)
	assert.NotContains(t, rawStr, `"meta"`, "must not emit legacy meta envelope; Agave uses data")
}

func TestMinContextSlotNotReachedError_FromJSONRPCError(t *testing.T) {
	rpcErr := jsonrpc.JSONRPCError{
		Code:    -32016,
		Message: "Minimum context slot has not been reached",
		Data:    map[string]interface{}{"contextSlot": float64(42)},
	}
	e := &MinContextSlotNotReachedError{}
	require.NoError(t, e.FromJSONRPCError(rpcErr))
	assert.Equal(t, uint64(42), e.ContextSlot)
}

func TestMinContextSlotNotReachedError_FromJSONRPCError_RejectsWrongCode(t *testing.T) {
	rpcErr := jsonrpc.JSONRPCError{Code: 1}
	e := &MinContextSlotNotReachedError{}
	assert.Error(t, e.FromJSONRPCError(rpcErr))
}

func TestInvalidParamsError_ToJSONRPCError(t *testing.T) {
	e := &InvalidParamsError{Message: "bad input"}
	got, err := e.ToJSONRPCError()
	require.NoError(t, err)
	assert.Equal(t, jsonrpc.ErrorCode(-32602), got.Code)
	assert.Equal(t, "bad input", got.Message)
}

func TestInvalidParamsError_WireShape(t *testing.T) {
	e := &InvalidParamsError{Message: "sigVerify may not be used with replaceRecentBlockhash"}
	got, err := e.ToJSONRPCError()
	require.NoError(t, err)
	raw, err := json.Marshal(&got)
	require.NoError(t, err)
	rawStr := string(raw)
	assert.Contains(t, rawStr, `"code":-32602`)
	assert.Contains(t, rawStr, `"message":"sigVerify may not be used with replaceRecentBlockhash"`)
	assert.NotContains(t, rawStr, `"data"`, "InvalidParams emits no data; only code+message")
}

func TestInvalidParamsError_FromJSONRPCError(t *testing.T) {
	e := &InvalidParamsError{}
	require.NoError(t, e.FromJSONRPCError(jsonrpc.JSONRPCError{Code: -32602, Message: "x"}))
	assert.Equal(t, "x", e.Message)
}
