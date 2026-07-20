package alpenglow

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
	require.False(t, history.AddParentReady(44, BlockID{Slot: 44, Hash: solana.Hash{9}}))
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

func TestVoteHistorySignedMalformedStateFailsClosed(t *testing.T) {
	identity := ed25519.NewKeyFromSeed(bytesOf(14, ed25519.SeedSize))
	node := solana.PublicKey(identity.Public().(ed25519.PublicKey))
	tests := []struct {
		name    string
		mutate  func(*VoteHistory)
		wantErr string
	}{
		{
			name: "non-canonical vote hash",
			mutate: func(history *VoteHistory) {
				vote := Vote{Type: VoteTypeSkip, Slot: 41, BlockHash: historyHash(1)}
				history.VotesCast[41] = []Vote{vote}
				history.Voted[41] = true
				history.Skipped[41] = true
			},
			wantErr: "non-canonical block hash",
		},
		{
			name: "conflicting base votes",
			mutate: func(history *VoteHistory) {
				history.VotesCast[41] = []Vote{
					NewNotarizationVote(41, historyHash(2)),
					NewSkipVote(41),
				}
				history.Voted[41] = true
				history.VotedNotar[41] = historyHash(2)
				history.Skipped[41] = true
			},
			wantErr: "already has a notarize-or-skip vote",
		},
		{
			name: "redundant map disagreement",
			mutate: func(history *VoteHistory) {
				require.NoError(t, history.AddVote(NewSkipVote(41)))
				delete(history.Skipped, 41)
			},
			wantErr: "skipped map disagrees",
		},
		{
			name: "unbounded ParentReady gap",
			mutate: func(history *VoteHistory) {
				history.PersistedParentReady = []persistedParentReady{{
					Slot:    maxParentReadyRestoreGap + 2,
					Parents: []BlockID{{Slot: 1, Hash: historyHash(3)}},
				}}
			},
			wantErr: "invalid parent",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			history := NewVoteHistory(node, 1)
			tt.mutate(history)
			writeSignedVoteHistoryFixture(t, dir, history, identity)
			_, err := LoadVoteHistory(dir, node)
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestSaveVoteHistoryRejectsInconsistentState(t *testing.T) {
	identity := ed25519.NewKeyFromSeed(bytesOf(15, ed25519.SeedSize))
	node := solana.PublicKey(identity.Public().(ed25519.PublicKey))
	history := NewVoteHistory(node, 40)
	require.NoError(t, history.AddVote(NewSkipVote(41)))
	delete(history.Skipped, 41)
	require.ErrorContains(t, SaveVoteHistory(t.TempDir(), history, identity), "skipped map disagrees")
}

func TestSaveVoteHistoryRejectsFalseSetMembership(t *testing.T) {
	identity := ed25519.NewKeyFromSeed(bytesOf(16, ed25519.SeedSize))
	node := solana.PublicKey(identity.Public().(ed25519.PublicKey))

	t.Run("notarized block", func(t *testing.T) {
		history := NewVoteHistory(node, 40)
		history.NotarizedBlocks[BlockID{Slot: 41, Hash: solana.Hash{0x41}}] = false
		require.ErrorContains(t, SaveVoteHistory(t.TempDir(), history, identity), "false membership")
	})
	t.Run("ParentReady parent", func(t *testing.T) {
		history := NewVoteHistory(node, 40)
		history.ParentReadyBySlot[42] = map[BlockID]bool{
			{Slot: 41, Hash: solana.Hash{0x41}}: false,
		}
		require.ErrorContains(t, SaveVoteHistory(t.TempDir(), history, identity), "false membership")
	})
}

func TestSaveVoteHistoryAtomicallyReplacesPrivateFile(t *testing.T) {
	identity := ed25519.NewKeyFromSeed(bytesOf(12, ed25519.SeedSize))
	node := solana.PublicKey(identity.Public().(ed25519.PublicKey))
	dir := filepath.Join(t.TempDir(), "nested", "history")
	history := NewVoteHistory(node, 40)

	require.NoError(t, history.AddVote(NewSkipVote(41)))
	require.NoError(t, SaveVoteHistory(dir, history, identity))

	filename := VoteHistoryFilename(dir, node)
	info, err := os.Stat(filename)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	assertOnlyVoteHistoryFile(t, dir, filepath.Base(filename))

	require.NoError(t, history.AddVote(NewSkipVote(42)))
	require.NoError(t, SaveVoteHistory(dir, history, identity))
	loaded, err := LoadVoteHistory(dir, node)
	require.NoError(t, err)
	require.True(t, loaded.HasSkipped(41))
	require.True(t, loaded.HasSkipped(42))
	assertOnlyVoteHistoryFile(t, dir, filepath.Base(filename))
}

func TestSaveVoteHistoryCleansTemporaryFileWhenRenameFails(t *testing.T) {
	identity := ed25519.NewKeyFromSeed(bytesOf(13, ed25519.SeedSize))
	node := solana.PublicKey(identity.Public().(ed25519.PublicKey))
	dir := t.TempDir()
	filename := VoteHistoryFilename(dir, node)

	// A directory cannot be atomically replaced by the temporary regular file.
	// This exercises cleanup after write, file sync, and close have succeeded.
	require.NoError(t, os.Mkdir(filename, 0o700))
	err := SaveVoteHistory(dir, NewVoteHistory(node, 40), identity)
	require.ErrorContains(t, err, "replace vote history")

	entries, readErr := os.ReadDir(dir)
	require.NoError(t, readErr)
	require.Len(t, entries, 1, "failed persistence must not leave a temporary file")
	require.Equal(t, filepath.Base(filename), entries[0].Name())
	require.True(t, entries[0].IsDir())
}

func assertOnlyVoteHistoryFile(t *testing.T, dir, filename string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "successful persistence must not leave a temporary file")
	require.Equal(t, filename, entries[0].Name())
	require.False(t, entries[0].IsDir())
}

func writeSignedVoteHistoryFixture(t *testing.T, dir string, history *VoteHistory, identity ed25519.PrivateKey) {
	t.Helper()
	data, err := json.Marshal(history)
	require.NoError(t, err)
	envelope := savedVoteHistory{
		Version:   voteHistoryVersion,
		Node:      history.NodePubkey,
		Data:      data,
		Signature: ed25519.Sign(identity, data),
	}
	encoded, err := json.Marshal(envelope)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(VoteHistoryFilename(dir, history.NodePubkey), encoded, 0o600))
}

func historyHash(v byte) (hash solana.Hash) {
	hash[0] = v
	return hash
}
