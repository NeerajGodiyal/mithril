package rewards

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/gagliardetto/solana-go"
)

// SpoolRecordSize is the binary size of a spool record.
// Format: stake_pubkey(32) + vote_pubkey(32) + stake_lamports(8) +
//
//	credits_observed(8) + reward_lamports(8) = 88 bytes
//
// Note: partition_index is no longer stored in records since we use per-partition files.
const SpoolRecordSize = 88

// SpoolRecord represents a single stake reward record.
type SpoolRecord struct {
	StakePubkey     solana.PublicKey
	VotePubkey      solana.PublicKey
	StakeLamports   uint64
	CreditsObserved uint64
	RewardLamports  uint64
	PartitionIndex  uint32 // Only used during calculation, not serialized
}

// encodeRecord encodes a record into the buffer (without partition index).
func encodeRecord(rec *SpoolRecord, buf []byte) {
	copy(buf[0:32], rec.StakePubkey[:])
	copy(buf[32:64], rec.VotePubkey[:])
	binary.LittleEndian.PutUint64(buf[64:72], rec.StakeLamports)
	binary.LittleEndian.PutUint64(buf[72:80], rec.CreditsObserved)
	binary.LittleEndian.PutUint64(buf[80:88], rec.RewardLamports)
}

// decodeRecord decodes a record from the buffer.
func decodeRecord(buf []byte, rec *SpoolRecord) {
	copy(rec.StakePubkey[:], buf[0:32])
	copy(rec.VotePubkey[:], buf[32:64])
	rec.StakeLamports = binary.LittleEndian.Uint64(buf[64:72])
	rec.CreditsObserved = binary.LittleEndian.Uint64(buf[72:80])
	rec.RewardLamports = binary.LittleEndian.Uint64(buf[80:88])
}

// PartitionedSpoolWriters manages per-partition spool files.
// Thread-safe - multiple goroutines can write concurrently.
type PartitionedSpoolWriters struct {
	baseDir       string
	slot          uint64
	numPartitions uint64
	writers       map[uint32]*partitionWriter
	mu            sync.Mutex
	closed        bool
}

// partitionWriter is a writer for a single partition file.
type partitionWriter struct {
	file  *os.File
	count int
}

// NewPartitionedSpoolWriters creates a new set of per-partition spool writers.
func NewPartitionedSpoolWriters(baseDir string, slot uint64, numPartitions uint64) *PartitionedSpoolWriters {
	return &PartitionedSpoolWriters{
		baseDir:       baseDir,
		slot:          slot,
		numPartitions: numPartitions,
		writers:       make(map[uint32]*partitionWriter),
	}
}

// SpoolDir returns the base directory for spool files.
func (p *PartitionedSpoolWriters) SpoolDir() string {
	return p.baseDir
}

// Slot returns the slot this spool is for.
func (p *PartitionedSpoolWriters) Slot() uint64 {
	return p.slot
}

// WriteRecord writes a record to the appropriate partition file.
// Thread-safe - lazily opens partition files as needed.
func (p *PartitionedSpoolWriters) WriteRecord(rec *SpoolRecord) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return fmt.Errorf("spool writers are closed")
	}

	partition := rec.PartitionIndex

	// Get or create writer for this partition
	w, exists := p.writers[partition]
	if !exists {
		path := partitionFilePath(p.baseDir, p.slot, partition)
		f, err := os.Create(path)
		if err != nil {
			return fmt.Errorf("creating partition %d spool file: %w", partition, err)
		}
		w = &partitionWriter{file: f}
		p.writers[partition] = w
	}

	// Write record
	var buf [SpoolRecordSize]byte
	encodeRecord(rec, buf[:])
	if _, err := w.file.Write(buf[:]); err != nil {
		return fmt.Errorf("writing to partition %d: %w", partition, err)
	}
	w.count++
	return nil
}

// Close closes all partition files and syncs to disk.
// Returns the first error encountered.
func (p *PartitionedSpoolWriters) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil
	}
	p.closed = true

	var firstErr error
	for partition, w := range p.writers {
		if err := w.file.Sync(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("syncing partition %d: %w", partition, err)
		}
		if err := w.file.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("closing partition %d: %w", partition, err)
		}
	}
	return firstErr
}

// TotalRecords returns the total number of records written across all partitions.
func (p *PartitionedSpoolWriters) TotalRecords() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	total := 0
	for _, w := range p.writers {
		total += w.count
	}
	return total
}

// PartitionReader reads records sequentially from a partition spool file.
type PartitionReader struct {
	file *os.File
	buf  [SpoolRecordSize]byte
}

// NewPartitionReader opens a partition file for sequential reading.
func NewPartitionReader(baseDir string, slot uint64, partition uint32) (*PartitionReader, error) {
	path := partitionFilePath(baseDir, slot, partition)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// No records for this partition - return empty reader
			return nil, nil
		}
		return nil, fmt.Errorf("opening partition %d spool: %w", partition, err)
	}
	return &PartitionReader{file: f}, nil
}

// Next reads the next record. Returns io.EOF when done.
func (r *PartitionReader) Next() (*SpoolRecord, error) {
	_, err := io.ReadFull(r.file, r.buf[:])
	if err == io.EOF {
		return nil, io.EOF
	}
	if err != nil {
		return nil, fmt.Errorf("reading spool record: %w", err)
	}

	rec := &SpoolRecord{}
	decodeRecord(r.buf[:], rec)
	return rec, nil
}

// ReadAll reads all records from the partition file.
func (r *PartitionReader) ReadAll() ([]SpoolRecord, error) {
	var records []SpoolRecord
	for {
		rec, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		records = append(records, *rec)
	}
	return records, nil
}

// Close closes the partition file.
func (r *PartitionReader) Close() error {
	return r.file.Close()
}

// partitionFilePath returns the path for a partition spool file.
func partitionFilePath(baseDir string, slot uint64, partition uint32) string {
	return filepath.Join(baseDir, fmt.Sprintf("reward_spool_%d_p%d.bin", slot, partition))
}

// CleanupPartitionedSpoolFiles removes all partition spool files for a slot.
func CleanupPartitionedSpoolFiles(baseDir string, slot uint64, numPartitions uint64) {
	for p := uint64(0); p < numPartitions; p++ {
		path := partitionFilePath(baseDir, slot, uint32(p))
		os.Remove(path) // Ignore errors - file may not exist
	}
}

// ===========================================================================
// Legacy single-file spool (kept for compatibility, may be removed later)
// ===========================================================================

// LegacySpoolRecordSize includes the partition index (old format).
const LegacySpoolRecordSize = 92

// SpoolWriter writes stake reward records to a spool file.
// Thread-safe - multiple goroutines can write concurrently.
// DEPRECATED: Use PartitionedSpoolWriters instead.
type SpoolWriter struct {
	file   *os.File
	mu     sync.Mutex
	count  int
	closed bool
}

// NewSpoolWriter creates a new spool file for writing.
func NewSpoolWriter(path string) (*SpoolWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("creating spool file: %w", err)
	}
	return &SpoolWriter{file: f}, nil
}

// WriteRecord writes a single record to the spool file.
// Thread-safe.
func (w *SpoolWriter) WriteRecord(rec *SpoolRecord) error {
	var buf [LegacySpoolRecordSize]byte

	// Pack record into buffer (legacy format with partition index)
	copy(buf[0:32], rec.StakePubkey[:])
	copy(buf[32:64], rec.VotePubkey[:])
	binary.LittleEndian.PutUint64(buf[64:72], rec.StakeLamports)
	binary.LittleEndian.PutUint64(buf[72:80], rec.CreditsObserved)
	binary.LittleEndian.PutUint64(buf[80:88], rec.RewardLamports)
	binary.LittleEndian.PutUint32(buf[88:92], rec.PartitionIndex)

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return fmt.Errorf("spool writer is closed")
	}

	_, err := w.file.Write(buf[:])
	if err != nil {
		return fmt.Errorf("writing spool record: %w", err)
	}
	w.count++
	return nil
}

// Count returns the number of records written.
func (w *SpoolWriter) Count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.count
}

// Close closes the spool file and syncs to disk.
func (w *SpoolWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return nil
	}
	w.closed = true

	if err := w.file.Sync(); err != nil {
		w.file.Close()
		return fmt.Errorf("syncing spool file: %w", err)
	}
	return w.file.Close()
}

// SpoolReader reads stake reward records from a spool file.
// Builds an in-memory index at open time for efficient partition reads.
// DEPRECATED: Use PartitionReader instead.
type SpoolReader struct {
	file           *os.File
	fileSize       int64
	partitionIndex map[uint32][]int64 // partition -> list of file offsets
}

// NewSpoolReader opens a spool file and builds the partition index.
func NewSpoolReader(path string) (*SpoolReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening spool file: %w", err)
	}

	stat, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("stat spool file: %w", err)
	}

	fileSize := stat.Size()
	if fileSize%LegacySpoolRecordSize != 0 {
		f.Close()
		return nil, fmt.Errorf("spool file size %d is not a multiple of record size %d", fileSize, LegacySpoolRecordSize)
	}

	// Build partition index by scanning file once
	partitionIndex := make(map[uint32][]int64)
	numRecords := fileSize / LegacySpoolRecordSize
	var buf [LegacySpoolRecordSize]byte

	for i := int64(0); i < numRecords; i++ {
		offset := i * LegacySpoolRecordSize
		_, err := f.ReadAt(buf[:], offset)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("reading record at offset %d: %w", offset, err)
		}

		partitionIdx := binary.LittleEndian.Uint32(buf[88:92])
		partitionIndex[partitionIdx] = append(partitionIndex[partitionIdx], offset)
	}

	return &SpoolReader{
		file:           f,
		fileSize:       fileSize,
		partitionIndex: partitionIndex,
	}, nil
}

// ReadPartition reads all records for a given partition index.
func (r *SpoolReader) ReadPartition(partitionIndex uint32) ([]SpoolRecord, error) {
	offsets, ok := r.partitionIndex[partitionIndex]
	if !ok {
		return nil, nil // No records for this partition
	}

	records := make([]SpoolRecord, 0, len(offsets))
	var buf [LegacySpoolRecordSize]byte

	for _, offset := range offsets {
		_, err := r.file.ReadAt(buf[:], offset)
		if err != nil {
			return nil, fmt.Errorf("reading record at offset %d: %w", offset, err)
		}

		rec := SpoolRecord{
			StakeLamports:   binary.LittleEndian.Uint64(buf[64:72]),
			CreditsObserved: binary.LittleEndian.Uint64(buf[72:80]),
			RewardLamports:  binary.LittleEndian.Uint64(buf[80:88]),
			PartitionIndex:  binary.LittleEndian.Uint32(buf[88:92]),
		}
		copy(rec.StakePubkey[:], buf[0:32])
		copy(rec.VotePubkey[:], buf[32:64])

		records = append(records, rec)
	}

	return records, nil
}

// NumPartitions returns the number of distinct partitions in the spool file.
func (r *SpoolReader) NumPartitions() int {
	return len(r.partitionIndex)
}

// RecordCount returns the total number of records in the spool file.
func (r *SpoolReader) RecordCount() int64 {
	return r.fileSize / LegacySpoolRecordSize
}

// Close closes the spool file.
func (r *SpoolReader) Close() error {
	return r.file.Close()
}

// CleanupSpoolFile removes the spool file after distribution is complete.
// DEPRECATED: Use CleanupPartitionedSpoolFiles instead.
func CleanupSpoolFile(path string) {
	if path != "" {
		os.Remove(path)
	}
}
