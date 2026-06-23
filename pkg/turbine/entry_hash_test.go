package turbine

import (
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestAlpentickHashMatchesAlpenglowLowPower(t *testing.T) {
	parent := solana.Hash{1, 2, 3}
	got := AlpentickHash(parent)
	want := sha256Hash(parent[:])
	require.Equal(t, want, got)
}

func TestNextAlpenglowEntryHashZeroStepIsIdentity(t *testing.T) {
	start := solana.Hash{9}
	require.Equal(t, start, NextAlpenglowEntryHash(start, 0, nil))
}
