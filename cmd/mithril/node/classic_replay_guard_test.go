package node

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/snapshot"
	"github.com/stretchr/testify/require"
)

func TestClassicReplayGuardRequiresCleanCompletion(t *testing.T) {
	dir := t.TempDir()
	guard, err := beginClassicReplay(dir, "run-one", 42)
	require.NoError(t, err)

	interrupted, err := classicReplayWasInterrupted(dir)
	require.NoError(t, err)
	require.True(t, interrupted)
	_, err = beginClassicReplay(dir, "run-two", 43)
	require.ErrorContains(t, err, "did not shut down cleanly")

	info, err := os.Stat(filepath.Join(dir, accountsdb.ClassicReplayMarkerName))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	require.NoError(t, guard.Complete())
	interrupted, err = classicReplayWasInterrupted(dir)
	require.NoError(t, err)
	require.False(t, interrupted)
}

func TestClassicReplayMarkerPolicy(t *testing.T) {
	tests := []struct {
		name          string
		alpenglow     bool
		rootedDurable bool
		want          bool
	}{
		{name: "classic per-slot", want: true},
		{name: "classic rooted-durable", rootedDurable: true},
		{name: "alpenglow", alpenglow: true, rootedDurable: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, classicReplayMarkerRequired(test.alpenglow, test.rootedDurable))
		})
	}
}

func TestClassicReplayGuardRejectsExistingSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(target, []byte("untouched"), 0o600))
	require.NoError(t, os.Symlink(target, filepath.Join(dir, accountsdb.ClassicReplayMarkerName)))

	_, err := beginClassicReplay(dir, "run", 1)
	require.ErrorContains(t, err, "did not shut down cleanly")
	contents, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "untouched", string(contents))
}

func TestSnapshotRebuildClearsInterruptedClassicReplay(t *testing.T) {
	dir := t.TempDir()
	_, err := beginClassicReplay(dir, "interrupted-run", 42)
	require.NoError(t, err)

	require.NoError(t, snapshot.CleanAccountsDbDir(dir))
	guard, err := beginClassicReplay(dir, "replacement-run", 43)
	require.NoError(t, err)
	require.NoError(t, guard.Complete())
}

func TestClassicReplayGuardDoesNotAcceptMissingMarker(t *testing.T) {
	dir := t.TempDir()
	guard, err := beginClassicReplay(dir, "run", 42)
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(dir, accountsdb.ClassicReplayMarkerName)))
	require.ErrorContains(t, guard.Complete(), "remove classic replay marker")
}
