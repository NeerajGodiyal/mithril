package rootedevents

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/state"
	"github.com/Overclock-Validator/mithril/pkg/txstatus"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestPrepareAndReadSidecar(t *testing.T) {
	root := t.TempDir()
	deltas := []accounts.SlotDelta{
		{Slot: 10, Delta: []*accounts.Account{{Key: solana.PublicKey{2}, Lamports: 2}, {Key: solana.PublicKey{1}, Lamports: 1}}},
		{Slot: 12},
	}
	meta10 := testSlotMeta(10, 9)
	meta10.Transactions = []TransactionObservation{testTransactionObservation(t)}
	metadata := map[uint64]SlotMeta{10: meta10, 12: testSlotMeta(12, 10)}

	ref, err := PrepareSidecar(root, deltas, metadata)
	require.NoError(t, err)
	require.NoError(t, ValidateSidecarRef(ref))
	require.Equal(t, uint64(10), ref.FromSlot)
	require.Equal(t, uint64(12), ref.ThroughSlot)

	var events []Event
	require.NoError(t, ReadSidecar(root, ref, func(event Event) error {
		events = append(events, event)
		return nil
	}))
	require.Len(t, events, 5)
	require.Equal(t, TransactionExecuted, events[0].Kind)
	require.Equal(t, solana.PublicKey{1}.String(), events[1].Account.Pubkey)
	require.Equal(t, SlotRooted, events[len(events)-1].Kind)

	retry, err := PrepareSidecar(root, deltas, metadata)
	require.NoError(t, err)
	require.Equal(t, ref, retry, "identical preparation must be idempotent")
}

func TestSidecarRoundTripsV1Transaction(t *testing.T) {
	config := solana.TransactionConfig{}.
		WithPriorityFee(9_001).
		WithComputeUnitLimit(300_000).
		WithLoadedAccountsDataSizeLimit(65_536).
		WithHeapSize(64 * 1024)
	message := solana.Message{
		Header: solana.MessageHeader{
			NumRequiredSignatures:       2,
			NumReadonlyUnsignedAccounts: 1,
		},
		AccountKeys: []solana.PublicKey{{1}, {2}, {3}},
		Instructions: []solana.CompiledInstruction{{
			ProgramIDIndex: 2,
			Accounts:       []uint16{0, 1},
		}},
		TransactionConfig: config,
	}
	_, err := message.SetVersion(solana.MessageVersionV1)
	require.NoError(t, err)
	tx := &solana.Transaction{Signatures: []solana.Signature{{1}, {2}}, Message: message}
	wire, err := tx.MarshalBinary()
	require.NoError(t, err)
	messageHash, err := txstatus.TransactionMessageHash(tx)
	require.NoError(t, err)

	root := t.TempDir()
	meta := testSlotMeta(10, 9)
	meta.Transactions = []TransactionObservation{{
		Signature: tx.Signatures[0].String(), Transaction: wire,
		MessageHash: solana.Hash(messageHash).String(),
		AccountKeys: []string{solana.PublicKey{1}.String(), solana.PublicKey{2}.String(), solana.PublicKey{3}.String()}, Succeeded: true,
	}}
	ref, err := PrepareSidecar(root,
		[]accounts.SlotDelta{{Slot: 10}},
		map[uint64]SlotMeta{10: meta},
	)
	require.NoError(t, err)

	var record *TransactionRecord
	require.NoError(t, ReadSidecar(root, ref, func(event Event) error {
		if event.Transaction != nil {
			record = event.Transaction
		}
		return nil
	}))
	require.NotNil(t, record)
	decoded, err := solana.TransactionFromBytes(record.Transaction)
	require.NoError(t, err)
	require.Equal(t, solana.MessageVersionV1, decoded.Message.GetVersion())
	require.Equal(t, config, decoded.Message.TransactionConfig)
	require.Equal(t, tx.Signatures, decoded.Signatures)
}

func TestReadSidecarRejectsTamperAndSymlink(t *testing.T) {
	root := t.TempDir()
	deltas := []accounts.SlotDelta{{Slot: 10}}
	metadata := map[uint64]SlotMeta{10: testSlotMeta(10, 9)}
	ref, err := PrepareSidecar(root, deltas, metadata)
	require.NoError(t, err)
	path := filepath.Join(root, SidecarDirectory, ref.File)

	require.NoError(t, os.Chmod(path, 0o644))
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte{'['}, 0)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	called := false
	require.Error(t, ReadSidecar(root, ref, func(Event) error {
		called = true
		return nil
	}))
	require.False(t, called, "unverified events must not reach the consumer")

	require.NoError(t, os.Remove(path))
	target := filepath.Join(root, "other")
	require.NoError(t, os.WriteFile(target, []byte("not events"), 0o644))
	require.NoError(t, os.Symlink(target, path))
	require.ErrorContains(t, ReadSidecar(root, ref, nil), "non-symlink")
}

func TestSidecarOperationsRejectSymlinkDirectory(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	require.NoError(t, os.Symlink(external, filepath.Join(root, SidecarDirectory)))

	digest := strings.Repeat("0", 64)
	ref := &state.RootedEventBatchRef{
		Version:     SidecarVersion,
		FromSlot:    10,
		ThroughSlot: 10,
		File:        sidecarBasename(10, 10, digest),
		Size:        1,
		SHA256:      digest,
	}
	externalSidecar := filepath.Join(external, ref.File)
	sentinel := filepath.Join(external, "sentinel")
	require.NoError(t, os.WriteFile(externalSidecar, []byte{1}, 0o644))
	require.NoError(t, os.WriteFile(sentinel, []byte("keep"), 0o644))

	_, err := PrepareSidecar(root,
		[]accounts.SlotDelta{{Slot: 10}},
		map[uint64]SlotMeta{10: testSlotMeta(10, 9)},
	)
	require.ErrorContains(t, err, "non-symlink directory")
	require.ErrorContains(t, ReadSidecar(root, ref, nil), "non-symlink directory")
	_, err = CleanupSidecars(root, nil)
	require.ErrorContains(t, err, "non-symlink directory")
	require.FileExists(t, externalSidecar)
	require.FileExists(t, sentinel)
}

func TestReadSidecarRejectsUnknownFields(t *testing.T) {
	root := t.TempDir()
	ref, err := PrepareSidecar(root,
		[]accounts.SlotDelta{{Slot: 10}},
		map[uint64]SlotMeta{10: testSlotMeta(10, 9)},
	)
	require.NoError(t, err)
	dir := filepath.Join(root, SidecarDirectory)
	original, err := os.ReadFile(filepath.Join(dir, ref.File))
	require.NoError(t, err)
	modified := []byte(strings.Replace(string(original), `"kind":`, `"unknown":true,"kind":`, 1))
	digest := sha256.Sum256(modified)
	digestHex := hex.EncodeToString(digest[:])
	modifiedRef := &state.RootedEventBatchRef{
		Version:     SidecarVersion,
		FromSlot:    ref.FromSlot,
		ThroughSlot: ref.ThroughSlot,
		File:        sidecarBasename(ref.FromSlot, ref.ThroughSlot, digestHex),
		Size:        uint64(len(modified)),
		SHA256:      digestHex,
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, modifiedRef.File), modified, 0o444))
	require.ErrorContains(t, ReadSidecar(root, modifiedRef, nil), "unknown field")
}

func TestStreamValidatorRejectsInvalidAccountAndRootSemantics(t *testing.T) {
	validAccount := func() Event {
		return Event{
			SchemaVersion: SchemaVersion,
			Cursor:        Cursor{Slot: 10},
			Kind:          AccountUpdated,
			Account: &AccountUpdate{
				Pubkey:   solana.PublicKey{1}.String(),
				Owner:    solana.PublicKey{2}.String(),
				Lamports: 1,
			},
		}
	}
	t.Run("oversized account", func(t *testing.T) {
		event := validAccount()
		event.Account.Data = make([]byte, maxAccountDataBytes+1)
		err := newStreamValidator(&state.RootedEventBatchRef{FromSlot: 10, ThroughSlot: 10}).accept(event)
		require.ErrorContains(t, err, "data exceeds")
	})
	t.Run("inconsistent tombstone", func(t *testing.T) {
		event := validAccount()
		event.Account.Tombstone = true
		err := newStreamValidator(&state.RootedEventBatchRef{FromSlot: 10, ThroughSlot: 10}).accept(event)
		require.ErrorContains(t, err, "tombstone")
	})
	t.Run("empty bankhash", func(t *testing.T) {
		event := Event{
			SchemaVersion: SchemaVersion,
			Cursor:        Cursor{Slot: 10},
			Kind:          SlotRooted,
			Root:          &RootedSlot{ParentSlot: 9, Bankhash: solana.Hash{}.String()},
		}
		err := newStreamValidator(&state.RootedEventBatchRef{FromSlot: 10, ThroughSlot: 10}).accept(event)
		require.ErrorContains(t, err, "empty bankhash")
	})
}

func TestValidateSidecarRefRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	ref, err := PrepareSidecar(root,
		[]accounts.SlotDelta{{Slot: 10}},
		map[uint64]SlotMeta{10: testSlotMeta(10, 9)},
	)
	require.NoError(t, err)
	ref.File = "../" + ref.File
	require.ErrorContains(t, ValidateSidecarRef(ref), "filename")
}

func TestStreamValidatorRejectsTransactionAfterAccount(t *testing.T) {
	validator := newStreamValidator(&state.RootedEventBatchRef{FromSlot: 10, ThroughSlot: 10})
	require.NoError(t, validator.accept(Event{
		SchemaVersion: SchemaVersion,
		Cursor:        Cursor{Slot: 10},
		Kind:          AccountUpdated,
		Account: &AccountUpdate{
			Pubkey:   solana.PublicKey{1}.String(),
			Owner:    solana.PublicKey{2}.String(),
			Lamports: 1,
		},
	}))
	err := validator.accept(Event{
		SchemaVersion: SchemaVersion,
		Cursor:        Cursor{Slot: 10, Ordinal: 1},
		Kind:          TransactionExecuted,
		Transaction: &TransactionRecord{
			Index:       0,
			Signature:   solana.Signature{1}.String(),
			Transaction: []byte{1},
			Succeeded:   true,
		},
	})
	require.ErrorContains(t, err, "malformed transaction event")
}

func TestCleanupSidecarsKeepsSelectedAndUnknownFiles(t *testing.T) {
	root := t.TempDir()
	prepare := func(slot uint64) *state.RootedEventBatchRef {
		ref, err := PrepareSidecar(root,
			[]accounts.SlotDelta{{Slot: slot}},
			map[uint64]SlotMeta{slot: testSlotMeta(slot, slot-1)},
		)
		require.NoError(t, err)
		return ref
	}
	keep := prepare(10)
	remove := prepare(11)
	dir := filepath.Join(root, SidecarDirectory)
	unknown := filepath.Join(dir, "operator-note")
	require.NoError(t, os.WriteFile(unknown, []byte("keep"), 0o644))
	partial := filepath.Join(dir, ".events-orphan.partial")
	require.NoError(t, os.WriteFile(partial, []byte("partial"), 0o644))

	removed, err := CleanupSidecars(root, []*state.RootedEventBatchRef{keep})
	require.NoError(t, err)
	require.Len(t, removed, 2)
	require.FileExists(t, filepath.Join(dir, keep.File))
	require.NoFileExists(t, filepath.Join(dir, remove.File))
	require.NoFileExists(t, partial)
	require.FileExists(t, unknown)
}
