package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsWritable_ExistingWritableDir(t *testing.T) {
	dir := t.TempDir()
	assert.True(t, isWritable(dir), "tempdir should be writable")
}

func TestIsWritable_NonExistentUnderWritableParent(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "does-not-exist-yet")
	// Path doesn't exist but parent is writable — Mithril may create it at
	// runtime, so we treat it as writable.
	assert.True(t, isWritable(child))
}

func TestIsWritable_File(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "regular-file")
	require.NoError(t, os.WriteFile(f, []byte("x"), 0600))
	// A file (not a dir) is never a valid storage path.
	assert.False(t, isWritable(f))
}

func TestIsWritable_ReadOnlyDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission semantics differ on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses permission checks")
	}
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0500)) // r-x, no write
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	assert.False(t, isWritable(dir))
}

func TestIsWritable_NonExistentNoWritableParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("path semantics differ on Windows")
	}
	// /proc/1/nonexistent: parent /proc/1 exists but is not writable for non-root.
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses permission checks")
	}
	assert.False(t, isWritable("/proc/1/mithril-test-nonexistent"))
}

func TestDefaultStoragePaths_AllFieldsPopulated(t *testing.T) {
	p := DefaultStoragePaths()
	assert.NotEmpty(t, p.Accounts)
	assert.NotEmpty(t, p.Snapshots)
	assert.NotEmpty(t, p.Logs)
	assert.NotEmpty(t, p.Shredstore)
}

func TestDefaultStoragePaths_AllOrNothingRoot(t *testing.T) {
	p := DefaultStoragePaths()
	// Either all paths are under /mnt (production) or all are under HOME
	// (fallback). We never mix to avoid configs that span multiple roots.
	allMnt := strings.HasPrefix(p.Accounts, "/mnt/") &&
		strings.HasPrefix(p.Snapshots, "/mnt/") &&
		strings.HasPrefix(p.Logs, "/mnt/") &&
		strings.HasPrefix(p.Shredstore, "/mnt/")
	allHome := strings.Contains(p.Accounts, ".mithril") &&
		strings.Contains(p.Snapshots, ".mithril") &&
		strings.Contains(p.Logs, ".mithril") &&
		strings.Contains(p.Shredstore, ".mithril")
	assert.True(t, allMnt || allHome, "paths should be all /mnt or all under .mithril, got %+v", p)
}

func TestIsProductionLayout(t *testing.T) {
	// Exact match -> true
	assert.True(t, IsProductionLayout(productionStoragePaths()))

	// All home paths -> false
	assert.False(t, IsProductionLayout(StoragePaths{
		Accounts:   "/home/user/.mithril/accounts",
		Snapshots:  "/home/user/.mithril/snapshots",
		Logs:       "/home/user/.mithril/logs",
		Shredstore: "/home/user/.mithril/shredstore",
	}))

	// Mixed: production Accounts but home dir for Logs -> false (no mislabel)
	mixed := productionStoragePaths()
	mixed.Logs = "/home/user/.mithril/logs"
	assert.False(t, IsProductionLayout(mixed),
		"mixed root config should not be labeled production")
}

func TestProductionStoragePaths_Stable(t *testing.T) {
	// These are documented in README.md and config.example.toml — changes
	// here would silently break operators who follow the docs.
	p := productionStoragePaths()
	assert.Equal(t, "/mnt/mithril-accounts", p.Accounts)
	assert.Equal(t, "/mnt/mithril-ledger/snapshots", p.Snapshots)
	assert.Equal(t, "/mnt/mithril-logs", p.Logs)
	assert.Equal(t, "/mnt/mithril-ledger/shredstore", p.Shredstore)
}
