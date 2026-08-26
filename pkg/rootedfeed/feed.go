// Package rootedfeed reads the manifest-selected rooted event suffix for
// external indexers. It never opens the mutable AccountsDB index.
package rootedfeed

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/rootedevents"
	"github.com/Overclock-Validator/mithril/pkg/state"
	"github.com/gagliardetto/solana-go"
)

const (
	RecordTypeSource = "mithril.rooted_source"
	RecordTypeStart  = "mithril.rooted_start"
	RecordTypeBatch  = "mithril.rooted_batch"

	manifestHeadVersion = uint32(1)
	manifestHeadFile    = ".manifest-head.json"
)

var (
	// ErrNoBatches means rooted-event capture has no retained committed batch.
	ErrNoBatches = errors.New("no retained rooted-event batches")
	// ErrCursorNotFound means the requested cursor is not in the retained feed.
	ErrCursorNotFound = errors.New("rooted-event cursor not found")
)

// CursorGapError reports that retention has already passed a requested cursor.
type CursorGapError struct {
	Requested rootedevents.Cursor
	Earliest  rootedevents.Cursor
}

func (e *CursorGapError) Error() string {
	return fmt.Sprintf("rooted-event cursor %d:%d is older than retained feed beginning at %d:%d",
		e.Requested.Slot, e.Requested.Ordinal, e.Earliest.Slot, e.Earliest.Ordinal)
}

// Batch is one immutable event sidecar selected by a fold manifest.
type Batch struct {
	Sequence uint64
	Ref      *state.RootedEventBatchRef
}

// SourceDescriptor binds a framed export to one ready AccountsDB lineage.
type SourceDescriptor struct {
	Cluster             string `json:"cluster"`
	GenesisHash         string `json:"genesis_hash"`
	AccountsDBRootRunID string `json:"accountsdb_root_run_id"`
}

// BatchDescriptor identifies one immutable sidecar and its selecting manifest.
type BatchDescriptor struct {
	ManifestSequence uint64 `json:"manifest_sequence"`
	SidecarVersion   uint32 `json:"sidecar_version"`
	FromSlot         uint64 `json:"from_slot"`
	ThroughSlot      uint64 `json:"through_slot"`
	SHA256           string `json:"sha256"`
}

// StartDescriptor records the exclusive cursor requested by a framed stream.
type StartDescriptor struct {
	After *rootedevents.Cursor `json:"after"`
}

// MetadataRecord carries one framed stream source, start, or batch record.
type MetadataRecord struct {
	RecordType string            `json:"record_type"`
	Source     *SourceDescriptor `json:"source,omitempty"`
	Start      *StartDescriptor  `json:"start,omitempty"`
	Batch      *BatchDescriptor  `json:"batch,omitempty"`
}

type manifestContext struct {
	Slot             uint64                     `json:"slot"`
	RootedEventBatch *state.RootedEventBatchRef `json:"rooted_event_batch,omitempty"`
}

type cachedManifest struct {
	header  accountsdb.ManifestHeader
	info    os.FileInfo
	ref     *state.RootedEventBatchRef
	enabled bool
}

type manifestSelection struct {
	sequence uint64
	ref      *state.RootedEventBatchRef
}

type manifestHead struct {
	Version     uint32 `json:"version"`
	BatchSeq    uint64 `json:"batch_sequence"`
	FromSlot    uint64 `json:"from_slot"`
	ThroughSlot uint64 `json:"through_slot"`
	FileID      uint64 `json:"file_id"`
	DataLen     uint64 `json:"data_len"`
	DataCRC     uint32 `json:"data_crc"`
}

type rootIdentity struct {
	slot      uint64
	blockhash string
	blockID   string
}

// Follower caches verified immutable manifests for one stream. It is not safe
// for concurrent use; an unchanged tail reads only the small published head.
type Follower struct {
	accountsDBRoot string
	retainBatches  uint64
	manifests      map[uint64]cachedManifest
	headers        []accountsdb.ManifestHeader
	accountsDir    os.FileInfo

	tailSequence uint64
	tailCursor   *rootedevents.Cursor
	tailRoot     *rootIdentity

	source          *SourceDescriptor
	start           *StartDescriptor
	preambleEmitted bool
}

// NewFollower opens one logical stream over an AccountsDB rooted-event suffix.
func NewFollower(accountsDBRoot string, retainBatches uint64) (*Follower, error) {
	if strings.TrimSpace(accountsDBRoot) == "" {
		return nil, errors.New("rooted-event AccountsDB root is empty")
	}
	if retainBatches == 0 {
		return nil, errors.New("rooted-event retained batch count must be positive")
	}
	return &Follower{
		accountsDBRoot: accountsDBRoot,
		retainBatches:  retainBatches,
		manifests:      make(map[uint64]cachedManifest),
	}, nil
}

// AvailableBatches returns the newest contiguous suffix selected by retained
// fold manifests.
func AvailableBatches(accountsDBRoot string, retainBatches uint64) ([]Batch, error) {
	follower, err := NewFollower(accountsDBRoot, retainBatches)
	if err != nil {
		return nil, err
	}
	return follower.availableBatches()
}

func (f *Follower) availableBatches() ([]Batch, error) {
	selections, err := f.retainedSelections()
	if err != nil {
		return nil, err
	}
	var batches []Batch
	for _, selection := range selections {
		if selection.ref == nil {
			batches = nil
			continue
		}
		ref := *selection.ref
		batches = append(batches, Batch{Sequence: selection.sequence, Ref: &ref})
	}
	if len(batches) == 0 {
		return nil, ErrNoBatches
	}
	for i := 1; i < len(batches); i++ {
		if batches[i].Sequence != batches[i-1].Sequence+1 {
			return nil, fmt.Errorf("rooted-event batch sequence %d does not follow %d", batches[i].Sequence, batches[i-1].Sequence)
		}
	}
	return batches, nil
}

func (f *Follower) retainedSelections() ([]manifestSelection, error) {
	rescanned := false
retry:
	headers, err := f.retainedHeaders()
	if err != nil {
		return nil, err
	}
	nextCache := make(map[uint64]cachedManifest, len(headers))
	selections := make([]manifestSelection, 0, len(headers))
	for _, header := range headers {
		cached, ok := f.manifests[header.BatchSeq]
		info, statErr := os.Stat(header.Path)
		if statErr != nil {
			if f.retryMissingManifest(statErr, &rescanned) {
				goto retry
			}
			return nil, fmt.Errorf("stat rooted-event fold manifest %s: %w", header.Path, statErr)
		}
		if !ok || cached.header != header || !sameFileVersion(cached.info, info) {
			manifest, readErr := accountsdb.ReadSegmentManifestContextVerified(header.Path)
			if readErr != nil {
				if f.retryMissingManifest(readErr, &rescanned) {
					goto retry
				}
				return nil, fmt.Errorf("verify rooted-event fold manifest %s: %w", header.Path, readErr)
			}
			ctx, decodeErr := decodeManifestContext(manifest)
			if decodeErr != nil {
				return nil, decodeErr
			}
			if manifest.Kind != header.Kind || manifest.BatchSeq != header.BatchSeq ||
				manifest.FromSlot != header.FromSlot || manifest.ThroughSlot != header.ThroughSlot ||
				manifest.FileId != header.FileId || manifest.DataLen != header.DataLen || manifest.DataCRC != header.DataCRC {
				return nil, fmt.Errorf("rooted-event fold manifest %s does not match its discovery header", header.Path)
			}
			cached = cachedManifest{header: header, info: info}
			if ctx.RootedEventBatch != nil {
				if refErr := validateManifestRef(manifest, ctx.RootedEventBatch); refErr != nil {
					return nil, refErr
				}
				ref := *ctx.RootedEventBatch
				cached.ref = &ref
				cached.enabled = true
			}
		}
		nextCache[header.BatchSeq] = cached
		selection := manifestSelection{sequence: header.BatchSeq}
		if cached.enabled {
			ref := *cached.ref
			selection.ref = &ref
		}
		selections = append(selections, selection)
	}
	f.manifests = nextCache
	return selections, nil
}

func (f *Follower) retryMissingManifest(err error, retried *bool) bool {
	if !os.IsNotExist(err) || *retried {
		return false
	}
	f.headers = nil
	f.accountsDir = nil
	*retried = true
	return true
}

func sameFileVersion(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime() == b.ModTime()
}

// LoadSourceDescriptor describes the ready state that owns accountsDBRoot.
func LoadSourceDescriptor(accountsDBRoot string) (SourceDescriptor, error) {
	s, err := state.LoadState(accountsDBRoot)
	if err != nil {
		return SourceDescriptor{}, fmt.Errorf("read rooted-event source state: %w", err)
	}
	if s == nil || !s.IsReady() {
		return SourceDescriptor{}, errors.New("rooted-event source state is not ready")
	}
	if !s.RootedDurable {
		return SourceDescriptor{}, errors.New("rooted-event source is not rooted-durable")
	}
	switch s.Cluster {
	case "alpenglow":
	case "mainnet-beta", "testnet", "devnet":
	default:
		return SourceDescriptor{}, fmt.Errorf("rooted-event source cluster %q is unsupported", s.Cluster)
	}
	genesis, err := solana.HashFromBase58(s.GenesisHash)
	if err != nil || genesis == (solana.Hash{}) || genesis.String() != s.GenesisHash {
		return SourceDescriptor{}, errors.New("rooted-event source genesis hash is invalid")
	}
	runID, err := hex.DecodeString(s.RootRunID)
	if err != nil || len(runID) != 4 && len(runID) != 16 || hex.EncodeToString(runID) != s.RootRunID {
		return SourceDescriptor{}, errors.New("rooted-event source root run ID is invalid")
	}
	return SourceDescriptor{Cluster: s.Cluster, GenesisHash: s.GenesisHash, AccountsDBRootRunID: s.RootRunID}, nil
}

func describeBatch(batch Batch) BatchDescriptor {
	return BatchDescriptor{
		ManifestSequence: batch.Sequence, SidecarVersion: batch.Ref.Version,
		FromSlot: batch.Ref.FromSlot, ThroughSlot: batch.Ref.ThroughSlot, SHA256: batch.Ref.SHA256,
	}
}

// StreamAfter verifies and emits retained events strictly after after.
func StreamAfter(accountsDBRoot string, retainBatches uint64, after *rootedevents.Cursor, emit func(rootedevents.Event) error) (*rootedevents.Cursor, uint64, error) {
	follower, err := NewFollower(accountsDBRoot, retainBatches)
	if err != nil {
		return nil, 0, err
	}
	return follower.StreamAfter(after, emit)
}

func (f *Follower) StreamAfter(after *rootedevents.Cursor, emit func(rootedevents.Event) error) (*rootedevents.Cursor, uint64, error) {
	return f.stream(after, nil, emit)
}

// StreamFramedAfter adds source, start, and batch records to StreamAfter.
func StreamFramedAfter(accountsDBRoot string, retainBatches uint64, after *rootedevents.Cursor, emitMetadata func(MetadataRecord) error, emitEvent func(rootedevents.Event) error) (*rootedevents.Cursor, uint64, error) {
	follower, err := NewFollower(accountsDBRoot, retainBatches)
	if err != nil {
		return nil, 0, err
	}
	return follower.StreamFramedAfter(after, emitMetadata, emitEvent)
}

func (f *Follower) StreamFramedAfter(after *rootedevents.Cursor, emitMetadata func(MetadataRecord) error, emitEvent func(rootedevents.Event) error) (*rootedevents.Cursor, uint64, error) {
	source, err := LoadSourceDescriptor(f.accountsDBRoot)
	if err != nil {
		return nil, 0, err
	}
	if f.source == nil {
		copy := source
		f.source = &copy
		start := StartDescriptor{}
		if after != nil {
			cursor := *after
			start.After = &cursor
		}
		f.start = &start
	} else if *f.source != source {
		return nil, 0, errors.New("rooted-event source identity changed while following")
	}
	emitBatch := func(batch BatchDescriptor) error {
		if !f.preambleEmitted {
			if emitMetadata != nil {
				if err := emitMetadata(MetadataRecord{RecordType: RecordTypeSource, Source: f.source}); err != nil {
					return err
				}
				if err := emitMetadata(MetadataRecord{RecordType: RecordTypeStart, Start: f.start}); err != nil {
					return err
				}
			}
			f.preambleEmitted = true
		}
		if emitMetadata != nil {
			return emitMetadata(MetadataRecord{RecordType: RecordTypeBatch, Batch: &batch})
		}
		return nil
	}
	return f.stream(after, emitBatch, emitEvent)
}

func (f *Follower) stream(after *rootedevents.Cursor, emitBatch func(BatchDescriptor) error, emit func(rootedevents.Event) error) (*rootedevents.Cursor, uint64, error) {
	batches, err := f.availableBatches()
	if err != nil {
		return nil, 0, err
	}
	start, found, previousRoot, err := f.startPosition(batches, after)
	if err != nil {
		return nil, 0, err
	}
	if start == len(batches) {
		return nil, 0, nil
	}

	var last *rootedevents.Cursor
	var count uint64
	for i := start; i < len(batches); i++ {
		batchEmitted := false
		firstRoot := true
		readErr := rootedevents.ReadSidecar(f.accountsDBRoot, batches[i].Ref, func(event rootedevents.Event) error {
			if event.Root != nil {
				if firstRoot && previousRoot != nil {
					if err := validateNextRoot(batches[i].Sequence, previousRoot, event); err != nil {
						return err
					}
				}
				firstRoot = false
				previousRoot = identityForRoot(event)
			}
			if !found {
				if event.Cursor == *after {
					found = true
				}
				return nil
			}
			if !batchEmitted && emitBatch != nil {
				if err := emitBatch(describeBatch(batches[i])); err != nil {
					return err
				}
				batchEmitted = true
			}
			if emit != nil {
				if err := emit(event); err != nil {
					return err
				}
			}
			cursor := event.Cursor
			last = &cursor
			count++
			return nil
		})
		if readErr != nil {
			return last, count, readErr
		}
		if after != nil && i == start && !found {
			return nil, 0, fmt.Errorf("%w: %d:%d", ErrCursorNotFound, after.Slot, after.Ordinal)
		}
	}
	if last != nil {
		cursor := *last
		f.tailCursor = &cursor
	} else if after != nil && found {
		cursor := *after
		f.tailCursor = &cursor
	}
	f.tailSequence = batches[len(batches)-1].Sequence
	f.tailRoot = previousRoot
	return last, count, nil
}

func (f *Follower) startPosition(batches []Batch, after *rootedevents.Cursor) (int, bool, *rootIdentity, error) {
	if after == nil {
		return 0, true, nil, nil
	}
	if after.Slot < batches[0].Ref.FromSlot {
		return 0, false, nil, &CursorGapError{Requested: *after, Earliest: rootedevents.Cursor{Slot: batches[0].Ref.FromSlot}}
	}
	if f.tailCursor != nil && *after == *f.tailCursor && f.tailSequence != 0 {
		if batches[0].Sequence > f.tailSequence && batches[0].Sequence-f.tailSequence > 1 {
			return 0, false, nil, &CursorGapError{
				Requested: *after,
				Earliest:  rootedevents.Cursor{Slot: batches[0].Ref.FromSlot},
			}
		}
		for i, batch := range batches {
			if batch.Sequence > f.tailSequence {
				return i, true, f.tailRoot, nil
			}
		}
		return len(batches), true, f.tailRoot, nil
	}
	for i, batch := range batches {
		if after.Slot >= batch.Ref.FromSlot && after.Slot <= batch.Ref.ThroughSlot {
			if i == 0 {
				return i, false, nil, nil
			}
			previousRoot, err := f.lastRoot(batches[i-1])
			if err != nil {
				return 0, false, nil, err
			}
			return i, false, previousRoot, nil
		}
	}
	return 0, false, nil, fmt.Errorf("%w: %d:%d", ErrCursorNotFound, after.Slot, after.Ordinal)
}

func identityForRoot(event rootedevents.Event) *rootIdentity {
	return &rootIdentity{
		slot: event.Cursor.Slot, blockhash: event.Root.Blockhash,
		blockID: event.Root.BlockID,
	}
}

func validateNextRoot(sequence uint64, previous *rootIdentity, event rootedevents.Event) error {
	root := event.Root
	if root.ParentSlot != previous.slot || root.ParentBlockhash != previous.blockhash ||
		root.ParentBlockID != previous.blockID {
		return fmt.Errorf("rooted-event batch %d does not continue the previous block lineage", sequence)
	}
	return nil
}

// LatestCursor verifies the newest selected sidecar and returns its last cursor.
func LatestCursor(accountsDBRoot string, retainBatches uint64) (*rootedevents.Cursor, error) {
	follower, err := NewFollower(accountsDBRoot, retainBatches)
	if err != nil {
		return nil, err
	}
	return follower.LatestCursor()
}

func (f *Follower) LatestCursor() (*rootedevents.Cursor, error) {
	batches, err := f.availableBatches()
	if err != nil {
		return nil, err
	}
	var previousRoot *rootIdentity
	if len(batches) > 1 {
		previousRoot, err = f.lastRoot(batches[len(batches)-2])
		if err != nil {
			return nil, err
		}
	}
	var last *rootedevents.Cursor
	var lastRoot *rootIdentity
	firstRoot := true
	if err := rootedevents.ReadSidecar(f.accountsDBRoot, batches[len(batches)-1].Ref, func(event rootedevents.Event) error {
		cursor := event.Cursor
		last = &cursor
		if event.Root != nil {
			if firstRoot && previousRoot != nil {
				if err := validateNextRoot(batches[len(batches)-1].Sequence, previousRoot, event); err != nil {
					return err
				}
			}
			firstRoot = false
			lastRoot = identityForRoot(event)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if last == nil || lastRoot == nil {
		return nil, errors.New("newest rooted-event batch is empty")
	}
	f.tailSequence = batches[len(batches)-1].Sequence
	f.tailCursor = last
	f.tailRoot = lastRoot
	return last, nil
}

func (f *Follower) lastRoot(batch Batch) (*rootIdentity, error) {
	var root *rootIdentity
	if err := rootedevents.ReadSidecar(f.accountsDBRoot, batch.Ref, func(event rootedevents.Event) error {
		if event.Root != nil {
			root = identityForRoot(event)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if root == nil {
		return nil, errors.New("rooted-event batch has no terminal root")
	}
	return root, nil
}

func retainedHeaders(accountsDBRoot string, retainBatches uint64) ([]accountsdb.ManifestHeader, error) {
	if retainBatches == 0 {
		return nil, errors.New("rooted-event retained batch count must be positive")
	}
	dir, err := accountsDir(accountsDBRoot)
	if err != nil {
		return nil, err
	}
	headers, err := accountsdb.ListFoldManifests(dir)
	if err != nil {
		return nil, fmt.Errorf("list rooted-event fold manifests: %w", err)
	}
	if uint64(len(headers)) > retainBatches {
		headers = headers[len(headers)-int(retainBatches):]
	}
	return headers, nil
}

func (f *Follower) retainedHeaders() ([]accountsdb.ManifestHeader, error) {
	if len(f.headers) == 0 {
		return f.scanHeaders()
	}
	head, ok, err := readManifestHead(f.accountsDBRoot)
	if err != nil {
		return nil, fmt.Errorf("read rooted-event manifest head: %w", err)
	}
	if ok {
		last := f.headers[len(f.headers)-1]
		switch {
		case head.BatchSeq == last.BatchSeq && head == last:
			return append([]accountsdb.ManifestHeader(nil), f.headers...), nil
		case head.BatchSeq == last.BatchSeq+1:
			f.headers = append(f.headers, head)
			if uint64(len(f.headers)) > f.retainBatches {
				f.headers = append([]accountsdb.ManifestHeader(nil), f.headers[len(f.headers)-int(f.retainBatches):]...)
			}
			return append([]accountsdb.ManifestHeader(nil), f.headers...), nil
		}
	}
	dir, dirErr := accountsDir(f.accountsDBRoot)
	if dirErr != nil {
		return nil, dirErr
	}
	info, statErr := os.Stat(dir)
	if statErr == nil && sameFileVersion(f.accountsDir, info) {
		return append([]accountsdb.ManifestHeader(nil), f.headers...), nil
	}
	return f.scanHeaders()
}

func (f *Follower) scanHeaders() ([]accountsdb.ManifestHeader, error) {
	headers, err := retainedHeaders(f.accountsDBRoot, f.retainBatches)
	if err != nil {
		return nil, err
	}
	dir, err := accountsDir(f.accountsDBRoot)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	f.headers = append(f.headers[:0], headers...)
	f.accountsDir = info
	return append([]accountsdb.ManifestHeader(nil), headers...), nil
}

// PublishManifestHead atomically publishes one advisory discovery record.
// Readers still verify the immutable selecting manifest before using it.
func PublishManifestHead(accountsDBRoot string, header accountsdb.ManifestHeader) error {
	if header.Kind != accountsdb.ManifestKindFold || header.BatchSeq == 0 {
		return errors.New("publish rooted-event manifest head: invalid fold header")
	}
	dir, err := accountsDir(accountsDBRoot)
	if err != nil {
		return err
	}
	wantPath := filepath.Join(dir, accountsdb.SegmentDataName(header.ThroughSlot, header.FileId)+".manifest")
	if filepath.Clean(header.Path) != wantPath {
		return errors.New("publish rooted-event manifest head: non-canonical manifest path")
	}
	record := manifestHead{
		Version: manifestHeadVersion, BatchSeq: header.BatchSeq,
		FromSlot: header.FromSlot, ThroughSlot: header.ThroughSlot, FileID: header.FileId,
		DataLen: header.DataLen, DataCRC: header.DataCRC,
	}
	eventDir := filepath.Join(filepath.Dir(dir), rootedevents.SidecarDirectory)
	info, err := os.Lstat(eventDir)
	if os.IsNotExist(err) {
		if err := os.Mkdir(eventDir, 0o755); err != nil {
			return fmt.Errorf("create rooted-event directory for manifest head: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("stat rooted-event directory for manifest head: %w", err)
	} else if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("rooted-event path for manifest head is not a directory")
	}
	tmp, err := os.CreateTemp(eventDir, ".manifest-head-*.tmp")
	if err != nil {
		return fmt.Errorf("create rooted-event manifest head: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	encoder := json.NewEncoder(tmp)
	if err := encoder.Encode(record); err != nil {
		tmp.Close()
		return fmt.Errorf("encode rooted-event manifest head: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync rooted-event manifest head: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close rooted-event manifest head: %w", err)
	}
	if err := os.Rename(tmpPath, filepath.Join(eventDir, manifestHeadFile)); err != nil {
		return fmt.Errorf("publish rooted-event manifest head: %w", err)
	}
	d, err := os.Open(eventDir)
	if err != nil {
		return fmt.Errorf("open rooted-event directory after manifest head publish: %w", err)
	}
	err = d.Sync()
	closeErr := d.Close()
	if err != nil {
		return fmt.Errorf("sync rooted-event directory after manifest head publish: %w", err)
	}
	return closeErr
}

func readManifestHead(accountsDBRoot string) (accountsdb.ManifestHeader, bool, error) {
	accounts, err := accountsDir(accountsDBRoot)
	if err != nil {
		return accountsdb.ManifestHeader{}, false, err
	}
	path := filepath.Join(filepath.Dir(accounts), rootedevents.SidecarDirectory, manifestHeadFile)
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return accountsdb.ManifestHeader{}, false, nil
		}
		return accountsdb.ManifestHeader{}, false, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return accountsdb.ManifestHeader{}, false, errors.New("rooted-event manifest head is not a bounded regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return accountsdb.ManifestHeader{}, false, nil
		}
		return accountsdb.ManifestHeader{}, false, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) || opened.Size() > 4096 {
		return accountsdb.ManifestHeader{}, false, errors.New("rooted-event manifest head changed while opening")
	}
	decoder := json.NewDecoder(io.LimitReader(f, 4097))
	decoder.DisallowUnknownFields()
	var record manifestHead
	if err := decoder.Decode(&record); err != nil {
		return accountsdb.ManifestHeader{}, false, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return accountsdb.ManifestHeader{}, false, err
	}
	if record.Version != manifestHeadVersion || record.BatchSeq == 0 {
		return accountsdb.ManifestHeader{}, false, errors.New("rooted-event manifest head is invalid")
	}
	return accountsdb.ManifestHeader{
		Kind: accountsdb.ManifestKindFold, BatchSeq: record.BatchSeq,
		FromSlot: record.FromSlot, ThroughSlot: record.ThroughSlot, FileId: record.FileID,
		DataLen: record.DataLen, DataCRC: record.DataCRC,
		Path: filepath.Join(accounts, accountsdb.SegmentDataName(record.ThroughSlot, record.FileID)+".manifest"),
	}, true, nil
}

func accountsDir(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("rooted-event AccountsDB root is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve rooted-event AccountsDB root: %w", err)
	}
	return filepath.Join(filepath.Clean(abs), "accounts"), nil
}

func decodeManifestContext(manifest *accountsdb.SegmentManifest) (*manifestContext, error) {
	if manifest == nil || len(manifest.ResumeCtx) == 0 {
		return nil, errors.New("fold manifest carries no resume context")
	}
	var ctx manifestContext
	if err := json.Unmarshal(manifest.ResumeCtx, &ctx); err != nil {
		return nil, fmt.Errorf("decode fold manifest context through slot %d: %w", manifest.ThroughSlot, err)
	}
	if ctx.Slot != manifest.ThroughSlot {
		return nil, fmt.Errorf("fold manifest through slot %d carries context for slot %d", manifest.ThroughSlot, ctx.Slot)
	}
	return &ctx, nil
}

func validateManifestRef(manifest *accountsdb.SegmentManifest, ref *state.RootedEventBatchRef) error {
	if err := rootedevents.ValidateSidecarRef(ref); err != nil {
		return fmt.Errorf("fold manifest through slot %d has invalid rooted-event reference: %w", manifest.ThroughSlot, err)
	}
	expectedFrom := manifest.FromSlot + 1
	if manifest.ThroughSlot == 0 {
		expectedFrom = 0
	}
	if ref.FromSlot != expectedFrom || ref.ThroughSlot != manifest.ThroughSlot {
		return fmt.Errorf("fold manifest range (%d,%d] selects rooted-event range %d..%d",
			manifest.FromSlot, manifest.ThroughSlot, ref.FromSlot, ref.ThroughSlot)
	}
	return nil
}
