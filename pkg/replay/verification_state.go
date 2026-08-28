package replay

import (
	"errors"
	"sync"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/state"
)

// VerificationState is the bounded state of replay verification coverage.
type VerificationState string

const (
	VerificationComplete      VerificationState = "complete"
	VerificationIncomplete    VerificationState = "incomplete"
	VerificationStalled       VerificationState = "stalled"
	VerificationDiverged      VerificationState = "diverged"
	VerificationUnavailable   VerificationState = "unavailable"
	VerificationNotApplicable VerificationState = "not_applicable"
)

func (s VerificationState) Valid() bool {
	switch s {
	case VerificationComplete, VerificationIncomplete, VerificationStalled,
		VerificationDiverged, VerificationUnavailable, VerificationNotApplicable:
		return true
	default:
		return false
	}
}

func (s VerificationState) Healthy() bool {
	return s == VerificationComplete || s == VerificationNotApplicable
}

type VerificationStatus struct {
	mu           sync.RWMutex
	state        VerificationState
	required     bool
	verifiedSlot uint64
	eligibleSlot uint64
}

func NewVerificationStatus(required bool) *VerificationStatus {
	state := VerificationNotApplicable
	if required {
		state = VerificationUnavailable
	}
	return &VerificationStatus{state: state, required: required}
}

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
		return false
	}
	v.state = state
	return true
}

func (v *VerificationStatus) MarkDiverged() {
	if v == nil {
		return
	}
	v.mu.Lock()
	v.state = VerificationDiverged
	v.mu.Unlock()
}

func (v *VerificationStatus) SetWatermarks(verified, eligible uint64) {
	if v == nil {
		return
	}
	v.mu.Lock()
	v.verifiedSlot, v.eligibleSlot = verified, eligible
	v.mu.Unlock()
}

func (v *VerificationStatus) Snapshot() (VerificationState, bool, uint64, uint64) {
	if v == nil {
		return VerificationNotApplicable, false, 0, 0
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.state, v.required, v.verifiedSlot, v.eligibleSlot
}

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

var (
	verificationTransitionMu sync.RWMutex
	verificationStatus       = NewVerificationStatus(false)
)

func initializeVerificationStatus(required bool, initialWatermark uint64, diverged bool) {
	verificationTransitionMu.Lock()
	defer verificationTransitionMu.Unlock()
	verificationStatus = NewVerificationStatus(required)
	verificationStatus.SetWatermarks(initialWatermark, initialWatermark)
	if diverged {
		verificationStatus.MarkDiverged()
	}
}

// ResetVerificationStatus initializes the process-wide status before RPC starts
// serving requests. Required verification starts unavailable until usable evidence is
// observed; modes with no trailing verifier report not_applicable.
func ResetVerificationStatus(required bool, initialWatermark uint64) {
	initializeVerificationStatus(required, initialWatermark, false)
}

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
		state, _, verified, eligible = verificationStatus.Snapshot()
	}
	publishVerificationProgress(state, verified, eligible)
}

func MarkVerificationDiverged() {
	verificationTransitionMu.Lock()
	defer verificationTransitionMu.Unlock()
	verificationStatus.MarkDiverged()
	_, _, verified, eligible := verificationStatus.Snapshot()
	publishVerificationProgress(VerificationDiverged, verified, eligible)
}

// HasActivePersistedVerificationDivergence restores the fail-closed gate on a
// restart. Alpenglow evidence at or below the durable root is historical; an
// unresolved record above it still protects an uncommitted disputed slot.
func HasActivePersistedVerificationDivergence(st *state.MithrilState, alpenglowMode bool) bool {
	if st == nil {
		return false
	}
	if len(st.ReplayDivergenceEvidence) != 0 {
		return true
	}
	if alpenglowMode {
		for _, evidence := range st.AlpenglowEvidence {
			if evidence.Slot > st.LastRootedSlot {
				return true
			}
		}
	}
	return false
}

func markVerificationDivergedForError(err error) bool {
	var finality *AlpenglowFinalityMismatch
	if !errors.As(err, &finality) {
		return false
	}
	MarkVerificationDiverged()
	return true
}
