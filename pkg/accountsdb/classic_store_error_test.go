package accountsdb

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/stretchr/testify/require"
)

func TestClassicStoreFailureIsReportedAndSuppressesCallback(t *testing.T) {
	db, dir := newFoldTestDb(t)
	db.RootedDurable = false
	db.AcctsDir = filepath.Join(dir, "missing", "accounts")
	previousWorkers := StoreAccountsWorkers
	StoreAccountsWorkers = 1
	t.Cleanup(func() { StoreAccountsWorkers = previousWorkers })

	var called atomic.Bool
	require.NoError(t, db.StoreAccounts([]*accounts.Account{foldAcct(1, 1, nil)}, 42, func() {
		called.Store(true)
	}))
	require.ErrorContains(t, db.WaitForStoreWorkerWithError(), "open appendvec")
	require.False(t, called.Load())
	require.Error(t, db.CloseDbWithError())
}

func TestClassicSynchronousStoreReturnsDiskFailure(t *testing.T) {
	db, dir := newFoldTestDb(t)
	db.RootedDurable = false
	db.AcctsDir = filepath.Join(dir, "missing", "accounts")
	previousWorkers := StoreAccountsWorkers
	StoreAccountsWorkers = 1
	t.Cleanup(func() { StoreAccountsWorkers = previousWorkers })

	require.ErrorContains(t, db.StoreAccountsAndWait([]*accounts.Account{foldAcct(1, 1, nil)}, 42), "open appendvec")
	db.CloseDb()
}

func TestClassicSynchronousStorePreservesQueuedWriteOrder(t *testing.T) {
	db, _ := newFoldTestDb(t)
	db.RootedDurable = false
	previousWorkers := StoreAccountsWorkers
	StoreAccountsWorkers = 1
	t.Cleanup(func() { StoreAccountsWorkers = previousWorkers })

	oldStarted := make(chan struct{})
	releaseOld := make(chan struct{})
	db.storeBeforePersist = func(slot uint64) {
		if slot == 41 {
			close(oldStarted)
			<-releaseOld
		}
	}
	require.NoError(t, db.StoreAccounts([]*accounts.Account{foldAcct(1, 1, nil)}, 41, nil))
	<-oldStarted

	newStarted := make(chan struct{})
	newDone := make(chan error, 1)
	go func() {
		close(newStarted)
		newDone <- db.StoreAccountsAndWait([]*accounts.Account{foldAcct(1, 2, nil)}, 42)
	}()
	<-newStarted
	select {
	case err := <-newDone:
		close(releaseOld)
		_ = db.CloseDbWithError()
		t.Fatalf("synchronous store bypassed blocked older request: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseOld)
	require.NoError(t, <-newDone)
	stored := mustColdRead(t, db, 42, foldAcct(1, 0, nil).Key)
	require.Equal(t, uint64(2), stored.Lamports)
	require.NoError(t, db.CloseDbWithError())
}

func TestClassicSynchronousStoreReportsEarlierWorkerFailure(t *testing.T) {
	db, dir := newFoldTestDb(t)
	db.RootedDurable = false
	db.AcctsDir = filepath.Join(dir, "missing", "accounts")
	previousWorkers := StoreAccountsWorkers
	StoreAccountsWorkers = 1
	t.Cleanup(func() { StoreAccountsWorkers = previousWorkers })

	failed := make(chan error, 1)
	require.NoError(t, db.enqueueStoreRequest([]*accounts.Account{foldAcct(1, 1, nil)}, 41, nil, failed))
	require.ErrorContains(t, <-failed, "open appendvec")
	require.NoError(t, os.MkdirAll(db.AcctsDir, 0o755))
	require.ErrorContains(t, db.StoreAccountsAndWait([]*accounts.Account{foldAcct(1, 2, nil)}, 42), "open appendvec")
	require.Error(t, db.CloseDbWithError())
}
