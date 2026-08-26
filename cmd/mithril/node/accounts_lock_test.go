package node

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccountsDBLockRefusesOnlySameLineage(t *testing.T) {
	dir := t.TempDir()
	first, err := acquireAccountsDBLock(dir)
	require.NoError(t, err)
	t.Cleanup(first.release)

	_, err = acquireAccountsDBLock(dir)
	require.ErrorContains(t, err, "already using this AccountsDB")

	other, err := acquireAccountsDBLock(t.TempDir())
	require.NoError(t, err)
	other.release()

	first.release()
	again, err := acquireAccountsDBLock(dir)
	require.NoError(t, err)
	again.release()
}

func TestAccountsDBLockRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "outside")
	require.NoError(t, os.WriteFile(target, nil, 0o600))
	require.NoError(t, os.Symlink(target, filepath.Join(root, accountsDBLockFileName)))

	_, err := acquireAccountsDBLock(root)
	require.Error(t, err)
}
