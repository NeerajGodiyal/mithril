package replay

import (
	"sync"
	"time"
)

// VerificationState is the bounded state of trailing verification coverage.
//
// The states are deliberately exhaustive, including the two that are easy to
// omit: not_applicable, for a configuration where trailing verification is not
// required at all, and unavailable, for a required evidence source that cannot
// return usable evidence. Collapsing either into "incomplete" would make a node
// that is fine and a node that cannot see look identical.
type VerificationState string

const (
	// VerificationComplete means required evidence covers the eligible watermark.
	VerificationComplete VerificationState = "complete"
	// VerificationIncomplete means coverage is behind but progressing.
	VerificationIncomplete VerificationState = "incomplete"
	// VerificationStalled means the eligible target is ahead and no progress
	// occurred for the configured window.
	VerificationStalled VerificationState = "stalled"
	// VerificationDiverged means replay, footer, bank-hash or finality evidence
	// disagrees. This is terminal for the affected range.
	VerificationDiverged VerificationState = "diverged"
	// VerificationUnavailable means the required evidence source cannot return
	// usable evidence. It is NOT the same as being behind.
	VerificationUnavailable VerificationState = "unavailable"
	// VerificationNotApplicable means trailing verification is not required in
	// this mode or configuration — classic, or verification disabled. It must
	// never be reported as complete or incomplete, which would imply a
	// judgement about coverage that does not apply.
	VerificationNotApplicable VerificationState = "not_applicable"
)

// Valid reports whether s is one of the six defined states. These become
// Prometheus label values and MCP output, so an undefined value is rejected
// rather than passed through.
func (s VerificationState) Valid() bool {
	switch s {
	case VerificationComplete, VerificationIncomplete, VerificationStalled,
		VerificationDiverged, VerificationUnavailable, VerificationNotApplicable:
		return true
	default:
		return false
	}
}

// Healthy reports whether the state permits treating verified progress as
// trustworthy. not_applicable is healthy because nothing was required;
// unavailable is NOT, because a required source that cannot answer is a gap,
// not an absence of concern.
func (s VerificationState) Healthy() bool {
	return s == VerificationComplete || s == VerificationNotApplicable
}

// VerificationStatus is the runtime bookkeeping replay owns for trailing
// verification. Required is explicit rather than inferred from the state,
// because "not required" and "required but unknown" must stay distinguishable.
type VerificationStatus struct {
	mu           sync.RWMutex
	state        VerificationState
	required     bool
	verifiedSlot uint64
	eligibleSlot uint64
}

// NewVerificationStatus returns bookkeeping for a mode where verification is or
// is not required. A not-required configuration starts in not_applicable, never
// in a coverage state.
func NewVerificationStatus(required bool) *VerificationStatus {
	state := VerificationNotApplicable
	if required {
		// Required but not yet observed is unavailable, not complete: nothing
		// has demonstrated coverage yet.
		state = VerificationUnavailable
	}
	return &VerificationStatus{state: state, required: required}
}

// Set records a new state. It refuses undefined values and refuses to move out
// of diverged: divergence is terminal for the affected range, and letting later
// progress overwrite it would erase the very evidence an operator must triage.
func (v *VerificationStatus) Set(state VerificationState) bool {
	if v == nil || !state.Valid() {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.state == VerificationDiverged && state != VerificationDiverged {
		return false
	}
	if !v.required && state != VerificationNotApplicable && state != VerificationDiverged {
		// A configuration that does not require verification cannot acquire a
		// coverage judgement. Divergence is still recordable because a
		// bank-hash mismatch matters regardless of trailing verification.
		return false
	}
	v.state = state
	return true
}

// MarkDiverged records divergence. It is separate from Set because it must
// succeed in every configuration and must be callable before returning a
// mismatch error, so the state is visible before any halt.
func (v *VerificationStatus) MarkDiverged() {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.state = VerificationDiverged
}

// SetWatermarks records verified and eligible progress.
func (v *VerificationStatus) SetWatermarks(verified, eligible uint64) {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.verifiedSlot, v.eligibleSlot = verified, eligible
}

// Snapshot returns the current bookkeeping.
func (v *VerificationStatus) Snapshot() (state VerificationState, required bool, verified, eligible uint64) {
	if v == nil {
		return VerificationNotApplicable, false, 0, 0
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.state, v.required, v.verifiedSlot, v.eligibleSlot
}

// EvaluateCoverage derives the coverage state from watermarks and progress
// time. It is a pure function of its inputs so the six-state matrix is testable
// with a fake clock rather than by waiting.
//
// The rules, in order:
//   - not required            -> not_applicable, always
//   - evidence unusable       -> unavailable (NOT incomplete: a source that
//     cannot answer is a gap, not lateness)
//   - verified >= eligible    -> complete
//   - behind, progressed      -> incomplete
//   - behind, no progress for
//     the stall window        -> stalled
//
// Divergence is not derived here. It is recorded explicitly at the point a
// mismatch is detected, before the error is returned.
func EvaluateCoverage(required, evidenceUsable bool, verified, eligible uint64,
	sinceProgress, stallWindow time.Duration) VerificationState {
	if !required {
		return VerificationNotApplicable
	}
	if !evidenceUsable {
		return VerificationUnavailable
	}
	if verified >= eligible {
		return VerificationComplete
	}
	if stallWindow > 0 && sinceProgress >= stallWindow {
		return VerificationStalled
	}
	return VerificationIncomplete
}

// verificationStatus is the process-wide bookkeeping for the current replay
// run. It follows the existing TrailingVerifierCfg pattern: replay is a
// single-run process, and the trailing verifier is already configured through
// package state.
var (
	verificationTransitionMu sync.RWMutex
	verificationStatus       = NewVerificationStatus(false)
)

// initializeVerificationStatus installs bookkeeping for a run and restores a
// terminal divergence recorded by an earlier process.
func initializeVerificationStatus(required bool, initialWatermark uint64, diverged bool) VerificationState {
	verificationTransitionMu.Lock()
	defer verificationTransitionMu.Unlock()

	verificationStatus = NewVerificationStatus(required)
	if required {
		verificationStatus.SetWatermarks(initialWatermark, initialWatermark)
	}
	if diverged {
		verificationStatus.MarkDiverged()
	}
	state, _, _, _ := verificationStatus.Snapshot()
	return state
}

// ResetVerificationStatus installs fresh bookkeeping for a run. The initial
// watermark is the durable prefix already admitted by the verification gate.
func ResetVerificationStatus(required bool, initialWatermark uint64) {
	initializeVerificationStatus(required, initialWatermark, false)
}

// VerificationSnapshot exposes the current bookkeeping for RPC and MCP.
func VerificationSnapshot() (VerificationState, bool, uint64, uint64) {
	verificationTransitionMu.RLock()
	defer verificationTransitionMu.RUnlock()

	return verificationStatus.Snapshot()
}

func updateVerificationProgress(state VerificationState, verified, eligible uint64) {
	verificationTransitionMu.Lock()
	defer verificationTransitionMu.Unlock()

	verificationStatus.SetWatermarks(verified, eligible)
	if !verificationStatus.Set(state) {
		return
	}
}

// MarkVerificationDiverged records divergence before the mismatch leaves the
// replay loop. Durable evidence is written separately before shutdown
// promotion runs.
func MarkVerificationDiverged() {
	verificationTransitionMu.Lock()
	defer verificationTransitionMu.Unlock()

	verificationStatus.MarkDiverged()
}
