package rpcserver

import (
	"encoding/json"

	"github.com/Overclock-Validator/mithril/pkg/replay"
)

// A node that has detected its own replay divergence must not answer account RPC
// as though nothing were wrong. Trailing verification intentionally checks an
// eligible watermark behind the processed tip, so even "complete" does not mean
// the current bank was verified. Routine catch-up debt remains visible through
// getVerificationStatus without making RPC intermittently unavailable. Known
// divergence, a stalled verifier, or unavailable evidence still fails closed.
type evidenceGateReason string

const (
	// evidenceGateOpen means the node may answer.
	evidenceGateOpen evidenceGateReason = ""
	// evidenceGateDiverged means replay, footer, bank-hash or finality evidence
	// disagreed. Terminal: VerificationStatus refuses to leave this state.
	evidenceGateDiverged evidenceGateReason = "diverged"
	// evidenceGateStalled means required verification made no progress for the
	// configured window, so coverage is going stale rather than catching up.
	evidenceGateStalled evidenceGateReason = "stalled"
	// evidenceGateUnavailable means a required evidence source cannot answer.
	// This is a gap, not lateness.
	evidenceGateUnavailable evidenceGateReason = "unavailable"
	// evidenceGateUnknown means the verification state is not one of the six
	// defined values. An unrecognized state is treated as unsafe.
	evidenceGateUnknown evidenceGateReason = "unknown_verification_state"
)

// defaultVerificationSnapshot is the live replay state, used whenever no test
// seam is installed.
var defaultVerificationSnapshot verificationSnapshotFunc = replay.VerificationSnapshot

// verificationSnapshotFunc matches replay.VerificationSnapshot. It is a seam so
// tests can drive every state without running a replay.
type verificationSnapshotFunc func() (replay.VerificationState, bool, uint64, uint64)

// gateForVerificationState maps a verification state to the reason the node
// must refuse evidence, or evidenceGateOpen if it may answer. It is a pure
// function so the whole matrix is testable without a node.
func gateForVerificationState(state replay.VerificationState) evidenceGateReason {
	switch state {
	case replay.VerificationComplete, replay.VerificationIncomplete, replay.VerificationNotApplicable:
		return evidenceGateOpen
	case replay.VerificationDiverged:
		return evidenceGateDiverged
	case replay.VerificationStalled:
		return evidenceGateStalled
	case replay.VerificationUnavailable:
		return evidenceGateUnavailable
	default:
		// Not one of the six defined states. Refusing is the only safe reading:
		// an unrecognized value cannot be shown to mean the node is trustworthy.
		return evidenceGateUnknown
	}
}

// ungatedRPCMethods are the methods that stay answerable on an unhealthy node.
//
// The allowlist is deliberately inverted: a method is gated unless it appears
// here, so a method added later fails closed by default rather than silently
// inheriting the ability to answer from a diverged node.
//
// getGenesisHash is exempt because it reports which cluster this node was built
// for. That is fixed at bootstrap and independent of replay state, and it is
// exactly what an operator needs to read while diagnosing an unhealthy node.
var ungatedRPCMethods = map[string]struct{}{
	"getGenesisHash": {},
	// A health endpoint that the gate silences is a health endpoint that
	// reports nothing exactly when it matters. These two exist to describe
	// unhealth, so they must answer while unhealthy.
	"getVerificationStatus": {},
	"getHealth":             {},
}

func methodRequiresHealthyNode(method string) bool {
	_, exempt := ungatedRPCMethods[method]
	return !exempt
}

// evidenceGate reports why the node must refuse this method, or
// evidenceGateOpen if it may answer.
func (rpcServer *RpcServer) evidenceGate(method string) (evidenceGateReason, uint64, uint64) {
	if !methodRequiresHealthyNode(method) {
		return evidenceGateOpen, 0, 0
	}
	snapshot := rpcServer.verificationSnapshot
	if snapshot == nil {
		snapshot = defaultVerificationSnapshot
	}
	state, _, verified, eligible := snapshot()
	return gateForVerificationState(state), verified, eligible
}

// nodeUnhealthyErrorResponse carries the machine-readable reason. It is separate
// from rpcProbeErrorResponse because that shape has no data field, and adding
// one there would change every other error this server emits.
type nodeUnhealthyErrorResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    any    `json:"data"`
	} `json:"error"`
}

// marshalNodeUnhealthyError renders the refusal a caller receives. The body is
// built from NodeUnhealthyError.ToJSONRPCError so the wire shape has a single
// source of truth: the registered error type and this path cannot drift into
// disagreeing about the code or the data field.
//
// The response is deliberately HTTP 200 carrying a JSON-RPC error, never 503.
// Clients treat a 5xx as a retryable transport hiccup and a 200-with-error as a
// terminal answer, and a refusal must stick rather than invite a retry storm.
func marshalNodeUnhealthyError(
	id json.RawMessage, reason evidenceGateReason, verified, eligible uint64,
) json.RawMessage {
	typed := &NodeUnhealthyError{
		Reason:       string(reason),
		VerifiedSlot: verified,
		EligibleSlot: eligible,
	}
	encoded, err := typed.ToJSONRPCError()
	if err == nil {
		response := nodeUnhealthyErrorResponse{
			JSONRPC: "2.0",
			ID:      normalizedRPCProbeID(id),
		}
		response.Error.Code = int(encoded.Code)
		response.Error.Message = encoded.Message
		response.Error.Data = encoded.Data
		if payload, marshalErr := json.Marshal(response); marshalErr == nil {
			return payload
		}
	}
	// Refusing must never fall through to serving. If the structured form
	// cannot be built, emit a plain error carrying the same code.
	return marshalRPCProbeError(id, int(rpcCodeNodeUnhealthy),
		"Node is unhealthy")
}
