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
	// Healthy is a convenience for callers that only branch two ways. It never
	// replaces State: the components are returned alongside it so nobody has
	// to make a decision from one boolean.
	Healthy bool `json:"healthy"`
	// EvidenceServed reports whether the node is currently answering evidence
	// requests at all. When false, Reason names why in the same vocabulary the
	// refusal itself uses.
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

// GetHealthResp deliberately does not imitate Agave's getHealth, which answers
// "am I near my peers' highest slot" — a liveness proxy this node cannot
// honestly compute, because it observes no cluster tip. This one answers a
// stronger and different question: does my own replayed state check out.
type GetHealthResp struct {
	// Status is "ok" when the node will serve evidence, otherwise the gate
	// reason: diverged, stalled, unavailable, unknown_verification_state.
	Status string `json:"status"`
	// VerificationState is the underlying six-state value, so a caller is
	// never forced to infer it from the status string.
	VerificationState string `json:"verificationState"`
	VerifiedSlot      uint64 `json:"verifiedSlot"`
}

func (rpcServer *RpcServer) GetHealth(
	_ context.Context, _ jsonrpc.RawParams,
) (GetHealthResp, error) {
	snapshot := rpcServer.verificationSnapshot
	if snapshot == nil {
		snapshot = defaultVerificationSnapshot
	}
	state, _, verified, _ := snapshot()
	status := "ok"
	if reason := gateForVerificationState(state); reason != evidenceGateOpen {
		status = string(reason)
	}
	return GetHealthResp{
		Status:            status,
		VerificationState: string(state),
		VerifiedSlot:      verified,
	}, nil
}
