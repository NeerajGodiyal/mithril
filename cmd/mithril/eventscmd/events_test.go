package eventscmd

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/config"
	"github.com/Overclock-Validator/mithril/pkg/rootedevents"
	"github.com/Overclock-Validator/mithril/pkg/rootedfeed"
	"github.com/Overclock-Validator/mithril/pkg/state"
	"github.com/gagliardetto/solana-go"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

func TestParseCursorAndFilters(t *testing.T) {
	cursor, err := parseCursor("42:7")
	require.NoError(t, err)
	require.Equal(t, rootedevents.Cursor{Slot: 42, Ordinal: 7}, cursor)
	_, err = parseCursor("42")
	require.ErrorContains(t, err, "SLOT:ORDINAL")

	owner := solana.PublicKey{1}
	account := solana.PublicKey{2}
	other := solana.PublicKey{3}
	update := rootedevents.Event{Kind: rootedevents.AccountUpdated, Account: &rootedevents.AccountUpdate{
		Owner: owner.String(), Pubkey: account.String(),
	}}
	require.True(t, matches(update, &owner, &account, nil))
	require.False(t, matches(update, &owner, &other, nil))
	require.True(t, matches(rootedevents.Event{Kind: rootedevents.SlotRooted}, &owner, &other, nil))
	transaction := rootedevents.Event{Kind: rootedevents.TransactionExecuted, Transaction: &rootedevents.TransactionRecord{
		AccountKeys: []string{account.String()},
	}}
	require.True(t, matches(transaction, nil, nil, &account))
	require.False(t, matches(transaction, nil, nil, &other))
}

func TestEventsCommandResumesAndFramesOutput(t *testing.T) {
	root := t.TempDir()
	writeCommandSourceState(t, root)
	writeCommandBatch(t, root, 1, 10, 9)
	writeCommandBatch(t, root, 2, 12, 10)
	var output bytes.Buffer
	cmd := newEventsCommand()
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"--accounts", root, "--after", "10:0", "--framed"})
	require.NoError(t, cmd.Execute())

	decoder := json.NewDecoder(&output)
	var source, start, batch rootedfeed.MetadataRecord
	var event rootedevents.Event
	require.NoError(t, decoder.Decode(&source))
	require.NoError(t, decoder.Decode(&start))
	require.NoError(t, decoder.Decode(&batch))
	require.NoError(t, decoder.Decode(&event))
	require.Equal(t, rootedfeed.RecordTypeSource, source.RecordType)
	require.Equal(t, "devnet", source.Source.Cluster)
	require.Equal(t, rootedfeed.RecordTypeStart, start.RecordType)
	require.Equal(t, &rootedfeed.StartDescriptor{After: &rootedevents.Cursor{Slot: 10}}, start.Start)
	require.Equal(t, rootedfeed.RecordTypeBatch, batch.RecordType)
	require.Equal(t, uint64(2), batch.Batch.ManifestSequence)
	require.Equal(t, rootedevents.Cursor{Slot: 12, Ordinal: 0}, event.Cursor)
	require.ErrorIs(t, decoder.Decode(&source), io.EOF)
}

func TestEventsCommandLatestFramedWritesNoRecords(t *testing.T) {
	root := t.TempDir()
	writeCommandSourceState(t, root)
	writeCommandBatch(t, root, 1, 10, 9)
	var output bytes.Buffer
	cmd := newEventsCommand()
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"--accounts", root, "--latest", "--framed"})
	require.NoError(t, cmd.Execute())
	require.Empty(t, output.String())
}

func TestEventsCommandUsesConfiguredRetentionWithAccountsOverride(t *testing.T) {
	root := t.TempDir()
	writeCommandSourceState(t, root)
	writeCommandBatch(t, root, 1, 10, 9)
	writeCommandBatch(t, root, 2, 12, 10)
	writeCommandBatch(t, root, 3, 14, 12)

	configPath := filepath.Join(t.TempDir(), "node.toml")
	require.NoError(t, os.WriteFile(configPath, []byte("[storage]\naccounts = '/unused'\nrewind_horizon_batches = 1\n"), 0o600))
	originalConfig := config.ConfigFile
	config.ConfigFile = configPath
	viper.Reset()
	t.Cleanup(func() {
		config.ConfigFile = originalConfig
		viper.Reset()
	})

	var output bytes.Buffer
	cmd := newEventsCommand()
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"--accounts", root, "--framed"})
	require.NoError(t, cmd.Execute())

	decoder := json.NewDecoder(&output)
	var source, start, batch rootedfeed.MetadataRecord
	require.NoError(t, decoder.Decode(&source))
	require.NoError(t, decoder.Decode(&start))
	require.NoError(t, decoder.Decode(&batch))
	require.Equal(t, uint64(2), batch.Batch.ManifestSequence)
}

func writeCommandBatch(t *testing.T, root string, sequence, slot, parent uint64) {
	t.Helper()
	ref, err := rootedevents.PrepareSidecar(root,
		[]accounts.SlotDelta{{Slot: slot}},
		map[uint64]rootedevents.SlotMeta{slot: {
			Slot: slot, ParentSlot: parent,
			Blockhash: commandHash(slot), ParentBlockhash: commandHash(parent), Bankhash: commandHash(slot + 100),
			FinalitySource: rootedevents.FinalityRPCFinalized,
		}},
	)
	require.NoError(t, err)
	ctx, err := json.Marshal(&state.ResumeContext{Slot: slot, RootedEventBatch: ref})
	require.NoError(t, err)
	dir := filepath.Join(root, "accounts")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, accountsdb.WriteSegmentManifest(dir, &accountsdb.SegmentManifest{
		Version: 1, Kind: accountsdb.ManifestKindFold, BatchSeq: sequence,
		FromSlot: slot - 1, ThroughSlot: slot, FileId: sequence, ResumeCtx: ctx,
	}))
}

func writeCommandSourceState(t *testing.T, root string) {
	t.Helper()
	s := state.NewReadyStateWithOpts(state.NewReadyStateOpts{Cluster: "devnet", GenesisHash: solana.Hash{9}.String()})
	s.RootRunID = "00112233"
	s.RootedDurable = true
	require.NoError(t, s.Save(root))
}

func commandHash(value uint64) [32]byte {
	var hash [32]byte
	hash[0] = byte(value)
	hash[1] = byte(value >> 8)
	return hash
}
