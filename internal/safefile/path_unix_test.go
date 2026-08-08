//go:build unix

package safefile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateNoSymlinkPathAndAncestors(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	realParent := filepath.Join(base, "real")
	nested := filepath.Join(realParent, "nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(nested, "value")
	if err := os.WriteFile(file, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateNoSymlinkPath(file); err != nil {
		t.Fatalf("valid path rejected: %v", err)
	}
	if err := ValidateNoSymlinkAncestors(filepath.Join(nested, "new")); err != nil {
		t.Fatalf("valid ancestors rejected: %v", err)
	}

	link := filepath.Join(base, "link")
	if err := os.Symlink(realParent, link); err != nil {
		t.Fatal(err)
	}
	if err := ValidateNoSymlinkPath(filepath.Join(link, "nested", "value")); !errors.Is(err, ErrSymlink) {
		t.Fatalf("path through symlink error = %v", err)
	}
	if err := ValidateNoSymlinkAncestors(filepath.Join(link, "nested", "new")); !errors.Is(err, ErrSymlink) {
		t.Fatalf("ancestors through symlink error = %v", err)
	}

	// The offender is an ANCESTOR, not the path the operator typed, so the
	// refusal has to name which component is the symlink and where it points.
	// /var/run is a symlink to /run on every mainstream distribution: without
	// this, a refused "/var/run/credentials/unit/secret" is indistinguishable
	// from a genuinely unsafe path, and the fix — the same file under /run —
	// is invisible.
	symlinkErr := ValidateNoSymlinkPath(filepath.Join(link, "nested", "value"))
	if !strings.Contains(symlinkErr.Error(), link) {
		t.Errorf("refusal does not name the offending component: %v", symlinkErr)
	}
	if !strings.Contains(symlinkErr.Error(), realParent) {
		t.Errorf("refusal does not name where the symlink resolves to: %v", symlinkErr)
	}

	notDirectory := filepath.Join(base, "not-directory")
	if err := os.WriteFile(notDirectory, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateNoSymlinkAncestors(filepath.Join(notDirectory, "new")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("non-directory parent error = %v", err)
	}
}
