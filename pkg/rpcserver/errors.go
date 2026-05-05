package rpcserver

import (
	"encoding/json"
	"fmt"

	"github.com/filecoin-project/go-jsonrpc"
)

// JSON-RPC error codes that match Agave's solana_rpc_client_types.
const (
	// -32602 is the JSON-RPC standard "Invalid params" code.
	rpcCodeInvalidParams jsonrpc.ErrorCode = -32602
	// -32016 is Agave's reserved code for MinContextSlotNotReached.
	rpcCodeMinContextSlotNotReached jsonrpc.ErrorCode = -32016
)

// MinContextSlotNotReachedError is returned when the caller demands a
// minimum context slot we have not yet reached. The structured payload
// is emitted as the JSON-RPC error's `data` field (Agave-compatible),
// matching the shape `{"contextSlot": N}` that Solana clients consume.
type MinContextSlotNotReachedError struct {
	ContextSlot uint64
}

func (e *MinContextSlotNotReachedError) Error() string {
	return "Minimum context slot has not been reached"
}

func (e *MinContextSlotNotReachedError) ToJSONRPCError() (jsonrpc.JSONRPCError, error) {
	return jsonrpc.JSONRPCError{
		Code:    rpcCodeMinContextSlotNotReached,
		Message: e.Error(),
		Data:    map[string]uint64{"contextSlot": e.ContextSlot},
	}, nil
}

func (e *MinContextSlotNotReachedError) FromJSONRPCError(rpcErr jsonrpc.JSONRPCError) error {
	if rpcErr.Code != rpcCodeMinContextSlotNotReached {
		return fmt.Errorf("unexpected code %d for MinContextSlotNotReachedError", rpcErr.Code)
	}
	if rpcErr.Data == nil {
		return nil
	}
	// `Data` is interface{}; round-trip through JSON to read camelCase.
	raw, err := json.Marshal(rpcErr.Data)
	if err != nil {
		return fmt.Errorf("re-encoding MinContextSlotNotReachedError data: %w", err)
	}
	var payload struct {
		ContextSlot uint64 `json:"contextSlot"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("decoding MinContextSlotNotReachedError data: %w", err)
	}
	e.ContextSlot = payload.ContextSlot
	return nil
}

// InvalidParamsError maps a Mithril-side argument-validation failure to
// the standard JSON-RPC -32602. Use for shape and conflict checks like
// "sigVerify may not be used with replaceRecentBlockhash".
type InvalidParamsError struct {
	Message string
}

func (e *InvalidParamsError) Error() string { return e.Message }

func (e *InvalidParamsError) ToJSONRPCError() (jsonrpc.JSONRPCError, error) {
	return jsonrpc.JSONRPCError{
		Code:    rpcCodeInvalidParams,
		Message: e.Message,
	}, nil
}

func (e *InvalidParamsError) FromJSONRPCError(rpcErr jsonrpc.JSONRPCError) error {
	if rpcErr.Code != rpcCodeInvalidParams {
		return fmt.Errorf("unexpected code %d for InvalidParamsError", rpcErr.Code)
	}
	e.Message = rpcErr.Message
	return nil
}

// rpcErrorRegistry returns an Errors set with Mithril's custom codes
// registered. Pass to jsonrpc.NewServer via WithServerErrors.
func rpcErrorRegistry() jsonrpc.Errors {
	errs := jsonrpc.NewErrors()
	errs.Register(rpcCodeInvalidParams, new(*InvalidParamsError))
	errs.Register(rpcCodeMinContextSlotNotReached, new(*MinContextSlotNotReachedError))
	return errs
}
