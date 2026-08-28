package rpcserver

import (
	"context"
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/txstatus"
	"github.com/filecoin-project/go-jsonrpc"
	"github.com/gagliardetto/solana-go"
)

// maxSubmittedStatusQuery bounds how many signatures one call may ask about,
// so a single request cannot make the server do unbounded work.
const maxSubmittedStatusQuery = 256

// SetTransactionReceipts installs the receipt store. It must be called before
// Serve; submission and status calls fail closed without it.
func (rpcServer *RpcServer) SetTransactionReceipts(store txstatus.Store) {
	rpcServer.txReceipts = store
}

// recordSubmissionAttempt notes a transaction immediately before forwarding.
// Submission fails closed when the receipt store is unavailable or full:
// sending while discarding ambiguous outcomes is unsafe for automated callers.
func (rpcServer *RpcServer) recordSubmissionAttempt(tx *solana.Transaction, signature solana.Signature, slotCtx *sealevel.SlotCtx) error {
	if rpcServer.txReceipts == nil {
		return fmt.Errorf("transaction receipt tracking is unavailable")
	}
	if tx == nil {
		return fmt.Errorf("transaction is unavailable for receipt tracking")
	}
	var lastValidBlockHeight *uint64
	if slotCtx != nil && solana.Hash(slotCtx.Blockhash) == tx.Message.RecentBlockhash {
		if value, ok := recentBlockhashLastValidHeight(slotCtx.BlockHeight); ok {
			lastValidBlockHeight = &value
		}
	}
	return rpcServer.txReceipts.SubmissionAttempted(
		signature,
		solana.Hash(tx.Message.RecentBlockhash),
		lastValidBlockHeight,
	)
}

func (rpcServer *RpcServer) recordSubmissionForwarded(signature solana.Signature) {
	if rpcServer.txReceipts != nil {
		rpcServer.txReceipts.Forwarded(signature)
	}
}

// GetSubmittedTransactionStatusResp mirrors the context/value shape of the
// standard status methods without claiming their coverage.
type GetSubmittedTransactionStatusResp struct {
	Context GetAccountInfoRespContext     `json:"context"`
	Value   []*SubmittedTransactionStatus `json:"value"`
}

// SubmittedTransactionStatus is one signature's retained outcome. A nil entry
// means the in-memory receipt index has no evidence for that signature. It does
// not prove the transaction was never submitted or that it failed; receipts
// may also be absent after retention or a node restart.
type SubmittedTransactionStatus struct {
	Signature      string `json:"signature"`
	Status         string `json:"status"`
	Terminal       bool   `json:"terminal"`
	LandedSlot     uint64 `json:"landedSlot,omitempty"`
	ExecutionError string `json:"executionError,omitempty"`
	// LastValidBlockHeight is omitted when the transaction used an older
	// blockhash or durable nonce and this node cannot infer an exact deadline.
	LastValidBlockHeight *uint64 `json:"lastValidBlockHeight,omitempty"`
	SubmittedAtRFC       string  `json:"submittedAt"`
}

// GetSubmittedTransactionStatus answers only from this node's retained
// submission receipts. It is deliberately not named getSignatureStatuses: a
// verifying node that never indexed the chain cannot answer for an arbitrary
// signature, and a method with a standard name is expected to.
func (rpcServer *RpcServer) GetSubmittedTransactionStatus(ctx context.Context, p jsonrpc.RawParams) (GetSubmittedTransactionStatusResp, error) {
	params, err := jsonrpc.DecodeParams[[]interface{}](p)
	if err != nil {
		return GetSubmittedTransactionStatusResp{}, fmt.Errorf("decoding params: %w", err)
	}
	if len(params) < 1 {
		return GetSubmittedTransactionStatusResp{}, fmt.Errorf(
			"getSubmittedTransactionStatus requires an array of base58 signatures")
	}
	rawList, ok := params[0].([]interface{})
	if !ok {
		return GetSubmittedTransactionStatusResp{}, fmt.Errorf(
			"getSubmittedTransactionStatus requires an array of base58 signatures")
	}
	if len(rawList) == 0 {
		return GetSubmittedTransactionStatusResp{}, fmt.Errorf("no signatures supplied")
	}
	if len(rawList) > maxSubmittedStatusQuery {
		return GetSubmittedTransactionStatusResp{}, fmt.Errorf(
			"too many signatures: %d exceeds the limit of %d", len(rawList), maxSubmittedStatusQuery)
	}

	signatures := make([]solana.Signature, 0, len(rawList))
	for i, raw := range rawList {
		text, ok := raw.(string)
		if !ok {
			return GetSubmittedTransactionStatusResp{}, fmt.Errorf("signature %d is not a string", i)
		}
		parsed, err := solana.SignatureFromBase58(text)
		if err != nil {
			return GetSubmittedTransactionStatusResp{}, fmt.Errorf("signature %d is not valid base58", i)
		}
		signatures = append(signatures, parsed)
	}

	slotCtx, lifecycle := rpcServer.getSlotCtxWithLifecycle()
	contextSlot := uint64(0)
	if slotCtx != nil {
		contextSlot = slotCtx.Slot
	}
	resp := GetSubmittedTransactionStatusResp{
		Context: GetAccountInfoRespContext{ApiVersion: apiVersion, Slot: contextSlot},
		Value:   make([]*SubmittedTransactionStatus, len(signatures)),
	}
	if rpcServer.txReceipts == nil {
		return GetSubmittedTransactionStatusResp{}, fmt.Errorf("transaction receipt tracking is unavailable")
	}
	for i, signature := range signatures {
		receipt, known := rpcServer.txReceipts.Lookup(signature)
		if !known {
			continue
		}
		resp.Value[i] = &SubmittedTransactionStatus{
			Signature:            signature.String(),
			Status:               receipt.Status.String(),
			Terminal:             receipt.Status.Terminal(),
			LandedSlot:           receipt.LandedSlot,
			ExecutionError:       receipt.ExecutionError,
			LastValidBlockHeight: receipt.LastValidBlockHeight,
			SubmittedAtRFC:       receipt.SubmittedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		}
	}
	if err := rpcServer.validateProcessedBankPublication(slotCtx, lifecycle, "getSubmittedTransactionStatus"); err != nil {
		return GetSubmittedTransactionStatusResp{}, err
	}
	return resp, nil
}
