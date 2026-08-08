package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Overclock-Validator/mithril/internal/safefile"
)

// serviceAction is the bounded internal action type. Callers never supply this
// value directly; every external string passes through parseServiceAction, so
// an action outside the registry cannot reach dispatch.
type serviceAction string

const (
	actionStart   serviceAction = "start"
	actionStop    serviceAction = "stop"
	actionRestart serviceAction = "restart"
)

// serviceActionSpec is everything one action is allowed to do. The argv is
// fixed per action: no executable, unit or extra argument is ever taken from
// the caller, so there is nothing for a caller to inject into.
type serviceActionSpec struct {
	action serviceAction
	verb   string

	// allowedPreState is the one settled ActiveState from which the action may
	// run. Recovery from a failed unit is deliberately not implicit.
	allowedPreState string
	success         serviceActionSuccessSpec
}

type serviceActionSuccessSpec struct {
	activeState       string
	subState          string
	pid               servicePIDRequirement
	transition        serviceTransition
	invocationChanges bool
}

type servicePIDRequirement uint8

const (
	servicePIDInvalid servicePIDRequirement = iota
	servicePIDZero
	servicePIDPositive
)

type serviceTransition uint8

const (
	serviceTransitionInvalid serviceTransition = iota
	serviceTransitionActiveEnter
	serviceTransitionInactiveEnter
)

// serviceActionRegistry contains exactly start, stop and restart. It is the
// single source for each action's precondition, systemctl verb and successful
// transition.
var serviceActionRegistry = map[serviceAction]serviceActionSpec{
	actionStart: {
		action:          actionStart,
		verb:            "start",
		allowedPreState: "inactive",
		success: serviceActionSuccessSpec{
			activeState:       "active",
			subState:          "running",
			pid:               servicePIDPositive,
			transition:        serviceTransitionActiveEnter,
			invocationChanges: true,
		},
	},
	actionStop: {
		action:          actionStop,
		verb:            "stop",
		allowedPreState: "active",
		success: serviceActionSuccessSpec{
			activeState: "inactive",
			pid:         servicePIDZero,
			transition:  serviceTransitionInactiveEnter,
		},
	},
	actionRestart: {
		action:          actionRestart,
		verb:            "restart",
		allowedPreState: "active",
		success: serviceActionSuccessSpec{
			activeState:       "active",
			subState:          "running",
			pid:               servicePIDPositive,
			transition:        serviceTransitionActiveEnter,
			invocationChanges: true,
		},
	},
}

// transitionalStates are never a valid pre-state for any action. A unit that is
// mid-transition has no settled state to act from, and issuing another action
// races the one already running.
var transitionalStates = map[string]bool{
	"activating":   true,
	"reloading":    true,
	"deactivating": true,
}

// parseServiceAction is the only way an external string becomes a
// serviceAction. It normalizes, then requires membership in the registry.
func parseServiceAction(raw string) (serviceAction, error) {
	action := serviceAction(strings.ToLower(strings.TrimSpace(raw)))
	if _, ok := serviceActionRegistry[action]; !ok {
		return "", fmt.Errorf("action must be one of start, stop, restart")
	}
	return action, nil
}

// parseAllowedServiceActions converts the explicit operator allowlist.
func parseAllowedServiceActions(raw []string) (map[serviceAction]bool, error) {
	if len(raw) == 0 {
		return nil, errors.New("operator mode requires a non-empty allowed service action list")
	}
	allowed := make(map[serviceAction]bool, len(raw))
	for _, value := range raw {
		action, err := parseServiceAction(value)
		if err != nil {
			return nil, fmt.Errorf("allowed service action %q is not recognised", value)
		}
		allowed[action] = true
	}
	return allowed, nil
}

// NormalizeServiceActions validates a deployment allowlist and returns it in the
// registry's stable order. The CLI uses this wrapper so generated entries and
// server startup enforce the same action vocabulary.
func NormalizeServiceActions(raw []string) ([]string, error) {
	allowed, err := parseAllowedServiceActions(raw)
	if err != nil {
		return nil, err
	}
	normalized := make([]string, 0, len(allowed))
	for _, action := range []serviceAction{actionStart, actionStop, actionRestart} {
		if allowed[action] {
			normalized = append(normalized, string(action))
		}
	}
	return normalized, nil
}

// validateActionPreState refuses actions while systemd is already changing the
// unit or its state is not an explicit precondition for the requested action.
func validateActionPreState(spec serviceActionSpec, status serviceStatus) error {
	if status.LoadState != "loaded" {
		return fmt.Errorf("service unit is not loaded (load_state=%s)", status.LoadState)
	}
	// A queued job means systemd is already doing something to this unit.
	if strings.TrimSpace(status.Job) != "" {
		return errors.New("service has a pending systemd job; refusing to queue another action")
	}
	if transitionalStates[status.ActiveState] {
		return fmt.Errorf("service is %s; refusing to act during a state transition", status.ActiveState)
	}
	if status.ActiveState == spec.allowedPreState {
		return nil
	}
	return fmt.Errorf("service state %s does not permit %s", status.ActiveState, spec.action)
}

// executableIdentity is the recorded identity of the systemctl binary.
//
// It is captured at startup and re-checked immediately before every execve.
// A startup-only check leaves a window in which the binary can be replaced
// between validation and use, which is precisely the window an attacker wants.
type executableIdentity struct {
	path            string
	device          uint64
	inode           uint64
	mode            os.FileMode
	uid             uint32
	allowedOwnerUID uint32
	digest          string
}

// resolveExecutable validates the systemctl path and records its identity.
//
// The path itself, and every parent directory, must be free of symlinks and
// root-owned and not group- or world-writable: a writable or non-root parent
// lets another principal swap the binary without touching its permissions.
func resolveExecutable(path string) (executableIdentity, error) {
	return resolveExecutableOwnedBy(path, 0)
}

// resolveExecutableOwnedBy keeps filesystem-heavy tests unprivileged. Production
// passes zero, so only root ownership is accepted; tests may additionally allow
// the current uid for temporary fixture directories.
func resolveExecutableOwnedBy(path string, allowedOwnerUID uint32) (executableIdentity, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return executableIdentity{}, errors.New("systemctl path must be a clean absolute path")
	}

	// Reject a symlink anywhere in the path, including the final component.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return executableIdentity{}, errors.New("systemctl path could not be resolved")
	}
	if resolved != path {
		return executableIdentity{}, errors.New("systemctl path must not traverse a symlink")
	}

	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		info, err := os.Lstat(dir)
		if err != nil {
			return executableIdentity{}, errors.New("systemctl parent directory is unavailable")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return executableIdentity{}, errors.New("systemctl path must not traverse a symlink")
		}
		if err := requireExecutableOwner(dir, info, allowedOwnerUID); err != nil {
			return executableIdentity{}, err
		}
		if info.Mode().Perm()&0o022 != 0 {
			return executableIdentity{}, fmt.Errorf("systemctl parent directory %s is group or world writable", dir)
		}
		if dir == "/" {
			break
		}
	}

	info, err := os.Lstat(path)
	if err != nil {
		return executableIdentity{}, errors.New("systemctl executable is unavailable")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return executableIdentity{}, errors.New("systemctl path must not traverse a symlink")
	}
	if !info.Mode().IsRegular() {
		return executableIdentity{}, errors.New("systemctl path is not a regular file")
	}
	if err := requireExecutableOwner(path, info, allowedOwnerUID); err != nil {
		return executableIdentity{}, err
	}
	if info.Mode().Perm()&0o111 == 0 {
		return executableIdentity{}, errors.New("systemctl path is not executable")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return executableIdentity{}, errors.New("systemctl executable is group or world writable")
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return executableIdentity{}, errors.New("systemctl executable identity is unavailable")
	}

	digest, err := fileDigest(path)
	if err != nil {
		return executableIdentity{}, err
	}

	return executableIdentity{
		path:            path,
		device:          uint64(stat.Dev),
		inode:           stat.Ino,
		mode:            info.Mode(),
		uid:             stat.Uid,
		allowedOwnerUID: allowedOwnerUID,
		digest:          digest,
	}, nil
}

func requireExecutableOwner(path string, info os.FileInfo, allowedOwnerUID uint32) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("systemctl filesystem owner is unavailable")
	}
	if executableOwnerAllowed(stat.Uid, allowedOwnerUID) {
		return nil
	}
	return fmt.Errorf("systemctl path component %s must be root-owned", path)
}

func executableOwnerAllowed(uid, allowedOwnerUID uint32) bool {
	return uid == 0 || uid == allowedOwnerUID
}

// verifyUnchanged re-resolves the executable and requires every recorded value
// to match. Called immediately before dispatch, so a binary replaced after
// startup is caught before it runs rather than after.
func (id executableIdentity) verifyUnchanged() error {
	current, err := resolveExecutableOwnedBy(id.path, id.allowedOwnerUID)
	if err != nil {
		return err
	}
	switch {
	case current.device != id.device:
		return errors.New("systemctl executable device changed since startup")
	case current.inode != id.inode:
		return errors.New("systemctl executable inode changed since startup")
	case current.mode != id.mode:
		return errors.New("systemctl executable mode changed since startup")
	case current.uid != id.uid:
		return errors.New("systemctl executable owner changed since startup")
	case current.digest != id.digest:
		return errors.New("systemctl executable contents changed since startup")
	}
	return nil
}

func fileDigest(path string) (string, error) {
	f, err := safefile.OpenStableRegular(path)
	if err != nil {
		return "", errors.New("systemctl executable is unreadable")
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", errors.New("systemctl executable could not be digested")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// describeAllowedActions renders the allowlist for a tool description, in the
// registry's fixed order so the text is stable across runs.
func describeAllowedActions(allowed map[serviceAction]bool) string {
	names := make([]string, 0, len(serviceActionRegistry))
	for _, action := range []serviceAction{actionStart, actionStop, actionRestart} {
		if allowed[action] {
			names = append(names, string(action))
		}
	}
	switch len(names) {
	case 0:
		return "no lifecycle actions (none are allowed by this deployment)"
	case 1:
		return names[0]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
	}
}
