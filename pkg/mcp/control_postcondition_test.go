package mcp

import "testing"

func postconditionStatus(
	activeState string,
	subState string,
	pid uint64,
	activeAt uint64,
	inactiveAt uint64,
) serviceStatus {
	return serviceStatus{
		Unit:                            "mithril.service",
		Scope:                           "system",
		LoadState:                       "loaded",
		ActiveState:                     activeState,
		SubState:                        subState,
		MainPID:                         pid,
		ActiveEnterTimestampMonotonic:   activeAt,
		InactiveEnterTimestampMonotonic: inactiveAt,
	}
}

func authenticatedObservation(current serviceStatus) servicePostconditionObservation {
	return servicePostconditionObservation{
		Current:                  current,
		ObservationAuthenticated: true,
	}
}

func TestServicePostconditionSuccessMatrix(t *testing.T) {
	inactive := postconditionStatus("inactive", "dead", 0, 0, 10)
	running := postconditionStatus("active", "running", 101, 20, 10)

	tests := []struct {
		name        string
		action      serviceAction
		before      serviceStatus
		observation servicePostconditionObservation
	}{
		{
			name:        "start",
			action:      actionStart,
			before:      inactive,
			observation: authenticatedObservation(running),
		},
		{
			name:   "start without retained transition metadata",
			action: actionStart,
			before: inactive,
			observation: authenticatedObservation(
				postconditionStatus("active", "running", 101, 0, 0),
			),
		},
		{
			name:   "start changed invocation with unchanged timestamp",
			action: actionStart,
			before: func() serviceStatus {
				status := postconditionStatus("inactive", "dead", 0, 20, 10)
				status.InvocationID = "old"
				return status
			}(),
			observation: func() servicePostconditionObservation {
				current := running
				current.InvocationID = "new"
				return authenticatedObservation(current)
			}(),
		},
		{
			name:   "stop after inactive unit metadata is cleared",
			action: actionStop,
			before: running,
			observation: authenticatedObservation(
				postconditionStatus("inactive", "dead", 0, 0, 0),
			),
		},
		{
			name:   "restart changed invocation with reused PID",
			action: actionRestart,
			before: func() serviceStatus {
				status := running
				status.InvocationID = "old"
				return status
			}(),
			observation: func() servicePostconditionObservation {
				current := running
				current.InvocationID = "new"
				return authenticatedObservation(current)
			}(),
		},
		{
			name:   "restart newer activation with reused PID",
			action: actionRestart,
			before: func() serviceStatus {
				status := running
				status.InvocationID = "same"
				return status
			}(),
			observation: func() servicePostconditionObservation {
				current := running
				current.ActiveEnterTimestampMonotonic++
				current.InvocationID = "same"
				return authenticatedObservation(current)
			}(),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := evaluateServicePostcondition(
				test.action,
				test.before,
				test.observation,
			)
			if got != postconditionSucceeded {
				t.Fatalf("result = %v, want succeeded", got)
			}
		})
	}
}

func TestServicePostconditionRejectsIncompleteSuccess(t *testing.T) {
	inactive := postconditionStatus("inactive", "dead", 0, 0, 10)
	running := postconditionStatus("active", "running", 101, 20, 10)

	tests := []struct {
		name    string
		action  serviceAction
		before  serviceStatus
		current serviceStatus
	}{
		{"start activating", actionStart, inactive, postconditionStatus("activating", "start", 101, 0, 10)},
		{"start active but not running", actionStart, inactive, postconditionStatus("active", "exited", 101, 20, 10)},
		{"start missing PID", actionStart, inactive, postconditionStatus("active", "running", 0, 20, 10)},
		{"start unit not loaded", actionStart, inactive, func() serviceStatus {
			current := running
			current.LoadState = "not-found"
			return current
		}()},
		{"start has pending job", actionStart, inactive, func() serviceStatus {
			current := running
			current.Job = "42 stop"
			return current
		}()},
		{"start from active", actionStart, running, running},
		{"stop deactivating", actionStop, running, postconditionStatus("deactivating", "stop", 101, 20, 10)},
		{"stop retains PID", actionStop, running, postconditionStatus("inactive", "dead", 101, 20, 30)},
		{"stop from inactive", actionStop, inactive, inactive},
		{"restart from inactive", actionRestart, inactive, running},
		{"restart PID change alone", actionRestart, running, postconditionStatus("active", "running", 202, 20, 10)},
		{"restart reused PID alone", actionRestart, running, running},
		{"restart no identity evidence", actionRestart, running, postconditionStatus("active", "running", 202, 20, 10)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observation := authenticatedObservation(test.current)
			got := evaluateServicePostcondition(test.action, test.before, observation)
			if got != postconditionStillVerifying {
				t.Fatalf("result = %v, want still verifying", got)
			}
		})
	}
}

func TestServicePostconditionDeadlineLeavesAmbiguityUnknown(t *testing.T) {
	before := postconditionStatus("active", "running", 101, 20, 10)
	tests := []struct {
		name          string
		authenticated bool
		current       serviceStatus
	}{
		{
			name:          "undesired current state",
			authenticated: true,
			current:       postconditionStatus("failed", "failed", 0, 20, 30),
		},
		{
			name:          "restart lacks generation proof",
			authenticated: true,
			current:       postconditionStatus("active", "running", 202, 20, 10),
		},
		{
			name:          "unauthenticated apparent success",
			authenticated: false,
			current:       postconditionStatus("active", "running", 202, 30, 10),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observation := servicePostconditionObservation{
				Current:                  test.current,
				ObservationAuthenticated: test.authenticated,
				DeadlineReached:          true,
			}
			observation.Current.InvocationID = "old"
			before.InvocationID = "old"
			got := evaluateServicePostcondition(actionRestart, before, observation)
			if got != postconditionOutcomeUnknown {
				t.Fatalf("result = %v, want outcome unknown", got)
			}
		})
	}
}

func TestServicePostconditionUnknownActionIsBounded(t *testing.T) {
	observation := authenticatedObservation(
		postconditionStatus("active", "running", 101, 20, 10),
	)
	got := evaluateServicePostcondition(serviceAction("reload"), serviceStatus{}, observation)
	if got != postconditionOutcomeUnknown {
		t.Fatalf("result = %v, want outcome unknown", got)
	}
}
