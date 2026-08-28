package rpcserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/txstatus"
	"github.com/filecoin-project/go-jsonrpc"
	"github.com/gagliardetto/solana-go"
)

func statusSig(n byte) solana.Signature {
	var s solana.Signature
	s[0] = n
	return s
}

func statusBlockHeight(value uint64) *uint64 {
	return &value
}

func submitStatus(index *txstatus.Index, signature solana.Signature, recentBlockhash solana.Hash, deadline *uint64) {
	index.SubmissionAttempted(signature, recentBlockhash, deadline)
	index.Forwarded(signature)
}

func statusParams(t *testing.T, values ...any) jsonrpc.RawParams {
	t.Helper()
	encoded, err := json.Marshal(values)
	if err != nil {
		t.Fatalf("marshalling params: %v", err)
	}
	return jsonrpc.RawParams(encoded)
}

// newStatusServer builds a server with a live receipt index installed.
func newStatusServer(t *testing.T) (*RpcServer, *txstatus.Index) {
	t.Helper()
	idx, err := txstatus.NewIndex(txstatus.Config{MaxReceipts: 64, Retention: time.Hour})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	server := &RpcServer{}
	server.SetTransactionReceipts(idx)
	return server, idx
}

func TestRecordSubmissionOnlyInfersFreshBlockhashDeadline(t *testing.T) {
	server, index := newStatusServer(t)
	freshHash := solana.Hash{8}
	validatedBank := &sealevel.SlotCtx{Blockhash: freshHash, BlockHeight: 1_000}
	server.SetSlotCtx(validatedBank)

	freshSignature := statusSig(20)
	server.recordSubmissionAttempt(
		&solana.Transaction{
			Signatures: []solana.Signature{freshSignature},
			Message:    solana.Message{RecentBlockhash: freshHash},
		},
		freshSignature,
		validatedBank,
	)
	fresh, known := index.Lookup(freshSignature)
	if !known || fresh.LastValidBlockHeight == nil || *fresh.LastValidBlockHeight != 1_150 {
		t.Fatalf("fresh deadline = %v known=%v", fresh.LastValidBlockHeight, known)
	}

	olderSignature := statusSig(21)
	server.recordSubmissionAttempt(
		&solana.Transaction{
			Signatures: []solana.Signature{olderSignature},
			Message:    solana.Message{RecentBlockhash: solana.Hash{7}},
		},
		olderSignature,
		validatedBank,
	)
	older, known := index.Lookup(olderSignature)
	if !known || older.LastValidBlockHeight != nil {
		t.Fatalf("older blockhash deadline = %v known=%v", older.LastValidBlockHeight, known)
	}
}

func TestRecordSubmissionUsesValidatedBankDeadline(t *testing.T) {
	server, index := newStatusServer(t)
	validatedHash := solana.Hash{8}
	validatedBank := &sealevel.SlotCtx{Blockhash: validatedHash, BlockHeight: 1_000}
	server.SetSlotCtx(validatedBank)

	// Normal tip publication does not invalidate the immutable bank already
	// validated by sendTransaction. Receipt metadata must remain from that bank.
	server.SetSlotCtx(&sealevel.SlotCtx{Blockhash: solana.Hash{9}, BlockHeight: 2_000})
	signature := statusSig(22)
	err := server.recordSubmissionAttempt(
		&solana.Transaction{
			Signatures: []solana.Signature{signature},
			Message:    solana.Message{RecentBlockhash: validatedHash},
		},
		signature,
		validatedBank,
	)
	if err != nil {
		t.Fatalf("record submission: %v", err)
	}
	receipt, known := index.Lookup(signature)
	if !known || receipt.LastValidBlockHeight == nil || *receipt.LastValidBlockHeight != 1_150 {
		t.Fatalf("validated-bank deadline = %v known=%v", receipt.LastValidBlockHeight, known)
	}
}

// TestSubmittedStatusMissingEvidenceIsNullNotFailure is the property that
// makes this method safe to automate against. Missing retained evidence yields
// a null entry, never an error and never a fabricated failure.
func TestSubmittedStatusMissingEvidenceIsNullNotFailure(t *testing.T) {
	server, _ := newStatusServer(t)

	resp, err := server.GetSubmittedTransactionStatus(
		context.Background(),
		statusParams(t, []any{statusSig(1).String()}),
	)
	if err != nil {
		t.Fatalf("missing evidence produced an error: %v", err)
	}
	if len(resp.Value) != 1 {
		t.Fatalf("got %d entries, want 1", len(resp.Value))
	}
	if resp.Value[0] != nil {
		t.Errorf("missing evidence returned a status: %+v", resp.Value[0])
	}
	if resp.Context.ApiVersion == "" {
		t.Error("response carries no apiVersion")
	}
}

// TestSubmittedStatusReportsOurTransactions walks a submitted transaction
// through landing and rooting.
func TestSubmittedStatusReportsOurTransactions(t *testing.T) {
	server, idx := newStatusServer(t)
	s := statusSig(2)
	submitStatus(idx, s, solana.Hash{9}, statusBlockHeight(500))

	query := statusParams(t, []any{s.String()})

	resp, err := server.GetSubmittedTransactionStatus(context.Background(), query)
	if err != nil {
		t.Fatalf("GetSubmittedTransactionStatus: %v", err)
	}
	if resp.Value[0] == nil {
		t.Fatal("a submitted transaction was reported as not ours")
	}
	if resp.Value[0].Status != "submitted" || resp.Value[0].Terminal {
		t.Errorf("after submit: status=%q terminal=%v", resp.Value[0].Status, resp.Value[0].Terminal)
	}
	if resp.Value[0].LastValidBlockHeight == nil || *resp.Value[0].LastValidBlockHeight != 500 {
		t.Errorf("last valid block height = %v, want 500", resp.Value[0].LastValidBlockHeight)
	}

	idx.Landed(s, 300, "")
	idx.Rooted(300)
	resp, err = server.GetSubmittedTransactionStatus(context.Background(), query)
	if err != nil {
		t.Fatalf("GetSubmittedTransactionStatus: %v", err)
	}
	if resp.Value[0].Status != "rooted" || !resp.Value[0].Terminal {
		t.Errorf("after rooting: status=%q terminal=%v", resp.Value[0].Status, resp.Value[0].Terminal)
	}
	if resp.Value[0].LandedSlot != 300 {
		t.Errorf("landed slot = %d, want 300", resp.Value[0].LandedSlot)
	}
}

// TestSubmittedStatusSurfacesOnChainFailure pins that an execution failure is
// reported with its slot and becomes terminal only when that slot roots.
func TestSubmittedStatusSurfacesOnChainFailure(t *testing.T) {
	server, idx := newStatusServer(t)
	s := statusSig(3)
	submitStatus(idx, s, solana.Hash{9}, statusBlockHeight(500))
	idx.Landed(s, 310, "InstructionError: custom program error 0x1")

	resp, err := server.GetSubmittedTransactionStatus(
		context.Background(), statusParams(t, []any{s.String()}))
	if err != nil {
		t.Fatalf("GetSubmittedTransactionStatus: %v", err)
	}
	entry := resp.Value[0]
	if entry == nil {
		t.Fatal("a failed transaction was reported as not ours")
	}
	if entry.Status != "landed_failed" || entry.Terminal {
		t.Errorf("status=%q terminal=%v", entry.Status, entry.Terminal)
	}
	if entry.ExecutionError == "" {
		t.Error("the on-chain error was dropped")
	}
	if entry.LandedSlot != 310 {
		t.Errorf("a failed transaction still landed; slot = %d", entry.LandedSlot)
	}

	idx.Rooted(310)
	resp, err = server.GetSubmittedTransactionStatus(
		context.Background(), statusParams(t, []any{s.String()}))
	if err != nil {
		t.Fatalf("GetSubmittedTransactionStatus after root: %v", err)
	}
	entry = resp.Value[0]
	if entry.Status != "failed" || !entry.Terminal {
		t.Errorf("rooted status=%q terminal=%v", entry.Status, entry.Terminal)
	}
}

// TestSubmittedStatusPreservesRequestOrder pins that entry i answers signature
// i. A caller batching lookups relies on positional correspondence.
func TestSubmittedStatusPreservesRequestOrder(t *testing.T) {
	server, idx := newStatusServer(t)
	known, unknown := statusSig(4), statusSig(5)
	submitStatus(idx, known, solana.Hash{9}, statusBlockHeight(500))

	resp, err := server.GetSubmittedTransactionStatus(
		context.Background(),
		statusParams(t, []any{unknown.String(), known.String()}),
	)
	if err != nil {
		t.Fatalf("GetSubmittedTransactionStatus: %v", err)
	}
	if len(resp.Value) != 2 {
		t.Fatalf("got %d entries, want 2", len(resp.Value))
	}
	if resp.Value[0] != nil {
		t.Error("position 0 (unknown) returned a status")
	}
	if resp.Value[1] == nil || resp.Value[1].Signature != known.String() {
		t.Errorf("position 1 did not answer the known signature: %+v", resp.Value[1])
	}
}

// TestSubmittedStatusWithoutSinkFailsExplicitly keeps "tracking disabled"
// distinct from "no retained evidence for this signature."
func TestSubmittedStatusWithoutSinkFailsExplicitly(t *testing.T) {
	server := &RpcServer{}
	_, err := server.GetSubmittedTransactionStatus(
		context.Background(), statusParams(t, []any{statusSig(6).String()}))
	if err == nil || !strings.Contains(err.Error(), "tracking is unavailable") {
		t.Fatalf("tracking-disabled error = %v", err)
	}
}

// TestSubmittedStatusRejectsMalformedInput covers the request boundary.
func TestSubmittedStatusRejectsMalformedInput(t *testing.T) {
	server, _ := newStatusServer(t)

	tooMany := make([]any, maxSubmittedStatusQuery+1)
	for i := range tooMany {
		tooMany[i] = statusSig(1).String()
	}

	cases := map[string]jsonrpc.RawParams{
		"no params":      statusParams(t),
		"not an array":   statusParams(t, "a-single-string"),
		"empty array":    statusParams(t, []any{}),
		"non-string":     statusParams(t, []any{42}),
		"invalid base58": statusParams(t, []any{"not-a-signature!!"}),
		"too many":       statusParams(t, tooMany),
	}
	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := server.GetSubmittedTransactionStatus(context.Background(), params); err == nil {
				t.Error("malformed input was accepted")
			}
		})
	}
}

// TestSubmittedStatusIsBounded pins the per-request work limit, since this is
// reachable by anyone who can reach the RPC port.
func TestSubmittedStatusIsBounded(t *testing.T) {
	server, _ := newStatusServer(t)

	atLimit := make([]any, maxSubmittedStatusQuery)
	for i := range atLimit {
		atLimit[i] = statusSig(byte(i % 255)).String()
	}
	resp, err := server.GetSubmittedTransactionStatus(
		context.Background(), statusParams(t, atLimit))
	if err != nil {
		t.Fatalf("a request at the limit was rejected: %v", err)
	}
	if len(resp.Value) != maxSubmittedStatusQuery {
		t.Errorf("got %d entries, want %d", len(resp.Value), maxSubmittedStatusQuery)
	}

	overLimit := append(atLimit, statusSig(1).String())
	_, err = server.GetSubmittedTransactionStatus(
		context.Background(), statusParams(t, overLimit))
	if err == nil {
		t.Fatal("a request over the limit was accepted")
	}
	if !strings.Contains(err.Error(), "too many") {
		t.Errorf("error does not explain the limit: %v", err)
	}
}

// TestSubmittedStatusIsRegisteredAndAdditive pins that the method is reachable
// and that we did NOT take the standard getSignatureStatuses name, which
// implies coverage this node does not have.
func TestSubmittedStatusIsRegisteredAndAdditive(t *testing.T) {
	if _, ok := supportedRPCMethods["getSubmittedTransactionStatus"]; !ok {
		t.Error("getSubmittedTransactionStatus is not in the method allowlist")
	}
	if _, ok := supportedRPCMethods["getSignatureStatuses"]; ok {
		t.Error("the standard getSignatureStatuses name was claimed; this node " +
			"cannot answer for signatures it did not submit")
	}
}
