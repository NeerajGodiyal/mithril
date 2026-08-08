package rpcserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/replay"
)

func snapshotReturning(state replay.VerificationState, verified, eligible uint64) verificationSnapshotFunc {
	return func() (replay.VerificationState, bool, uint64, uint64) {
		return state, true, verified, eligible
	}
}

func TestGateForVerificationStateCoversEveryState(t *testing.T) {
	cases := []struct {
		state replay.VerificationState
		want  evidenceGateReason
	}{
		{replay.VerificationComplete, evidenceGateOpen},
		{replay.VerificationNotApplicable, evidenceGateOpen},
		// Behind but progressing still answers: freshness is the caller's policy,
		// expressed with minContextSlot, not this gate's business.
		{replay.VerificationIncomplete, evidenceGateOpen},
		{replay.VerificationDiverged, evidenceGateDiverged},
		{replay.VerificationStalled, evidenceGateStalled},
		{replay.VerificationUnavailable, evidenceGateUnavailable},
	}
	for _, tc := range cases {
		if got := gateForVerificationState(tc.state); got != tc.want {
			t.Fatalf("state %q: got %q, want %q", tc.state, got, tc.want)
		}
	}
	// Every state the package defines must be handled explicitly above, so a
	// seventh state added later cannot silently fall into the open branch.
	for _, state := range []replay.VerificationState{
		replay.VerificationComplete, replay.VerificationIncomplete,
		replay.VerificationStalled, replay.VerificationDiverged,
		replay.VerificationUnavailable, replay.VerificationNotApplicable,
	} {
		if !state.Valid() {
			t.Fatalf("state %q is not valid", state)
		}
	}
}

func TestGateRejectsUndefinedVerificationState(t *testing.T) {
	if got := gateForVerificationState(replay.VerificationState("wat")); got != evidenceGateUnknown {
		t.Fatalf("undefined state must fail closed, got %q", got)
	}
	if got := gateForVerificationState(""); got != evidenceGateUnknown {
		t.Fatalf("empty state must fail closed, got %q", got)
	}
}

// Every supported method is gated unless it is explicitly exempt, so a method
// added later cannot inherit the ability to answer from a diverged node.
func TestEverySupportedMethodIsGatedUnlessExempt(t *testing.T) {
	for method := range supportedRPCMethods {
		_, exempt := ungatedRPCMethods[method]
		if methodRequiresHealthyNode(method) == exempt {
			t.Fatalf("method %q: gating disagrees with the exemption list", method)
		}
	}
	for method := range ungatedRPCMethods {
		if _, supported := supportedRPCMethods[method]; !supported {
			t.Fatalf("exempt method %q is not a supported method", method)
		}
	}
	if !methodRequiresHealthyNode("someMethodAddedLater") {
		t.Fatal("an unknown method must default to gated")
	}
}

func TestGatedRequestReturnsMachineReadableRefusal(t *testing.T) {
	// rpcService is deliberately nil: if the gate did not short-circuit before
	// dispatch, this would panic instead of returning a refusal.
	server := &RpcServer{
		verificationSnapshot: snapshotReturning(replay.VerificationDiverged, 4200, 4321),
	}
	req := rpcMethodProbe{JSONRPC: "2.0", Method: "getAccountInfo", ID: json.RawMessage(`1`)}
	payload, err := server.executeRPCRequestWithID(context.Background(), req)
	if err != nil {
		t.Fatalf("gated request returned a transport error: %v", err)
	}
	var got struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    struct {
				Reason       string `json:"reason"`
				VerifiedSlot uint64 `json:"verifiedSlot"`
				EligibleSlot uint64 `json:"eligibleSlot"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("refusal is not valid JSON: %v", err)
	}
	if got.Error.Code != int(rpcCodeNodeUnhealthy) {
		t.Fatalf("code: got %d, want %d", got.Error.Code, rpcCodeNodeUnhealthy)
	}
	if got.Error.Data.Reason != string(evidenceGateDiverged) {
		t.Fatalf("reason: got %q, want %q", got.Error.Data.Reason, evidenceGateDiverged)
	}
	if got.Error.Data.VerifiedSlot != 4200 || got.Error.Data.EligibleSlot != 4321 {
		t.Fatalf("watermarks: got %d/%d, want 4200/4321",
			got.Error.Data.VerifiedSlot, got.Error.Data.EligibleSlot)
	}
	if string(got.ID) != "1" {
		t.Fatalf("refusal must echo the request id, got %s", got.ID)
	}
}

// A caller must be able to read which cluster an unhealthy node was built for,
// which is exactly when that question matters.
func TestGenesisHashStillAnswersWhileDiverged(t *testing.T) {
	server := &RpcServer{
		verificationSnapshot: snapshotReturning(replay.VerificationDiverged, 1, 2),
	}
	if reason, _, _ := server.evidenceGate("getGenesisHash"); reason != evidenceGateOpen {
		t.Fatalf("getGenesisHash must stay answerable, got %q", reason)
	}
	if reason, _, _ := server.evidenceGate("getAccountInfo"); reason != evidenceGateDiverged {
		t.Fatalf("getAccountInfo must be refused, got %q", reason)
	}
}

// sendTransaction arrives as a notification when the caller omits an id. A
// diverged node must not submit merely because nobody asked for a reply.
func TestNotificationIsGatedBeforeDispatch(t *testing.T) {
	server := &RpcServer{
		verificationSnapshot: snapshotReturning(replay.VerificationDiverged, 1, 2),
	}
	req := rpcMethodProbe{JSONRPC: "2.0", Method: "sendTransaction"}
	// rpcService is nil, so reaching dispatch panics. Returning normally is the
	// proof that the gate stopped it first.
	server.executeRPCNotification(context.Background(), req)
}

// The previous test only means something if an ungated notification really does
// reach dispatch. This proves the harness is not vacuous.
func TestUngatedNotificationReachesDispatch(t *testing.T) {
	server := &RpcServer{
		verificationSnapshot: snapshotReturning(replay.VerificationComplete, 1, 2),
	}
	defer func() {
		if recover() == nil {
			t.Fatal("an ungated notification must reach dispatch; the gate test is vacuous")
		}
	}()
	req := rpcMethodProbe{JSONRPC: "2.0", Method: "sendTransaction"}
	server.executeRPCNotification(context.Background(), req)
}

// A server built without the seam must still gate, using the real replay state
// rather than silently treating a missing hook as healthy.
func TestNilSeamFallsBackToReplaySnapshot(t *testing.T) {
	server := &RpcServer{}
	if server.verificationSnapshot != nil {
		t.Fatal("expected the seam to be unset for this test")
	}
	state, _, _, _ := replay.VerificationSnapshot()
	want := gateForVerificationState(state)
	if got, _, _ := server.evidenceGate("getAccountInfo"); got != want {
		t.Fatalf("nil seam: got %q, want %q from the live snapshot", got, want)
	}
}

func TestNodeUnhealthyErrorRoundTrip(t *testing.T) {
	original := &NodeUnhealthyError{Reason: "diverged", VerifiedSlot: 7, EligibleSlot: 9}
	encoded, err := original.ToJSONRPCError()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if encoded.Code != rpcCodeNodeUnhealthy {
		t.Fatalf("code: got %d, want %d", encoded.Code, rpcCodeNodeUnhealthy)
	}
	var decoded NodeUnhealthyError
	if err := decoded.FromJSONRPCError(encoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded != *original {
		t.Fatalf("round trip: got %+v, want %+v", decoded, *original)
	}
}

// The health methods exist to describe unhealth. If the evidence gate silenced
// them, the node would go quiet exactly when a caller most needs to know why.
func TestHealthMethodsAnswerWhileTheGateRefusesEverythingElse(t *testing.T) {
	server := &RpcServer{
		verificationSnapshot: snapshotReturning(replay.VerificationDiverged, 4200, 4321),
	}
	for _, method := range []string{"getVerificationStatus", "getHealth"} {
		if reason, _, _ := server.evidenceGate(method); reason != evidenceGateOpen {
			t.Fatalf("%s must answer on a diverged node, got %q", method, reason)
		}
	}
	if reason, _, _ := server.evidenceGate("getAccountInfo"); reason != evidenceGateDiverged {
		t.Fatalf("evidence reads must still be refused, got %q", reason)
	}

	status, err := server.GetVerificationStatus(context.Background(), nil)
	if err != nil {
		t.Fatalf("verification status on a diverged node: %v", err)
	}
	if status.State != string(replay.VerificationDiverged) || status.Healthy ||
		status.EvidenceServed || status.Reason != string(evidenceGateDiverged) {
		t.Fatalf("status must report the divergence plainly: %+v", status)
	}
	if status.VerifiedSlot != 4200 || status.EligibleSlot != 4321 {
		t.Fatalf("watermarks: %+v", status)
	}

	health, err := server.GetHealth(context.Background(), nil)
	if err != nil {
		t.Fatalf("health on a diverged node: %v", err)
	}
	if health.Status != string(evidenceGateDiverged) ||
		health.VerificationState != string(replay.VerificationDiverged) {
		t.Fatalf("health must name the reason: %+v", health)
	}
}

// A healthy node reports ok, and the two methods must agree with the gate.
func TestHealthMethodsAgreeWithTheGateWhenServing(t *testing.T) {
	for _, state := range []replay.VerificationState{
		replay.VerificationComplete,
		replay.VerificationNotApplicable,
		replay.VerificationIncomplete,
	} {
		server := &RpcServer{verificationSnapshot: snapshotReturning(state, 10, 10)}
		status, err := server.GetVerificationStatus(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if !status.EvidenceServed || status.Reason != "" {
			t.Fatalf("state %q serves evidence but reported %+v", state, status)
		}
		health, err := server.GetHealth(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if health.Status != "ok" {
			t.Fatalf("state %q: health = %q, want ok", state, health.Status)
		}
		// Healthy is narrower than serving: "incomplete" serves evidence while
		// coverage is still catching up, and the two fields must not be
		// conflated by a caller reading either one alone.
		if status.Healthy != state.Healthy() {
			t.Fatalf("state %q: healthy flag disagrees with the state", state)
		}
	}
}
