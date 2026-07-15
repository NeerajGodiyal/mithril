package turbine

import (
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestDoubleMerkleBlockIDMatchesAssemblerFixture(t *testing.T) {
	fecRoots := []solana.Hash{{1}, {2}, {3}}
	parentBlockID := solana.Hash{}
	parentBlockID[0] = 11

	got := DoubleMerkleBlockID(990, parentBlockID, fecRoots)
	want := doubleMerkleBlockID(fecRoots, 990, parentBlockID)
	require.Equal(t, want, got)
	require.NotEqual(t, fecRoots[len(fecRoots)-1], got)
}

func TestDoubleMerkleBlockIDEmptyRoots(t *testing.T) {
	require.Equal(t, solana.Hash{}, DoubleMerkleBlockID(1, solana.Hash{1}, nil))
}
