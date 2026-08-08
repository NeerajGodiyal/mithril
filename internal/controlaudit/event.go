package controlaudit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// ProtocolVersion identifies the first control-audit wire and store format.
	ProtocolVersion uint16 = 1

	// ApprovalAuditDomain is the only approval-attestation domain accepted by
	// the control-audit receiver.
	ApprovalAuditDomain = "mithril.service.lifecycle.audit.v1"

	MaxEventBytes            = 64 << 10
	MaxApprovalEvidenceBytes = 32 << 10
	MaxStateCheckpointBytes  = 32 << 10
	MaxApprovalWindow        = 5 * time.Minute
)

var (
	ErrInvalidEvent             = errors.New("invalid control-audit event")
	ErrApprovalVerifierRequired = errors.New("approval verifier is required")
	ErrApprovalRejected         = errors.New("approval evidence was rejected")

	identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,126}$`)
	unitPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@:-]{0,126}\.service$`)
	codePattern       = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

// Phase is one durable operation-state transition.
type Phase string

const (
	PhasePrepared        Phase = "prepared"
	PhaseDispatchStarted Phase = "dispatch_started"
	PhaseDispatched      Phase = "dispatched"
	PhaseVerifying       Phase = "verifying"
	PhaseSucceeded       Phase = "succeeded"
	PhaseFailed          Phase = "failed"
	PhaseOutcomeUnknown  Phase = "outcome_unknown"
)

// Action is the complete service lifecycle-action vocabulary.
type Action string

const (
	ActionStart   Action = "start"
	ActionStop    Action = "stop"
	ActionRestart Action = "restart"
)

// Event is the portable, non-authorizing audit record sent to the off-host
// receiver. ApprovalEvidence contains only the domain-separated audit
// attestation; it must never contain a bearer authorization token.
//
// The event is not independently signed. Its origin is authenticated by the
// pinned mutual-TLS connection and its approval evidence is checked through
// ApprovalVerifier before the receiver stores or acknowledges it.
type Event struct {
	Version                 uint16 `json:"version"`
	Sequence                uint64 `json:"sequence"`
	ID                      string `json:"id"`
	Timestamp               string `json:"timestamp"`
	SessionID               string `json:"session_id"`
	TargetID                string `json:"target_id"`
	ActionID                string `json:"action_id"`
	Phase                   Phase  `json:"phase"`
	Action                  Action `json:"action"`
	Unit                    string `json:"unit"`
	Scope                   string `json:"scope"`
	ApproverKeyID           string `json:"approver_key_id"`
	ApprovalEvidence        []byte `json:"approval_evidence"`
	StateCheckpoint         []byte `json:"state_checkpoint"`
	BeforeHash              string `json:"before_hash"`
	AfterHash               string `json:"after_hash,omitempty"`
	Outcome                 string `json:"outcome,omitempty"`
	ReasonCode              string `json:"reason_code,omitempty"`
	DispatchMayHaveOccurred bool   `json:"dispatch_may_have_occurred"`
	DispatchAccepted        bool   `json:"dispatch_accepted"`
	PreviousHash            string `json:"previous_hash,omitempty"`
	EventHash               string `json:"event_hash"`
}

// Receipt is returned only after the event is known durable on the receiver.
// It is transport-authenticated, not a portable signature.
type Receipt struct {
	Version   uint16 `json:"version"`
	EventID   string `json:"event_id"`
	EventHash string `json:"event_hash"`
	Sequence  uint64 `json:"sequence"`
}

// ApprovalBinding is the bounded, independently decoded meaning of one
// non-authorizing approval attestation.
type ApprovalBinding struct {
	SessionID      string
	TargetID       string
	ActionID       string
	Action         Action
	Unit           string
	Scope          string
	BeforeHash     string
	ApproverKeyID  string
	IssuedAtUnix   int64
	ExpiresAtUnix  int64
	EvidenceSHA256 string
	// CanStartAction is current receiver admission policy. It is ignored while
	// replaying durable history and for later phases of an accepted action.
	CanStartAction bool
}

// ApprovalVerifier verifies the detached proof and decodes its signed claims.
// Store compares the result to Event and enforces the initial validity window
// and every later transition. The verifier must not compare expiry to the wall
// clock or mutate replay state: historical restore and lost-ack retry must
// remain valid.
type ApprovalVerifier interface {
	VerifyApproval(context.Context, Event) (ApprovalBinding, error)
	VerifyStateTransition(context.Context, Event, Event) error
}

// CanonicalTimestamp returns the timestamp representation accepted on the
// wire and in the durable store.
func CanonicalTimestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// Seal computes EventHash over the canonical event with an empty EventHash.
func Seal(event Event) (Event, error) {
	event.EventHash = ""
	if err := event.validate(false); err != nil {
		return Event{}, err
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return Event{}, fmt.Errorf("%w: encode hash input", ErrInvalidEvent)
	}
	sum := sha256.Sum256(encoded)
	event.EventHash = hex.EncodeToString(sum[:])
	if err := event.validate(true); err != nil {
		return Event{}, err
	}
	return event, nil
}

// Validate checks the bounded event vocabulary and its content hash.
func (event Event) Validate() error {
	if err := event.validate(true); err != nil {
		return err
	}
	expected, err := Seal(event)
	if err != nil {
		return err
	}
	if event.EventHash != expected.EventHash {
		return fmt.Errorf("%w: event hash does not match content", ErrInvalidEvent)
	}
	return nil
}

func (event Event) validate(requireHash bool) error {
	if event.Version != ProtocolVersion {
		return fmt.Errorf("%w: unsupported version", ErrInvalidEvent)
	}
	if event.Sequence == 0 {
		return fmt.Errorf("%w: sequence must be positive", ErrInvalidEvent)
	}
	if !identifierPattern.MatchString(event.ID) ||
		!identifierPattern.MatchString(event.SessionID) ||
		!identifierPattern.MatchString(event.TargetID) ||
		!identifierPattern.MatchString(event.ActionID) ||
		!identifierPattern.MatchString(event.ApproverKeyID) {
		return fmt.Errorf("%w: identifier is invalid", ErrInvalidEvent)
	}
	if err := validateTimestamp(event.Timestamp); err != nil {
		return err
	}
	switch event.Phase {
	case PhasePrepared, PhaseDispatchStarted, PhaseDispatched, PhaseVerifying,
		PhaseSucceeded, PhaseFailed, PhaseOutcomeUnknown:
	default:
		return fmt.Errorf("%w: phase is invalid", ErrInvalidEvent)
	}
	switch event.Action {
	case ActionStart, ActionStop, ActionRestart:
	default:
		return fmt.Errorf("%w: action is invalid", ErrInvalidEvent)
	}
	if !unitPattern.MatchString(event.Unit) {
		return fmt.Errorf("%w: unit is invalid", ErrInvalidEvent)
	}
	if event.Scope != "system" && event.Scope != "user" {
		return fmt.Errorf("%w: scope is invalid", ErrInvalidEvent)
	}
	if len(event.ApprovalEvidence) == 0 || len(event.ApprovalEvidence) > MaxApprovalEvidenceBytes {
		return fmt.Errorf("%w: approval evidence size is invalid", ErrInvalidEvent)
	}
	if len(event.StateCheckpoint) == 0 ||
		len(event.StateCheckpoint) > MaxStateCheckpointBytes {
		return fmt.Errorf("%w: state checkpoint size is invalid", ErrInvalidEvent)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, event.StateCheckpoint); err != nil ||
		!bytes.Equal(compact.Bytes(), event.StateCheckpoint) {
		return fmt.Errorf("%w: state checkpoint is not canonical JSON", ErrInvalidEvent)
	}
	if err := validateHash(event.BeforeHash, false); err != nil {
		return fmt.Errorf("%w: before hash is invalid", ErrInvalidEvent)
	}
	if err := ValidateLifecycleFields(
		event.Phase,
		event.AfterHash,
		event.Outcome,
		event.ReasonCode,
		event.DispatchMayHaveOccurred,
		event.DispatchAccepted,
	); err != nil {
		return err
	}
	if err := validateHash(event.PreviousHash, true); err != nil {
		return fmt.Errorf("%w: previous hash is invalid", ErrInvalidEvent)
	}
	if requireHash {
		if err := validateHash(event.EventHash, false); err != nil {
			return fmt.Errorf("%w: event hash is invalid", ErrInvalidEvent)
		}
	} else if event.EventHash != "" {
		return fmt.Errorf("%w: hash input contains an event hash", ErrInvalidEvent)
	}
	return nil
}

// ValidateLifecycleFields applies the canonical result and dispatch model
// shared by the local control state and its portable audit event.
func ValidateLifecycleFields(
	phase Phase,
	afterHash string,
	outcome string,
	reasonCode string,
	dispatchMayHaveOccurred bool,
	dispatchAccepted bool,
) error {
	if err := validateHash(afterHash, true); err != nil {
		return fmt.Errorf("%w: after hash is invalid", ErrInvalidEvent)
	}
	if outcome != "" && !codePattern.MatchString(outcome) {
		return fmt.Errorf("%w: outcome is invalid", ErrInvalidEvent)
	}
	if reasonCode != "" && !codePattern.MatchString(reasonCode) {
		return fmt.Errorf("%w: reason code is invalid", ErrInvalidEvent)
	}
	if dispatchAccepted && !dispatchMayHaveOccurred {
		return fmt.Errorf("%w: accepted dispatch was never attempted", ErrInvalidEvent)
	}

	hasResult := afterHash != "" || outcome != "" || reasonCode != ""
	switch phase {
	case PhasePrepared, PhaseDispatchStarted:
		if hasResult || dispatchMayHaveOccurred || dispatchAccepted {
			return fmt.Errorf("%w: authority phase contains dispatch or result fields", ErrInvalidEvent)
		}
	case PhaseDispatched:
		if hasResult || !dispatchMayHaveOccurred || !dispatchAccepted {
			return fmt.Errorf("%w: dispatched phase fields are inconsistent", ErrInvalidEvent)
		}
	case PhaseVerifying:
		if hasResult || !dispatchMayHaveOccurred || !dispatchAccepted {
			return fmt.Errorf("%w: verifying phase fields are inconsistent", ErrInvalidEvent)
		}
	case PhaseSucceeded:
		if afterHash == "" || outcome != string(PhaseSucceeded) || reasonCode == "" ||
			!dispatchMayHaveOccurred || !dispatchAccepted {
			return fmt.Errorf("%w: succeeded phase fields are incomplete", ErrInvalidEvent)
		}
	case PhaseFailed:
		if afterHash == "" || outcome != string(PhaseFailed) || reasonCode == "" ||
			dispatchMayHaveOccurred || dispatchAccepted {
			return fmt.Errorf("%w: failed phase fields are incomplete", ErrInvalidEvent)
		}
	case PhaseOutcomeUnknown:
		if outcome != string(PhaseOutcomeUnknown) || reasonCode == "" ||
			!dispatchMayHaveOccurred {
			return fmt.Errorf("%w: outcome-unknown phase fields are incomplete", ErrInvalidEvent)
		}
	default:
		return fmt.Errorf("%w: phase is invalid", ErrInvalidEvent)
	}
	return nil
}

func validateTimestamp(value string) error {
	if len(value) == 0 || len(value) > len(time.RFC3339Nano)+10 || !utf8.ValidString(value) {
		return fmt.Errorf("%w: timestamp is invalid", ErrInvalidEvent)
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.Location() != time.UTC || parsed.Format(time.RFC3339Nano) != value {
		return fmt.Errorf("%w: timestamp is not canonical UTC", ErrInvalidEvent)
	}
	return nil
}

func validateHash(value string, optional bool) error {
	if value == "" && optional {
		return nil
	}
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return errors.New("invalid hash")
	}
	_, err := hex.DecodeString(value)
	return err
}

// MarshalEvent returns the one accepted JSON representation.
func MarshalEvent(event Event) ([]byte, error) {
	if err := event.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("%w: encode event", ErrInvalidEvent)
	}
	if len(encoded) > MaxEventBytes {
		return nil, fmt.Errorf("%w: event exceeds size limit", ErrInvalidEvent)
	}
	return encoded, nil
}

// ParseEvent accepts only the canonical, schema-exact event representation.
func ParseEvent(encoded []byte) (Event, error) {
	if len(encoded) == 0 || len(encoded) > MaxEventBytes {
		return Event{}, fmt.Errorf("%w: event size is invalid", ErrInvalidEvent)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var event Event
	if err := decoder.Decode(&event); err != nil {
		return Event{}, fmt.Errorf("%w: decode event", ErrInvalidEvent)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Event{}, fmt.Errorf("%w: trailing data", ErrInvalidEvent)
	}
	canonical, err := MarshalEvent(event)
	if err != nil {
		return Event{}, err
	}
	if !bytes.Equal(encoded, canonical) {
		return Event{}, fmt.Errorf("%w: event encoding is not canonical", ErrInvalidEvent)
	}
	return event, nil
}

func receiptFor(event Event) Receipt {
	return Receipt{
		Version:   ProtocolVersion,
		EventID:   event.ID,
		EventHash: event.EventHash,
		Sequence:  event.Sequence,
	}
}

func (receipt Receipt) validateFor(event Event) error {
	if receipt.Version != ProtocolVersion ||
		receipt.EventID != event.ID ||
		receipt.EventHash != event.EventHash ||
		receipt.Sequence != event.Sequence {
		return errors.New("audit receipt does not match event")
	}
	return nil
}
