package accountsdb

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/lthash"
	"github.com/cockroachdb/pebble"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func newSnapshotStateTestDB(t *testing.T) (*AccountsDb, string) {
	t.Helper()
	dir := t.TempDir()
	accountsDir := filepath.Join(dir, "accounts")
	require.NoError(t, os.Mkdir(accountsDir, 0o755))
	index, err := pebble.Open(filepath.Join(dir, "index"), NewAccountsIndexPebbleOptions(nil))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	return &AccountsDb{Index: index, AcctsDir: accountsDir}, accountsDir
}

func writeSnapshotStateAccount(t *testing.T, dir string, slot, fileID uint64, account AppendVecAccount) uint64 {
	t.Helper()
	account.DataLen = uint64(len(account.Data))
	var data bytes.Buffer
	_, err := account.MarshalReturningLength(&data)
	require.NoError(t, err)
	path := filepath.Join(dir, snapshotStateFilename(slot, fileID))
	info, statErr := os.Stat(path)
	var offset uint64
	if statErr == nil {
		offset = uint64(info.Size())
	} else {
		require.ErrorIs(t, statErr, os.ErrNotExist)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = file.Write(data.Bytes())
	require.NoError(t, err)
	require.NoError(t, file.Close())
	return offset
}

func snapshotStateFilename(slot, fileID uint64) string {
	return strconv.FormatUint(slot, 10) + "." + strconv.FormatUint(fileID, 10)
}

func setSnapshotStateIndex(t *testing.T, db *AccountsDb, key []byte, value []byte) {
	t.Helper()
	require.NoError(t, db.Index.Set(key, value, pebble.Sync))
}

func snapshotStateIndexValue(entry AccountIndexEntry) []byte {
	var raw [24]byte
	entry.Marshal(&raw)
	return raw[:]
}

func TestCalculateSnapshotStateUsesCanonicalIndexEntries(t *testing.T) {
	db, dir := newSnapshotStateTestDB(t)
	keyA := solana.PublicKey{1}
	keyB := solana.PublicKey{2}
	keyC := solana.PublicKey{3}
	oldOffset := writeSnapshotStateAccount(t, dir, 40, 1, AppendVecAccount{Pubkey: keyA, Lamports: 4})
	newOffset := writeSnapshotStateAccount(t, dir, 41, 2, AppendVecAccount{Pubkey: keyA, Lamports: 7, Data: []byte("new")})
	zeroOffset := writeSnapshotStateAccount(t, dir, 41, 2, AppendVecAccount{
		Pubkey:     keyB,
		RentEpoch:  42,
		Owner:      solana.SystemProgramID,
		Executable: true,
		Data:       []byte("retained tombstone metadata"),
	})
	otherFileOffset := writeSnapshotStateAccount(t, dir, 39, 3, AppendVecAccount{Pubkey: keyC, Lamports: 5})
	_ = oldOffset
	setSnapshotStateIndex(t, db, keyA[:], snapshotStateIndexValue(AccountIndexEntry{Slot: 41, FileId: 2, Offset: newOffset}))
	setSnapshotStateIndex(t, db, keyB[:], snapshotStateIndexValue(AccountIndexEntry{Slot: 41, FileId: 2, Offset: zeroOffset}))
	setSnapshotStateIndex(t, db, keyC[:], snapshotStateIndexValue(AccountIndexEntry{Slot: 39, FileId: 3, Offset: otherFileOffset}))

	actual, capitalization, count, err := db.CalculateSnapshotState(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(12), capitalization)
	require.Equal(t, uint64(3), count)
	expected := new(lthash.LtHash)
	expected.MixIn(new(lthash.LtHash).InitWithAcct((&AppendVecAccount{Pubkey: keyA, Lamports: 7, DataLen: 3, Data: []byte("new")}).ToAccount()))
	expected.MixIn(new(lthash.LtHash).InitWithAcct((&AppendVecAccount{Pubkey: keyC, Lamports: 5}).ToAccount()))
	require.True(t, actual.Equals(expected))
}

func TestCalculateSnapshotStateRejectsInvalidState(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*testing.T, *AccountsDb, string)
	}{
		{name: "malformed key", setup: func(t *testing.T, db *AccountsDb, _ string) {
			setSnapshotStateIndex(t, db, []byte{1}, make([]byte, 24))
		}},
		{name: "malformed value", setup: func(t *testing.T, db *AccountsDb, _ string) {
			key := solana.PublicKey{1}
			setSnapshotStateIndex(t, db, key[:], []byte{1})
		}},
		{name: "missing appendvec", setup: func(t *testing.T, db *AccountsDb, _ string) {
			key := solana.PublicKey{1}
			setSnapshotStateIndex(t, db, key[:], snapshotStateIndexValue(AccountIndexEntry{Slot: 1, FileId: 1}))
		}},
		{name: "pubkey mismatch", setup: func(t *testing.T, db *AccountsDb, dir string) {
			key := solana.PublicKey{1}
			offset := writeSnapshotStateAccount(t, dir, 1, 1, AppendVecAccount{Pubkey: solana.PublicKey{2}, Lamports: 1})
			setSnapshotStateIndex(t, db, key[:], snapshotStateIndexValue(AccountIndexEntry{Slot: 1, FileId: 1, Offset: offset}))
		}},
		{name: "capitalization overflow", setup: func(t *testing.T, db *AccountsDb, dir string) {
			first, second := solana.PublicKey{1}, solana.PublicKey{2}
			firstOffset := writeSnapshotStateAccount(t, dir, 1, 1, AppendVecAccount{Pubkey: first, Lamports: math.MaxUint64})
			secondOffset := writeSnapshotStateAccount(t, dir, 1, 1, AppendVecAccount{Pubkey: second, Lamports: 1})
			setSnapshotStateIndex(t, db, first[:], snapshotStateIndexValue(AccountIndexEntry{Slot: 1, FileId: 1, Offset: firstOffset}))
			setSnapshotStateIndex(t, db, second[:], snapshotStateIndexValue(AccountIndexEntry{Slot: 1, FileId: 1, Offset: secondOffset}))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, dir := newSnapshotStateTestDB(t)
			test.setup(t, db, dir)
			_, _, _, err := db.CalculateSnapshotState(context.Background())
			require.Error(t, err)
		})
	}
}

func TestCalculateSnapshotStateHonorsCancellation(t *testing.T) {
	db, _ := newSnapshotStateTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, count, err := db.CalculateSnapshotState(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, count)
}
