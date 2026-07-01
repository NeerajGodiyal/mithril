package accountsdb

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestDb scaffolds a minimal on-disk AccountsDb in a temp dir (Pebble DBs are
// auto-created by OpenDb).
func newTestDb(t *testing.T) *AccountsDb {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "accounts"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "largest_file_id"), make([]byte, 8), 0o644))
	db, err := OpenDb(dir)
	require.NoError(t, err)
	db.InitCaches() // node does this after OpenDb; needed for read-cache maintenance
	return db
}

// Happy path: a committed slot's accounts + bankhash read back.
func TestCommitSlotAtomicReadsBack(t *testing.T) {
	db := newTestDb(t)
	a := redoAcct(1, 500, []byte("hi"))
	require.NoError(t, db.CommitSlotAtomic([]*accounts.Account{a}, 100, []byte("bankhash-100")))

	got, err := db.GetAccount(100, solana.PublicKey{1})
	require.NoError(t, err)
	assertAcctEqual(t, a, got)

	bh, err := db.GetBankHashForSlot(100)
	require.NoError(t, err)
	assert.Equal(t, []byte("bankhash-100"), bh)
}

// Multi-account slot, read back FROM DISK (caches evicted) — actually exercises
// the appendvec encoding/offset/padding, not the cache.
func TestCommitSlotAtomicMultiAccountFromDisk(t *testing.T) {
	db := newTestDb(t)
	accts := []*accounts.Account{
		redoAcct(1, 100, []byte("12345")),     // dataLen 5 -> pad 3
		redoAcct(3, 200, nil),                 // dataLen 0 -> pad 0
		redoAcct(5, 300, []byte("eightbyt")),  // dataLen 8 -> pad 0
		redoAcct(7, 400, []byte("ninebytes")), // dataLen 9 -> pad 7
	}
	require.NoError(t, db.CommitSlotAtomic(accts, 100, []byte("bh")))

	for _, a := range accts {
		db.CommonAcctsCache.Delete(a.Key) // force a cold disk read
		got, err := db.GetAccount(100, a.Key)
		require.NoError(t, err)
		assertAcctEqual(t, a, got)
	}
}

// A nil entry in the slice must not panic (WriteRedo + apply both skip it).
func TestCommitSlotAtomicNilEntry(t *testing.T) {
	db := newTestDb(t)
	a := redoAcct(2, 50, []byte("ok"))
	require.NotPanics(t, func() {
		require.NoError(t, db.CommitSlotAtomic([]*accounts.Account{a, nil}, 100, []byte("bh")))
	})
	got, err := db.GetAccount(100, solana.PublicKey{2})
	require.NoError(t, err)
	assertAcctEqual(t, a, got)
}

// THE load-bearing test: a commit interrupted after staging the redo but before
// the store apply / checkpoint advance is rolled forward by recovery.
func TestApplyPendingCommitsRecoversAfterCrash(t *testing.T) {
	db := newTestDb(t)
	a := redoAcct(2, 700, []byte("recovered"))
	a.Slot = 101

	// Simulate: durable prepare landed, then crash before applying to the store.
	require.NoError(t, WriteRedo(db.AcctsDir, 101, []byte("bankhash-101"), []*accounts.Account{a}))
	_, err := db.GetAccount(101, solana.PublicKey{2})
	require.Error(t, err, "not in the store yet")

	applied, err := db.ApplyPendingCommits(100) // checkpoint is at slot 100
	require.NoError(t, err)
	assert.Equal(t, []uint64{101}, applied)

	got, err := db.GetAccount(101, solana.PublicKey{2})
	require.NoError(t, err)
	assertAcctEqual(t, a, got)
	bh, err := db.GetBankHashForSlot(101)
	require.NoError(t, err)
	assert.Equal(t, []byte("bankhash-101"), bh, "bankhash restored from redo")
}

// A redo for a slot at/below the checkpoint is STALE (already durable) and must
// be discarded, never re-applied — re-applying would regress the account.
func TestApplyPendingCommitsDiscardsStaleRedo(t *testing.T) {
	db := newTestDb(t)
	a := redoAcct(8, 1, []byte("old"))
	a.Slot = 50
	require.NoError(t, WriteRedo(db.AcctsDir, 50, []byte("bh50"), []*accounts.Account{a}))

	applied, err := db.ApplyPendingCommits(100) // checkpoint already past slot 50
	require.NoError(t, err)
	assert.Empty(t, applied, "stale redo not re-applied")

	pending, err := ListPendingRedo(db.AcctsDir)
	require.NoError(t, err)
	assert.Empty(t, pending, "stale redo deleted")

	_, err = db.GetAccount(50, solana.PublicKey{8})
	assert.Error(t, err, "no regression: stale value never written")
}

// Recovery is idempotent: re-running it (a crash during recovery) is safe.
func TestApplyPendingCommitsIdempotent(t *testing.T) {
	db := newTestDb(t)
	a := redoAcct(3, 900, []byte("x"))
	a.Slot = 102
	require.NoError(t, WriteRedo(db.AcctsDir, 102, []byte("bh102"), []*accounts.Account{a}))

	for range 2 { // recovery does not delete s>checkpoint redos, so a second pass re-applies
		applied, err := db.ApplyPendingCommits(100)
		require.NoError(t, err)
		assert.Equal(t, []uint64{102}, applied)
	}
	got, err := db.GetAccount(102, solana.PublicKey{3})
	require.NoError(t, err)
	assertAcctEqual(t, a, got)
}

// A torn redo above the checkpoint is quarantined (not applied, not left to loop).
func TestApplyPendingCommitsQuarantinesTornRedo(t *testing.T) {
	db := newTestDb(t)
	a := redoAcct(4, 100, []byte("y"))
	a.Slot = 103
	require.NoError(t, WriteRedo(db.AcctsDir, 103, []byte("bh103"), []*accounts.Account{a}))

	p := redoPath(db.AcctsDir, 103)
	data, err := os.ReadFile(p)
	require.NoError(t, err)
	data[len(data)-1] ^= 0xFF // corrupt the crc
	require.NoError(t, os.WriteFile(p, data, 0o644))

	applied, err := db.ApplyPendingCommits(100)
	require.NoError(t, err)
	assert.Empty(t, applied, "torn redo not applied")

	pending, err := ListPendingRedo(db.AcctsDir)
	require.NoError(t, err)
	assert.Empty(t, pending, "torn redo quarantined (no longer pending)")

	_, err = db.GetAccount(103, solana.PublicKey{4})
	assert.Error(t, err, "nothing committed for a torn prepare")
}
