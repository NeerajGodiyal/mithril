package rewards

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/gagliardetto/solana-go"
)

// SpoolRecord represents a single stake reward record in the spool file.
// Binary format: stake_pubkey(32) + vote_pubkey(32) + stake_lamports(8) +
//
//	credits_observed(8) + reward_lamports(8) + partition_index(4) = 92 bytes
const SpoolRecordSize = 92

type SpoolRecord struct {
	StakePubkey     solana.PublicKey
	VotePubkey      solana.PublicKey
	StakeLamports   uint64
	CreditsObserved uint64
	RewardLamports  uint64
	PartitionIndex  uint32
}

// SpoolWriter writes stake reward records to a spool file.
// Thread-safe - multiple goroutines can write concurrently.
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
	var buf [SpoolRecordSize]byte

	// Pack record into buffer
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
	if fileSize%SpoolRecordSize != 0 {
		f.Close()
		return nil, fmt.Errorf("spool file size %d is not a multiple of record size %d", fileSize, SpoolRecordSize)
	}

	// Build partition index by scanning file once
	partitionIndex := make(map[uint32][]int64)
	numRecords := fileSize / SpoolRecordSize
	var buf [SpoolRecordSize]byte

	for i := int64(0); i < numRecords; i++ {
		offset := i * SpoolRecordSize
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
	var buf [SpoolRecordSize]byte

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

// ReadAll reads all records from the spool file.
func (r *SpoolReader) ReadAll() ([]SpoolRecord, error) {
	numRecords := r.fileSize / SpoolRecordSize
	records := make([]SpoolRecord, 0, numRecords)
	var buf [SpoolRecordSize]byte

	for offset := int64(0); offset < r.fileSize; offset += SpoolRecordSize {
		_, err := r.file.ReadAt(buf[:], offset)
		if err != nil {
			if err == io.EOF {
				break
			}
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
	return r.fileSize / SpoolRecordSize
}

// Close closes the spool file.
func (r *SpoolReader) Close() error {
	return r.file.Close()
}

// CleanupSpoolFile removes the spool file after distribution is complete.
func CleanupSpoolFile(path string) {
	if path != "" {
		os.Remove(path)
	}
}
