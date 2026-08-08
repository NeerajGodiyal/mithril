package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/Overclock-Validator/mithril/internal/controlaudit"
)

const (
	controlStateVersion  = 1
	maxControlStateBytes = 64 << 10
)

type operationPhase string

const (
	phasePrepared        operationPhase = "prepared"
	phaseDispatchStarted operationPhase = "dispatch_started"
	phaseDispatched      operationPhase = "dispatched"
	phaseVerifying       operationPhase = "verifying"
	phaseSucceeded       operationPhase = "succeeded"
	phaseFailed          operationPhase = "failed"
	phaseOutcomeUnknown  operationPhase = "outcome_unknown"
)

func (p operationPhase) valid() bool {
	switch p {
	case phasePrepared, phaseDispatchStarted, phaseDispatched, phaseVerifying,
		phaseSucceeded, phaseFailed, phaseOutcomeUnknown:
		return true
	default:
		return false
	}
}

func (p operationPhase) terminal() bool {
	return p == phaseSucceeded || p == phaseFailed || p == phaseOutcomeUnknown
}

func (op serviceOperation) newOperationAllowedAtUnix() int64 {
	// Sibling approvals from another MCP process are session-bound but remain
	// usable by that process. A sibling prepared just before this operation was
	// accepted can expire later than the accepted token, so hold the barrier
	// for the maximum configured approval lifetime from acceptance.
	barrier := int64(math.MaxInt64)
	if op.StartedAtUnix <= math.MaxInt64-int64(MaxApprovalTTLSeconds) {
		barrier = op.StartedAtUnix + int64(MaxApprovalTTLSeconds)
	}
	if op.Approval.ExpiresAtUnix > barrier {
		barrier = op.Approval.ExpiresAtUnix
	}
	return barrier
}

func (op serviceOperation) blocksNewOperation(now time.Time) bool {
	if !op.Phase.terminal() {
		return true
	}
	return now.UTC().Unix() < op.newOperationAllowedAtUnix()
}

// serviceOperation is the single durable lifecycle operation for a service.
// A nonterminal record blocks every new preparation across MCP processes.
// A terminal record keeps that barrier until every sibling approval which
// could predate the accepted action has expired.
type serviceOperation struct {
	Version                 uint16                  `json:"version"`
	ID                      string                  `json:"id"`
	ServerSession           string                  `json:"server_session"`
	TargetID                string                  `json:"target_id"`
	Action                  serviceAction           `json:"action"`
	Unit                    string                  `json:"unit"`
	Scope                   string                  `json:"scope"`
	StatusBefore            serviceStatus           `json:"status_before"`
	BeforeHash              string                  `json:"before_hash"`
	Approval                ControlApprovalEvidence `json:"approval"`
	Phase                   operationPhase          `json:"phase"`
	StatusAfter             *serviceStatus          `json:"status_after,omitempty"`
	AfterHash               string                  `json:"after_hash,omitempty"`
	Outcome                 string                  `json:"outcome,omitempty"`
	ReasonCode              string                  `json:"reason_code,omitempty"`
	DispatchMayHaveOccurred bool                    `json:"dispatch_may_have_occurred"`
	DispatchAccepted        bool                    `json:"dispatch_accepted"`
	StartedAtUnix           int64                   `json:"started_at_unix"`
	UpdatedAtUnix           int64                   `json:"updated_at_unix"`
	DeadlineUnix            int64                   `json:"deadline_unix"`
}

func (op serviceOperation) validate() error {
	if op.Version != controlStateVersion ||
		op.ID == "" ||
		op.ServerSession == "" ||
		op.TargetID == "" ||
		op.BeforeHash == "" ||
		!op.Phase.valid() ||
		op.StartedAtUnix <= 0 ||
		op.UpdatedAtUnix < op.StartedAtUnix ||
		op.DeadlineUnix <= op.StartedAtUnix {
		return errors.New("control operation state is invalid")
	}
	action, err := parseServiceAction(string(op.Action))
	if err != nil || action != op.Action {
		return errors.New("control operation action is invalid")
	}
	if err := ValidateSystemdServiceUnit(op.Unit); err != nil {
		return errors.New("control operation unit is invalid")
	}
	if op.Scope != "system" && op.Scope != "user" {
		return errors.New("control operation scope is invalid")
	}
	if op.StatusBefore.Unit != op.Unit || op.StatusBefore.Scope != op.Scope ||
		!persistableServiceStatus(op.StatusBefore) ||
		serviceStateHash(op.StatusBefore) != op.BeforeHash {
		return errors.New("control operation before-state is invalid")
	}
	if op.Approval.Version != approvalVersion ||
		op.Approval.Domain != serviceApprovalAuditDomain ||
		op.Approval.ActionID != op.ID ||
		op.Approval.ApproverKeyID == "" ||
		op.Approval.IssuedAtUnix <= 0 ||
		op.Approval.ExpiresAtUnix <= op.Approval.IssuedAtUnix ||
		op.Approval.EvidenceSHA256 == ([32]byte{}) ||
		len(op.Approval.ClaimsCBOR) == 0 ||
		len(op.Approval.Proof) == 0 ||
		op.Approval.EvidenceSHA256 != approvalEvidenceHash(
			op.Approval.Domain,
			op.Approval.ClaimsCBOR,
			op.Approval.Proof,
		) {
		return errors.New("control operation approval evidence is invalid")
	}
	if op.StatusAfter == nil {
		if op.AfterHash != "" {
			return errors.New("control operation after-state hash has no status")
		}
	} else if op.StatusAfter.Unit != op.Unit ||
		op.StatusAfter.Scope != op.Scope ||
		!persistableServiceStatus(*op.StatusAfter) ||
		serviceStateHash(*op.StatusAfter) != op.AfterHash {
		return errors.New("control operation after-state is invalid")
	}
	if err := controlaudit.ValidateLifecycleFields(
		controlaudit.Phase(op.Phase),
		op.AfterHash,
		op.Outcome,
		op.ReasonCode,
		op.DispatchMayHaveOccurred,
		op.DispatchAccepted,
	); err != nil {
		return errors.New("control operation lifecycle fields are invalid")
	}
	return nil
}

func persistableServiceStatus(status serviceStatus) bool {
	if status.LoadState == "" || status.ActiveState == "" || status.SubState == "" {
		return false
	}
	for _, value := range []string{
		status.LoadState,
		status.ActiveState,
		status.SubState,
		status.Result,
		status.InvocationID,
		status.Job,
	} {
		bounded, changed := truncateUTF8Bytes(value, 256)
		if changed || bounded != value || redactUntrustedText(value) != value {
			return false
		}
	}
	if status.InvocationID == "" {
		return true
	}
	if len(status.InvocationID) != 32 {
		return false
	}
	for _, char := range status.InvocationID {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validOperationTransition(from, to operationPhase) bool {
	switch from {
	case phasePrepared:
		return to == phaseDispatchStarted || to == phaseFailed
	case phaseDispatchStarted:
		return to == phaseDispatched || to == phaseFailed || to == phaseOutcomeUnknown
	case phaseDispatched:
		return to == phaseVerifying || to == phaseOutcomeUnknown
	case phaseVerifying:
		return to == phaseSucceeded || to == phaseOutcomeUnknown
	default:
		return false
	}
}

func sameOperationIdentity(a, b serviceOperation) bool {
	return a.ID == b.ID &&
		a.ServerSession == b.ServerSession &&
		a.TargetID == b.TargetID &&
		a.Action == b.Action &&
		a.Unit == b.Unit &&
		a.Scope == b.Scope &&
		a.StatusBefore == b.StatusBefore &&
		a.BeforeHash == b.BeforeHash &&
		a.StartedAtUnix == b.StartedAtUnix &&
		sameControlApprovalEvidence(a.Approval, b.Approval)
}

func validOperationDeadlineTransition(
	current serviceOperation,
	next serviceOperation,
) bool {
	if current.Phase == phaseDispatchStarted &&
		(next.Phase == phaseDispatched || next.Phase == phaseOutcomeUnknown) {
		seconds := int64(postconditionTimeout / time.Second)
		return next.UpdatedAtUnix <= math.MaxInt64-seconds &&
			next.DeadlineUnix == next.UpdatedAtUnix+seconds
	}
	return current.DeadlineUnix == next.DeadlineUnix
}

func sameControlApprovalEvidence(a, b ControlApprovalEvidence) bool {
	return a.Version == b.Version &&
		a.Domain == b.Domain &&
		a.AuthorizationClaimsSHA256 == b.AuthorizationClaimsSHA256 &&
		a.ActionID == b.ActionID &&
		a.ApproverKeyID == b.ApproverKeyID &&
		a.NonceSHA256 == b.NonceSHA256 &&
		a.IssuedAtUnix == b.IssuedAtUnix &&
		a.ExpiresAtUnix == b.ExpiresAtUnix &&
		a.EvidenceSHA256 == b.EvidenceSHA256 &&
		bytes.Equal(a.ClaimsCBOR, b.ClaimsCBOR) &&
		bytes.Equal(a.Proof, b.Proof)
}

// transitionedWithResult changes the phase and all phase-dependent fields as
// one validated operation. Callers must use it when the target phase requires
// different result or dispatch fields; constructing an invalid intermediate
// state and fixing it later is intentionally rejected.
func (op serviceOperation) transitionedWithResult(
	phase operationPhase,
	now time.Time,
	status *serviceStatus,
	outcome string,
	reasonCode string,
	dispatchMayHaveOccurred bool,
	dispatchAccepted bool,
) (serviceOperation, error) {
	if !validOperationTransition(op.Phase, phase) {
		return serviceOperation{}, errors.New("control operation phase transition is invalid")
	}
	next := op
	next.Phase = phase
	next.UpdatedAtUnix = now.UTC().Unix()
	if status == nil {
		next.StatusAfter = nil
		next.AfterHash = ""
	} else {
		statusCopy := *status
		next.StatusAfter = &statusCopy
		next.AfterHash = serviceStateHash(statusCopy)
	}
	next.Outcome = outcome
	next.ReasonCode = reasonCode
	next.DispatchMayHaveOccurred = dispatchMayHaveOccurred
	next.DispatchAccepted = dispatchAccepted
	if next.UpdatedAtUnix < op.UpdatedAtUnix {
		return serviceOperation{}, errors.New("control operation update time moved backwards")
	}
	if err := next.validate(); err != nil {
		return serviceOperation{}, err
	}
	return next, nil
}

func (op serviceOperation) transitionedAfterDispatch(
	phase operationPhase,
	now time.Time,
	status *serviceStatus,
	outcome string,
	reasonCode string,
	dispatchAccepted bool,
) (serviceOperation, error) {
	if op.Phase != phaseDispatchStarted ||
		(phase != phaseDispatched && phase != phaseOutcomeUnknown) {
		return serviceOperation{}, errors.New("control operation is not at the dispatch boundary")
	}
	next, err := op.transitionedWithResult(
		phase,
		now,
		status,
		outcome,
		reasonCode,
		true,
		dispatchAccepted,
	)
	if err != nil {
		return serviceOperation{}, err
	}
	seconds := int64(postconditionTimeout / time.Second)
	if next.UpdatedAtUnix > math.MaxInt64-seconds {
		return serviceOperation{}, errors.New("control operation deadline overflows")
	}
	next.DeadlineUnix = next.UpdatedAtUnix + seconds
	if err := next.validate(); err != nil {
		return serviceOperation{}, err
	}
	return next, nil
}

type controlStateStore struct {
	path string
}

type controlStateTransaction struct {
	store   *controlStateStore
	current *serviceOperation
}

func cloneServiceOperation(operation serviceOperation) serviceOperation {
	cloned := operation
	cloned.Approval.ClaimsCBOR = bytes.Clone(operation.Approval.ClaimsCBOR)
	cloned.Approval.Proof = bytes.Clone(operation.Approval.Proof)
	if operation.StatusAfter != nil {
		status := *operation.StatusAfter
		cloned.StatusAfter = &status
	}
	return cloned
}

func newControlStateStore(path string) (*controlStateStore, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("control state path must be a clean absolute path")
	}
	if filepath.Base(path) == "." || filepath.Base(path) == string(filepath.Separator) {
		return nil, errors.New("control state path must name a file")
	}
	if err := validateControlDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	return &controlStateStore{path: path}, nil
}

// withLock serializes state inspection and mutation across goroutines and MCP
// processes. The callback receives a copy; returning nil deletes the state.
func (s *controlStateStore) withLock(
	ctx context.Context,
	update func(*serviceOperation) (*serviceOperation, error),
) error {
	return s.withTransaction(ctx, func(transaction *controlStateTransaction) error {
		next, err := update(transaction.operation())
		if err != nil {
			return err
		}
		if next == nil {
			return transaction.remove()
		}
		return transaction.save(*next)
	})
}

// withTransaction holds the host-shared lock while a caller performs multiple
// durable transitions. This is the serialization boundary around status
// re-check, pre-dispatch audit, dispatch, and postcondition verification.
func (s *controlStateStore) withTransaction(
	ctx context.Context,
	run func(*controlStateTransaction) error,
) error {
	unlock, err := lockControlState(ctx, s.path+".lock")
	if err != nil {
		return err
	}
	defer unlock()

	current, err := s.load()
	if err != nil {
		return err
	}
	return run(&controlStateTransaction{store: s, current: current})
}

func (transaction *controlStateTransaction) operation() *serviceOperation {
	if transaction.current == nil {
		return nil
	}
	cloned := cloneServiceOperation(*transaction.current)
	return &cloned
}

func (transaction *controlStateTransaction) save(next serviceOperation) error {
	if err := next.validate(); err != nil {
		return err
	}
	if current := transaction.current; current == nil {
		if next.Phase != phasePrepared {
			return errors.New("a control operation must begin prepared")
		}
	} else {
		switch {
		case current.ID == next.ID:
			if !sameOperationIdentity(*current, next) {
				return errors.New("control operation identity changed across a transition")
			}
			if !validOperationDeadlineTransition(*current, next) {
				return errors.New("control operation deadline changed outside dispatch")
			}
			if !validOperationTransition(current.Phase, next.Phase) {
				return errors.New("control operation phase transition is invalid")
			}
			if next.UpdatedAtUnix < current.UpdatedAtUnix {
				return errors.New("control operation update time moved backwards")
			}
		case !current.Phase.terminal():
			return errors.New("another control operation is still active")
		case next.Phase != phasePrepared:
			return errors.New("a replacement control operation must begin prepared")
		}
	}
	if err := transaction.store.save(next); err != nil {
		return err
	}
	cloned := cloneServiceOperation(next)
	transaction.current = &cloned
	return nil
}

// restore installs state recovered from a fully verified audit chain. Normal
// callers must use save so every live transition is checked against the
// current operation.
func (transaction *controlStateTransaction) restore(next serviceOperation) error {
	if transaction.current != nil {
		return errors.New("control operation state already exists")
	}
	if err := next.validate(); err != nil {
		return err
	}
	if err := transaction.store.save(next); err != nil {
		return err
	}
	cloned := cloneServiceOperation(next)
	transaction.current = &cloned
	return nil
}

func (transaction *controlStateTransaction) remove() error {
	if transaction.current != nil && !transaction.current.Phase.terminal() {
		return errors.New("cannot remove a nonterminal control operation")
	}
	if err := transaction.store.remove(); err != nil {
		return err
	}
	transaction.current = nil
	return nil
}

func (s *controlStateStore) load() (*serviceOperation, error) {
	raw, err := readControlFile(s.path, maxControlStateBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var op serviceOperation
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&op); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("control operation state is malformed")
	}
	if err := op.validate(); err != nil {
		return nil, err
	}
	canonical, err := marshalControlState(op)
	if err != nil || !bytes.Equal(raw, canonical) {
		return nil, errors.New("control operation state is not canonical")
	}
	return &op, nil
}

func (s *controlStateStore) save(op serviceOperation) error {
	raw, err := marshalControlState(op)
	if err != nil {
		return err
	}
	return writeControlFileAtomic(s.path, raw, 0o600)
}

func (s *controlStateStore) remove() error {
	err := os.Remove(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("remove control operation state")
	}
	return syncControlDirectory(filepath.Dir(s.path))
}

func marshalControlState(op serviceOperation) ([]byte, error) {
	raw, err := marshalControlStateCheckpoint(op)
	if err != nil {
		return nil, err
	}
	raw = append(raw, '\n')
	if len(raw) > maxControlStateBytes {
		return nil, errors.New("control operation state exceeds its size limit")
	}
	return raw, nil
}

func marshalControlStateCheckpoint(op serviceOperation) ([]byte, error) {
	if err := op.validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(op)
	if err != nil {
		return nil, errors.New("encode control operation state")
	}
	if len(raw) > controlaudit.MaxStateCheckpointBytes {
		return nil, errors.New("control operation state exceeds the audit checkpoint limit")
	}
	return raw, nil
}

func parseControlStateCheckpoint(raw []byte) (serviceOperation, error) {
	if len(raw) == 0 || len(raw) > controlaudit.MaxStateCheckpointBytes {
		return serviceOperation{}, errors.New("control operation checkpoint has an invalid size")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var operation serviceOperation
	if err := decoder.Decode(&operation); err != nil {
		return serviceOperation{}, errors.New("control operation checkpoint is malformed")
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return serviceOperation{}, errors.New("control operation checkpoint has trailing data")
	}
	if err := operation.validate(); err != nil {
		return serviceOperation{}, err
	}
	canonical, err := marshalControlStateCheckpoint(operation)
	if err != nil || !bytes.Equal(raw, canonical) {
		return serviceOperation{}, errors.New("control operation checkpoint is not canonical")
	}
	return operation, nil
}

func newServiceOperation(
	id, serverSession, targetID string,
	action serviceAction,
	status serviceStatus,
	approval ControlApprovalEvidence,
	now time.Time,
	deadline time.Time,
) (serviceOperation, error) {
	op := serviceOperation{
		Version:       controlStateVersion,
		ID:            id,
		ServerSession: serverSession,
		TargetID:      targetID,
		Action:        action,
		Unit:          status.Unit,
		Scope:         status.Scope,
		StatusBefore:  status,
		BeforeHash:    serviceStateHash(status),
		Approval:      approval,
		Phase:         phasePrepared,
		StartedAtUnix: now.UTC().Unix(),
		UpdatedAtUnix: now.UTC().Unix(),
		DeadlineUnix:  deadline.UTC().Unix(),
	}
	if err := op.validate(); err != nil {
		return serviceOperation{}, fmt.Errorf("create control operation: %w", err)
	}
	return op, nil
}
