package snapshot

import (
	"testing"

	bin "github.com/gagliardetto/binary"
	"github.com/stretchr/testify/require"
)

func TestSnapshotManifestV4BlockIDTail(t *testing.T) {
	blockID := [32]byte{1, 2, 3}
	tests := []struct {
		name    string
		data    []byte
		want    *[32]byte
		wantErr bool
	}{
		{name: "none", data: []byte{0}},
		{name: "some", data: append([]byte{1}, blockID[:]...), want: &blockID},
		{name: "truncated", data: []byte{1}, wantErr: true},
		{name: "trailing", data: []byte{0, 1}, wantErr: true},
		{name: "invalid option", data: []byte{2}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := new(SnapshotManifest)
			err := manifest.decodeV4Tail(bin.NewBinDecoder(test.data))
			if test.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.True(t, manifest.BlockIDFieldPresent)
			require.Equal(t, test.want, manifest.BlockID)
		})
	}

	manifest := &SnapshotManifest{BlockID: &blockID}
	require.NoError(t, manifest.decodeV4Tail(bin.NewBinDecoder([]byte{0})))
	require.Nil(t, manifest.BlockID)
}

func TestValidateManifestCountRejectsImpossibleAllocation(t *testing.T) {
	decoder := bin.NewBinDecoder(make([]byte, 11))
	require.Error(t, validateManifestCount(decoder, 1, 12, "versioned epoch stakes"))
	require.NoError(t, validateManifestCount(decoder, 11, 1, "bytes"))
}
