package global

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func resetStakeIndexTestState() {
	instance.pendingStakeMutex.Lock()
	defer instance.pendingStakeMutex.Unlock()
	instance.pendingStakeBySlot = nil
	instance.cachedStakeEntries = nil
	instance.entriesFlushedSinceCompact = 0
}

func TestFlushPendingStakePubkeysThroughRetainsEntriesOnOpenFailure(t *testing.T) {
	resetStakeIndexTestState()
	defer resetStakeIndexTestState()

	pubkey := solana.PublicKey{1}
	EnqueuePendingStakePubkey(7, pubkey)
	_, err := FlushPendingStakePubkeysThrough(filepath.Join(t.TempDir(), "missing"), 7)
	require.Error(t, err)
	require.Equal(t, []accountsdb.StakeIndexEntry{{Pubkey: pubkey}}, PendingStakeEntriesSnapshot())

	dir := t.TempDir()
	flushed, err := FlushPendingStakePubkeysThrough(dir, 7)
	require.NoError(t, err)
	require.Equal(t, 1, flushed)
	require.Empty(t, PendingStakeEntriesSnapshot())
	entries, err := LoadStakePubkeyIndex(dir)
	require.NoError(t, err)
	require.Equal(t, []accountsdb.StakeIndexEntry{{Pubkey: pubkey}}, entries)
}

func TestLoadStakePubkeyIndexRecoversIncompleteFinalRecord(t *testing.T) {
	resetStakeIndexTestState()
	defer resetStakeIndexTestState()

	dir := t.TempDir()
	path := filepath.Join(dir, StakePubkeyIndexFileName)
	pubkey := solana.PublicKey{2}
	data := make([]byte, 8+accountsdb.StakeIndexRecordSize+7)
	copy(data[:4], accountsdb.StakeIndexMagic[:])
	binary.LittleEndian.PutUint32(data[4:8], accountsdb.StakeIndexVersion)
	copy(data[8:40], pubkey[:])
	binary.LittleEndian.PutUint64(data[40:48], 11)
	binary.LittleEndian.PutUint64(data[48:56], 22)
	require.NoError(t, os.WriteFile(path, data, 0o644))

	entries, err := LoadStakePubkeyIndex(dir)
	require.NoError(t, err)
	require.Equal(t, []accountsdb.StakeIndexEntry{{Pubkey: pubkey, FileId: 11, Offset: 22}}, entries)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, int64(8+accountsdb.StakeIndexRecordSize), info.Size())
}

func TestFlushPendingStakePubkeysThroughRepairsPartialTailBeforeAppend(t *testing.T) {
	resetStakeIndexTestState()
	defer resetStakeIndexTestState()

	dir := t.TempDir()
	path := filepath.Join(dir, StakePubkeyIndexFileName)
	pubkey := solana.PublicKey{2}
	pendingPubkey := solana.PublicKey{3}
	data := make([]byte, 8+accountsdb.StakeIndexRecordSize+7)
	copy(data[:4], accountsdb.StakeIndexMagic[:])
	binary.LittleEndian.PutUint32(data[4:8], accountsdb.StakeIndexVersion)
	copy(data[8:40], pubkey[:])
	binary.LittleEndian.PutUint64(data[40:48], 11)
	binary.LittleEndian.PutUint64(data[48:56], 22)
	require.NoError(t, os.WriteFile(path, data, 0o644))
	EnqueuePendingStakePubkey(8, pendingPubkey)

	flushed, err := FlushPendingStakePubkeysThrough(dir, 8)
	require.NoError(t, err)
	require.Equal(t, 1, flushed)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, int64(8+2*accountsdb.StakeIndexRecordSize), info.Size())
	entries, err := LoadStakePubkeyIndex(dir)
	require.NoError(t, err)
	require.Equal(t, []accountsdb.StakeIndexEntry{
		{Pubkey: pendingPubkey},
		{Pubkey: pubkey, FileId: 11, Offset: 22},
	}, entries)
}

func TestFlushPendingStakePubkeysThroughRepairsTornHeader(t *testing.T) {
	for size := 1; size < 8; size++ {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			resetStakeIndexTestState()
			defer resetStakeIndexTestState()

			dir := t.TempDir()
			path := filepath.Join(dir, StakePubkeyIndexFileName)
			torn := make([]byte, size)
			copy(torn, accountsdb.StakeIndexMagic[:])
			require.NoError(t, os.WriteFile(path, torn, 0o644))
			pubkey := solana.PublicKey{byte(size)}
			EnqueuePendingStakePubkey(8, pubkey)

			flushed, err := FlushPendingStakePubkeysThrough(dir, 8)
			require.NoError(t, err)
			require.Equal(t, 1, flushed)
			entries, err := LoadStakePubkeyIndex(dir)
			require.NoError(t, err)
			require.Equal(t, []accountsdb.StakeIndexEntry{{Pubkey: pubkey}}, entries)
		})
	}
}
