package snapshot

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/base58"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

const maxSnapshotManifestBytes int64 = 512 << 20

func UnmarshalManifestFromSnapshot(ctx context.Context, filename string, accountsDbDir string) (*SnapshotManifest, error) {
	if err := os.MkdirAll(accountsDbDir, 0775); err != nil {
		return nil, err
	}

	manifest, manifestBytes, err := readManifestFromSnapshot(ctx, filename)
	if err != nil {
		return nil, err
	}
	if err := installSnapshotManifest(accountsDbDir, manifestBytes); err != nil {
		return nil, err
	}
	return manifest, nil
}

func readManifestFromSnapshot(ctx context.Context, filename string) (*SnapshotManifest, []byte, error) {
	tarReader, closer, err := newSnapshotReader(ctx, filename)
	if err != nil {
		return nil, nil, err
	}
	defer closer.Close()

	for {
		header, err := tarReader.Next()
		if err != nil {
			if err == io.EOF {
				return nil, nil, fmt.Errorf("snapshot archive is missing its bank manifest")
			}
			return nil, nil, err
		}

		archiveSlot, isManifest, err := snapshotManifestArchiveSlot(header)
		if err != nil {
			return nil, nil, err
		}
		if !isManifest {
			continue
		}
		manifestBytes, err := readSnapshotManifestBytes(tarReader, header.Size)
		if err != nil {
			return nil, nil, err
		}
		manifest, err := decodeSnapshotManifest(manifestBytes, archiveSlot)
		if err != nil {
			return nil, nil, err
		}
		return manifest, manifestBytes, nil
	}
}

func readSnapshotManifestBytes(reader io.Reader, declaredSize int64) ([]byte, error) {
	if declaredSize < 0 {
		return nil, fmt.Errorf("snapshot manifest has negative size %d", declaredSize)
	}
	if declaredSize > maxSnapshotManifestBytes {
		return nil, fmt.Errorf("snapshot manifest size %d exceeds limit %d", declaredSize, maxSnapshotManifestBytes)
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxSnapshotManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read snapshot manifest: %w", err)
	}
	if int64(len(data)) != declaredSize {
		return nil, fmt.Errorf("snapshot manifest size %d does not match declared size %d", len(data), declaredSize)
	}
	return data, nil
}

func installSnapshotManifest(accountsDbDir string, manifestBytes []byte) error {
	if err := writeAtomicSnapshotArtifact(filepath.Join(accountsDbDir, "manifest"), manifestBytes, 0o644); err != nil {
		return fmt.Errorf("install validated snapshot manifest: %w", err)
	}
	return nil
}

func snapshotArchiveFilename(filename string) (string, error) {
	lower := strings.ToLower(filename)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		parsed, err := url.Parse(filename)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return "", fmt.Errorf("snapshot URL is invalid")
		}
		if parsed.Path == "" {
			return "", fmt.Errorf("snapshot URL has no archive path")
		}
		return path.Base(parsed.Path), nil
	}
	name := filepath.Base(filename)
	if name == "." || name == string(filepath.Separator) || name == "" {
		return "", fmt.Errorf("snapshot path has no archive filename")
	}
	return name, nil
}

func selectedFullSnapshotSlot(filename string) (uint64, error) {
	name, err := snapshotArchiveFilename(filename)
	if err != nil {
		return 0, err
	}
	const prefix = "snapshot-"
	body, ok := trimSnapshotCompressionSuffix(strings.TrimPrefix(name, prefix))
	if !strings.HasPrefix(name, prefix) || !ok {
		return 0, fmt.Errorf("full snapshot filename %q is not canonical", name)
	}
	slotText, _, ok := strings.Cut(body, "-")
	if !ok || slotText == "" {
		return 0, fmt.Errorf("full snapshot filename %q is missing its hash", name)
	}
	slot, err := strconv.ParseUint(slotText, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("full snapshot filename %q has invalid slot: %w", name, err)
	}
	return slot, nil
}

func selectedIncrementalSnapshotSlots(filename string) (uint64, uint64, error) {
	name, err := snapshotArchiveFilename(filename)
	if err != nil {
		return 0, 0, err
	}
	const prefix = "incremental-snapshot-"
	body, ok := trimSnapshotCompressionSuffix(strings.TrimPrefix(name, prefix))
	if !strings.HasPrefix(name, prefix) || !ok {
		return 0, 0, fmt.Errorf("incremental snapshot filename %q is not canonical", name)
	}
	baseText, rest, ok := strings.Cut(body, "-")
	if !ok {
		return 0, 0, fmt.Errorf("incremental snapshot filename %q is missing its end slot", name)
	}
	endText, hash, ok := strings.Cut(rest, "-")
	if !ok || hash == "" {
		return 0, 0, fmt.Errorf("incremental snapshot filename %q is missing its hash", name)
	}
	base, err := strconv.ParseUint(baseText, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("incremental snapshot filename %q has invalid base slot: %w", name, err)
	}
	end, err := strconv.ParseUint(endText, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("incremental snapshot filename %q has invalid end slot: %w", name, err)
	}
	if end <= base {
		return 0, 0, fmt.Errorf("incremental snapshot filename %q does not advance its base slot", name)
	}
	return base, end, nil
}

func validateSnapshotArchiveHash(filename string, manifest *SnapshotManifest) error {
	if manifest == nil || manifest.LtHash == nil {
		return nil
	}
	archiveHash, err := snapshotArchiveHash(filename)
	if err != nil {
		return err
	}
	if !bytes.Equal(archiveHash[:], manifest.LtHash.Checksum()) {
		return fmt.Errorf("snapshot filename hash does not match its manifest AccountsLtHash")
	}
	return nil
}

func snapshotArchiveHash(filename string) ([32]byte, error) {
	var archiveHash [32]byte
	name, err := snapshotArchiveFilename(filename)
	if err != nil {
		return archiveHash, err
	}
	body, ok := trimSnapshotCompressionSuffix(name)
	if !ok {
		return archiveHash, fmt.Errorf("snapshot filename %q is not canonical", name)
	}
	separator := strings.LastIndexByte(body, '-')
	if separator < 0 || separator == len(body)-1 {
		return archiveHash, fmt.Errorf("snapshot filename %q is missing its hash", name)
	}
	if !base58.Decode32(&archiveHash, []byte(body[separator+1:])) {
		return archiveHash, fmt.Errorf("snapshot filename %q has an invalid hash", name)
	}
	return archiveHash, nil
}

func trimSnapshotCompressionSuffix(name string) (string, bool) {
	for _, suffix := range []string{".tar.zst", ".tar.lz4"} {
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSuffix(name, suffix), true
		}
	}
	return "", false
}

func snapshotManifestArchiveSlot(header *tar.Header) (uint64, bool, error) {
	if header == nil {
		return 0, false, nil
	}
	name := path.Clean(strings.TrimPrefix(header.Name, "./"))
	parts := strings.Split(name, "/")
	if len(parts) != 3 || parts[0] != "snapshots" {
		return 0, false, nil
	}
	first, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("snapshot manifest path %q has invalid slot: %w", header.Name, err)
	}
	second, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("snapshot manifest path %q has invalid bank slot: %w", header.Name, err)
	}
	if first != second {
		return 0, false, fmt.Errorf("snapshot manifest path %q names different slots %d and %d", header.Name, first, second)
	}
	if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
		return 0, false, fmt.Errorf("snapshot manifest %q is not a regular file", header.Name)
	}
	return first, true, nil
}

func decodeSnapshotManifest(data []byte, archiveSlot uint64) (*SnapshotManifest, error) {
	manifest := new(SnapshotManifest)
	if err := manifest.UnmarshalWithDecoder(bin.NewBinDecoder(data)); err != nil {
		return nil, fmt.Errorf("decode snapshot manifest at slot %d: %w", archiveSlot, err)
	}
	if manifest.Bank == nil || manifest.AccountsDb == nil {
		return nil, fmt.Errorf("snapshot manifest at slot %d is missing required bank or accounts fields", archiveSlot)
	}
	if manifest.Bank.Slot != archiveSlot {
		return nil, fmt.Errorf("snapshot manifest bank slot %d does not match archive slot %d", manifest.Bank.Slot, archiveSlot)
	}
	return manifest, nil
}

func writeAtomicSnapshotArtifact(destination string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(destination)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create artifact directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".snapshot-artifact-*.partial")
	if err != nil {
		return fmt.Errorf("create temporary artifact: %w", err)
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary artifact: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temporary artifact: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary artifact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary artifact: %w", err)
	}
	if err := os.Rename(tmpPath, destination); err != nil {
		return fmt.Errorf("install artifact: %w", err)
	}
	keep = true
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open artifact directory: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync artifact directory: %w", err)
	}
	return nil
}

type appendVecCopyingTask struct {
	Filename                string
	TarBuffer               *bytes.Buffer
	FromIncrementalSnapshot bool
}

type indexEntryBuilderTask struct {
	Data     []byte
	FileSize uint64
	Slot     uint64
	FileId   uint64
}

type indexEntryCommitterTask struct {
	IndexEntries []accountsdb.AccountIndexEntry
	Pubkeys      []solana.PublicKey
}

const (
	snapshotTypeZst = iota
	snapshotTypeLz4
)

var (
	ZstdDecoderConcurrency = runtime.NumCPU()
)

func readerForCompressionType(snapshotType int, bmr *bufmonreader) (io.Reader, io.Closer, error) {
	if snapshotType == snapshotTypeZst {
		zstdReader, err := zstd.NewReader(bmr, zstd.WithDecoderConcurrency(ZstdDecoderConcurrency))
		if err != nil {
			return nil, nil, err
		}
		readCloser := zstdReader.IOReadCloser()
		return readCloser, readCloser, nil
	}
	if snapshotType == snapshotTypeLz4 {
		return lz4.NewReader(bmr), nil, nil
	}
	return nil, nil, fmt.Errorf("unknown snapshot type %d", snapshotType)
}

func parseSnapshotType(snapshotFileName string) (int, error) {
	name, err := snapshotArchiveFilename(snapshotFileName)
	if err != nil {
		return 0, err
	}
	fileExt := filepath.Ext(name)

	if fileExt == ".zst" {
		return snapshotTypeZst, nil
	} else if fileExt == ".lz4" {
		return snapshotTypeLz4, nil
	}
	return 0, fmt.Errorf("unknown snapshot compression type %q", fileExt)
}

func newSnapshotReader(ctx context.Context, filename string) (*tar.Reader, io.Closer, error) {
	return newSnapshotReaderWithSave(ctx, filename, "")
}

// newSnapshotReaderWithSave creates a tar reader for a snapshot file or HTTP URL.
// If filename is an HTTP URL and savePath is non-empty, the data will be saved
// to disk while streaming (using io.TeeReader for parallel download+processing+saving).
func newSnapshotReaderWithSave(ctx context.Context, filename string, savePath string) (*tar.Reader, io.Closer, error) {
	tarReader, bmr, closer, err := newSnapshotReaderWithProgress(ctx, filename, savePath)
	if err != nil {
		return nil, nil, err
	}
	// Return closer, bmr is not exposed in this version
	_ = bmr
	return tarReader, closer, nil
}

// newSnapshotReaderWithProgress creates a tar reader and also returns the bufmonreader
// for progress tracking. Use bufmonreader.SetProgressCallback() to receive progress updates.
func newSnapshotReaderWithProgress(ctx context.Context, filename string, savePath string) (*tar.Reader, *bufmonreader, io.Closer, error) {
	snapshotType, err := parseSnapshotType(filename)
	if err != nil {
		return nil, nil, nil, err
	}
	var bmr *bufmonreader
	if strings.HasPrefix(filename, "https://") || strings.HasPrefix(filename, "http://") {
		bmr, err = NewBufMonReaderHTTPWithSave(ctx, filename, savePath)
	} else {
		snapshotFile, err := os.Open(filename)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("open %s: %w", filename, err)
		}
		bmr, err = NewBufMonReaderFromFile(snapshotFile)
	}
	if err != nil {
		return nil, nil, nil, err
	}
	reader, compressionCloser, err := readerForCompressionType(snapshotType, bmr)
	if err != nil {
		_ = bmr.Close()
		return nil, nil, nil, fmt.Errorf("opening compression reader: %v", err)
	}

	closer := &snapshotStreamCloser{
		reader:            reader,
		compressionCloser: compressionCloser,
		monitor:           bmr,
	}
	return tar.NewReader(reader), bmr, closer, nil
}

type snapshotStreamCloser struct {
	reader            io.Reader
	compressionCloser io.Closer
	monitor           *bufmonreader
	finished          bool
	closed            bool
}

func (s *snapshotStreamCloser) Finish() error {
	if s == nil || s.finished {
		return nil
	}
	if _, err := io.Copy(io.Discard, s.reader); err != nil {
		return fmt.Errorf("finish snapshot stream: %w", err)
	}
	if s.monitor != nil {
		if err := s.monitor.validateComplete(); err != nil {
			return err
		}
	}
	s.finished = true
	return nil
}

func (s *snapshotStreamCloser) Close() error {
	if s == nil || s.closed {
		return nil
	}
	s.closed = true
	var errs []error
	if s.compressionCloser != nil {
		errs = append(errs, s.compressionCloser.Close())
	}
	if s.monitor != nil {
		errs = append(errs, s.monitor.Close())
	}
	return errors.Join(errs...)
}

func LoadManifestFromFile(filename string) (*SnapshotManifest, error) {
	manifestBytes, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("reading manifest from file=%s: %w", filename, err)
	}

	manifest := new(SnapshotManifest)
	decoder := bin.NewBinDecoder(manifestBytes)
	err = manifest.UnmarshalWithDecoder(decoder)
	if err != nil {
		return nil, err
	}

	return manifest, nil
}
