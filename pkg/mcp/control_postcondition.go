package mcp

import "strings"

// postconditionResult is deliberately narrower than operationPhase: a single
// observation can only settle, continue, or become ambiguous.
type postconditionResult uint8

const (
	postconditionOutcomeUnknown postconditionResult = iota
	postconditionStillVerifying
	postconditionSucceeded
)

// servicePostconditionObservation contains one trusted, bounded status sample.
type servicePostconditionObservation struct {
	Current serviceStatus

	ObservationAuthenticated bool
	DeadlineReached          bool
}

// evaluateServicePostcondition is pure and evaluates exactly one observation.
// Polling and deadline enforcement remain the caller's responsibility.
func evaluateServicePostcondition(
	action serviceAction,
	before serviceStatus,
	observation servicePostconditionObservation,
) postconditionResult {
	spec, ok := serviceActionRegistry[action]
	if !ok {
		return postconditionOutcomeUnknown
	}
	if !observation.ObservationAuthenticated {
		return unresolvedPostcondition(observation.DeadlineReached)
	}

	if servicePostconditionSatisfied(spec, before, observation.Current) {
		return postconditionSucceeded
	}
	return unresolvedPostcondition(observation.DeadlineReached)
}

func servicePostconditionSatisfied(
	spec serviceActionSpec,
	before serviceStatus,
	current serviceStatus,
) bool {
	if before.ActiveState != spec.allowedPreState {
		return false
	}
	success := spec.success
	if current.LoadState != "loaded" || strings.TrimSpace(current.Job) != "" {
		return false
	}
	if current.ActiveState != success.activeState {
		return false
	}
	if success.subState != "" && current.SubState != success.subState {
		return false
	}
	switch success.pid {
	case servicePIDZero:
		if current.MainPID != 0 {
			return false
		}
	case servicePIDPositive:
		if current.MainPID == 0 {
			return false
		}
	default:
		return false
	}
	return serviceTransitionAttributed(success, before, current)
}

func invocationChanged(before, current string) bool {
	return before != "" && current != "" && before != current
}

func serviceTransitionAttributed(
	success serviceActionSuccessSpec,
	before serviceStatus,
	current serviceStatus,
) bool {
	// Start and stop change ActiveState. That before/after change remains
	// usable when systemd garbage-collects an inactive unit and clears its
	// transition timestamps. Restart stays in active and needs generation
	// evidence below.
	if before.ActiveState != success.activeState {
		return true
	}
	if success.invocationChanges && invocationChanged(before.InvocationID, current.InvocationID) {
		return true
	}
	switch success.transition {
	case serviceTransitionActiveEnter:
		return current.ActiveEnterTimestampMonotonic > before.ActiveEnterTimestampMonotonic
	case serviceTransitionInactiveEnter:
		return current.InactiveEnterTimestampMonotonic > before.InactiveEnterTimestampMonotonic
	default:
		return false
	}
}

func unresolvedPostcondition(deadlineReached bool) postconditionResult {
	if deadlineReached {
		return postconditionOutcomeUnknown
	}
	return postconditionStillVerifying
}
