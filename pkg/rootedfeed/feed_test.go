package rootedfeed

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/rootedevents"
	"github.com/Overclock-Validator/mithril/pkg/state"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

const testRootRunID = "00112233"

func TestStreamAfterAndCursorErrors(t *testing.T) {
	root := t.TempDir()
	writeClassicBatch(t, root, 1, 10, 9)
	writeClassicBatch(t, root, 2, 12, 10)

	after := rootedevents.Cursor{Slot: 10, Ordinal: 0}
	var got []rootedevents.Event
	last, count, err := StreamAfter(root, 3, &after, func(event rootedevents.Event) error {
		got = append(got, event)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, uint64(1), count)
	require.Equal(t, rootedevents.Cursor{Slot: 12, Ordinal: 0}, *last)
	require.Len(t, got, 1)

	_, _, err = StreamAfter(root, 3, &rootedevents.Cursor{Slot: 9}, nil)
	var gap *CursorGapError
	require.ErrorAs(t, err, &gap)

	_, _, err = StreamAfter(root, 3, &rootedevents.Cursor{Slot: 10, Ordinal: 1}, nil)
	require.ErrorIs(t, err, ErrCursorNotFound)
}

func TestAvailableBatchesUsesVerifiedManifestSuffix(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, writeManifest(root, 1, 8, 8, nil))
	ref := writeClassicBatch(t, root, 2, 10, 8)

	batches, err := AvailableBatches(root, 3)
	require.NoError(t, err)
	require.Len(t, batches, 1)
	require.Equal(t, ref, batches[0].Ref)

	headers, err := retainedHeaders(root, 3)
	require.NoError(t, err)
	data, err := os.ReadFile(headers[len(headers)-1].Path)
	require.NoError(t, err)
	data[len(data)-1] ^= 0xff
	require.NoError(t, os.WriteFile(headers[len(headers)-1].Path, data, 0o600))
	_, err = AvailableBatches(root, 3)
	require.Error(t, err)
}

func TestFollowerDoesNotRereadAnUnchangedTail(t *testing.T) {
	root := t.TempDir()
	ref := writeClassicBatch(t, root, 1, 10, 9)
	follower, err := NewFollower(root, 2)
	require.NoError(t, err)

	last, count, err := follower.StreamAfter(nil, nil)
	require.NoError(t, err)
	require.Equal(t, uint64(1), count)
	require.NotNil(t, last)

	path := filepath.Join(root, rootedevents.SidecarDirectory, ref.File)
	require.NoError(t, os.Rename(path, path+".parked"))
	got, count, err := follower.StreamAfter(last, nil)
	require.NoError(t, err)
	require.Nil(t, got)
	require.Zero(t, count)
}

func TestFollowerRejectsRetentionGapAfterTail(t *testing.T) {
	root := t.TempDir()
	writeClassicBatch(t, root, 1, 10, 9)
	follower, err := NewFollower(root, 1)
	require.NoError(t, err)
	last, _, err := follower.StreamAfter(nil, nil)
	require.NoError(t, err)

	writeClassicBatch(t, root, 3, 14, 10)
	_, _, err = follower.StreamAfter(last, nil)
	var gap *CursorGapError
	require.ErrorAs(t, err, &gap)
}

func TestFollowerUsesPublishedHeadForIncrementalDiscovery(t *testing.T) {
	root := t.TempDir()
	writeClassicBatch(t, root, 1, 10, 9)
	follower, err := NewFollower(root, 2)
	require.NoError(t, err)
	last, _, err := follower.StreamAfter(nil, nil)
	require.NoError(t, err)

	writeClassicBatch(t, root, 2, 12, 10)
	writeClassicBatch(t, root, 999, 999, 998)
	headers, err := retainedHeaders(root, 1000)
	require.NoError(t, err)
	var head accountsdb.ManifestHeader
	for _, header := range headers {
		if header.BatchSeq == 2 {
			head = header
			break
		}
	}
	require.Equal(t, uint64(2), head.BatchSeq)
	require.NoError(t, PublishManifestHead(root, head))

	got, count, err := follower.StreamAfter(last, nil)
	require.NoError(t, err)
	require.Equal(t, uint64(1), count)
	require.Equal(t, rootedevents.Cursor{Slot: 12, Ordinal: 0}, *got)

	headPath := filepath.Join(root, rootedevents.SidecarDirectory, manifestHeadFile)
	require.NoError(t, os.WriteFile(headPath, []byte("{}\n"), 0o600))
	_, _, err = follower.StreamAfter(got, nil)
	require.ErrorContains(t, err, "manifest head")
}

func TestFollowerRescansWhenCompactionRemovesCachedBoundary(t *testing.T) {
	root := t.TempDir()
	for sequence, slot := range map[uint64]uint64{1: 10, 2: 12, 3: 14} {
		writeClassicBatch(t, root, sequence, slot, slot-2)
	}
	follower, err := NewFollower(root, 3)
	require.NoError(t, err)
	last, _, err := follower.StreamAfter(nil, nil)
	require.NoError(t, err)

	headers, err := retainedHeaders(root, 3)
	require.NoError(t, err)
	require.NoError(t, PublishManifestHead(root, headers[2]))
	require.NoError(t, os.Remove(headers[0].Path))

	got, count, err := follower.StreamAfter(last, nil)
	require.NoError(t, err)
	require.Nil(t, got)
	require.Zero(t, count)
}

func TestFollowerRetriesManifestRemovalOnlyOnce(t *testing.T) {
	info, err := os.Stat(t.TempDir())
	require.NoError(t, err)
	follower := &Follower{
		headers:     []accountsdb.ManifestHeader{{BatchSeq: 1}},
		accountsDir: info,
	}
	retried := false
	missing := &os.PathError{Op: "open", Path: "removed.manifest", Err: os.ErrNotExist}
	require.True(t, follower.retryMissingManifest(missing, &retried))
	require.True(t, retried)
	require.Nil(t, follower.headers)
	require.Nil(t, follower.accountsDir)
	require.False(t, follower.retryMissingManifest(missing, &retried))
	require.False(t, follower.retryMissingManifest(errors.New("CRC mismatch"), new(bool)))
}

func TestRetainerKeepsLiveAndParkedRewindBatches(t *testing.T) {
	root := t.TempDir()
	refs := map[uint64]*state.RootedEventBatchRef{}
	for sequence, slot := range map[uint64]uint64{1: 10, 2: 12, 3: 14, 4: 16} {
		refs[sequence] = writeClassicBatch(t, root, sequence, slot, slot-2)
	}
	parkManifest(t, root, 1)
	orphan, err := rootedevents.PrepareSidecar(root,
		[]accounts.SlotDelta{{Slot: 50}},
		map[uint64]rootedevents.SlotMeta{50: {
			Slot: 50, ParentSlot: 49, Blockhash: testHash(50), ParentBlockhash: testHash(49),
			Bankhash: testHash(150), FinalitySource: rootedevents.FinalityRPCFinalized,
		}},
	)
	require.NoError(t, err)
	dir := filepath.Join(root, rootedevents.SidecarDirectory)
	partial := filepath.Join(dir, ".events-orphan.partial")
	headTemp := filepath.Join(dir, ".manifest-head-orphan.tmp")
	unknown := filepath.Join(dir, "operator-note")
	require.NoError(t, os.WriteFile(partial, []byte("partial"), 0o600))
	require.NoError(t, os.WriteFile(headTemp, []byte("partial"), 0o600))
	require.NoError(t, os.WriteFile(unknown, []byte("keep"), 0o600))

	retainer, err := NewRetainer(root, 1)
	require.NoError(t, err)
	removed, err := retainer.Cleanup(refs[4])
	require.NoError(t, err)
	require.Len(t, removed, 4)
	for _, sequence := range []uint64{1, 3, 4} {
		require.FileExists(t, filepath.Join(dir, refs[sequence].File))
	}
	require.NoFileExists(t, filepath.Join(dir, refs[2].File))
	require.NoFileExists(t, filepath.Join(dir, orphan.File))
	require.NoFileExists(t, partial)
	require.NoFileExists(t, headTemp)
	require.FileExists(t, unknown)
}

func TestRetainerKeepsEveryInHorizonReferenceAcrossCaptureGap(t *testing.T) {
	root := t.TempDir()
	before := writeClassicBatch(t, root, 1, 10, 9)
	require.NoError(t, writeManifest(root, 2, 10, 14, nil))
	after := writeClassicBatch(t, root, 3, 16, 14)

	retainer, err := NewRetainer(root, 2)
	require.NoError(t, err)
	_, err = retainer.Cleanup(after)
	require.NoError(t, err)
	dir := filepath.Join(root, rootedevents.SidecarDirectory)
	require.FileExists(t, filepath.Join(dir, before.File))
	require.FileExists(t, filepath.Join(dir, after.File))
}

func TestRetainerCancelsCleanupForCorruptParkedManifest(t *testing.T) {
	root := t.TempDir()
	writeClassicBatch(t, root, 1, 10, 9)
	parked := parkManifest(t, root, 1)
	data, err := os.ReadFile(parked)
	require.NoError(t, err)
	data[len(data)-1] ^= 0xff
	require.NoError(t, os.WriteFile(parked, data, 0o600))
	orphan, err := rootedevents.PrepareSidecar(root,
		[]accounts.SlotDelta{{Slot: 50}},
		map[uint64]rootedevents.SlotMeta{50: {
			Slot: 50, ParentSlot: 49, Blockhash: testHash(50), ParentBlockhash: testHash(49),
			Bankhash: testHash(150), FinalitySource: rootedevents.FinalityRPCFinalized,
		}},
	)
	require.NoError(t, err)
	partial := filepath.Join(root, rootedevents.SidecarDirectory, ".events-orphan.partial")
	require.NoError(t, os.WriteFile(partial, []byte("partial"), 0o600))

	retainer, err := NewRetainer(root, 1)
	require.NoError(t, err)
	_, err = retainer.Cleanup(nil)
	require.Error(t, err)
	require.FileExists(t, filepath.Join(root, rootedevents.SidecarDirectory, orphan.File))
	require.FileExists(t, partial)
}

func TestManifestHeadCleanupRejectsSymlinkDirectory(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "accounts"), 0o755))
	external := t.TempDir()
	outside := filepath.Join(external, ".manifest-head-outside.tmp")
	require.NoError(t, os.WriteFile(outside, []byte("keep"), 0o600))
	require.NoError(t, os.Symlink(external, filepath.Join(root, rootedevents.SidecarDirectory)))

	_, err := cleanupManifestHeadTemps(root)
	require.ErrorContains(t, err, "not a real directory")
	require.FileExists(t, outside)
}

func TestStreamAdvancesCursorOnlyAfterSuccessfulEmission(t *testing.T) {
	root := t.TempDir()
	writeClassicBatch(t, root, 1, 10, 9)
	writeClassicBatch(t, root, 2, 12, 10)
	want := errors.New("write failed")

	emitted := 0
	last, count, err := StreamAfter(root, 2, nil, func(rootedevents.Event) error {
		emitted++
		if emitted == 2 {
			return want
		}
		return nil
	})
	require.ErrorIs(t, err, want)
	require.Equal(t, uint64(1), count)
	require.Equal(t, rootedevents.Cursor{Slot: 10, Ordinal: 0}, *last)
}

func TestStreamBindsCrossBatchRootIdentity(t *testing.T) {
	root := t.TempDir()
	writeAlpenglowBatch(t, root, 1, 10, 9, rootedevents.FinalityAlpenglowCertificate)
	writeAlpenglowBatch(t, root, 2, 12, 10, rootedevents.FinalityAlpenglowCertificate)
	requireStreamOK(t, root)

	tests := []struct {
		name   string
		change func(*rootedevents.SlotMeta)
	}{
		{name: "parent blockhash", change: func(meta *rootedevents.SlotMeta) { meta.ParentBlockhash = testHash(99) }},
		{name: "parent block ID", change: func(meta *rootedevents.SlotMeta) { meta.AlpenglowParentBlockID = testHash(99) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeAlpenglowBatch(t, root, 1, 10, 9, rootedevents.FinalityAlpenglowCertificate)
			meta := alpenglowMeta(12, 10, rootedevents.FinalityAlpenglowCertificate)
			test.change(&meta)
			writeBatch(t, root, 2, meta)
			_, _, err := StreamAfter(root, 2, nil, nil)
			require.ErrorContains(t, err, "does not continue")
			_, _, err = StreamAfter(root, 2, &rootedevents.Cursor{Slot: 12, Ordinal: 0}, nil)
			require.ErrorContains(t, err, "does not continue")
			_, err = LatestCursor(root, 2)
			require.ErrorContains(t, err, "does not continue")
		})
	}
}

func TestStreamAllowsFinalitySourceTransitionAcrossBatches(t *testing.T) {
	root := t.TempDir()
	writeAlpenglowBatch(t, root, 1, 10, 9, rootedevents.FinalityAlpenglowDelegated)
	writeAlpenglowBatch(t, root, 2, 12, 10, rootedevents.FinalityAlpenglowCertificate)
	requireStreamOK(t, root)
}

func TestFramedFollowerEmitsOnePreambleAndNothingAtTail(t *testing.T) {
	root := t.TempDir()
	writeSourceState(t, root, "devnet", true)
	writeClassicBatch(t, root, 1, 10, 9)
	follower, err := NewFollower(root, 2)
	require.NoError(t, err)

	var metadata []MetadataRecord
	last, count, err := follower.StreamFramedAfter(nil, func(record MetadataRecord) error {
		metadata = append(metadata, record)
		return nil
	}, nil)
	require.NoError(t, err)
	require.Equal(t, uint64(1), count)
	require.Len(t, metadata, 3)
	require.Equal(t, RecordTypeSource, metadata[0].RecordType)
	require.Equal(t, RecordTypeStart, metadata[1].RecordType)
	require.Equal(t, RecordTypeBatch, metadata[2].RecordType)

	metadata = nil
	got, count, err := follower.StreamFramedAfter(last, func(record MetadataRecord) error {
		metadata = append(metadata, record)
		return nil
	}, nil)
	require.NoError(t, err)
	require.Nil(t, got)
	require.Zero(t, count)
	require.Empty(t, metadata)

	writeClassicBatch(t, root, 2, 12, 10)
	_, count, err = follower.StreamFramedAfter(last, func(record MetadataRecord) error {
		metadata = append(metadata, record)
		return nil
	}, nil)
	require.NoError(t, err)
	require.Equal(t, uint64(1), count)
	require.Len(t, metadata, 1)
	require.Equal(t, RecordTypeBatch, metadata[0].RecordType)
}

func TestFramedFollowerRejectsSourceChange(t *testing.T) {
	root := t.TempDir()
	writeSourceState(t, root, "devnet", true)
	writeClassicBatch(t, root, 1, 10, 9)
	follower, err := NewFollower(root, 2)
	require.NoError(t, err)
	last, _, err := follower.StreamFramedAfter(nil, nil, nil)
	require.NoError(t, err)

	writeSourceState(t, root, "testnet", true)
	_, _, err = follower.StreamFramedAfter(last, nil, nil)
	require.ErrorContains(t, err, "source identity changed")
}

func TestLoadSourceDescriptorRejectsInvalidIdentity(t *testing.T) {
	tests := []struct {
		name    string
		cluster string
		genesis string
		runID   string
		rooted  bool
		want    string
	}{
		{name: "classic profile", cluster: "devnet", genesis: solana.Hash{9}.String(), runID: testRootRunID, want: "not rooted-durable"},
		{name: "Alpenglow profile", cluster: "alpenglow", genesis: solana.Hash{9}.String(), runID: testRootRunID, want: "not rooted-durable"},
		{name: "cluster", cluster: "localnet", genesis: solana.Hash{9}.String(), runID: testRootRunID, rooted: true, want: "unsupported"},
		{name: "genesis", cluster: "alpenglow", genesis: "invalid", runID: testRootRunID, rooted: true, want: "genesis hash is invalid"},
		{name: "root run", cluster: "alpenglow", genesis: solana.Hash{9}.String(), runID: "ABC", rooted: true, want: "root run ID is invalid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			s := state.NewReadyStateWithOpts(state.NewReadyStateOpts{Cluster: test.cluster, GenesisHash: test.genesis})
			s.RootRunID = test.runID
			s.RootedDurable = test.rooted
			require.NoError(t, s.Save(root))
			_, err := LoadSourceDescriptor(root)
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestGoldenFramedContractBindsCanonicalSidecarBytes(t *testing.T) {
	file, err := os.Open("testdata/framed-v1.jsonl")
	require.NoError(t, err)
	defer file.Close()

	var batch BatchDescriptor
	digest := sha256.New()
	scanner := bufio.NewScanner(file)
	line := 0
	for scanner.Scan() {
		line++
		wire := bytes.Clone(scanner.Bytes())
		var value any
		var metadata *MetadataRecord
		if line <= 3 {
			metadata = &MetadataRecord{}
			value = metadata
		} else {
			value = &rootedevents.Event{}
			_, err := digest.Write(append(wire, '\n'))
			require.NoError(t, err)
		}
		require.NoError(t, json.Unmarshal(wire, value))
		if line == 3 {
			require.NotNil(t, metadata.Batch)
			batch = *metadata.Batch
		}
		canonical, err := json.Marshal(value)
		require.NoError(t, err)
		require.Equal(t, string(wire), string(canonical), "fixture line %d", line)
	}
	require.NoError(t, scanner.Err())
	require.Equal(t, 5, line)
	require.Equal(t, hex.EncodeToString(digest.Sum(nil)), batch.SHA256)
}

func requireStreamOK(t *testing.T, root string) {
	t.Helper()
	_, count, err := StreamAfter(root, 2, nil, nil)
	require.NoError(t, err)
	require.Equal(t, uint64(2), count)
}

func writeClassicBatch(t *testing.T, root string, sequence, slot, parent uint64) *state.RootedEventBatchRef {
	t.Helper()
	return writeBatch(t, root, sequence, rootedevents.SlotMeta{
		Slot: slot, ParentSlot: parent,
		Blockhash: testHash(slot), ParentBlockhash: testHash(parent), Bankhash: testHash(slot + 100),
		FinalitySource: rootedevents.FinalityRPCFinalized,
	})
}

func writeAlpenglowBatch(t *testing.T, root string, sequence, slot, parent uint64, source rootedevents.FinalitySource) *state.RootedEventBatchRef {
	t.Helper()
	return writeBatch(t, root, sequence, alpenglowMeta(slot, parent, source))
}

func alpenglowMeta(slot, parent uint64, source rootedevents.FinalitySource) rootedevents.SlotMeta {
	return rootedevents.SlotMeta{
		Slot: slot, ParentSlot: parent,
		Blockhash: testHash(slot), ParentBlockhash: testHash(parent), Bankhash: testHash(slot + 100),
		AlpenglowBlockID: testHash(slot + 20), HasAlpenglowBlockID: true,
		AlpenglowParentBlockID: testHash(parent + 20), HasAlpenglowParentBlockID: true,
		FinalitySource: source,
	}
}

func writeBatch(t *testing.T, root string, sequence uint64, meta rootedevents.SlotMeta) *state.RootedEventBatchRef {
	t.Helper()
	ref, err := rootedevents.PrepareSidecar(root,
		[]accounts.SlotDelta{{Slot: meta.Slot}},
		map[uint64]rootedevents.SlotMeta{meta.Slot: meta},
	)
	require.NoError(t, err)
	require.NoError(t, writeManifest(root, sequence, meta.Slot-1, meta.Slot, ref))
	return ref
}

func writeSourceState(t *testing.T, root, cluster string, rooted bool) {
	t.Helper()
	s := state.NewReadyStateWithOpts(state.NewReadyStateOpts{Cluster: cluster, GenesisHash: solana.Hash{9}.String()})
	s.RootRunID = testRootRunID
	s.RootedDurable = rooted
	require.NoError(t, s.Save(root))
}

func writeManifest(root string, sequence, from, through uint64, ref *state.RootedEventBatchRef) error {
	dir, err := accountsDir(root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ctx, err := json.Marshal(&state.ResumeContext{Slot: through, RootedEventBatch: ref})
	if err != nil {
		return err
	}
	return accountsdb.WriteSegmentManifest(dir, &accountsdb.SegmentManifest{
		Version: 1, Kind: accountsdb.ManifestKindFold, BatchSeq: sequence,
		FromSlot: from, ThroughSlot: through, FileId: sequence, ResumeCtx: ctx,
	})
}

func parkManifest(t *testing.T, root string, sequence uint64) string {
	t.Helper()
	headers, err := retainedHeaders(root, ^uint64(0))
	require.NoError(t, err)
	for _, header := range headers {
		if header.BatchSeq != sequence {
			continue
		}
		parked := header.Path + ".rewound"
		require.NoError(t, os.Rename(header.Path, parked))
		return parked
	}
	t.Fatalf("manifest for sequence %d not found among %s", sequence, strings.Join(manifestPaths(headers), ", "))
	return ""
}

func manifestPaths(headers []accountsdb.ManifestHeader) []string {
	paths := make([]string, 0, len(headers))
	for _, header := range headers {
		paths = append(paths, header.Path)
	}
	return paths
}

func testHash(value uint64) [32]byte {
	var hash [32]byte
	hash[0] = byte(value)
	hash[1] = byte(value >> 8)
	return hash
}
