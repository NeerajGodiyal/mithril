package accountsdb

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func redoAcct(b byte, lamports uint64, data []byte) *accounts.Account {
	return &accounts.Account{
		Slot:       100,
		Key:        solana.PublicKey{b},
		Lamports:   lamports,
		Data:       data,
		Owner:      [32]byte{b, b},
		Executable: b%2 == 0,
		RentEpoch:  uint64(b),
	}
}

func assertAcctEqual(t *testing.T, want, got *accounts.Account) {
	t.Helper()
	assert.Equal(t, want.Key, got.Key)
	assert.Equal(t, want.Lamports, got.Lamports)
	assert.Equal(t, want.Owner, got.Owner)
	assert.Equal(t, want.Executable, got.Executable)
	assert.Equal(t, want.RentEpoch, got.RentEpoch)
	assert.Equal(t, want.Slot, got.Slot)
	assert.True(t, bytes.Equal(want.Data, got.Data), "data mismatch for %s", want.Key)
}

// Round-trip across varied account shapes plus the bankhash.
func TestRedoRoundTrip(t *testing.T) {
	dir := t.TempDir()
	accts := []*accounts.Account{
		redoAcct(1, 1000, []byte("hello")),
		redoAcct(2, 0, nil),                                  // zero lamports, no data
		redoAcct(3, 1<<40, bytes.Repeat([]byte{0xAB}, 4096)), // large data
	}
	require.NoError(t, WriteRedo(dir, 100, []byte("bankhash-100"), accts))

	got, bankhash, err := ReadRedo(dir, 100)
	require.NoError(t, err)
	require.Len(t, got, len(accts))
	assert.Equal(t, []byte("bankhash-100"), bankhash)
	for i := range accts {
		assertAcctEqual(t, accts[i], got[i])
	}
}

// Nil account entries are skipped, not panicked on (callers may pass sparse slices).
func TestRedoSkipsNilEntries(t *testing.T) {
	dir := t.TempDir()
	accts := []*accounts.Account{redoAcct(1, 5, []byte("x")), nil, redoAcct(2, 9, nil)}
	require.NotPanics(t, func() {
		require.NoError(t, WriteRedo(dir, 50, []byte("bh"), accts))
	})
	got, _, err := ReadRedo(dir, 50)
	require.NoError(t, err)
	assert.Len(t, got, 2, "nil filtered out")
}

// An empty modified-account set still round-trips (a real slot can touch only sysvars).
func TestRedoEmpty(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, WriteRedo(dir, 7, []byte("bh7"), nil))
	got, bankhash, err := ReadRedo(dir, 7)
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Equal(t, []byte("bh7"), bankhash)
}

// A corrupted checksum (torn write) is detected, not silently accepted.
func TestRedoTornChecksumDetected(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, WriteRedo(dir, 100, []byte("bh"), []*accounts.Account{redoAcct(1, 5, []byte("x"))}))

	p := redoPath(dir, 100)
	data, err := os.ReadFile(p)
	require.NoError(t, err)
	data[len(data)-1] ^= 0xFF // flip a crc byte
	require.NoError(t, os.WriteFile(p, data, 0o644))

	_, _, err = ReadRedo(dir, 100)
	assert.ErrorIs(t, err, ErrTornRedo)
}

// A truncated file (crash mid-write) is detected as torn.
func TestRedoTruncatedDetected(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, WriteRedo(dir, 100, []byte("bh"), []*accounts.Account{redoAcct(1, 5, bytes.Repeat([]byte{1}, 200))}))

	p := redoPath(dir, 100)
	data, err := os.ReadFile(p)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p, data[:len(data)/2], 0o644)) // chop the tail

	_, _, err = ReadRedo(dir, 100)
	assert.ErrorIs(t, err, ErrTornRedo)
}

// Pending redo slots are listed ascending; finalized (deleted) ones disappear.
func TestRedoListAndDelete(t *testing.T) {
	dir := t.TempDir()
	for _, s := range []uint64{5, 3, 9} {
		require.NoError(t, WriteRedo(dir, s, []byte("bh"), []*accounts.Account{redoAcct(1, s, nil)}))
	}
	pending, err := ListPendingRedo(dir)
	require.NoError(t, err)
	assert.Equal(t, []uint64{3, 5, 9}, pending)

	require.NoError(t, DeleteRedo(dir, 5))
	pending, err = ListPendingRedo(dir)
	require.NoError(t, err)
	assert.Equal(t, []uint64{3, 9}, pending)

	_, _, err = ReadRedo(dir, 5)
	assert.Error(t, err, "deleted redo cannot be read")
}

// ListPendingRedo on a missing redo dir is empty, not an error.
func TestRedoListNoDir(t *testing.T) {
	pending, err := ListPendingRedo(t.TempDir())
	require.NoError(t, err)
	assert.Empty(t, pending)
}

// WriteRedo leaves no .tmp file behind (rename completed).
func TestRedoNoTmpLeftover(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, WriteRedo(dir, 100, []byte("bh"), []*accounts.Account{redoAcct(1, 5, nil)}))
	entries, err := os.ReadDir(redoDir(dir))
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, filepath.Ext(e.Name()) == ".tmp", "stray tmp file: %s", e.Name())
	}
}
