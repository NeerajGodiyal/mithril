package notifier

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyDeployConfigRejectsUnreplacedPlaceholders(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("alertmanager.yml", "receivers:\n  - name: deadman\n    webhook_configs:\n      - url: https://"+DeployPlaceholder+".invalid/deadman\n")
	write("clean.yml", "route:\n  receiver: telegram-primary\n")

	err := VerifyDeployConfig(dir)
	if err == nil {
		t.Fatal("an unreplaced placeholder was accepted")
	}
	if !strings.Contains(err.Error(), "alertmanager.yml:4") {
		t.Fatalf("error %q does not locate the placeholder", err)
	}
	// The line itself can carry an endpoint or credential; only the location
	// may be reported.
	if strings.Contains(err.Error(), "invalid/deadman") {
		t.Fatalf("error leaked the configured value: %q", err)
	}
}

func TestVerifyDeployConfigAcceptsFullyConfiguredDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "alertmanager.yml"),
		[]byte("receivers:\n  - name: deadman\n    webhook_configs:\n      - url: https://heartbeat.example.com/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"),
		[]byte(DeployPlaceholder+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyDeployConfig(dir); err != nil {
		t.Fatalf("a configured directory was rejected: %v", err)
	}
}

// The shipped templates are the exact thing an operator must fix, so the
// checker has to flag them rather than treat them as a special case.
func TestVerifyDeployConfigFlagsShippedTemplates(t *testing.T) {
	err := VerifyDeployConfig("../../prometheus")
	if err == nil {
		t.Fatal("shipped templates reported clean; the placeholders they document are gone or the scan missed them")
	}
	for _, want := range []string{"alertmanager.yml", "prometheus.yml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("scan did not flag %s", want)
		}
	}
}

func TestVerifyDeployConfigScansSymlinkedDeploymentFiles(t *testing.T) {
	realDir := t.TempDir()
	config := filepath.Join(realDir, "alertmanager.yml")
	if err := os.WriteFile(config, []byte(DeployPlaceholder+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkedDir := t.TempDir()
	if err := os.Symlink(config, filepath.Join(linkedDir, "alertmanager.yml")); err != nil {
		t.Fatal(err)
	}
	if err := VerifyDeployConfig(linkedDir); err == nil {
		t.Fatal("placeholder behind a deployment symlink was accepted")
	}
}

func TestVerifyDeployConfigRejectsNestedSymlinkedDirectories(t *testing.T) {
	realDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(realDir, "targets.yml"), []byte(DeployPlaceholder+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Symlink(realDir, filepath.Join(root, "targets")); err != nil {
		t.Fatal(err)
	}

	err := VerifyDeployConfig(root)
	if err == nil || !strings.Contains(err.Error(), "symlinked directory") {
		t.Fatalf("nested symlinked directory error = %v", err)
	}
}

func TestVerifyDeployConfigRejectsOversizedFiles(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "prometheus.yml")
	file, err := os.Create(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxDeployScanBytes + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := VerifyDeployConfig(dir); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized deployment config error = %v", err)
	}
}
