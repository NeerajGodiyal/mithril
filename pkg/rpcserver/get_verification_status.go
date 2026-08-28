package rpcserver

import (
	"context"

	"github.com/filecoin-project/go-jsonrpc"
)

// A verifying node knows something no RPC provider can know: whether its own
// replayed state agrees with what it verified. Until now it only said so by
// refusing — the evidence gate returns -32005 and a reason, but a caller had
// no way to ask before committing to a request.
//
// These two methods are the pre-flight. They are deliberately exempt from the
// evidence gate: a health endpoint that goes silent exactly when the node is
// unhealthy reports nothing at all.
type GetVerificationStatusResp struct {
	// State is one of the six verification states: complete, incomplete,
	// stalled, diverged, unavailable, not_applicable.
	State string `json:"state"`
	// Required is false when this mode does not run trailing verification, so
	// a caller can tell "nothing was required" from "everything checked out".
	Required bool `json:"required"`
	// VerifiedSlot is the watermark verification has reached; EligibleSlot is
	// how far it could have reached. Their gap is the coverage debt.
	VerifiedSlot uint64 `json:"verifiedSlot"`
	EligibleSlot uint64 `json:"eligibleSlot"`
	// Healthy means verification has caught up to its eligible watermark (or is
	// not required). It never replaces State or means the current processed bank
	// was verified; the watermarks describe the actual lagged coverage.
	Healthy bool `json:"healthy"`
	// EvidenceServed reports whether gate policy currently permits evidence
	// requests. Normal catch-up debt can leave this true while Healthy is false.
	// When false, Reason names why in the refusal's vocabulary.
	EvidenceServed bool   `json:"evidenceServed"`
	Reason         string `json:"reason,omitempty"`
}

func (rpcServer *RpcServer) GetVerificationStatus(
	_ context.Context, _ jsonrpc.RawParams,
) (GetVerificationStatusResp, error) {
	snapshot := rpcServer.verificationSnapshot
	if snapshot == nil {
		snapshot = defaultVerificationSnapshot
	}
	state, required, verified, eligible := snapshot()
	reason := gateForVerificationState(state)
	return GetVerificationStatusResp{
		State:          string(state),
		Required:       required,
		VerifiedSlot:   verified,
		EligibleSlot:   eligible,
		Healthy:        state.Healthy(),
		EvidenceServed: reason == evidenceGateOpen,
		Reason:         string(reason),
	}, nil
}

func (rpcServer *RpcServer) GetHealth(
	_ context.Context, _ jsonrpc.RawParams,
) (string, error) {
	// This standard service-health endpoint follows refusal policy: ordinary
	// catch-up lag stays available. Callers that need verification coverage use
	// getVerificationStatus and its explicit state and watermarks.
	snapshot := rpcServer.verificationSnapshot
	if snapshot == nil {
		snapshot = defaultVerificationSnapshot
	}
	state, _, verified, eligible := snapshot()
	if reason := gateForVerificationState(state); reason != evidenceGateOpen {
		return "", &NodeUnhealthyError{
			Reason:       string(reason),
			VerifiedSlot: verified,
			EligibleSlot: eligible,
		}
	}
	return "ok", nil
}
