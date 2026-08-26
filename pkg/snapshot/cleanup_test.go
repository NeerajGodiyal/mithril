package snapshot

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/rootedevents"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCleanAccountsDbDirRemovesLineageSidecars(t *testing.T) {
	root := t.TempDir()
	checkpointDir := filepath.Join(root, "transaction-status-checkpoints")
	require.NoError(t, os.MkdirAll(checkpointDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(checkpointDir, "stale.bin"), []byte("stale"), 0o644))
	eventDir := filepath.Join(root, rootedevents.SidecarDirectory)
	require.NoError(t, os.MkdirAll(eventDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(eventDir, "stale.jsonl"), []byte("stale"), 0o644))

	require.NoError(t, CleanAccountsDbDir(root))
	assert.NoDirExists(t, checkpointDir)
	assert.NoDirExists(t, eventDir)
}

func TestCleanAccountsDbDirRejectsUnsafeRoots(t *testing.T) {
	require.Error(t, CleanAccountsDbDir(""))

	parent := t.TempDir()
	realRoot := filepath.Join(parent, "real")
	linkedRoot := filepath.Join(parent, "linked")
	require.NoError(t, os.Mkdir(realRoot, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(realRoot, "mithril_state.json"), []byte("ready"), 0o600))
	require.NoError(t, os.Symlink(realRoot, linkedRoot))

	require.Error(t, CleanAccountsDbDir(linkedRoot))
	assert.FileExists(t, filepath.Join(realRoot, "mithril_state.json"))
}

func TestCleanAccountsDbDirInvalidatesStateBeforeArtifacts(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(root, "mithril_state.json")
	accountsPath := filepath.Join(root, "accounts")
	require.NoError(t, os.WriteFile(statePath, []byte("ready"), 0o600))
	require.NoError(t, os.Mkdir(accountsPath, 0o755))

	var events []string
	err := cleanAccountsDbRoot(
		func(name string) error {
			events = append(events, "remove:"+name)
			return os.RemoveAll(filepath.Join(root, name))
		},
		func(name string) (os.FileInfo, error) { return os.Lstat(filepath.Join(root, name)) },
		func() ([]os.DirEntry, error) { return os.ReadDir(root) },
		func() error {
			events = append(events, "sync")
			return nil
		},
	)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(events), 4)
	assert.Equal(t, "remove:mithril_state.json", events[0])
	assert.Equal(t, "sync", events[1])
	assert.Equal(t, "remove:accounts", events[2])
	assert.Equal(t, "sync", events[len(events)-1])
}

func TestCleanAccountsDbDirStopsAfterStateSyncFailure(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(root, "mithril_state.json")
	accountsPath := filepath.Join(root, "accounts")
	require.NoError(t, os.WriteFile(statePath, []byte("ready"), 0o600))
	require.NoError(t, os.Mkdir(accountsPath, 0o755))
	sentinel := errors.New("injected directory sync failure")

	err := cleanAccountsDbRoot(
		func(name string) error { return os.RemoveAll(filepath.Join(root, name)) },
		func(name string) (os.FileInfo, error) { return os.Lstat(filepath.Join(root, name)) },
		func() ([]os.DirEntry, error) { return os.ReadDir(root) },
		func() error { return sentinel },
	)
	require.ErrorIs(t, err, sentinel)
	assert.NoFileExists(t, statePath)
	assert.DirExists(t, accountsPath)
}

func TestCleanSnapshotDownloadDirRemovesLZ4Archives(t *testing.T) {
	dir := t.TempDir()
	archives := []string{
		"snapshot-41-test.tar.lz4",
		"incremental-snapshot-41-42-test.tar.lz4",
	}
	for _, name := range archives {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0o644))
	}
	unrelated := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(unrelated, nil, 0o644))

	CleanSnapshotDownloadDir(dir, 0)

	for _, name := range archives {
		assert.NoFileExists(t, filepath.Join(dir, name))
	}
	assert.FileExists(t, unrelated)
}
