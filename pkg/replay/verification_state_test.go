package replay

import (
	"sync"
	"testing"
	"time"
)

func TestEvaluateCoverageMatrix(t *testing.T) {
	const window = 30 * time.Second

	cases := []struct {
		name           string
		required       bool
		evidenceUsable bool
		verified       uint64
		eligible       uint64
		sinceProgress  time.Duration
		want           VerificationState
	}{
		{"not required is never a coverage judgement", false, true, 0, 100, time.Hour, VerificationNotApplicable},
		{"not required even when evidence is fine", false, true, 100, 100, 0, VerificationNotApplicable},
		{"required but evidence unusable", true, false, 90, 100, 0, VerificationUnavailable},
		{"unusable outranks being caught up", true, false, 100, 100, 0, VerificationUnavailable},
		{"covered", true, true, 100, 100, 0, VerificationComplete},
		{"covered beyond the target", true, true, 120, 100, time.Hour, VerificationComplete},
		{"behind but progressing", true, true, 90, 100, window - time.Second, VerificationIncomplete},
		{"behind with no progress for the window", true, true, 90, 100, window, VerificationStalled},
		{"behind, long past the window", true, true, 1, 100, time.Hour, VerificationStalled},
		{"no stall window configured never stalls", true, true, 90, 100, time.Hour, VerificationIncomplete},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := window
			if tc.name == "no stall window configured never stalls" {
				w = 0
			}
			got := EvaluateCoverage(tc.required, tc.evidenceUsable, tc.verified, tc.eligible, tc.sinceProgress, w)
			if got != tc.want {
				t.Fatalf("state = %q, want %q", got, tc.want)
			}
			if !got.Valid() {
				t.Fatalf("state %q is outside the bounded set", got)
			}
		})
	}
}

func TestVerificationStateHealthiness(t *testing.T) {
	healthy := map[VerificationState]bool{
		VerificationComplete:      true,
		VerificationNotApplicable: true,
	}
	for _, s := range []VerificationState{
		VerificationComplete, VerificationIncomplete, VerificationStalled,
		VerificationDiverged, VerificationUnavailable, VerificationNotApplicable,
	} {
		if got := s.Healthy(); got != healthy[s] {
			t.Errorf("%q.Healthy() = %v, want %v", s, got, healthy[s])
		}
	}
}

func TestVerificationStateIsBounded(t *testing.T) {
	for _, s := range []VerificationState{"", "ok", "COMPLETE", "healthy", "unknown", "notapplicable"} {
		if s.Valid() {
			t.Errorf("undefined state %q was accepted", s)
		}
	}
}

func TestDivergenceIsTerminal(t *testing.T) {
	v := NewVerificationStatus(true)
	v.MarkDiverged()

	for _, attempt := range []VerificationState{
		VerificationComplete, VerificationIncomplete, VerificationStalled,
		VerificationUnavailable, VerificationNotApplicable,
	} {
		if v.Set(attempt) {
			t.Errorf("Set(%q) succeeded after divergence", attempt)
		}
		if state, _, _, _ := v.Snapshot(); state != VerificationDiverged {
			t.Fatalf("state became %q after divergence", state)
		}
	}
}

func TestNotRequiredCannotAcquireCoverageState(t *testing.T) {
	v := NewVerificationStatus(false)
	state, required, _, _ := v.Snapshot()
	if required {
		t.Fatal("a not-required status reports required")
	}
	if state != VerificationNotApplicable {
		t.Fatalf("initial state = %q, want not_applicable", state)
	}

	for _, attempt := range []VerificationState{
		VerificationComplete, VerificationIncomplete, VerificationStalled, VerificationUnavailable,
	} {
		if v.Set(attempt) {
			t.Errorf("Set(%q) succeeded on a not-required status", attempt)
		}
	}
	if state, _, _, _ := v.Snapshot(); state != VerificationNotApplicable {
		t.Fatalf("state drifted to %q", state)
	}

	v.MarkDiverged()
	if state, _, _, _ := v.Snapshot(); state != VerificationDiverged {
		t.Fatal("divergence was not recordable on a not-required status")
	}
}

func TestRequiredStartsUnavailableNotComplete(t *testing.T) {
	v := NewVerificationStatus(true)
	state, required, _, _ := v.Snapshot()
	if !required {
		t.Fatal("a required status reports not required")
	}
	if state != VerificationUnavailable {
		t.Fatalf("initial state = %q, want unavailable", state)
	}
	if state.Healthy() {
		t.Fatal("a node that has observed nothing reports healthy verification")
	}
}

func TestResetRestoresDurableVerifiedPrefix(t *testing.T) {
	ResetVerificationStatus(true, 123)
	defer ResetVerificationStatus(false, 0)

	state, required, verified, eligible := VerificationSnapshot()
	if state != VerificationUnavailable || !required || verified != 123 || eligible != 123 {
		t.Fatalf("restored status = %q/%v/%d/%d", state, required, verified, eligible)
	}
}

func TestNilVerificationStatusIsSafe(t *testing.T) {
	var v *VerificationStatus
	state, required, verified, eligible := v.Snapshot()
	if state != VerificationNotApplicable || required || verified != 0 || eligible != 0 {
		t.Fatalf("nil snapshot = %q/%v/%d/%d", state, required, verified, eligible)
	}
	if v.Set(VerificationComplete) {
		t.Error("Set succeeded on a nil status")
	}
	v.MarkDiverged()
	v.SetWatermarks(1, 2)
}

func TestModeConfigMatrix(t *testing.T) {
	cases := []struct {
		name     string
		enabled  bool
		required bool
		tail     bool
		want     VerificationState
	}{
		{"classic (no unrooted tail)", false, false, false, VerificationNotApplicable},
		{"alpenglow, verifier disabled", false, false, true, VerificationNotApplicable},
		{"alpenglow, advisory (enabled, not required)", true, false, true, VerificationNotApplicable},
		{"alpenglow, required but no tail state", true, true, false, VerificationNotApplicable},
		{"alpenglow, enabled and required", true, true, true, VerificationUnavailable},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ResetVerificationStatus(tc.enabled && tc.required && tc.tail, 0)
			state, required, _, _ := VerificationSnapshot()
			if state != tc.want {
				t.Fatalf("state = %q, want %q", state, tc.want)
			}
			if want := tc.enabled && tc.required && tc.tail; required != want {
				t.Errorf("required = %v, want %v", required, want)
			}
			// No row may start out claiming coverage it has not demonstrated.
			if state == VerificationComplete {
				t.Error("a freshly configured run reports complete coverage")
			}
		})
	}
	ResetVerificationStatus(false, 0)
}

func TestDivergenceIsVisibleBeforeHalt(t *testing.T) {
	ResetVerificationStatus(true, 0)
	if state, _, _, _ := VerificationSnapshot(); state == VerificationDiverged {
		t.Fatal("a fresh run starts diverged")
	}

	MarkVerificationDiverged()

	state, _, _, _ := VerificationSnapshot()
	if state != VerificationDiverged {
		t.Fatalf("state = %q after marking divergence", state)
	}
	if state.Healthy() {
		t.Fatal("a diverged run reports healthy")
	}
	ResetVerificationStatus(false, 0)
}

func TestConcurrentProgressCannotReplacePublishedDivergence(t *testing.T) {
	ResetVerificationStatus(true, 0)
	defer ResetVerificationStatus(false, 0)

	var started sync.WaitGroup
	started.Add(1)
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		started.Done()
		<-release
		updateVerificationProgress(VerificationIncomplete, 10, 20)
		close(done)
	}()
	started.Wait()

	MarkVerificationDiverged()
	close(release)
	<-done

	state, _, verified, eligible := VerificationSnapshot()
	if state != VerificationDiverged {
		t.Fatalf("state = %q after concurrent progress, want diverged", state)
	}
	if verified != 10 || eligible != 20 {
		t.Fatalf("watermarks = %d/%d, want latest 10/20", verified, eligible)
	}
}
