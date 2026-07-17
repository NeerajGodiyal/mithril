package alpenglow

import (
	"crypto/ed25519"
	"errors"
	"os"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestVoteHistoryRejectsEquivocationAndRoundTrips(t *testing.T) {
	identity := ed25519.NewKeyFromSeed(bytesOf(9, ed25519.SeedSize))
	node := solana.PublicKey(identity.Public().(ed25519.PublicKey))
	history := NewVoteHistory(node, 40)
	parent := BlockID{Slot: 43, Hash: historyHash(1)}
	block := BlockID{Slot: 44, Hash: historyHash(2)}
	require.True(t, history.AddParentReady(44, parent))
	history.AddBlockNotarized(block)
	require.NoError(t, history.AddVote(NewNotarizationVote(block.Slot, block.Hash)))
	require.Error(t, history.AddVote(NewSkipVote(block.Slot)))
	require.NoError(t, history.AddVote(NewFinalizationVote(block.Slot)))
	require.Error(t, history.AddVote(NewSkipFallbackVote(block.Slot)))
	fallbackBlock := BlockID{Slot: 45, Hash: historyHash(3)}
	require.NoError(t, history.AddVote(NewSkipVote(fallbackBlock.Slot)))
	require.NoError(t, history.AddVote(NewNotarizationFallbackVote(fallbackBlock.Slot, fallbackBlock.Hash)))
	require.NoError(t, history.AddVote(NewSkipFallbackVote(fallbackBlock.Slot)))

	dir := t.TempDir()
	require.NoError(t, SaveVoteHistory(dir, history, identity))
	restored, err := LoadVoteHistory(dir, node)
	require.NoError(t, err)
	require.Equal(t, history.Root, restored.Root)
	require.True(t, restored.IsParentReady(44, parent))
	require.True(t, restored.IsBlockNotarized(block))
	require.Equal(t, block.Hash, restored.VotedNotar[block.Slot])
	require.True(t, restored.IsOver(block.Slot))
	require.True(t, restored.HasNotarFallback(fallbackBlock))
	require.True(t, restored.HasSkipFallback(fallbackBlock.Slot))
	require.True(t, restored.HasSkipped(fallbackBlock.Slot))
	require.Len(t, restored.VotesAfter(43), 5)
}

func TestVoteHistoryTamperFailsClosed(t *testing.T) {
	identity := ed25519.NewKeyFromSeed(bytesOf(11, ed25519.SeedSize))
	node := solana.PublicKey(identity.Public().(ed25519.PublicKey))
	dir := t.TempDir()
	_, err := LoadVoteHistory(dir, node)
	require.True(t, errors.Is(err, ErrVoteHistoryNotFound))

	history := NewVoteHistory(node, 1)
	require.NoError(t, history.AddVote(NewSkipVote(4)))
	require.NoError(t, SaveVoteHistory(dir, history, identity))
	path := VoteHistoryFilename(dir, node)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	data[len(data)/2] ^= 1
	require.NoError(t, os.WriteFile(path, data, 0o600))
	_, err = LoadVoteHistory(dir, node)
	require.Error(t, err)
}

func historyHash(v byte) (hash solana.Hash) {
	hash[0] = v
	return hash
}
