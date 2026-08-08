package controlaudit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var testNow = time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

type testSummarySource interface {
	Summary() (Summary, error)
}

func testSummary(t testing.TB, source testSummarySource) Summary {
	t.Helper()
	summary, err := source.Summary()
	if err != nil {
		t.Fatalf("audit summary: %v", err)
	}
	return summary
}

type testApprovalClaims struct {
	Domain     string `json:"domain"`
	ActionID   string `json:"action_id"`
	SessionID  string `json:"session_id"`
	TargetID   string `json:"target_id"`
	Action     Action `json:"action"`
	Unit       string `json:"unit"`
	Scope      string `json:"scope"`
	BeforeHash string `json:"before_hash"`
	KeyID      string `json:"key_id"`
	Nonce      string `json:"nonce"`
	IssuedAt   int64  `json:"issued_at"`
	ExpiresAt  int64  `json:"expires_at"`
}

type testApprovalEnvelope struct {
	Claims testApprovalClaims `json:"claims"`
	Proof  []byte             `json:"proof"`
}

type testApprovalVerifier struct {
	publicKey ed25519.PublicKey
	keyID     string
	retired   bool
}

func newTestApprovalVerifier(publicKey ed25519.PublicKey) *testApprovalVerifier {
	return &testApprovalVerifier{
		publicKey: publicKey,
		keyID:     "operator-key-1",
	}
}

func (verifier *testApprovalVerifier) VerifyApproval(ctx context.Context, event Event) (ApprovalBinding, error) {
	if err := ctx.Err(); err != nil {
		return ApprovalBinding{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(event.ApprovalEvidence))
	decoder.DisallowUnknownFields()
	var envelope testApprovalEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return ApprovalBinding{}, errors.New("invalid approval envelope")
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return ApprovalBinding{}, errors.New("approval envelope has trailing data")
	}
	claimsBytes, err := json.Marshal(envelope.Claims)
	if err != nil || !ed25519.Verify(verifier.publicKey, claimsBytes, envelope.Proof) {
		return ApprovalBinding{}, errors.New("invalid approval proof")
	}
	claims := envelope.Claims
	if claims.Domain != ApprovalAuditDomain || claims.KeyID != verifier.keyID {
		return ApprovalBinding{}, errors.New("approval authority is not allowed")
	}
	evidenceHash := sha256.Sum256(event.ApprovalEvidence)
	return ApprovalBinding{
		SessionID:      claims.SessionID,
		TargetID:       claims.TargetID,
		ActionID:       claims.ActionID,
		Action:         claims.Action,
		Unit:           claims.Unit,
		Scope:          claims.Scope,
		BeforeHash:     claims.BeforeHash,
		ApproverKeyID:  claims.KeyID,
		IssuedAtUnix:   claims.IssuedAt,
		ExpiresAtUnix:  claims.ExpiresAt,
		EvidenceSHA256: hex.EncodeToString(evidenceHash[:]),
		CanStartAction: !verifier.retired,
	}, nil
}

func (*testApprovalVerifier) VerifyStateTransition(
	ctx context.Context,
	_ Event,
	_ Event,
) error {
	return ctx.Err()
}

func newTestEvent(t *testing.T, privateKey ed25519.PrivateKey, sequence uint64, previous string, phase Phase) Event {
	t.Helper()
	before := sha256.Sum256([]byte("before-state"))
	event := Event{
		Version:         ProtocolVersion,
		Sequence:        sequence,
		ID:              "event-" + strconv.FormatUint(sequence, 10),
		Timestamp:       CanonicalTimestamp(testNow.Add(time.Duration(sequence) * time.Second)),
		SessionID:       "session-1",
		TargetID:        "node-target-1",
		ActionID:        "action-1",
		Phase:           phase,
		Action:          ActionRestart,
		Unit:            "mithril.service",
		Scope:           "system",
		ApproverKeyID:   "operator-key-1",
		StateCheckpoint: []byte("{}"),
		BeforeHash:      hexHash(before),
		PreviousHash:    previous,
	}
	setTestEventLifecycle(t, &event, phase)
	claims := testApprovalClaims{
		Domain:     ApprovalAuditDomain,
		ActionID:   event.ActionID,
		SessionID:  event.SessionID,
		TargetID:   event.TargetID,
		Action:     event.Action,
		Unit:       event.Unit,
		Scope:      event.Scope,
		BeforeHash: event.BeforeHash,
		KeyID:      event.ApproverKeyID,
		Nonce:      "nonce-action-1",
		IssuedAt:   testNow.Add(-time.Minute).Unix(),
		ExpiresAt:  testNow.Add(time.Minute).Unix(),
	}
	claimsBytes, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := json.Marshal(testApprovalEnvelope{
		Claims: claims,
		Proof:  ed25519.Sign(privateKey, claimsBytes),
	})
	if err != nil {
		t.Fatal(err)
	}
	event.ApprovalEvidence = evidence
	sealed, err := Seal(event)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

func setTestEventLifecycle(t *testing.T, event *Event, phase Phase) {
	t.Helper()
	event.Phase = phase
	event.AfterHash = ""
	event.Outcome = ""
	event.ReasonCode = ""
	event.DispatchMayHaveOccurred = false
	event.DispatchAccepted = false
	switch phase {
	case PhasePrepared, PhaseDispatchStarted:
	case PhaseDispatched:
		event.DispatchMayHaveOccurred = true
		event.DispatchAccepted = true
	case PhaseVerifying:
		event.DispatchMayHaveOccurred = true
		event.DispatchAccepted = true
	case PhaseSucceeded:
		after := sha256.Sum256([]byte("after-state"))
		event.AfterHash = hexHash(after)
		event.Outcome = string(phase)
		event.ReasonCode = "postcondition_observed"
		event.DispatchMayHaveOccurred = true
		event.DispatchAccepted = true
	case PhaseFailed:
		after := sha256.Sum256([]byte("after-state"))
		event.AfterHash = hexHash(after)
		event.Outcome = string(phase)
		event.ReasonCode = "precondition_changed"
	case PhaseOutcomeUnknown:
		event.Outcome = string(phase)
		event.ReasonCode = "postcondition_deadline"
		event.DispatchMayHaveOccurred = true
	default:
		t.Fatalf("unsupported test phase %q", phase)
	}
}

func updateTestApproval(
	t *testing.T,
	event Event,
	privateKey ed25519.PrivateKey,
	update func(*testApprovalClaims, *testApprovalEnvelope),
) Event {
	t.Helper()
	var envelope testApprovalEnvelope
	if err := json.Unmarshal(event.ApprovalEvidence, &envelope); err != nil {
		t.Fatal(err)
	}
	update(&envelope.Claims, &envelope)
	claimsBytes, err := json.Marshal(envelope.Claims)
	if err != nil {
		t.Fatal(err)
	}
	envelope.Proof = ed25519.Sign(privateKey, claimsBytes)
	evidence, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	event.ApprovalEvidence = evidence
	event, err = Seal(event)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func hexHash(sum [sha256.Size]byte) string {
	return hex.EncodeToString(sum[:])
}

func testApprovalKeys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return publicKey, privateKey
}

func privateStorePath(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(directory, "control-audit.jsonl")
}

func TestCanonicalEventRejectsMutationAndAlternateEncoding(t *testing.T) {
	_, privateKey := testApprovalKeys(t)
	event := newTestEvent(t, privateKey, 1, "", PhaseDispatchStarted)
	encoded, err := MarshalEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseEvent(encoded); err != nil {
		t.Fatalf("parse canonical event: %v", err)
	}
	if _, err := ParseEvent(append([]byte(" "), encoded...)); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("alternate encoding error = %v", err)
	}
	mutated := event
	mutated.Action = ActionStop
	if err := mutated.Validate(); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("mutated hash error = %v", err)
	}
	mutated = event
	mutated.ApprovalEvidence = bytes.Repeat([]byte{1}, MaxApprovalEvidenceBytes+1)
	if _, err := Seal(mutated); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("oversized evidence error = %v", err)
	}
}

func TestEventIdentifierLengthMatchesApprovalProtocol(t *testing.T) {
	_, privateKey := testApprovalKeys(t)
	event := newTestEvent(t, privateKey, 1, "", PhaseDispatchStarted)
	event.ID = "e" + strings.Repeat("a", 126)
	if _, err := Seal(event); err != nil {
		t.Fatalf("127-byte identifier was rejected: %v", err)
	}
	event.ID += "a"
	if _, err := Seal(event); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("128-byte identifier error = %v", err)
	}
}

func TestEventLifecycleFields(t *testing.T) {
	_, privateKey := testApprovalKeys(t)
	after := sha256.Sum256([]byte("observed-after-state"))
	afterHash := hexHash(after)
	tests := []struct {
		name      string
		phase     Phase
		afterHash string
		outcome   string
		reason    string
		may       bool
		accepted  bool
		valid     bool
	}{
		{"prepared", PhasePrepared, "", "", "", false, false, true},
		{"dispatch started", PhaseDispatchStarted, "", "", "", false, false, true},
		{"dispatched", PhaseDispatched, "", "", "", true, true, true},
		{"verifying accepted", PhaseVerifying, "", "", "", true, true, true},
		{"verifying ambiguous", PhaseVerifying, "", "", "", true, false, false},
		{"succeeded", PhaseSucceeded, afterHash, "succeeded", "postcondition_observed", true, true, true},
		{"failed before dispatch", PhaseFailed, afterHash, "failed", "precondition_changed", false, false, true},
		{"failed after dispatch", PhaseFailed, afterHash, "failed", "postcondition_failed", true, true, false},
		{"unknown without after state", PhaseOutcomeUnknown, "", "outcome_unknown", "postcondition_deadline", true, false, true},
		{"unknown with after state", PhaseOutcomeUnknown, afterHash, "outcome_unknown", "postcondition_deadline", true, true, true},
		{"prepared with result", PhasePrepared, afterHash, "", "", false, false, false},
		{"dispatch started with attempt", PhaseDispatchStarted, "", "", "", true, false, false},
		{"dispatched without acceptance", PhaseDispatched, "", "", "", true, false, false},
		{"dispatched with result", PhaseDispatched, afterHash, "", "", true, true, false},
		{"verifying without attempt", PhaseVerifying, "", "", "", false, false, false},
		{"verifying with result", PhaseVerifying, "", "failed", "postcondition_failed", true, true, false},
		{"succeeded without after state", PhaseSucceeded, "", "succeeded", "postcondition_observed", true, true, false},
		{"succeeded with wrong outcome", PhaseSucceeded, afterHash, "failed", "postcondition_observed", true, true, false},
		{"succeeded without reason", PhaseSucceeded, afterHash, "succeeded", "", true, true, false},
		{"succeeded without attempt", PhaseSucceeded, afterHash, "succeeded", "postcondition_observed", false, false, false},
		{"failed without after state", PhaseFailed, "", "failed", "precondition_changed", false, false, false},
		{"failed with wrong outcome", PhaseFailed, afterHash, "succeeded", "precondition_changed", false, false, false},
		{"failed without reason", PhaseFailed, afterHash, "failed", "", false, false, false},
		{"unknown with wrong outcome", PhaseOutcomeUnknown, "", "failed", "postcondition_deadline", true, false, false},
		{"unknown without reason", PhaseOutcomeUnknown, "", "outcome_unknown", "", true, false, false},
		{"unknown without attempt", PhaseOutcomeUnknown, "", "outcome_unknown", "postcondition_deadline", false, false, false},
		{"accepted without attempt", PhaseFailed, afterHash, "failed", "precondition_changed", false, true, false},
		{"invalid result code", PhaseFailed, afterHash, "failed", "Bad-Code", false, false, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := newTestEvent(t, privateKey, 1, "", PhasePrepared)
			event.Phase = test.phase
			event.AfterHash = test.afterHash
			event.Outcome = test.outcome
			event.ReasonCode = test.reason
			event.DispatchMayHaveOccurred = test.may
			event.DispatchAccepted = test.accepted
			_, err := Seal(event)
			if (err == nil) != test.valid {
				t.Fatalf("Seal() error = %v, valid = %v", err, test.valid)
			}
		})
	}
}

func TestStoreDurabilityDuplicatesChainAndRestore(t *testing.T) {
	publicKey, privateKey := testApprovalKeys(t)
	verifier := newTestApprovalVerifier(publicKey)
	path := privateStorePath(t)
	store, err := OpenStore(path, verifier)
	if err != nil {
		t.Fatal(err)
	}
	first := newTestEvent(t, privateKey, 1, "", PhasePrepared)
	receipt, duplicate, err := store.Append(context.Background(), first)
	if err != nil || duplicate || receipt.validateFor(first) != nil {
		t.Fatalf("append first: receipt=%+v duplicate=%v err=%v", receipt, duplicate, err)
	}
	retryReceipt, duplicate, err := store.Append(context.Background(), first)
	if err != nil || !duplicate || retryReceipt != receipt {
		t.Fatalf("retry first: receipt=%+v duplicate=%v err=%v", retryReceipt, duplicate, err)
	}

	conflict := first
	conflict.Timestamp = CanonicalTimestamp(testNow.Add(30 * time.Second))
	conflict, err = Seal(conflict)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Append(context.Background(), conflict); !errors.Is(err, ErrConflictingDuplicate) {
		t.Fatalf("conflicting duplicate error = %v", err)
	}

	wrongSequence := newTestEvent(t, privateKey, 3, first.EventHash, PhaseSucceeded)
	if _, _, err := store.Append(context.Background(), wrongSequence); !errors.Is(err, ErrSequenceMismatch) {
		t.Fatalf("wrong sequence error = %v", err)
	}
	wrongChain := newTestEvent(t, privateKey, 2, strings.Repeat("0", 64), PhaseSucceeded)
	if _, _, err := store.Append(context.Background(), wrongChain); !errors.Is(err, ErrChainMismatch) {
		t.Fatalf("wrong chain error = %v", err)
	}

	second := newTestEvent(t, privateKey, 2, first.EventHash, PhaseFailed)
	if _, _, err := store.Append(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if got := testSummary(t, store); got.Records != 2 || got.TipHash != second.EventHash {
		t.Fatalf("summary = %+v", got)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	restored, summary, err := Restore(context.Background(), path, newTestApprovalVerifier(publicKey))
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 2 || !reflect.DeepEqual(restored[0], first) || !reflect.DeepEqual(restored[1], second) ||
		summary.Records != 2 || summary.TipHash != second.EventHash {
		t.Fatalf("restored=%+v summary=%+v", restored, summary)
	}
}

func TestStoreTruncatesShortAppendBeforePoisoning(t *testing.T) {
	publicKey, privateKey := testApprovalKeys(t)
	path := privateStorePath(t)
	store, err := OpenStore(path, newTestApprovalVerifier(publicKey))
	if err != nil {
		t.Fatal(err)
	}
	first := newTestEvent(t, privateKey, 1, "", PhasePrepared)
	if _, _, err := store.Append(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	want := testSummary(t, store)
	second := newTestEvent(t, privateKey, 2, first.EventHash, PhaseDispatchStarted)
	store.write = func(record []byte) (int, error) {
		written, err := store.file.Write(record[:len(record)/2])
		if err != nil {
			return written, err
		}
		return written, io.ErrShortWrite
	}
	if _, _, err := store.Append(context.Background(), second); !errors.Is(err, ErrStoreUncertain) {
		t.Fatalf("short append error = %v, want %v", err, ErrStoreUncertain)
	}
	if err := store.Close(); !errors.Is(err, ErrStoreUncertain) {
		t.Fatalf("close after short append = %v, want %v", err, ErrStoreUncertain)
	}

	reopened, err := OpenStore(path, newTestApprovalVerifier(publicKey))
	if err != nil {
		t.Fatalf("reopen after short append: %v", err)
	}
	defer reopened.Close()
	if got := testSummary(t, reopened); got != want {
		t.Fatalf("recovered summary = %+v, want %+v", got, want)
	}
}

func TestOpenStoreWithSnapshotReturnsStableVerifiedPrefix(t *testing.T) {
	publicKey, privateKey := testApprovalKeys(t)
	verifier := newTestApprovalVerifier(publicKey)
	path := privateStorePath(t)

	store, err := OpenStore(path, verifier)
	if err != nil {
		t.Fatal(err)
	}
	first := newTestEvent(t, privateKey, 1, "", PhasePrepared)
	if _, _, err := store.Append(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, events, summary, err := OpenStoreWithSnapshot(context.Background(), path, verifier)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if len(events) != 1 || !reflect.DeepEqual(events[0], first) {
		t.Fatalf("snapshot = %+v", events)
	}
	if current := testSummary(t, store); summary != current || summary.Records != 1 || summary.TipHash != first.EventHash ||
		summary.Bytes == 0 {
		t.Fatalf("snapshot summary=%+v store summary=%+v", summary, current)
	}

	second := newTestEvent(t, privateKey, 2, first.EventHash, PhaseDispatchStarted)
	if _, _, err := store.Append(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || summary.Records != 1 {
		t.Fatalf("snapshot changed after append: events=%d summary=%+v", len(events), summary)
	}
	if current := testSummary(t, store); current.Records != 2 || current.TipHash != second.EventHash ||
		current.Bytes <= summary.Bytes {
		t.Fatalf("current summary = %+v", current)
	}
}

func TestOpenStoreWithSnapshotHoldsExclusiveLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("secure store is Unix-only")
	}
	publicKey, _ := testApprovalKeys(t)
	verifier := newTestApprovalVerifier(publicKey)
	path := privateStorePath(t)

	store, _, _, err := OpenStoreWithSnapshot(context.Background(), path, verifier)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := OpenStoreWithSnapshot(context.Background(), path, verifier); err == nil {
		t.Fatal("second writer acquired the store")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, events, summary, err := OpenStoreWithSnapshot(context.Background(), path, verifier)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if len(events) != 0 || summary != (Summary{}) {
		t.Fatalf("reopened snapshot=%+v summary=%+v", events, summary)
	}
}

func TestOpenStoreWithSnapshotRejectsTamperedChain(t *testing.T) {
	publicKey, privateKey := testApprovalKeys(t)
	verifier := newTestApprovalVerifier(publicKey)
	path := privateStorePath(t)
	store, err := OpenStore(path, verifier)
	if err != nil {
		t.Fatal(err)
	}
	event := newTestEvent(t, privateKey, 1, "", PhasePrepared)
	if _, _, err := store.Append(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(contents, []byte(`"action":"restart"`), []byte(`"action":"stop"`), 1)
	if bytes.Equal(tampered, contents) {
		t.Fatal("test did not alter the stored event")
	}
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := OpenStoreWithSnapshot(context.Background(), path, verifier); err == nil {
		t.Fatal("tampered store opened")
	}
}

func TestStoreLimitsApplyAtAppendAndOpen(t *testing.T) {
	publicKey, privateKey := testApprovalKeys(t)
	verifier := newTestApprovalVerifier(publicKey)

	t.Run("records", func(t *testing.T) {
		path := privateStorePath(t)
		limits := StoreLimits{
			MaxBytes:   DefaultMaxStoreBytes,
			MaxRecords: 1,
		}
		store, err := OpenStoreWithLimits(path, verifier, limits)
		if err != nil {
			t.Fatal(err)
		}
		first := newTestEvent(t, privateKey, 1, "", PhasePrepared)
		receipt, duplicate, err := store.Append(context.Background(), first)
		if err != nil || duplicate {
			t.Fatalf("first append: receipt=%+v duplicate=%v err=%v", receipt, duplicate, err)
		}
		if retry, duplicate, err := store.Append(context.Background(), first); err != nil ||
			!duplicate || retry != receipt {
			t.Fatalf("duplicate at limit: receipt=%+v duplicate=%v err=%v", retry, duplicate, err)
		}
		second := newTestEvent(t, privateKey, 2, first.EventHash, PhaseFailed)
		if _, _, err := store.Append(context.Background(), second); !errors.Is(err, ErrStoreLimit) {
			t.Fatalf("record limit error = %v", err)
		}
		if summary := testSummary(t, store); summary.Records != 1 ||
			summary.TipHash != first.EventHash {
			t.Fatalf("record-limited summary = %+v", summary)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := OpenStoreWithLimits(path, verifier, StoreLimits{
			MaxBytes:   DefaultMaxStoreBytes,
			MaxRecords: 1,
		})
		if err != nil {
			t.Fatalf("store at exact record limit did not reopen: %v", err)
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("records at open", func(t *testing.T) {
		path := privateStorePath(t)
		store, err := OpenStore(path, verifier)
		if err != nil {
			t.Fatal(err)
		}
		first := newTestEvent(t, privateKey, 1, "", PhasePrepared)
		second := newTestEvent(t, privateKey, 2, first.EventHash, PhaseFailed)
		if _, _, err := store.Append(context.Background(), first); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Append(context.Background(), second); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenStoreWithLimits(path, verifier, StoreLimits{
			MaxBytes:   DefaultMaxStoreBytes,
			MaxRecords: 1,
		}); !errors.Is(err, ErrStoreLimit) {
			t.Fatalf("over-record store open error = %v", err)
		}
	})

	t.Run("bytes", func(t *testing.T) {
		path := privateStorePath(t)
		first := newTestEvent(t, privateKey, 1, "", PhasePrepared)
		encoded, err := MarshalEvent(first)
		if err != nil {
			t.Fatal(err)
		}
		exact := uint64(len(encoded) + 1)
		store, err := OpenStoreWithLimits(path, verifier, StoreLimits{
			MaxBytes:   exact,
			MaxRecords: DefaultMaxStoreRecords,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Append(context.Background(), first); err != nil {
			t.Fatal(err)
		}
		second := newTestEvent(t, privateKey, 2, first.EventHash, PhaseFailed)
		if _, _, err := store.Append(context.Background(), second); !errors.Is(err, ErrStoreLimit) {
			t.Fatalf("byte limit error = %v", err)
		}
		if summary := testSummary(t, store); summary.Bytes != exact || summary.Records != 1 {
			t.Fatalf("byte-limited summary = %+v", summary)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenStoreWithLimits(path, verifier, StoreLimits{
			MaxBytes:   exact - 1,
			MaxRecords: DefaultMaxStoreRecords,
		}); !errors.Is(err, ErrStoreLimit) {
			t.Fatalf("oversized store open error = %v", err)
		}
	})
}

func TestStoreRejectsApprovalMismatchBeforeAppend(t *testing.T) {
	publicKey, privateKey := testApprovalKeys(t)
	path := privateStorePath(t)
	store, err := OpenStore(path, newTestApprovalVerifier(publicKey))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	event := newTestEvent(t, privateKey, 1, "", PhasePrepared)
	event.TargetID = "different-target"
	event, err = Seal(event)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Append(context.Background(), event); !errors.Is(err, ErrApprovalRejected) {
		t.Fatalf("approval mismatch error = %v", err)
	}
	if testSummary(t, store).Records != 0 {
		t.Fatal("rejected approval changed the store")
	}
}

func TestConcurrentDuplicateIsStoredOnce(t *testing.T) {
	publicKey, privateKey := testApprovalKeys(t)
	store, err := OpenStore(privateStorePath(t), newTestApprovalVerifier(publicKey))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	event := newTestEvent(t, privateKey, 1, "", PhasePrepared)

	const callers = 24
	results := make(chan error, callers)
	var firstsMu sync.Mutex
	firsts := 0
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, duplicate, err := store.Append(context.Background(), event)
			if err == nil && !duplicate {
				firstsMu.Lock()
				firsts++
				firstsMu.Unlock()
			}
			results <- err
		}()
	}
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent append: %v", err)
		}
	}
	if summary := testSummary(t, store); firsts != 1 || summary.Records != 1 {
		t.Fatalf("firsts=%d summary=%+v", firsts, summary)
	}
}

func TestStoreRejectsWrongDomainProofKeyAndReplay(t *testing.T) {
	publicKey, privateKey := testApprovalKeys(t)
	tests := []struct {
		name   string
		mutate func(Event) Event
	}{
		{
			name: "cross domain",
			mutate: func(event Event) Event {
				return updateTestApproval(t, event, privateKey, func(claims *testApprovalClaims, _ *testApprovalEnvelope) {
					claims.Domain = "different.domain"
				})
			},
		},
		{
			name: "unsupported key claim",
			mutate: func(event Event) Event {
				event.ApproverKeyID = "other-key"
				return updateTestApproval(t, event, privateKey, func(claims *testApprovalClaims, _ *testApprovalEnvelope) {
					claims.KeyID = event.ApproverKeyID
				})
			},
		},
		{
			name: "invalid proof",
			mutate: func(event Event) Event {
				var envelope testApprovalEnvelope
				if err := json.Unmarshal(event.ApprovalEvidence, &envelope); err != nil {
					t.Fatal(err)
				}
				envelope.Proof[0] ^= 0xff
				evidence, err := json.Marshal(envelope)
				if err != nil {
					t.Fatal(err)
				}
				event.ApprovalEvidence = evidence
				event, err = Seal(event)
				if err != nil {
					t.Fatal(err)
				}
				return event
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, err := OpenStore(privateStorePath(t), newTestApprovalVerifier(publicKey))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			event := test.mutate(newTestEvent(t, privateKey, 1, "", PhasePrepared))
			if _, _, err := store.Append(context.Background(), event); !errors.Is(err, ErrApprovalRejected) {
				t.Fatalf("approval error = %v", err)
			}
			if testSummary(t, store).Records != 0 {
				t.Fatal("rejected approval changed the store")
			}
		})
	}

	verifier := newTestApprovalVerifier(publicKey)
	store, err := OpenStore(privateStorePath(t), verifier)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first := newTestEvent(t, privateKey, 1, "", PhasePrepared)
	if _, _, err := store.Append(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	replay := newTestEvent(t, privateKey, 2, first.EventHash, PhaseDispatchStarted)
	replay.ActionID = "action-2"
	replay.ID = "event-replay"
	replay, err = Seal(replay)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Append(context.Background(), replay); !errors.Is(err, ErrApprovalRejected) {
		t.Fatalf("replayed nonce error = %v", err)
	}
	if testSummary(t, store).Records != 1 {
		t.Fatal("replayed approval changed the store")
	}
}

func TestRestoreChecksSignedEventTimeNotWallClock(t *testing.T) {
	publicKey, privateKey := testApprovalKeys(t)
	path := privateStorePath(t)
	store, err := OpenStore(path, newTestApprovalVerifier(publicKey))
	if err != nil {
		t.Fatal(err)
	}
	event := newTestEvent(t, privateKey, 1, "", PhasePrepared)
	historical := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	event.Timestamp = CanonicalTimestamp(historical)
	event = updateTestApproval(t, event, privateKey, func(claims *testApprovalClaims, _ *testApprovalEnvelope) {
		claims.IssuedAt = historical.Add(-time.Minute).Unix()
		claims.ExpiresAt = historical.Add(time.Minute).Unix()
	})
	if _, _, err := store.Append(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	terminal := newTestEvent(t, privateKey, 2, event.EventHash, PhaseFailed)
	terminal.Timestamp = CanonicalTimestamp(historical.Add(24 * time.Hour))
	terminal.ApprovalEvidence = bytes.Clone(event.ApprovalEvidence)
	terminal, err = Seal(terminal)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Append(context.Background(), terminal); err != nil {
		t.Fatalf("terminal event after approval expiry: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	events, _, err := Restore(context.Background(), path, newTestApprovalVerifier(publicKey))
	if err != nil {
		t.Fatalf("historical restore: %v", err)
	}
	if len(events) != 2 ||
		events[0].Timestamp != CanonicalTimestamp(historical) ||
		events[1].Timestamp != CanonicalTimestamp(historical.Add(24*time.Hour)) {
		t.Fatalf("events = %+v", events)
	}
}

func TestStoreRejectsInitialEventOutsideApprovalWindowAndInvalidPhase(t *testing.T) {
	publicKey, privateKey := testApprovalKeys(t)
	store, err := OpenStore(privateStorePath(t), newTestApprovalVerifier(publicKey))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	late := newTestEvent(t, privateKey, 1, "", PhasePrepared)
	late.Timestamp = CanonicalTimestamp(testNow.Add(2 * time.Minute))
	late, err = Seal(late)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Append(context.Background(), late); !errors.Is(err, ErrApprovalRejected) {
		t.Fatalf("late initial event error = %v", err)
	}

	first := newTestEvent(t, privateKey, 1, "", PhasePrepared)
	if _, _, err := store.Append(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	invalid := newTestEvent(t, privateKey, 2, first.EventHash, PhaseSucceeded)
	if _, _, err := store.Append(context.Background(), invalid); !errors.Is(err, ErrInvalidPhase) {
		t.Fatalf("invalid phase error = %v", err)
	}
}

func TestStoreHoldsOutcomeUnknownUntilApprovalBarrierExpires(t *testing.T) {
	publicKey, privateKey := testApprovalKeys(t)
	path := privateStorePath(t)
	store, err := OpenStore(path, newTestApprovalVerifier(publicKey))
	if err != nil {
		t.Fatal(err)
	}

	prepared := newTestEvent(t, privateKey, 1, "", PhasePrepared)
	if _, _, err := store.Append(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	dispatchStarted := newTestEvent(t, privateKey, 2, prepared.EventHash, PhaseDispatchStarted)
	dispatchStarted.ApprovalEvidence = bytes.Clone(prepared.ApprovalEvidence)
	dispatchStarted, err = Seal(dispatchStarted)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Append(context.Background(), dispatchStarted); err != nil {
		t.Fatal(err)
	}
	unknown := newTestEvent(t, privateKey, 3, dispatchStarted.EventHash, PhaseOutcomeUnknown)
	unknown.ApprovalEvidence = bytes.Clone(prepared.ApprovalEvidence)
	unknown, err = Seal(unknown)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Append(context.Background(), unknown); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(path, newTestApprovalVerifier(publicKey))
	if err != nil {
		t.Fatalf("reopen store with unknown outcome: %v", err)
	}
	defer store.Close()

	nextAction := newTestEvent(t, privateKey, 4, unknown.EventHash, PhasePrepared)
	nextAction.ID = "event-next-action"
	nextAction.ActionID = "action-2"
	nextAction = updateTestApproval(
		t,
		nextAction,
		privateKey,
		func(claims *testApprovalClaims, _ *testApprovalEnvelope) {
			claims.ActionID = nextAction.ActionID
			claims.Nonce = "nonce-action-2"
		},
	)
	if _, _, err := store.Append(context.Background(), nextAction); !errors.Is(err, ErrInvalidPhase) {
		t.Fatalf("new action after unknown outcome error = %v", err)
	}

	verifying := newTestEvent(t, privateKey, 4, unknown.EventHash, PhaseVerifying)
	verifying.ApprovalEvidence = bytes.Clone(prepared.ApprovalEvidence)
	verifying.DispatchAccepted = true
	verifying, err = Seal(verifying)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Append(context.Background(), verifying); !errors.Is(err, ErrInvalidPhase) {
		t.Fatalf("outcome_unknown resumed verification: %v", err)
	}

	nextAction.Timestamp = CanonicalTimestamp(testNow.Add(MaxApprovalWindow + time.Second))
	nextAction = updateTestApproval(
		t,
		nextAction,
		privateKey,
		func(claims *testApprovalClaims, _ *testApprovalEnvelope) {
			claims.IssuedAt = testNow.Add(MaxApprovalWindow).Unix()
			claims.ExpiresAt = testNow.Add(MaxApprovalWindow + time.Minute).Unix()
		},
	)
	store.now = func() time.Time {
		return testNow.Add(MaxApprovalWindow - time.Second)
	}
	if _, _, err := store.Append(context.Background(), nextAction); !errors.Is(err, ErrInvalidPhase) {
		t.Fatalf("future-dated action bypassed receiver hold: %v", err)
	}
	store.now = func() time.Time {
		return testNow.Add(MaxApprovalWindow + time.Second)
	}
	if _, _, err := store.Append(context.Background(), nextAction); err != nil {
		t.Fatalf("new action after approval barrier: %v", err)
	}
}

func TestStoreRetiredApproverFinishesExistingActionButCannotStartAnother(
	t *testing.T,
) {
	publicKey, privateKey := testApprovalKeys(t)
	path := privateStorePath(t)
	activeVerifier := newTestApprovalVerifier(publicKey)
	store, err := OpenStore(path, activeVerifier)
	if err != nil {
		t.Fatal(err)
	}
	prepared := newTestEvent(t, privateKey, 1, "", PhasePrepared)
	if _, _, err := store.Append(t.Context(), prepared); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	retiredVerifier := newTestApprovalVerifier(publicKey)
	retiredVerifier.retired = true
	store, err = OpenStore(path, retiredVerifier)
	if err != nil {
		t.Fatalf("historical prepared event failed after retirement: %v", err)
	}
	failed := newTestEvent(t, privateKey, 2, prepared.EventHash, PhaseFailed)
	failed.ApprovalEvidence = bytes.Clone(prepared.ApprovalEvidence)
	failed, err = Seal(failed)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Append(t.Context(), failed); err != nil {
		t.Fatalf("existing action could not finish after retirement: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenStore(path, retiredVerifier)
	if err != nil {
		t.Fatalf("completed historical action failed after retirement: %v", err)
	}
	defer store.Close()
	store.now = func() time.Time {
		return testNow.Add(MaxApprovalWindow + 2*time.Second)
	}
	next := newTestEvent(t, privateKey, 3, failed.EventHash, PhasePrepared)
	next.ID = "event-retired-new-action"
	next.ActionID = "action-retired-new"
	next.Timestamp = CanonicalTimestamp(testNow.Add(MaxApprovalWindow + 2*time.Second))
	next = updateTestApproval(
		t,
		next,
		privateKey,
		func(claims *testApprovalClaims, _ *testApprovalEnvelope) {
			claims.ActionID = next.ActionID
			claims.Nonce = "nonce-retired-new"
			claims.IssuedAt = testNow.Add(MaxApprovalWindow + time.Second).Unix()
			claims.ExpiresAt = testNow.Add(MaxApprovalWindow + time.Minute).Unix()
		},
	)
	if _, _, err := store.Append(t.Context(), next); !errors.Is(
		err,
		ErrApprovalRejected,
	) {
		t.Fatalf("retired approver started a new action: %v", err)
	}
}

func TestStoreRejectsTamperTruncationSymlinkAndSecondWriter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("secure store is Unix-only")
	}
	publicKey, privateKey := testApprovalKeys(t)
	path := privateStorePath(t)
	store, err := OpenStore(path, newTestApprovalVerifier(publicKey))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(path, newTestApprovalVerifier(publicKey)); err == nil {
		t.Fatal("second receiver acquired the store")
	}
	event := newTestEvent(t, privateKey, 1, "", PhasePrepared)
	if _, _, err := store.Append(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.TrimSuffix(contents, []byte{'\n'}), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Restore(context.Background(), path, newTestApprovalVerifier(publicKey)); err == nil {
		t.Fatal("truncated store verified")
	}
	if err := os.WriteFile(path, bytes.Replace(contents, []byte(`"action":"restart"`), []byte(`"action":"stop"`), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Restore(context.Background(), path, newTestApprovalVerifier(publicKey)); err == nil {
		t.Fatal("tampered store verified")
	}

	target := privateStorePath(t)
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(target), "link.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(link, newTestApprovalVerifier(publicKey)); err == nil {
		t.Fatal("symlink store opened")
	}

	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	realParent := filepath.Join(base, "real")
	nested := filepath.Join(realParent, "nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	ancestorLink := filepath.Join(base, "ancestor-link")
	if err := os.Symlink(realParent, ancestorLink); err != nil {
		t.Fatal(err)
	}
	ancestorPath := filepath.Join(ancestorLink, "nested", "control-audit.jsonl")
	if _, err := OpenStore(ancestorPath, newTestApprovalVerifier(publicKey)); err == nil {
		t.Fatal("store beneath a symlinked ancestor opened")
	}
}

func TestStoreRejectsLiveSameInodeMutation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("secure store is Unix-only")
	}
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "truncate",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Truncate(path, info.Size()-1); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "same length rewrite with restored mtime",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				file, err := os.OpenFile(path, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				changed := []byte{'x'}
				if _, err := file.WriteAt(changed, info.Size()/2); err != nil {
					_ = file.Close()
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
				after, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
					t.Fatal("same-length mutation fixture changed size or mtime")
				}
			},
		},
	} {
		for _, operation := range []string{"summary", "duplicate", "append"} {
			t.Run(test.name+"/"+operation, func(t *testing.T) {
				publicKey, privateKey := testApprovalKeys(t)
				path := privateStorePath(t)
				store, err := OpenStore(path, newTestApprovalVerifier(publicKey))
				if err != nil {
					t.Fatal(err)
				}
				first := newTestEvent(t, privateKey, 1, "", PhasePrepared)
				if _, _, err := store.Append(context.Background(), first); err != nil {
					t.Fatal(err)
				}
				test.mutate(t, path)

				switch operation {
				case "summary":
					_, err = store.Summary()
				case "duplicate":
					_, _, err = store.Append(context.Background(), first)
				case "append":
					second := newTestEvent(
						t,
						privateKey,
						2,
						first.EventHash,
						PhaseDispatchStarted,
					)
					second.ApprovalEvidence = bytes.Clone(first.ApprovalEvidence)
					second, err = Seal(second)
					if err == nil {
						_, _, err = store.Append(context.Background(), second)
					}
				}
				if !errors.Is(err, ErrStoreUncertain) {
					t.Fatalf("%s after live mutation error = %v", operation, err)
				}
				if store.summary.Records != 1 {
					t.Fatalf("live mutation changed acknowledged record count to %d", store.summary.Records)
				}
				if err := store.Close(); !errors.Is(err, ErrStoreUncertain) {
					t.Fatalf("close after live mutation error = %v", err)
				}
			})
		}
	}
}

func TestStorePeriodicFullVerification(t *testing.T) {
	publicKey, privateKey := testApprovalKeys(t)
	path := privateStorePath(t)
	store, err := OpenStore(path, newTestApprovalVerifier(publicKey))
	if err != nil {
		t.Fatal(err)
	}
	first := newTestEvent(t, privateKey, 1, "", PhasePrepared)
	if _, _, err := store.Append(context.Background(), first); err != nil {
		t.Fatal(err)
	}

	offset := int64(store.summary.Bytes / 2)
	var original [1]byte
	if _, err := store.file.ReadAt(original[:], offset); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{original[0] ^ 0xff}, offset); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	// Model a filesystem whose change token missed the rewrite. The ordinary
	// O(1) check accepts the token, while the scheduled digest still catches
	// the modified bytes.
	store.mu.Lock()
	store.change, err = store.inspectOpenFile(store.summary.Bytes)
	store.verified = time.Now()
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Summary(); err != nil {
		t.Fatalf("ordinary summary performed an unexpected full scan: %v", err)
	}

	store.mu.Lock()
	store.verified = time.Time{}
	store.mu.Unlock()
	if _, err := store.Summary(); !errors.Is(err, ErrStoreUncertain) {
		t.Fatalf("periodic full verification error = %v", err)
	}
	if err := store.Close(); !errors.Is(err, ErrStoreUncertain) {
		t.Fatalf("close after periodic verification failure = %v", err)
	}
}

func TestStoreRejectsAncestorReplacedBySymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("secure store is Unix-only")
	}
	publicKey, privateKey := testApprovalKeys(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(base, "parent")
	nested := filepath.Join(parent, "nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(nested, "control-audit.jsonl")
	store, err := OpenStore(path, newTestApprovalVerifier(publicKey))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	relocated := filepath.Join(base, "relocated")
	if err := os.Rename(parent, relocated); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(relocated, parent); err != nil {
		t.Fatal(err)
	}
	event := newTestEvent(t, privateKey, 1, "", PhasePrepared)
	if _, _, err := store.Append(context.Background(), event); !errors.Is(err, ErrStoreUncertain) {
		t.Fatalf("append after ancestor replacement error = %v", err)
	}
}

type tlsMaterial struct {
	roots           *x509.CertPool
	server          tls.Certificate
	serverLeaf      *x509.Certificate
	client          tls.Certificate
	clientLeaf      *x509.Certificate
	otherClient     tls.Certificate
	otherClientLeaf *x509.Certificate
}

func newTLSMaterial(t *testing.T) tlsMaterial {
	t.Helper()
	certificateTime := time.Now().UTC()
	caPublic, caPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "audit-test-ca"},
		NotBefore:             certificateTime.Add(-time.Hour),
		NotAfter:              certificateTime.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, caPublic, caPrivate)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	server, serverLeaf := makeLeaf(t, ca, caPrivate, certificateTime, 2, "audit.test", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	client, clientLeaf := makeLeaf(t, ca, caPrivate, certificateTime, 3, "node-client", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	otherClient, otherClientLeaf := makeLeaf(t, ca, caPrivate, certificateTime, 4, "other-client", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	return tlsMaterial{
		roots:           roots,
		server:          server,
		serverLeaf:      serverLeaf,
		client:          client,
		clientLeaf:      clientLeaf,
		otherClient:     otherClient,
		otherClientLeaf: otherClientLeaf,
	}
}

func makeLeaf(
	t *testing.T,
	ca *x509.Certificate,
	caPrivate ed25519.PrivateKey,
	certificateTime time.Time,
	serial int64,
	name string,
	usage []x509.ExtKeyUsage,
) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    certificateTime.Add(-time.Hour),
		NotAfter:     certificateTime.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  usage,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, publicKey, caPrivate)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{
		Certificate: [][]byte{der, ca.Raw},
		PrivateKey:  privateKey,
		Leaf:        leaf,
	}, leaf
}

type runningReceiver struct {
	receiver *Receiver
	listener net.Listener
	done     chan error
	once     sync.Once
}

type readCountingReader struct {
	reads int
}

func (reader *readCountingReader) Read([]byte) (int, error) {
	reader.reads++
	return 0, io.EOF
}

func newUnservedTestReceiver(
	t *testing.T,
	material tlsMaterial,
	verifier ApprovalVerifier,
) *Receiver {
	t.Helper()
	clientPin, err := PinCertificate(material.clientLeaf)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := NewReceiver(ReceiverConfig{
		StorePath:         privateStorePath(t),
		Certificate:       material.server,
		ClientCAs:         material.roots,
		AllowedClientPins: []PublicKeyPin{clientPin},
		ExpectedTargetID:  "node-target-1",
		ExpectedUnit:      "mithril.service",
		ExpectedScope:     "system",
	}, verifier)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = receiver.Close() })
	return receiver
}

func startReceiver(
	t *testing.T,
	path string,
	material tlsMaterial,
	verifier ApprovalVerifier,
) (*runningReceiver, *Client) {
	return startReceiverWithLimits(t, path, material, verifier, StoreLimits{})
}

func startReceiverWithLimits(
	t *testing.T,
	path string,
	material tlsMaterial,
	verifier ApprovalVerifier,
	limits StoreLimits,
) (*runningReceiver, *Client) {
	t.Helper()
	clientPin, err := PinCertificate(material.clientLeaf)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := NewReceiver(ReceiverConfig{
		StorePath:         path,
		Certificate:       material.server,
		ClientCAs:         material.roots,
		AllowedClientPins: []PublicKeyPin{clientPin},
		ExpectedTargetID:  "node-target-1",
		ExpectedUnit:      "mithril.service",
		ExpectedScope:     "system",
		StoreLimits:       limits,
	}, verifier)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	running := &runningReceiver{receiver: receiver, listener: listener, done: make(chan error, 1)}
	go func() {
		running.done <- receiver.Serve(listener)
	}()
	serverPin, err := PinCertificate(material.serverLeaf)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		Endpoint:     "https://" + listener.Addr().String(),
		Certificate:  material.client,
		ServerRoots:  material.roots,
		ServerName:   "audit.test",
		ServerKeyPin: serverPin,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		client.Close()
		running.stop()
	})
	return running, client
}

func TestReceiverRejectsAdmissionOverloadBeforeReadingBody(t *testing.T) {
	publicKey, _ := testApprovalKeys(t)
	material := newTLSMaterial(t)

	assertOverloaded := func(t *testing.T, receiver *Receiver) {
		t.Helper()
		body := &readCountingReader{}
		request := httptest.NewRequest(
			http.MethodPost,
			"https://audit.test"+eventPath,
			body,
		)
		request.Header.Set("Content-Type", "application/json")
		request.TLS = &tls.ConnectionState{
			VerifiedChains: [][]*x509.Certificate{{material.clientLeaf}},
		}
		response := httptest.NewRecorder()
		receiver.ServeHTTP(response, request)

		if response.Code != http.StatusTooManyRequests ||
			response.Header().Get("Retry-After") != receiverOverloadRetrySeconds ||
			response.Body.String() != `{"error":"receiver_overloaded"}`+"\n" {
			t.Fatalf(
				"overload response status=%d retry=%q body=%q",
				response.Code,
				response.Header().Get("Retry-After"),
				response.Body.String(),
			)
		}
		if body.reads != 0 {
			t.Fatalf("overloaded request body reads = %d, want 0", body.reads)
		}
		if summary := testSummary(t, receiver); summary != (Summary{}) {
			t.Fatalf("overloaded request changed store summary: %+v", summary)
		}
	}

	t.Run("concurrent_request_cap", func(t *testing.T) {
		receiver := newUnservedTestReceiver(t, material, newTestApprovalVerifier(publicKey))
		for range cap(receiver.requestSlots) {
			receiver.requestSlots <- struct{}{}
		}
		assertOverloaded(t, receiver)
		for range cap(receiver.requestSlots) {
			<-receiver.requestSlots
		}
	})

	t.Run("request_rate", func(t *testing.T) {
		receiver := newUnservedTestReceiver(t, material, newTestApprovalVerifier(publicKey))
		now := testNow
		receiver.requestTokens = newRequestTokenBucket(
			receiverRequestsPerSecond,
			receiverRequestBurst,
			func() time.Time { return now },
		)
		for range receiverRequestBurst {
			if !receiver.requestTokens.allow() {
				t.Fatal("initial request burst was not admitted")
			}
		}
		assertOverloaded(t, receiver)
		if len(receiver.requestSlots) != 0 {
			t.Fatal("rate rejection retained a concurrent request slot")
		}
	})
}

func TestRequestTokenBucketRefillsAndBoundsBurst(t *testing.T) {
	now := testNow
	bucket := newRequestTokenBucket(
		receiverRequestsPerSecond,
		receiverRequestBurst,
		func() time.Time { return now },
	)
	for range receiverRequestBurst {
		if !bucket.allow() {
			t.Fatal("initial token burst was unavailable")
		}
	}
	if bucket.allow() {
		t.Fatal("request beyond initial burst was admitted")
	}

	now = now.Add(time.Second/time.Duration(receiverRequestsPerSecond) + time.Nanosecond)
	if !bucket.allow() || bucket.allow() {
		t.Fatal("token bucket did not refill exactly one request")
	}

	now = now.Add(24 * time.Hour)
	for range receiverRequestBurst {
		if !bucket.allow() {
			t.Fatal("long refill did not restore the bounded burst")
		}
	}
	if bucket.allow() {
		t.Fatal("long refill exceeded the burst cap")
	}

	concurrent := newRequestTokenBucket(
		receiverRequestsPerSecond,
		receiverRequestBurst,
		func() time.Time { return testNow },
	)
	const callers = 64
	start := make(chan struct{})
	results := make(chan bool, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			results <- concurrent.allow()
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	admitted := 0
	for allowed := range results {
		if allowed {
			admitted++
		}
	}
	if admitted != receiverRequestBurst {
		t.Fatalf("concurrent admitted requests = %d, want %d", admitted, receiverRequestBurst)
	}
}

func TestConnectionLimitListenerCapsLiveConnections(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := newConnectionLimitListener(base, 1)
	t.Cleanup(func() { _ = listener.Close() })

	firstClient, err := net.DialTimeout("tcp", base.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer firstClient.Close()
	firstServer, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if len(listener.slots) != 1 {
		t.Fatalf("live connection slots = %d, want 1", len(listener.slots))
	}

	secondClient, err := net.DialTimeout("tcp", base.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer secondClient.Close()
	type acceptResult struct {
		connection net.Conn
		err        error
	}
	started := make(chan struct{})
	accepted := make(chan acceptResult, 1)
	go func() {
		close(started)
		connection, err := listener.Accept()
		accepted <- acceptResult{connection: connection, err: err}
	}()
	<-started
	select {
	case result := <-accepted:
		if result.connection != nil {
			_ = result.connection.Close()
		}
		t.Fatalf("second connection passed a full cap: %v", result.err)
	case <-time.After(25 * time.Millisecond):
	}

	if err := firstServer.Close(); err != nil {
		t.Fatal(err)
	}
	var secondServer net.Conn
	select {
	case result := <-accepted:
		if result.err != nil {
			t.Fatal(result.err)
		}
		secondServer = result.connection
	case <-time.After(time.Second):
		t.Fatal("released connection slot did not admit the next connection")
	}
	if len(listener.slots) != 1 {
		t.Fatalf("replacement live connection slots = %d, want 1", len(listener.slots))
	}
	if err := secondServer.Close(); err != nil {
		t.Fatal(err)
	}
	_ = secondServer.Close()
	if len(listener.slots) != 0 {
		t.Fatalf("closed connection slots = %d, want 0", len(listener.slots))
	}
}

func (running *runningReceiver) stop() {
	running.once.Do(func() {
		_ = running.receiver.Close()
		<-running.done
	})
}

func TestMutualTLSPinsDeliveryDuplicateAndRestore(t *testing.T) {
	publicKey, privateKey := testApprovalKeys(t)
	material := newTLSMaterial(t)
	path := privateStorePath(t)
	running, client := startReceiver(t, path, material, newTestApprovalVerifier(publicKey))
	first := newTestEvent(t, privateKey, 1, "", PhasePrepared)

	if running.receiver.store.limits != (StoreLimits{
		MaxBytes:   DefaultMaxStoreBytes,
		MaxRecords: DefaultMaxStoreRecords,
	}) {
		t.Fatalf("receiver defaults = %+v", running.receiver.store.limits)
	}
	if summary, err := client.Summary(context.Background()); err != nil ||
		summary != (Summary{}) {
		t.Fatalf("empty summary=%+v err=%v", summary, err)
	}
	receipt, err := client.Append(context.Background(), first)
	if err != nil || receipt.validateFor(first) != nil {
		t.Fatalf("append: receipt=%+v err=%v", receipt, err)
	}
	retry, err := client.Append(context.Background(), first)
	if summary := testSummary(t, running.receiver); err != nil || retry != receipt || summary.Records != 1 {
		t.Fatalf("retry: receipt=%+v summary=%+v err=%v", retry, summary, err)
	}
	second := newTestEvent(t, privateKey, 2, first.EventHash, PhaseFailed)
	if _, err := client.Append(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Append(context.Background(), second); err != nil {
		t.Fatalf("duplicate terminal event: %v", err)
	}
	summaryOverTLS, err := client.Summary(context.Background())
	if err != nil ||
		summaryOverTLS.Records != 2 ||
		summaryOverTLS.LastSequence != 2 ||
		summaryOverTLS.TipHash != second.EventHash ||
		summaryOverTLS.Bytes == 0 {
		t.Fatalf("summary over mTLS=%+v err=%v", summaryOverTLS, err)
	}

	running.stop()
	events, summary, err := Restore(context.Background(), path, newTestApprovalVerifier(publicKey))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || summary.TipHash != second.EventHash {
		t.Fatalf("restored=%d summary=%+v", len(events), summary)
	}
}

func TestReceiverEnforcesConfiguredStoreLimit(t *testing.T) {
	publicKey, privateKey := testApprovalKeys(t)
	material := newTLSMaterial(t)
	clientPin, err := PinCertificate(material.clientLeaf)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := NewReceiver(ReceiverConfig{
		StorePath:         privateStorePath(t),
		Certificate:       material.server,
		ClientCAs:         material.roots,
		AllowedClientPins: []PublicKeyPin{clientPin},
		ExpectedTargetID:  "node-target-1",
		ExpectedUnit:      "mithril.service",
		ExpectedScope:     "system",
		StoreLimits: StoreLimits{
			MaxBytes:   DefaultMaxStoreBytes,
			MaxRecords: 1,
		},
	}, newTestApprovalVerifier(publicKey))
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()

	first := newTestEvent(t, privateKey, 1, "", PhasePrepared)
	firstBody, err := MarshalEvent(first)
	if err != nil {
		t.Fatal(err)
	}
	firstRequest := httptest.NewRequest(
		http.MethodPost,
		"https://audit.test"+eventPath,
		bytes.NewReader(firstBody),
	)
	firstRequest.Header.Set("Content-Type", "application/json")
	firstRequest.TLS = &tls.ConnectionState{
		VerifiedChains: [][]*x509.Certificate{{material.clientLeaf}},
	}
	firstResponse := httptest.NewRecorder()
	receiver.ServeHTTP(firstResponse, firstRequest)
	if firstResponse.Code != http.StatusCreated {
		t.Fatalf("first response status = %d", firstResponse.Code)
	}

	second := newTestEvent(t, privateKey, 2, first.EventHash, PhaseFailed)
	secondBody, err := MarshalEvent(second)
	if err != nil {
		t.Fatal(err)
	}
	secondRequest := httptest.NewRequest(
		http.MethodPost,
		"https://audit.test"+eventPath,
		bytes.NewReader(secondBody),
	)
	secondRequest.Header.Set("Content-Type", "application/json")
	secondRequest.TLS = &tls.ConnectionState{
		VerifiedChains: [][]*x509.Certificate{{material.clientLeaf}},
	}
	secondResponse := httptest.NewRecorder()
	receiver.ServeHTTP(secondResponse, secondRequest)
	if summary := testSummary(t, receiver); secondResponse.Code != http.StatusInsufficientStorage ||
		summary.Records != 1 {
		t.Fatalf(
			"limited response=%d summary=%+v",
			secondResponse.Code,
			summary,
		)
	}
}

func TestClientTreatsStoreLimitAsPermanent(t *testing.T) {
	publicKey, privateKey := testApprovalKeys(t)
	material := newTLSMaterial(t)
	_, client := startReceiverWithLimits(
		t,
		privateStorePath(t),
		material,
		newTestApprovalVerifier(publicKey),
		StoreLimits{
			MaxBytes:   DefaultMaxStoreBytes,
			MaxRecords: 1,
		},
	)
	first := newTestEvent(t, privateKey, 1, "", PhasePrepared)
	if _, err := client.Append(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := newTestEvent(t, privateKey, 2, first.EventHash, PhaseFailed)
	if _, err := client.Append(context.Background(), second); !errors.Is(err, ErrPermanentRejection) {
		t.Fatalf("store-limit delivery error = %v", err)
	}
}

func TestAllowedRotationClientCannotCrossReceiverIdentityPolicy(t *testing.T) {
	publicKey, privateKey := testApprovalKeys(t)
	material := newTLSMaterial(t)
	clientPin, err := PinCertificate(material.clientLeaf)
	if err != nil {
		t.Fatal(err)
	}
	rotationPin, err := PinCertificate(material.otherClientLeaf)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := NewReceiver(ReceiverConfig{
		StorePath:         privateStorePath(t),
		Certificate:       material.server,
		ClientCAs:         material.roots,
		AllowedClientPins: []PublicKeyPin{clientPin, rotationPin},
		ExpectedTargetID:  "node-target-1",
		ExpectedUnit:      "mithril.service",
		ExpectedScope:     "system",
	}, newTestApprovalVerifier(publicKey))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = receiver.Close()
		t.Fatal(err)
	}
	running := &runningReceiver{
		receiver: receiver,
		listener: listener,
		done:     make(chan error, 1),
	}
	go func() {
		running.done <- receiver.Serve(listener)
	}()
	t.Cleanup(running.stop)

	serverPin, err := PinCertificate(material.serverLeaf)
	if err != nil {
		t.Fatal(err)
	}
	rotationClient, err := NewClient(ClientConfig{
		Endpoint:     "https://" + listener.Addr().String(),
		Certificate:  material.otherClient,
		ServerRoots:  material.roots,
		ServerName:   "audit.test",
		ServerKeyPin: serverPin,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rotationClient.Close()

	tests := []struct {
		name         string
		mutateEvent  func(*Event)
		mutateClaims func(*testApprovalClaims)
	}{
		{
			name: "target",
			mutateEvent: func(event *Event) {
				event.TargetID = "node-target-2"
			},
			mutateClaims: func(claims *testApprovalClaims) {
				claims.TargetID = "node-target-2"
			},
		},
		{
			name: "unit",
			mutateEvent: func(event *Event) {
				event.Unit = "other.service"
			},
			mutateClaims: func(claims *testApprovalClaims) {
				claims.Unit = "other.service"
			},
		},
		{
			name: "scope",
			mutateEvent: func(event *Event) {
				event.Scope = "user"
			},
			mutateClaims: func(claims *testApprovalClaims) {
				claims.Scope = "user"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := newTestEvent(t, privateKey, 1, "", PhasePrepared)
			test.mutateEvent(&event)
			event = updateTestApproval(t, event, privateKey, func(claims *testApprovalClaims, _ *testApprovalEnvelope) {
				test.mutateClaims(claims)
			})
			if _, err := rotationClient.Append(context.Background(), event); !errors.Is(err, ErrPermanentRejection) {
				t.Fatalf("mismatched %s error = %v", test.name, err)
			}
			if summary := testSummary(t, receiver); summary.Records != 0 {
				t.Fatalf("mismatched %s was appended: %+v", test.name, summary)
			}
		})
	}
}

func TestReceiverRejectsStoreFromAnotherIdentityPolicy(t *testing.T) {
	publicKey, privateKey := testApprovalKeys(t)
	verifier := newTestApprovalVerifier(publicKey)
	path := privateStorePath(t)
	store, err := OpenStore(path, verifier)
	if err != nil {
		t.Fatal(err)
	}
	event := newTestEvent(t, privateKey, 1, "", PhasePrepared)
	event.TargetID = "node-target-2"
	event = updateTestApproval(t, event, privateKey, func(claims *testApprovalClaims, _ *testApprovalEnvelope) {
		claims.TargetID = event.TargetID
	})
	if _, _, err := store.Append(context.Background(), event); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	material := newTLSMaterial(t)
	clientPin, err := PinCertificate(material.clientLeaf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewReceiver(ReceiverConfig{
		StorePath:         path,
		Certificate:       material.server,
		ClientCAs:         material.roots,
		AllowedClientPins: []PublicKeyPin{clientPin},
		ExpectedTargetID:  "node-target-1",
		ExpectedUnit:      "mithril.service",
		ExpectedScope:     "system",
	}, verifier); !errors.Is(err, ErrApprovalRejected) {
		t.Fatalf("foreign-identity store error = %v", err)
	}
}

func TestReceiverRequiresFixedIdentityPolicy(t *testing.T) {
	publicKey, _ := testApprovalKeys(t)
	material := newTLSMaterial(t)
	clientPin, err := PinCertificate(material.clientLeaf)
	if err != nil {
		t.Fatal(err)
	}
	base := ReceiverConfig{
		StorePath:         privateStorePath(t),
		Certificate:       material.server,
		ClientCAs:         material.roots,
		AllowedClientPins: []PublicKeyPin{clientPin},
		ExpectedTargetID:  "node-target-1",
		ExpectedUnit:      "mithril.service",
		ExpectedScope:     "system",
	}
	tests := []struct {
		name   string
		mutate func(*ReceiverConfig)
	}{
		{name: "target", mutate: func(config *ReceiverConfig) { config.ExpectedTargetID = "" }},
		{name: "unit", mutate: func(config *ReceiverConfig) { config.ExpectedUnit = "" }},
		{name: "scope", mutate: func(config *ReceiverConfig) { config.ExpectedScope = "" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := base
			test.mutate(&config)
			if receiver, err := NewReceiver(config, newTestApprovalVerifier(publicKey)); err == nil {
				_ = receiver.Close()
				t.Fatalf("missing %s identity policy was accepted", test.name)
			}
		})
	}
}

func TestMutualTLSRejectsWrongServerAndClientPins(t *testing.T) {
	publicKey, privateKey := testApprovalKeys(t)
	material := newTLSMaterial(t)
	running, _ := startReceiver(t, privateStorePath(t), material, newTestApprovalVerifier(publicKey))
	event := newTestEvent(t, privateKey, 1, "", PhasePrepared)

	wrongServerPin, err := PinCertificate(material.clientLeaf)
	if err != nil {
		t.Fatal(err)
	}
	wrongServerClient, err := NewClient(ClientConfig{
		Endpoint:     "https://" + running.listener.Addr().String(),
		Certificate:  material.client,
		ServerRoots:  material.roots,
		ServerName:   "audit.test",
		ServerKeyPin: wrongServerPin,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer wrongServerClient.Close()
	if _, err := wrongServerClient.Append(context.Background(), event); err == nil {
		t.Fatal("wrong server pin connected")
	}

	serverPin, err := PinCertificate(material.serverLeaf)
	if err != nil {
		t.Fatal(err)
	}
	wrongClient, err := NewClient(ClientConfig{
		Endpoint:     "https://" + running.listener.Addr().String(),
		Certificate:  material.otherClient,
		ServerRoots:  material.roots,
		ServerName:   "audit.test",
		ServerKeyPin: serverPin,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer wrongClient.Close()
	if _, err := wrongClient.Append(context.Background(), event); err == nil {
		t.Fatal("unpinned client connected")
	}

	encoded, err := MarshalEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	withoutClientCert := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    material.roots,
		ServerName: "audit.test",
	}}}
	request, err := http.NewRequest(http.MethodPost, "https://"+running.listener.Addr().String()+eventPath, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if _, err := withoutClientCert.Do(request); err == nil {
		t.Fatal("client without certificate connected")
	}
}

type lostResponseWriter struct {
	header http.Header
	status int
}

func (writer *lostResponseWriter) Header() http.Header {
	if writer.header == nil {
		writer.header = make(http.Header)
	}
	return writer.header
}

func (writer *lostResponseWriter) WriteHeader(status int) {
	writer.status = status
}

func (*lostResponseWriter) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}

func TestLostAcknowledgementRetriesAsDuplicate(t *testing.T) {
	publicKey, privateKey := testApprovalKeys(t)
	material := newTLSMaterial(t)
	clientPin, err := PinCertificate(material.clientLeaf)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := NewReceiver(ReceiverConfig{
		StorePath:         privateStorePath(t),
		Certificate:       material.server,
		ClientCAs:         material.roots,
		AllowedClientPins: []PublicKeyPin{clientPin},
		ExpectedTargetID:  "node-target-1",
		ExpectedUnit:      "mithril.service",
		ExpectedScope:     "system",
	}, newTestApprovalVerifier(publicKey))
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	event := newTestEvent(t, privateKey, 1, "", PhasePrepared)
	encoded, err := MarshalEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "https://audit.test"+eventPath, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{material.clientLeaf}}}
	lost := &lostResponseWriter{}
	receiver.ServeHTTP(lost, request)
	if summary := testSummary(t, receiver); lost.status != http.StatusCreated || summary.Records != 1 {
		t.Fatalf("lost response status=%d summary=%+v", lost.status, summary)
	}

	retryRequest := httptest.NewRequest(http.MethodPost, "https://audit.test"+eventPath, bytes.NewReader(encoded))
	retryRequest.Header.Set("Content-Type", "application/json")
	retryRequest.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{material.clientLeaf}}}
	recorder := httptest.NewRecorder()
	receiver.ServeHTTP(recorder, retryRequest)
	if summary := testSummary(t, receiver); recorder.Code != http.StatusOK || summary.Records != 1 {
		t.Fatalf("retry status=%d summary=%+v", recorder.Code, summary)
	}
}

func TestReceiverHasNoActionEndpointAndFailsClosedWithoutVerifier(t *testing.T) {
	material := newTLSMaterial(t)
	clientPin, err := PinCertificate(material.clientLeaf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewReceiver(ReceiverConfig{
		StorePath:         privateStorePath(t),
		Certificate:       material.server,
		ClientCAs:         material.roots,
		AllowedClientPins: []PublicKeyPin{clientPin},
		ExpectedTargetID:  "node-target-1",
		ExpectedUnit:      "mithril.service",
		ExpectedScope:     "system",
	}, nil); !errors.Is(err, ErrApprovalVerifierRequired) {
		t.Fatalf("nil verifier error = %v", err)
	}

	publicKey, _ := testApprovalKeys(t)
	receiver, err := NewReceiver(ReceiverConfig{
		StorePath:         privateStorePath(t),
		Certificate:       material.server,
		ClientCAs:         material.roots,
		AllowedClientPins: []PublicKeyPin{clientPin},
		ExpectedTargetID:  "node-target-1",
		ExpectedUnit:      "mithril.service",
		ExpectedScope:     "system",
	}, newTestApprovalVerifier(publicKey))
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	for _, target := range []string{
		"https://audit.test/v1/actions",
		"https://audit.test/v1/%65vents",
		"https://audit.test/v1/events?",
	} {
		request := httptest.NewRequest(http.MethodPost, target, nil)
		request.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{material.clientLeaf}}}
		recorder := httptest.NewRecorder()
		receiver.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("target %q status = %d", target, recorder.Code)
		}
	}
}

func TestAuditEndpointRejectsAmbiguousForms(t *testing.T) {
	if _, err := auditEndpoint("https://audit.test"); err != nil {
		t.Fatalf("valid endpoint: %v", err)
	}
	for _, endpoint := range []string{
		"https://audit.test?",
		"https://audit.test/%2f",
		"https://audit.test/path",
		"https://user@audit.test",
		"https://audit.test?query=1",
		"http://audit.test",
	} {
		if _, err := auditEndpoint(endpoint); err == nil {
			t.Errorf("endpoint %q was accepted", endpoint)
		}
	}
}

func TestSummaryProtocolIsCanonicalAndPrefixOnly(t *testing.T) {
	tip := sha256.Sum256([]byte("tip"))
	summary := Summary{
		Records:      7,
		LastSequence: 7,
		TipHash:      hexHash(tip),
		Bytes:        1234,
	}
	encoded, err := marshalSummary(summary)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseSummary(encoded)
	if err != nil || parsed != summary {
		t.Fatalf("parsed=%+v err=%v", parsed, err)
	}
	for _, invalid := range [][]byte{
		append([]byte(" "), encoded...),
		bytes.Replace(encoded, []byte(`"records":7`), []byte(`"records":7,"records":7`), 1),
		bytes.Replace(encoded, []byte(`"version":1`), []byte(`"version":2`), 1),
		bytes.Replace(encoded, []byte(`"last_sequence":7`), []byte(`"last_sequence":6`), 1),
	} {
		if _, err := parseSummary(invalid); err == nil {
			t.Fatalf("noncanonical summary was accepted: %s", invalid)
		}
	}
}

func TestClientFailsWhenReceiverIsUnavailable(t *testing.T) {
	material := newTLSMaterial(t)
	serverPin, err := PinCertificate(material.serverLeaf)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		Endpoint:       "https://127.0.0.1:1",
		Certificate:    material.client,
		ServerRoots:    material.roots,
		ServerName:     "audit.test",
		ServerKeyPin:   serverPin,
		ConnectTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, privateKey := testApprovalKeys(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.Append(ctx, newTestEvent(t, privateKey, 1, "", PhaseDispatchStarted)); err == nil {
		t.Fatal("unavailable receiver acknowledged an event")
	}
}

func TestClientRequestTimeoutIncludesResponseBody(t *testing.T) {
	material := newTLSMaterial(t)
	responseStarted := make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(
		func(response http.ResponseWriter, request *http.Request) {
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusCreated)
			_, _ = response.Write([]byte("{"))
			if flusher, ok := response.(http.Flusher); ok {
				flusher.Flush()
			}
			close(responseStarted)
			<-request.Context().Done()
		},
	))
	server.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{material.server},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    material.roots,
	}
	server.StartTLS()
	t.Cleanup(server.Close)

	serverPin, err := PinCertificate(material.serverLeaf)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		Endpoint:       server.URL,
		Certificate:    material.client,
		ServerRoots:    material.roots,
		ServerName:     "audit.test",
		ServerKeyPin:   serverPin,
		RequestTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)

	_, privateKey := testApprovalKeys(t)
	started := time.Now()
	_, err = client.Append(
		t.Context(),
		newTestEvent(t, privateKey, 1, "", PhasePrepared),
	)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("slow response body was accepted")
	}
	select {
	case <-responseStarted:
	default:
		t.Fatal("request did not reach the response body")
	}
	if elapsed > time.Second {
		t.Fatalf("request timeout took %s, want less than one second", elapsed)
	}
}
