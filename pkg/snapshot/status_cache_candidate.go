package snapshot

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/Overclock-Validator/mithril/pkg/txstatus"
)

const (
	agaveStatusCacheArchiveMember = "snapshots/status_cache"
	// This bounds both retained input and the later in-memory replay import.
	maxAgaveStatusCacheSize = int64(512 * 1024 * 1024)
	// Agave v4.2 retains at most MAX_RECENT_BLOCKHASHES rooted deltas.
	maxAgaveStatusCacheSlotDeltas = uint64(300)
)

func retainedStatusCachePath(accountsDbDir string) string {
	return filepath.Join(accountsDbDir, txstatus.SnapshotSeedFileName)
}

// statusCacheCandidate retains an extracted member under a temporary name
// until the complete archive has been consumed. A truncated incremental can
// therefore never replace the valid seed retained from its full snapshot.
type statusCacheCandidate struct {
	destination string
	temporary   string
	found       bool
}

func newStatusCacheCandidate(destination string) *statusCacheCandidate {
	return &statusCacheCandidate{destination: destination}
}

func (c *statusCacheCandidate) capture(ctx context.Context, header *tar.Header, src io.Reader) (handled bool, written int64, err error) {
	if c == nil || c.destination == "" || header == nil {
		return false, 0, nil
	}
	name := path.Clean(strings.TrimPrefix(header.Name, "./"))
	if name != agaveStatusCacheArchiveMember {
		return false, 0, nil
	}
	if c.found {
		return true, 0, fmt.Errorf("snapshot archive contains duplicate %s members", agaveStatusCacheArchiveMember)
	}
	if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
		return true, 0, fmt.Errorf("snapshot %s member is not a regular file", agaveStatusCacheArchiveMember)
	}
	if header.Size < 8 {
		return true, 0, fmt.Errorf("snapshot %s member is too small: %d bytes", agaveStatusCacheArchiveMember, header.Size)
	}
	if header.Size > maxAgaveStatusCacheSize {
		return true, 0, fmt.Errorf("snapshot %s member is too large: %d bytes (max %d)", agaveStatusCacheArchiveMember, header.Size, maxAgaveStatusCacheSize)
	}

	dir := filepath.Dir(c.destination)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return true, 0, fmt.Errorf("create status-cache destination directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".snapshot-status-cache-*.partial")
	if err != nil {
		return true, 0, fmt.Errorf("create temporary status cache: %w", err)
	}
	tmpPath := tmp.Name()
	keep := false
	closed := false
	defer func() {
		if !closed {
			if closeErr := tmp.Close(); err == nil && closeErr != nil {
				err = fmt.Errorf("close temporary status cache: %w", closeErr)
			}
		}
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()

	written, err = copyExactlyWithContext(ctx, tmp, src, header.Size)
	if err != nil {
		return true, written, fmt.Errorf("extract snapshot status cache: %w", err)
	}
	if written != header.Size {
		return true, written, fmt.Errorf("extract snapshot status cache: copied %d bytes, expected %d", written, header.Size)
	}
	if err = tmp.Chmod(0o644); err != nil {
		return true, written, fmt.Errorf("chmod temporary status cache: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return true, written, fmt.Errorf("sync temporary status cache: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return true, written, fmt.Errorf("close temporary status cache: %w", err)
	}
	closed = true
	c.temporary = tmpPath
	c.found = true
	keep = true
	return true, written, nil
}

func (c *statusCacheCandidate) commit(ctx context.Context, expectedRoot *uint64) error {
	if c == nil || c.destination == "" {
		return nil
	}
	if expectedRoot == nil {
		return fmt.Errorf("snapshot manifest root is required to install %s", agaveStatusCacheArchiveMember)
	}
	if !c.found || c.temporary == "" {
		return fmt.Errorf("snapshot archive is missing required %s member", agaveStatusCacheArchiveMember)
	}
	if err := c.validateRoot(ctx, *expectedRoot); err != nil {
		return err
	}
	if err := os.Rename(c.temporary, c.destination); err != nil {
		return fmt.Errorf("install snapshot status cache: %w", err)
	}
	c.temporary = ""
	dir, err := os.Open(filepath.Dir(c.destination))
	if err != nil {
		return fmt.Errorf("open status-cache destination directory: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync status-cache destination directory: %w", err)
	}
	return nil
}

func (c *statusCacheCandidate) validateRoot(ctx context.Context, expectedRoot uint64) error {
	if c == nil || !c.found || c.temporary == "" {
		return fmt.Errorf("snapshot archive is missing required %s member", agaveStatusCacheArchiveMember)
	}
	file, err := os.Open(c.temporary)
	if err != nil {
		return fmt.Errorf("open temporary snapshot status cache: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat temporary snapshot status cache: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("temporary snapshot status cache is not a regular file")
	}
	if info.Size() < 8 || info.Size() > maxAgaveStatusCacheSize {
		return fmt.Errorf("temporary snapshot status cache has invalid size %d", info.Size())
	}
	latestRoot, err := validateStatusCacheStream(ctx, file, info.Size(), expectedRoot)
	if err != nil {
		return fmt.Errorf("decode snapshot status cache: %w", err)
	}
	if latestRoot != expectedRoot {
		return fmt.Errorf("snapshot status cache latest root %d does not match manifest root %d", latestRoot, expectedRoot)
	}
	return nil
}

// validateStatusCacheStream validates the Agave v4.2 snapshot wire format and
// its root-local invariants without loading the retained cache into memory.
// Proving that no rooted slot is missing also requires the bank's SlotHistory.
func validateStatusCacheStream(ctx context.Context, source io.Reader, size int64, expectedRoot uint64) (uint64, error) {
	r := &statusCacheStream{
		ctx:       ctx,
		reader:    bufio.NewReaderSize(source, 64*1024),
		remaining: size,
	}
	slotCount, err := r.readCount("slot deltas", 17)
	if err != nil {
		return 0, err
	}
	if slotCount > maxAgaveStatusCacheSlotDeltas {
		return 0, fmt.Errorf("slot delta count %d exceeds maximum %d", slotCount, maxAgaveStatusCacheSlotDeltas)
	}

	seenSlots := make(map[uint64]struct{}, slotCount)
	var latestRoot uint64
	for slotIndex := uint64(0); slotIndex < slotCount; slotIndex++ {
		slot, err := r.readU64()
		if err != nil {
			return 0, fmt.Errorf("slot delta %d: read slot: %w", slotIndex, err)
		}
		if slot > expectedRoot {
			return 0, fmt.Errorf("slot delta %d: slot %d exceeds snapshot root %d", slotIndex, slot, expectedRoot)
		}
		if _, duplicate := seenSlots[slot]; duplicate {
			return 0, fmt.Errorf("slot delta %d: repeated slot %d", slotIndex, slot)
		}
		seenSlots[slot] = struct{}{}
		rootFlag, err := r.readU8()
		if err != nil {
			return 0, fmt.Errorf("slot delta %d: read root flag: %w", slotIndex, err)
		}
		if rootFlag != 1 {
			return 0, fmt.Errorf("slot delta %d: slot %d is not rooted (flag %d)", slotIndex, slot, rootFlag)
		}
		statusCount, err := r.readCount(fmt.Sprintf("slot delta %d statuses", slotIndex), 48)
		if err != nil {
			return 0, err
		}
		for statusIndex := uint64(0); statusIndex < statusCount; statusIndex++ {
			if err := r.skip(32); err != nil {
				return 0, fmt.Errorf("slot delta %d status %d: read blockhash: %w", slotIndex, statusIndex, err)
			}
			keyIndex, err := r.readU64()
			if err != nil {
				return 0, fmt.Errorf("slot delta %d status %d: read key index: %w", slotIndex, statusIndex, err)
			}
			if keyIndex > txstatus.MaxCachedKeyIndex {
				return 0, fmt.Errorf("slot delta %d status %d: key index %d exceeds maximum %d", slotIndex, statusIndex, keyIndex, txstatus.MaxCachedKeyIndex)
			}
			keyCount, err := r.readCount(fmt.Sprintf("slot delta %d status %d keys", slotIndex, statusIndex), txstatus.CachedKeySize+4)
			if err != nil {
				return 0, err
			}
			for keyIndex := uint64(0); keyIndex < keyCount; keyIndex++ {
				if err := r.skip(txstatus.CachedKeySize); err != nil {
					return 0, fmt.Errorf("slot delta %d status %d key %d: read key: %w", slotIndex, statusIndex, keyIndex, err)
				}
				if err := r.skipTransactionResult(); err != nil {
					return 0, fmt.Errorf("slot delta %d status %d key %d: %w", slotIndex, statusIndex, keyIndex, err)
				}
			}
		}
		if len(seenSlots) == 1 || slot > latestRoot {
			latestRoot = slot
		}
	}
	if r.remaining != 0 {
		return 0, fmt.Errorf("%d trailing bytes", r.remaining)
	}
	if len(seenSlots) == 0 {
		return 0, fmt.Errorf("contains no rooted bank deltas")
	}
	return latestRoot, nil
}

type statusCacheStream struct {
	ctx       context.Context
	reader    *bufio.Reader
	remaining int64
}

func (r *statusCacheStream) readU8() (uint8, error) {
	var data [1]byte
	if err := r.readFull(data[:]); err != nil {
		return 0, err
	}
	return data[0], nil
}

func (r *statusCacheStream) readU32() (uint32, error) {
	var data [4]byte
	if err := r.readFull(data[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(data[:]), nil
}

func (r *statusCacheStream) readU64() (uint64, error) {
	var data [8]byte
	if err := r.readFull(data[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(data[:]), nil
}

func (r *statusCacheStream) readFull(data []byte) error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	if int64(len(data)) > r.remaining {
		return io.ErrUnexpectedEOF
	}
	if _, err := io.ReadFull(r.reader, data); err != nil {
		return err
	}
	r.remaining -= int64(len(data))
	return nil
}

func (r *statusCacheStream) readCount(name string, minimumElementSize int) (uint64, error) {
	count, err := r.readU64()
	if err != nil {
		return 0, fmt.Errorf("read %s length: %w", name, err)
	}
	if minimumElementSize > 0 && count > uint64(r.remaining)/uint64(minimumElementSize) {
		return 0, fmt.Errorf("%s length %d cannot fit in %d remaining bytes", name, count, r.remaining)
	}
	return count, nil
}

func (r *statusCacheStream) skip(count int) error {
	var scratch [32 * 1024]byte
	remaining := count
	for remaining > 0 {
		chunk := min(remaining, len(scratch))
		if err := r.readFull(scratch[:chunk]); err != nil {
			return err
		}
		remaining -= chunk
	}
	return nil
}

func (r *statusCacheStream) skipTransactionResult() error {
	resultTag, err := r.readU32()
	if err != nil {
		return fmt.Errorf("read transaction result tag: %w", err)
	}
	switch resultTag {
	case 0:
		return nil
	case 1:
		return r.skipTransactionError()
	default:
		return fmt.Errorf("unknown transaction result tag %d", resultTag)
	}
}

func (r *statusCacheStream) skipTransactionError() error {
	tag, err := r.readU32()
	if err != nil {
		return fmt.Errorf("read transaction error tag: %w", err)
	}
	switch tag {
	case 8:
		if _, err := r.readU8(); err != nil {
			return fmt.Errorf("read instruction index: %w", err)
		}
		return r.skipInstructionError()
	case 30, 31, 35:
		if _, err := r.readU8(); err != nil {
			return fmt.Errorf("read transaction error %d payload: %w", tag, err)
		}
		return nil
	default:
		if tag <= 38 {
			return nil
		}
		return fmt.Errorf("unknown transaction error tag %d", tag)
	}
}

func (r *statusCacheStream) skipInstructionError() error {
	tag, err := r.readU32()
	if err != nil {
		return fmt.Errorf("read instruction error tag: %w", err)
	}
	switch tag {
	case 25:
		if _, err := r.readU32(); err != nil {
			return fmt.Errorf("read custom instruction error: %w", err)
		}
		return nil
	case 44:
		length, err := r.readCount("BorshIoError string", 1)
		if err != nil {
			return err
		}
		return r.readUTF8(length)
	default:
		if tag <= 53 {
			return nil
		}
		return fmt.Errorf("unknown instruction error tag %d", tag)
	}
}

func (r *statusCacheStream) readUTF8(length uint64) error {
	if length > uint64(r.remaining) {
		return io.ErrUnexpectedEOF
	}
	consumed := uint64(0)
	for consumed < length {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		value, size, err := r.reader.ReadRune()
		if err != nil {
			return err
		}
		if value == utf8.RuneError && size == 1 {
			return fmt.Errorf("invalid UTF-8")
		}
		if consumed+uint64(size) > length {
			return fmt.Errorf("UTF-8 sequence crosses string boundary")
		}
		consumed += uint64(size)
		r.remaining -= int64(size)
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(data)
}

func copyExactlyWithContext(ctx context.Context, destination io.Writer, source io.Reader, size int64) (int64, error) {
	if size < 0 {
		return 0, fmt.Errorf("negative copy size %d", size)
	}
	written, err := io.Copy(destination, contextReader{ctx: ctx, reader: io.LimitReader(source, size)})
	if err != nil {
		return written, err
	}
	if written != size {
		return written, io.ErrUnexpectedEOF
	}
	return written, nil
}

func (c *statusCacheCandidate) cleanup() {
	if c == nil || c.temporary == "" {
		return
	}
	_ = os.Remove(c.temporary)
	c.temporary = ""
}
