package snapshot

import (
	"archive/tar"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/lthash"
	"github.com/cockroachdb/pebble"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestSnapshotManifestArchiveSlotValidation(t *testing.T) {
	for _, test := range []struct {
		name    string
		header  tar.Header
		slot    uint64
		found   bool
		wantErr bool
	}{
		{name: "canonical", header: tar.Header{Name: "snapshots/42/42", Typeflag: tar.TypeReg}, slot: 42, found: true},
		{name: "leading dot", header: tar.Header{Name: "./snapshots/42/42", Typeflag: tar.TypeRegA}, slot: 42, found: true},
		{name: "unrelated", header: tar.Header{Name: "accounts/42.1", Typeflag: tar.TypeReg}},
		{name: "different roots", header: tar.Header{Name: "snapshots/41/42", Typeflag: tar.TypeReg}, wantErr: true},
		{name: "invalid root", header: tar.Header{Name: "snapshots/not-a-slot/not-a-slot", Typeflag: tar.TypeReg}, wantErr: true},
		{name: "not regular", header: tar.Header{Name: "snapshots/42/42", Typeflag: tar.TypeSymlink}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			slot, found, err := snapshotManifestArchiveSlot(&test.header)
			require.Equal(t, test.wantErr, err != nil)
			require.Equal(t, test.slot, slot)
			require.Equal(t, test.found, found)
		})
	}
}

func TestSnapshotManifestReadIsBounded(t *testing.T) {
	_, err := readSnapshotManifestBytes(http.NoBody, maxSnapshotManifestBytes+1)
	require.Error(t, err)
	_, err = readSnapshotManifestBytes(http.NoBody, -1)
	require.Error(t, err)
}

func TestIncrementalManifestMustNameSelectedFullRoot(t *testing.T) {
	fullHash := [32]byte{1}
	full := &SnapshotManifest{
		Bank:       &DeserializableVersionedBank{Slot: 41, Capitalization: 100},
		AccountsDb: &AccountsDbFields{BankHashInfo: BankHashInfo{SnapshotHash: fullHash}},
	}
	incremental := &SnapshotManifest{
		Bank:       &DeserializableVersionedBank{Slot: 42},
		AccountsDb: &AccountsDbFields{},
		BankIncrementalSnapshotPersistence: &BankIncrementalSnapshotPersistence{
			FullSlot:           41,
			FullHash:           fullHash,
			FullCapitalization: 100,
		},
	}
	require.NoError(t, validateIncrementalManifestBase(full, incremental))

	for _, mutate := range []func(*BankIncrementalSnapshotPersistence){
		func(value *BankIncrementalSnapshotPersistence) { value.FullSlot-- },
		func(value *BankIncrementalSnapshotPersistence) { value.FullHash[0]++ },
		func(value *BankIncrementalSnapshotPersistence) { value.FullCapitalization++ },
	} {
		copy := *incremental
		persistence := *incremental.BankIncrementalSnapshotPersistence
		copy.BankIncrementalSnapshotPersistence = &persistence
		mutate(&persistence)
		require.Error(t, validateIncrementalManifestBase(full, &copy))
	}

	full.LtHash = new(lthash.LtHash)
	incremental.LtHash = new(lthash.LtHash)
	incremental.BankIncrementalSnapshotPersistence = nil
	require.NoError(t, validateIncrementalManifestBase(full, incremental))
	incremental.LtHash = nil
	require.Error(t, validateIncrementalManifestBase(full, incremental))
}

func TestSnapshotFilenameIdentityValidation(t *testing.T) {
	manifest := &SnapshotManifest{LtHash: new(lthash.LtHash).InitWithBytes([]byte("snapshot"))}
	hash := solana.HashFromBytes(manifest.LtHash.Checksum()).String()
	require.NoError(t, validateSnapshotArchiveHash(
		"https://snapshots.example/snapshot-42-"+hash+".tar.zst?token=secret", manifest,
	))
	require.Error(t, validateSnapshotArchiveHash("snapshot-42-11111111111111111111111111111111.tar.zst", manifest))
	require.Error(t, validateSnapshotArchiveHash("snapshot-42-not-base58.tar.zst", manifest))
	require.Error(t, validateSnapshotArchiveHash("snapshot-42-"+hash+".zip", manifest))
}

func emptyVerifiedSnapshotSelection(t *testing.T, dir string) validatedSnapshotSelection {
	t.Helper()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "accounts"), 0o755))
	index, err := pebble.Open(filepath.Join(dir, "mithril_db"), accountsdb.NewAccountsIndexPebbleOptions(nil))
	require.NoError(t, err)
	require.NoError(t, index.Close())
	manifest := &SnapshotManifest{Bank: &DeserializableVersionedBank{Slot: 42}, LtHash: new(lthash.LtHash)}
	return validatedSnapshotSelection{
		manifest:       manifest,
		bytes:          []byte("selected-manifest"),
		archiveHash:    [32]byte(solana.HashFromBytes(manifest.LtHash.Checksum())),
		hasArchiveHash: true,
	}
}

func TestFinalizeSnapshotBootstrapPersistsVerifiedManifest(t *testing.T) {
	dir := t.TempDir()
	selection := emptyVerifiedSnapshotSelection(t, dir)
	selection.manifest.Bank.Hash[0] = 0x7a
	db, err := finalizeSnapshotBootstrap(context.Background(), dir, 9, selection, nil)
	require.NoError(t, err)
	db.CloseDb()

	bankHash, err := os.ReadFile(filepath.Join(dir, "bank_hash"))
	require.NoError(t, err)
	require.Equal(t, selection.manifest.Bank.Hash[:], bankHash)
	require.NoFileExists(t, filepath.Join(dir, snapshotPendingManifestName))
	require.FileExists(t, filepath.Join(dir, "manifest"))
}

func TestResumeSnapshotBootstrapAfterCanceledVerification(t *testing.T) {
	dir := t.TempDir()
	selection := emptyVerifiedSnapshotSelection(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db, err := finalizeSnapshotBootstrap(ctx, dir, 9, selection, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, db)
	require.FileExists(t, filepath.Join(dir, snapshotPendingManifestName))
	require.NoFileExists(t, filepath.Join(dir, "manifest"))

	db, resumed, err := resumeSnapshotBootstrap(context.Background(), dir, selection)
	require.NoError(t, err)
	require.True(t, resumed)
	db.CloseDb()
	require.NoFileExists(t, filepath.Join(dir, snapshotPendingManifestName))
	require.FileExists(t, filepath.Join(dir, "manifest"))

	changed := selection
	changed.bytes = []byte("different-manifest")
	db, resumed, err = resumeSnapshotBootstrap(context.Background(), dir, changed)
	require.NoError(t, err)
	require.False(t, resumed)
	require.Nil(t, db)
}

func TestResumeSnapshotBootstrapRejectsSymlinkMarker(t *testing.T) {
	dir := t.TempDir()
	manifest := &SnapshotManifest{Bank: &DeserializableVersionedBank{Slot: 42}, LtHash: new(lthash.LtHash)}
	selection := validatedSnapshotSelection{
		manifest:       manifest,
		bytes:          []byte("selected-manifest"),
		archiveHash:    [32]byte(solana.HashFromBytes(manifest.LtHash.Checksum())),
		hasArchiveHash: true,
	}
	outside := filepath.Join(t.TempDir(), "manifest")
	require.NoError(t, os.WriteFile(outside, selection.bytes, 0o644))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, snapshotPendingManifestName)))
	db, resumed, err := resumeSnapshotBootstrap(context.Background(), dir, selection)
	require.ErrorContains(t, err, "not a regular file")
	require.True(t, resumed)
	require.Nil(t, db)
}

func TestResumeSnapshotBootstrapRejectsSymlinkRoot(t *testing.T) {
	realRoot := t.TempDir()
	linkedRoot := filepath.Join(t.TempDir(), "accountsdb")
	require.NoError(t, os.Symlink(realRoot, linkedRoot))
	selection := validatedSnapshotSelection{
		manifest:       &SnapshotManifest{Bank: new(DeserializableVersionedBank), LtHash: new(lthash.LtHash)},
		bytes:          []byte("selected-manifest"),
		hasArchiveHash: true,
	}
	db, resumed, err := resumeSnapshotBootstrap(context.Background(), linkedRoot, selection)
	require.ErrorContains(t, err, "not a real directory")
	require.False(t, resumed)
	require.Nil(t, db)
}

func TestFinalizeSnapshotBootstrapRejectsUnverifiedState(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*validatedSnapshotSelection)
	}{
		{name: "missing AccountsLtHash", mutate: func(value *validatedSnapshotSelection) { value.manifest.LtHash = nil }},
		{name: "AccountsLtHash mismatch", mutate: func(value *validatedSnapshotSelection) {
			value.manifest.LtHash = new(lthash.LtHash).InitWithBytes([]byte("wrong"))
		}},
		{name: "capitalization mismatch", mutate: func(value *validatedSnapshotSelection) { value.manifest.Bank.Capitalization = 1 }},
		{name: "archive hash mismatch", mutate: func(value *validatedSnapshotSelection) { value.archiveHash[0]++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			selection := emptyVerifiedSnapshotSelection(t, dir)
			test.mutate(&selection)
			db, err := finalizeSnapshotBootstrap(context.Background(), dir, 9, selection, nil)
			require.Error(t, err)
			require.Nil(t, db)
			require.NoFileExists(t, filepath.Join(dir, "manifest"))
		})
	}
}
