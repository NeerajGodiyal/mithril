//go:build darwin

package accountsdb

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSyncAccountsFilesystemVisitsAppendvecFiles(t *testing.T) {
	dir := t.TempDir()
	appendvec := filepath.Join(dir, "42.1")
	if err := os.WriteFile(appendvec, []byte("accounts"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(appendvec, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(appendvec, 0o600) })

	if err := syncAccountsFilesystem(dir); err == nil {
		t.Fatal("unreadable appendvec was skipped instead of being synced")
	}
}
