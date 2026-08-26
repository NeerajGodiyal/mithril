package replay

import (
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/state"
)

func TestEvaluateCoverageStates(t *testing.T) {
	tests := []struct {
		name                       string
		required, usable           bool
		verified, eligible         uint64
		sinceProgress, stallWindow time.Duration
		want                       VerificationState
	}{
		{"not applicable", false, false, 0, 0, 0, 0, VerificationNotApplicable},
		{"unavailable", true, false, 10, 20, 0, 0, VerificationUnavailable},
		{"complete", true, true, 20, 20, 0, 0, VerificationComplete},
		{"incomplete", true, true, 19, 20, time.Second, time.Minute, VerificationIncomplete},
		{"stalled", true, true, 19, 20, time.Minute, time.Minute, VerificationStalled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EvaluateCoverage(tt.required, tt.usable, tt.verified, tt.eligible, tt.sinceProgress, tt.stallWindow); got != tt.want {
				t.Fatalf("state=%q want %q", got, tt.want)
			}
		})
	}
}

func TestPersistedAndRuntimeAlpenglowMismatchCloseVerification(t *testing.T) {
	st := &state.MithrilState{LastRootedSlot: 9, AlpenglowEvidence: []state.AlpenglowFinalityEvidence{{Slot: 10}}}
	if !HasActivePersistedVerificationDivergence(st, true) || HasActivePersistedVerificationDivergence(st, false) {
		t.Fatal("active Alpenglow evidence classification is wrong")
	}
	st.LastRootedSlot = 10
	if HasActivePersistedVerificationDivergence(st, true) {
		t.Fatal("historical rooted evidence remained active")
	}

	ResetVerificationStatus(false, 0)
	t.Cleanup(func() { ResetVerificationStatus(false, 0) })
	if !markVerificationDivergedForError(&AlpenglowFinalityMismatch{Slot: 11}) {
		t.Fatal("finality mismatch was not recognized")
	}
	got, _, _, _ := VerificationSnapshot()
	if got != VerificationDiverged {
		t.Fatalf("state=%q", got)
	}
}

func TestVerificationDivergenceIsTerminal(t *testing.T) {
	v := NewVerificationStatus(true)
	v.MarkDiverged()
	if v.Set(VerificationComplete) {
		t.Fatal("diverged status must be terminal")
	}
	state, required, _, _ := v.Snapshot()
	if state != VerificationDiverged || !required {
		t.Fatalf("snapshot=(%q,%v)", state, required)
	}
}

func TestVerificationSnapshotReset(t *testing.T) {
	ResetVerificationStatus(true, 42)
	t.Cleanup(func() { ResetVerificationStatus(false, 0) })
	state, required, verified, eligible := VerificationSnapshot()
	if state != VerificationUnavailable || !required || verified != 42 || eligible != 42 {
		t.Fatalf("snapshot=(%q,%v,%d,%d)", state, required, verified, eligible)
	}
}
