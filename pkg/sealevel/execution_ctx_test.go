package sealevel

import (
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAddModifiedVoteStateInitializesMapLazily(t *testing.T) {
	execCtx := new(ExecutionCtx)
	pubkey := solana.PublicKey{1}
	state := new(VoteStateVersions)

	require.NotPanics(t, func() {
		execCtx.AddModifiedVoteState(pubkey, state)
	})
	require.Len(t, execCtx.ModifiedVoteStates, 1)
	assert.Same(t, state, execCtx.ModifiedVoteStates[pubkey])
}
