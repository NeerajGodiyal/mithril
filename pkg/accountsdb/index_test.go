package accountsdb

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestWriteStakePubkeyIndexAtomicallyReplacesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stake_pubkeys.bin")
	require.NoError(t, os.WriteFile(path, []byte("old index must survive until rename"), 0o644))

	entry := StakeIndexEntry{Pubkey: solana.PublicKey{1}, FileId: 2, Offset: 3}
	require.NoError(t, WriteStakePubkeyIndex(path, []StakeIndexEntry{entry}))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Len(t, data, 8+StakeIndexRecordSize)
	require.Equal(t, StakeIndexMagic[:], data[:4])
	require.Equal(t, StakeIndexVersion, binary.LittleEndian.Uint32(data[4:8]))
	require.Equal(t, entry.Pubkey[:], data[8:40])
	require.Equal(t, entry.FileId, binary.LittleEndian.Uint64(data[40:48]))
	require.Equal(t, entry.Offset, binary.LittleEndian.Uint64(data[48:56]))

	temporaries, err := filepath.Glob(filepath.Join(dir, ".stake_pubkeys.bin.tmp-*"))
	require.NoError(t, err)
	require.Empty(t, temporaries)
}
