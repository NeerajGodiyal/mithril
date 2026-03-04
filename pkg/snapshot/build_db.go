package snapshot

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/progress"
	"github.com/Overclock-Validator/mithril/pkg/statsd"
	"github.com/cockroachdb/pebble"
	"github.com/panjf2000/ants/v2"
)

const (
	maxIndexEntryCommitter = 512
	maxIndexEntryBuilder   = 500
)

var (
	// SnapshotBufSize is the size of each pooled buffer used during snapshot
	// unpacking. Tar entries are packed into these buffers; when the next entry
	// won't fit, the buffer is flushed (written to disk + dispatched to index
	// builders) and a fresh buffer is taken from the pool. Larger values mean
	// fewer flushes but more memory.
	SnapshotBufSize = 128 * 1024 * 1024 // 128 MB

	// snapshotBufCount is the number of pooled buffers. This controls how many
	// buffers can be in-flight (held by index builders) before readTar blocks.
	snapshotBufCount = 4
)

// CleanAccountsDbDir removes all artifacts from a previous incomplete snapshot run.
// This prevents corruption from Ctrl+C or partial downloads.
// Exported so it can be called early in startup before any failures.
func CleanAccountsDbDir(accountsDbDir string) {
	// List of all files/directories that may be left from a previous incomplete run
	artifacts := []string{
		"accounts",
		"mithril_db",
		"mithril_db_log_shards",
		"bankhash_db",
		"largest_file_id",
		"manifest",
		"mithril_state.json", // State file for tracking valid builds and replay progress
	}
	for _, artifact := range artifacts {
		path := filepath.Join(accountsDbDir, artifact)
		if err := os.RemoveAll(path); err != nil {
			mlog.Log.Errorf("failed to remove %s: %v", path, err)
		}
	}
}

// CleanSnapshotDownloadDir removes old snapshot files based on retention settings.
// maxSnapshots controls how many snapshots to keep:
//   - 0 = delete all snapshots (stream-only mode, used by new-snapshot bootstrap)
//   - N > 0 = keep N newest snapshots, delete the rest
func CleanSnapshotDownloadDir(downloadPath string, maxSnapshots int) {
	if downloadPath == "" || maxSnapshots < 0 {
		return
	}
	entries, err := os.ReadDir(downloadPath)
	if err != nil {
		return // Directory may not exist yet
	}

	// Always clean up .partial files first (incomplete downloads from crashes)
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), PartialSuffix) {
			path := filepath.Join(downloadPath, entry.Name())
			mlog.Log.Infof("Cleaning up incomplete download from previous run: %s", entry.Name())
			if err := os.Remove(path); err != nil {
				mlog.Log.Errorf("Failed to remove partial download %s: %v", entry.Name(), err)
			}
		}
	}

	// Collect snapshot files with their info
	type snapshotFile struct {
		name    string
		path    string
		modTime time.Time
	}
	var fullSnapshots []snapshotFile
	var incrSnapshots []snapshotFile

	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "snapshot-") && strings.HasSuffix(name, ".tar.zst") {
			path := filepath.Join(downloadPath, name)
			info, err := entry.Info()
			if err != nil {
				continue
			}
			fullSnapshots = append(fullSnapshots, snapshotFile{name, path, info.ModTime()})
		}
		if strings.HasPrefix(name, "incremental-snapshot-") && strings.HasSuffix(name, ".tar.zst") {
			path := filepath.Join(downloadPath, name)
			info, err := entry.Info()
			if err != nil {
				continue
			}
			incrSnapshots = append(incrSnapshots, snapshotFile{name, path, info.ModTime()})
		}
	}

	// Sort by modification time (newest first)
	sortByTime := func(files []snapshotFile) {
		for i := 0; i < len(files)-1; i++ {
			for j := i + 1; j < len(files); j++ {
				if files[j].modTime.After(files[i].modTime) {
					files[i], files[j] = files[j], files[i]
				}
			}
		}
	}

	// maxSnapshots = 0 means delete ALL (stream-only mode, used by new-snapshot bootstrap)
	if maxSnapshots == 0 {
		for _, snap := range fullSnapshots {
			if err := os.Remove(snap.path); err != nil {
				mlog.Log.Errorf("failed to remove snapshot %s: %v", snap.name, err)
			} else {
				mlog.Log.Infof("Cleaned up snapshot file: %s", snap.name)
			}
		}
		for _, snap := range incrSnapshots {
			if err := os.Remove(snap.path); err != nil {
				mlog.Log.Errorf("failed to remove incremental snapshot %s: %v", snap.name, err)
			} else {
				mlog.Log.Infof("Cleaned up incremental snapshot file: %s", snap.name)
			}
		}
		return
	}

	// maxSnapshots > 0: keep N newest, delete the rest
	if len(fullSnapshots) > maxSnapshots {
		sortByTime(fullSnapshots)
		for i := maxSnapshots; i < len(fullSnapshots); i++ {
			if err := os.Remove(fullSnapshots[i].path); err != nil {
				mlog.Log.Errorf("failed to remove old snapshot %s: %v", fullSnapshots[i].name, err)
			} else {
				mlog.Log.Infof("Cleaned up old snapshot file (retention limit %d): %s", maxSnapshots, fullSnapshots[i].name)
			}
		}
	}

	// Clean incremental snapshots beyond the retention limit (same limit as full)
	if len(incrSnapshots) > maxSnapshots {
		sortByTime(incrSnapshots)
		for i := maxSnapshots; i < len(incrSnapshots); i++ {
			if err := os.Remove(incrSnapshots[i].path); err != nil {
				mlog.Log.Errorf("failed to remove old incremental snapshot %s: %v", incrSnapshots[i].name, err)
			} else {
				mlog.Log.Infof("Cleaned up old incremental snapshot file (retention limit %d): %s", maxSnapshots, incrSnapshots[i].name)
			}
		}
	}
}

var (
	indexEntryCommitterInProgress = &atomic.Int64{}
	indexEntryBuilderInProgress   = &atomic.Int64{}
)

func BuildAccountsDbPaths(
	ctx context.Context,
	snapshotFile string,
	incrementalSnapshotFile string,
	accountsDbDir string,
	dp *progress.DualProgress,
) (*accountsdb.AccountsDb, *SnapshotManifest, error) {
	// Clean any leftover artifacts from previous incomplete runs (e.g., Ctrl+C)
	CleanAccountsDbDir(accountsDbDir)

	mlog.Log.Infof("Parsing full snapshot manifest...")
	manifest, err := UnmarshalManifestFromSnapshot(ctx, snapshotFile, accountsDbDir)
	if err != nil {
		return nil, nil, fmt.Errorf("reading snapshot manifest: %v", err)
	}
	mlog.Log.Infof("Parsed full snapshot manifest")

	var incrementalManifest *SnapshotManifest
	if incrementalSnapshotFile != "" {
		mlog.Log.Infof("Parsing incremental snapshot manifest...")
		incrementalManifest, err = UnmarshalManifestFromSnapshot(ctx, incrementalSnapshotFile, accountsDbDir)
		if err != nil {
			return nil, nil, fmt.Errorf("reading incremental snapshot manifest: %v", err)
		}
		mlog.Log.Infof("Parsed incremental snapshot manifest")
	}

	start := time.Now()

	appendVecsOutputDir := filepath.Join(accountsDbDir, "accounts")
	if err = os.MkdirAll(appendVecsOutputDir, 0775); err != nil {
		return nil, nil, err
	}

	defer ants.Release()

	// Compute largestFileId from manifest upfront
	largestFileId := computeLargestFileId(manifest, incrementalManifest)
	if largestFileId < accountsdb.FirstReplayFileId {
		largestFileId = accountsdb.FirstReplayFileId
	}
	var largestFileIdAtomic atomic.Uint64
	largestFileIdAtomic.Store(largestFileId)

	wg := &sync.WaitGroup{}

	logsDir := filepath.Join(accountsDbDir, "mithril_db_log_shards")
	if err = os.MkdirAll(logsDir, 0775); err != nil {
		return nil, nil, err
	}
	numShards := 256
	sl := NewShardLogger(numShards, logsDir)

	// Create stake pubkey collector for building stake index during appendvec processing
	stakeCollector := &stakeIndexCollector{
		entries: make([]accountsdb.StakeIndexEntry, 0, 1000000), // Pre-allocate for ~1M stake accounts
	}

	pools, err := initWorkerPools(wg, sl, stakeCollector)
	if err != nil {
		return nil, nil, fmt.Errorf("initializing worker pools: %w", err)
	}

	// Create and fallocate the big snapshot file
	fullTotalSize := computeTotalSize(manifest)
	mlog.Log.Infof("Full snapshot total size: %d bytes (%.1f GB)", fullTotalSize, float64(fullTotalSize)/(1024*1024*1024))

	snapshotDatPath := filepath.Join(appendVecsOutputDir, accountsdb.SnapshotDatFilename)
	snapshotDat, err := accountsdb.OpenDirect(snapshotDatPath, os.O_CREATE|os.O_RDWR, 0644, int64(fullTotalSize))
	if err != nil {
		return nil, nil, fmt.Errorf("opening %s: %w", accountsdb.SnapshotDatFilename, err)
	}

	// Start progress display if provided
	if dp != nil {
		mlog.Flush()
		os.Stdout.Sync()
		os.Stderr.Sync()
		dp.Start()
	}

	// Process full snapshot — read tar and write sequentially to SnapshotDatFilename
	fullFileSizes := buildFileSizeMap(manifest)
	err = readTar(ctx, wg, snapshotFile, readTarOptions{
		progress:              dp,
		sentinelFileId:        accountsdb.SnapshotFileId,
		fileSizes:             fullFileSizes,
		indexEntryBuilderPool: pools.indexEntryBuilder,
		bigFile:               snapshotDat,
		bigFileTotalSize:      fullTotalSize,
	})

	// Wait for ALL worker tasks from full snapshot to complete before starting incremental
	wg.Wait()

	// Stop progress display after full snapshot
	if dp != nil {
		if err != nil {
			dp.Interrupt(err)
		} else {
			dp.Stop()
		}
	}

	if err != nil {
		snapshotDat.Close()
		return nil, nil, err
	}

	snapshotDat.Close()

	// Process incremental snapshot (if provided)
	if incrementalSnapshotFile != "" {
		incrTotalSize := computeTotalSize(incrementalManifest)
		mlog.Log.Infof("Incremental snapshot total size: %d bytes (%.1f GB)", incrTotalSize, float64(incrTotalSize)/(1024*1024*1024))

		incrDatPath := filepath.Join(appendVecsOutputDir, accountsdb.IncrementalDatFilename)
		incrDat, err := accountsdb.OpenDirect(incrDatPath, os.O_CREATE|os.O_RDWR, 0644, int64(incrTotalSize))
		if err != nil {
			return nil, nil, fmt.Errorf("opening %s: %w", accountsdb.IncrementalDatFilename, err)
		}

		incrFileSizes := buildFileSizeMap(manifest, incrementalManifest)
		err = readTar(ctx, wg, incrementalSnapshotFile, readTarOptions{
			sentinelFileId:        accountsdb.IncrementalFileId,
			fileSizes:             incrFileSizes,
			indexEntryBuilderPool: pools.indexEntryBuilder,
			bigFile:               incrDat,
			bigFileTotalSize:      incrTotalSize,
		})
		if err != nil {
			incrDat.Close()
			return nil, nil, err
		}
		// Wait for all incremental worker tasks to complete
		wg.Wait()

		incrDat.Close()
	}

	mlog.Log.Debugf("done processing snapshots in %s.", fmtDuration(time.Since(start)))

	// Show indexing progress for shard flush (no gap between DualProgress and this)
	indexProgress := progress.NewIndexingProgress("Flush (shard logs)")
	indexProgress.Start(numShards)
	err = sl.CloseWithProgress(ctx, func(completed, total int) {
		indexProgress.Update(completed, total)
	})
	if err != nil {
		return nil, nil, fmt.Errorf("closing shard logger: %w", err)
	}
	indexDir := filepath.Join(accountsDbDir, "mithril_db")
	index, err := ingestSSTFiles(indexDir, logsDir)
	if err != nil {
		return nil, nil, fmt.Errorf("initializing pebble from SST files: %w", err)
	}
	index.Close()

	mlog.Log.Infof("Snapshot processed in %s.", fmtDuration(time.Since(start)))

	var largestFileIdBytes [8]byte
	binary.LittleEndian.PutUint64(largestFileIdBytes[:], largestFileIdAtomic.Load())

	path := filepath.Join(accountsDbDir, "largest_file_id")
	if err := os.WriteFile(path, largestFileIdBytes[:], 0644); err != nil {
		mlog.Log.Errorf("error while writing largest file ID=%d to %s: %s", largestFileIdAtomic.Load(), path, err)
		return nil, nil, err
	}

	bankHashOutputFileName := filepath.Join(accountsDbDir, "bank_hash")
	if err := os.WriteFile(bankHashOutputFileName, manifest.Bank.Hash[:], 0644); err != nil {
		mlog.Log.Errorf("error writing bank hash=%x to file=%s: %s", manifest.Bank.Hash, bankHashOutputFileName, err)
		return nil, nil, err
	}

	pools.Release()

	// Write stake pubkey index file (with appendvec location hints)
	stakeIndexPath := filepath.Join(accountsDbDir, "stake_pubkeys.idx")
	if err := accountsdb.WriteStakePubkeyIndex(stakeIndexPath, stakeCollector.entries); err != nil {
		return nil, nil, fmt.Errorf("writing stake pubkey index: %w", err)
	}

	bankhashDir := filepath.Join(accountsDbDir, "bankhash_db")
	bankhashDb, err := pebble.Open(bankhashDir, &pebble.Options{})
	if err != nil {
		return nil, nil, fmt.Errorf("opening bankhashDir=%s: %w", bankhashDir, err)
	}
	bankhashDb.Close()

	accountsDb, err := accountsdb.OpenDb(accountsDbDir)
	if err != nil {
		return nil, nil, err
	}

	if incrementalManifest != nil {
		return accountsDb, incrementalManifest, nil
	} else {
		return accountsDb, manifest, nil
	}
}

// identify appendvec files, whose path is of the form "accounts/SLOT.ID"
func isAppendVec(filename string) bool {
	return strings.Contains(filename, "accounts/") && strings.Contains(filename, ".")
}

type readTarOptions struct {
	// Saves snapshot to a file if non-empty.
	savePath string
	// Update a progress bar if Progress is non-nil.
	progress *progress.DualProgress
	// Big file sequential write support (O_DIRECT fd)
	bigFile               *os.File
	bigFileTotalSize      uint64
	sentinelFileId        uint64
	fileSizes             map[fileSizeKey]uint64
	indexEntryBuilderPool *ants.PoolWithFunc
}

// writeTask is sent to the async writer goroutine in readTar.
type writeTask struct {
	buf      []byte           // page-aligned buffer
	writeLen int              // page-aligned number of bytes to write
	entries  []appendVecEntry // appendvec entries to dispatch to builder after write
	fileId   uint64           // sentinel FileId
	bufPool  chan []byte       // pool to return buf to (nil for one-off oversized buffers)
}

func readTar(
	ctx context.Context,
	wg *sync.WaitGroup,
	filename string,
	options readTarOptions,
) error {
	dp := options.progress
	savePath := options.savePath
	tarReader, bmr, closer, err := newSnapshotReaderWithProgress(ctx, filename, options.savePath)
	if err != nil {
		return err
	}
	defer closer.Close()

	// Set up download progress callback
	if dp != nil {
		dp.SetDownloadTotal(bmr.TotalSize())
		bmr.SetProgressCallback(func(bytesRead, totalBytes int64) {
			dp.Download.Add(bytesRead - dp.Download.Current())
		})
	}

	cleanupPartial := func(reason string) {
		if savePath != "" {
			mlog.Log.Infof("Cleaning up partial download (%s)", reason)
			CleanupPartialDownload(savePath)
		}
	}

	fd := options.bigFile
	const pageSize = 4096

	// Create a pool of page-aligned buffers. Only fileSize bytes per entry
	// are packed contiguously (tar padding is skipped). When full, the
	// page-aligned portion is sent to the async writer goroutine while
	// readTar immediately starts filling the next buffer.
	bufSize := SnapshotBufSize
	bufPool := make(chan []byte, snapshotBufCount)
	for range snapshotBufCount {
		bufPool <- accountsdb.AlignedAlloc(bufSize)
	}

	// Async writer goroutine: receives write tasks, does O_DIRECT WriteAt,
	// then dispatches entries to the builder pool. This decouples disk I/O
	// from decompression so they run in parallel.
	writeCh := make(chan writeTask, snapshotBufCount)
	writerDone := make(chan error, 1)
	go func() {
		// drain consumes remaining tasks, returning buffers to the pool
		// so the reader doesn't deadlock on <-bufPool.
		drain := func() {
			for task := range writeCh {
				if task.bufPool != nil {
					task.bufPool <- task.buf
				}
			}
		}

		var fileOffset int64
		for task := range writeCh {
			if task.writeLen > 0 {
				n, err := fd.WriteAt(task.buf[:task.writeLen], fileOffset)
				if err != nil {
					writerDone <- fmt.Errorf("async WriteAt offset=%d len=%d: %w", fileOffset, task.writeLen, err)
					drain()
					return
				}
				if n != task.writeLen {
					writerDone <- fmt.Errorf("async WriteAt: short write %d != %d", n, task.writeLen)
					drain()
					return
				}
				fileOffset += int64(task.writeLen)
			}

			// Dispatch entries to builder pool, then builder returns buf to pool.
			if len(task.entries) > 0 {
				builderTask := indexEntryBuilderTask{
					Entries: task.entries,
					FileId:  task.fileId,
					Buf:     task.buf,
					Pool:    task.bufPool,
				}
				wg.Add(1)
				if err := options.indexEntryBuilderPool.Invoke(builderTask); err != nil {
					writerDone <- err
					drain()
					return
				}
			} else if task.bufPool != nil {
				task.bufPool <- task.buf
			}
		}
		writerDone <- nil
	}()

	currentBuf := <-bufPool
	currentBufPool := bufPool // nil for one-off oversized buffers
	writePos := 0
	var pendingEntries []appendVecEntry
	currentOffset := uint64(0)

	// Timing instrumentation
	var totalTarNext, totalReadFull, totalFlush, totalBufPoolWait time.Duration
	var flushCount, entryCount int

	for {
		if ctx.Err() != nil {
			mlog.Log.Infof("Context cancelled, stopping snapshot unpack: %v", ctx.Err())
			cleanupPartial("cancelled")
			close(writeCh)
			if currentBufPool != nil {
				currentBufPool <- currentBuf
			}
			return ctx.Err()
		}
		t0 := time.Now()
		header, err := tarReader.Next()
		totalTarNext += time.Since(t0)
		if err == io.EOF {
			break
		} else if err != nil {
			mlog.Log.Errorf("reading next tar: %s\n", err)
			cleanupPartial("read error")
			close(writeCh)
			if currentBufPool != nil {
				currentBufPool <- currentBuf
			}
			return err
		}

		if !isAppendVec(header.Name) {
			continue
		}

		// Parse slot.fileId and look up fileSize before reading so we
		// only read fileSize bytes (tar reader auto-skips the rest).
		var slot, fileId uint64
		if n, err := fmt.Sscanf(filepath.Base(header.Name), "%d.%d", &slot, &fileId); n != 2 || err != nil {
			panic(fmt.Sprintf(
				"failed to parse slot and file from filename=%s basename=%s; parsed n=%d arguments (expected 2) and had err=%v",
				header.Name, filepath.Base(header.Name), n, err))
		}

		fileSize := options.fileSizes[fileSizeKey{slot, fileId}]
		if fileSize == 0 {
			panic(fmt.Sprintf("programming error - fileSize for appendvec slot=%d fileId=%d was 0", slot, fileId))
		}

		entrySize := int(fileSize)

		// If this entry won't fit in the current buffer, flush and swap.
		if writePos+entrySize > bufSize {
			// Compute the page-aligned write boundary. Trailing bytes
			// ("carry") are copied to the start of the next buffer so
			// the writer always gets page-aligned, page-multiple data.
			alignedLen := writePos &^ (pageSize - 1)
			tailLen := writePos - alignedLen

			t1 := time.Now()

			if entrySize > bufSize {
				mlog.Log.Warnf("appendvec %s (%d bytes) exceeds buffer size (%d bytes), allocating one-off buffer", header.Name, entrySize, bufSize)
			}

			// Get the next buffer BEFORE sending the current one to the writer.
			// This ensures we can copy carry bytes from the current buffer.
			var newBuf []byte
			var newBufPool chan []byte
			if entrySize > bufSize {
				newBuf = accountsdb.AlignedAlloc(accountsdb.AlignUp(tailLen+entrySize, pageSize))
				newBufPool = nil
			} else {
				t2 := time.Now()
				newBuf = <-bufPool
				totalBufPoolWait += time.Since(t2)
				newBufPool = bufPool
			}

			// Copy carry (trailing partial page) to the new buffer.
			if tailLen > 0 {
				copy(newBuf[:tailLen], currentBuf[alignedLen:writePos])
			}

			// Send the page-aligned portion to the async writer.
			writeCh <- writeTask{
				buf:      currentBuf,
				writeLen: alignedLen,
				entries:  pendingEntries,
				fileId:   options.sentinelFileId,
				bufPool:  currentBufPool,
			}
			totalFlush += time.Since(t1)
			flushCount++
			pendingEntries = nil

			currentBuf = newBuf
			currentBufPool = newBufPool
			writePos = tailLen
		}

		// Read only fileSize bytes into the buffer. The tar reader
		// auto-skips any remaining (header.Size - fileSize) on Next().
		t3 := time.Now()
		_, err = io.ReadFull(tarReader, currentBuf[writePos:writePos+entrySize])
		totalReadFull += time.Since(t3)
		if err != nil {
			mlog.Log.Errorf("err reading tar entry: %s\n", err)
			cleanupPartial("read error")
			close(writeCh)
			if currentBufPool != nil {
				currentBufPool <- currentBuf
			}
			return err
		}
		appendVecBytes := currentBuf[writePos : writePos+entrySize]
		writePos += entrySize
		entryCount++

		statsd.Count(statsd.SnapshotTarBytesRead, int64(header.Size), nil)
		if dp != nil {
			dp.Extract.Add(int64(header.Size))
		}

		pendingEntries = append(pendingEntries, appendVecEntry{
			Data:       appendVecBytes,
			FileSize:   fileSize,
			Slot:       slot,
			BaseOffset: currentOffset,
		})

		currentOffset += fileSize
	}

	// Flush the last buffer. Pad to page boundary for O_DIRECT.
	alignedLen := accountsdb.AlignUp(writePos, pageSize)
	for i := writePos; i < alignedLen; i++ {
		currentBuf[i] = 0
	}
	writeCh <- writeTask{
		buf:      currentBuf,
		writeLen: alignedLen,
		entries:  pendingEntries,
		fileId:   options.sentinelFileId,
		bufPool:  currentBufPool,
	}
	flushCount++
	close(writeCh)

	// Wait for writer goroutine to finish processing all tasks.
	// This ensures all builder tasks have been dispatched (wg.Add called)
	// before the caller calls wg.Wait().
	if err := <-writerDone; err != nil {
		return err
	}

	// Truncate to exact size (O_DIRECT writes are page-padded).
	if options.bigFileTotalSize > 0 {
		if err := fd.Truncate(int64(options.bigFileTotalSize)); err != nil {
			return fmt.Errorf("truncating big file: %w", err)
		}
	}

	mlog.Log.Infof("readTar timing: entries=%d flushes=%d tarNext=%s readFull=%s flush=%s bufPoolWait=%s",
		entryCount, flushCount, fmtDuration(totalTarNext), fmtDuration(totalReadFull), fmtDuration(totalFlush), fmtDuration(totalBufPoolWait))

	if err := FinalizePartialDownload(savePath); err != nil {
		mlog.Log.Errorf("Failed to finalize snapshot download: %v", err)
	}

	return nil
}

type snapshotWorkerPools struct {
	indexEntryBuilder   *ants.PoolWithFunc
	indexEntryCommitter *ants.PoolWithFunc
}

// stakeIndexCollector aggregates stake account pubkeys from multiple worker goroutines
// during appendvec processing. Used to build the stake pubkey index file.
//
// WHY: The manifest's delegation list can be stale/incomplete (Firedancer notes:
// "the cache in the manifest is partially incomplete"). Instead of trusting manifest
// data, we:
//  1. Collect stake pubkeys during appendvec parsing (by checking owner == StakeProgramAddr)
//  2. Write them to stake_pubkeys.idx after snapshot processing
//  3. At startup, load pubkeys from index and read ALL delegation fields from AccountsDB
//
// This ensures stake cache contains fresh data from AccountsDB, not potentially stale
// manifest data.
type stakeIndexCollector struct {
	mu      sync.Mutex
	entries []accountsdb.StakeIndexEntry
}

func (c *stakeIndexCollector) Add(entries []accountsdb.StakeIndexEntry) {
	if len(entries) == 0 {
		return
	}
	c.mu.Lock()
	c.entries = append(c.entries, entries...)
	c.mu.Unlock()
}

func initWorkerPools(
	wg *sync.WaitGroup,
	sl *ShardLogger,
	stakeCollector *stakeIndexCollector,
) (*snapshotWorkerPools, error) {
	indexEntryCommitterPool, err := ants.NewPoolWithFunc(maxIndexEntryCommitter, func(i any) {
		tasks := indexEntryCommitterInProgress.Add(1)
		statsd.Gauge(statsd.SnapshotWorkerPoolUtilization, float64(tasks)/float64(maxIndexEntryCommitter), []string{"index_entry_committer"})
		start := time.Now()
		defer wg.Done()
		task := i.(indexEntryCommitterTask)

		for idx, entry := range task.IndexEntries {
			sl.EnqueueRequest(task.Pubkeys[idx], entry)
		}
		statsd.Timing(statsd.TaskIndexEntryCommitterLatency, uint64(time.Since(start)), nil)
		indexEntryCommitterInProgress.Add(-1)
	})
	if err != nil {
		return nil, err
	}

	indexEntryBuilderPool, err := ants.NewPoolWithFunc(maxIndexEntryBuilder, func(i any) {
		tasks := indexEntryBuilderInProgress.Add(1)
		statsd.Gauge(statsd.SnapshotWorkerPoolUtilization, float64(tasks)/float64(maxIndexEntryBuilder), []string{"index_entry_builder"})
		start := time.Now()
		defer wg.Done()
		task := i.(indexEntryBuilderTask)

		for _, entry := range task.Entries {
			pubkeys, entries, stakeEntries, err := accountsdb.BuildIndexEntriesFromAppendVecs(entry.Data, entry.FileSize, entry.Slot, task.FileId)
			if err != nil {
				mlog.Log.Errorf("BuildIndexEntriesFromAppendVecs: %v", err)
				continue
			}

			// Adjust offsets by BaseOffset for big file positioning
			for i := range entries {
				entries[i].Offset += entry.BaseOffset
			}
			for i := range stakeEntries {
				stakeEntries[i].Offset += entry.BaseOffset
			}

			stakeCollector.Add(stakeEntries)

			commitTask := indexEntryCommitterTask{IndexEntries: entries, Pubkeys: pubkeys}
			wg.Add(1)
			err = indexEntryCommitterPool.Invoke(commitTask)
			if err != nil {
				mlog.Log.Errorf("indexEntryCommitterPool.Invoke: %v", err)
			}
		}

		// Return the buffer to the pool now that all entries are processed.
		if task.Pool != nil {
			task.Pool <- task.Buf
		}

		indexEntryBuilderInProgress.Add(-1)
		statsd.Timing(statsd.TasksIndexEntryBuilderLatency, uint64(time.Since(start)), nil)
	})
	if err != nil {
		return nil, err
	}

	return &snapshotWorkerPools{
		indexEntryBuilderPool,
		indexEntryCommitterPool,
	}, nil
}

func (p *snapshotWorkerPools) Release() {
	p.indexEntryBuilder.Release()
	p.indexEntryCommitter.Release()
}

// computeTotalSize sums FileSize across all appendvecs in the manifest.
func computeTotalSize(manifest *SnapshotManifest) uint64 {
	var total uint64
	for _, slotAcctVecs := range manifest.AccountsDb.Storages {
		for _, av := range slotAcctVecs.AcctVecs {
			total += av.FileSize
		}
	}
	return total
}

// computeLargestFileId finds the maximum FileId across all appendvecs in the manifest(s).
func computeLargestFileId(manifest *SnapshotManifest, incrementalManifest *SnapshotManifest) uint64 {
	var largest uint64
	for _, slotAcctVecs := range manifest.AccountsDb.Storages {
		for _, av := range slotAcctVecs.AcctVecs {
			if av.Id > largest {
				largest = av.Id
			}
		}
	}
	if incrementalManifest != nil {
		for _, slotAcctVecs := range incrementalManifest.AccountsDb.Storages {
			for _, av := range slotAcctVecs.AcctVecs {
				if av.Id > largest {
					largest = av.Id
				}
			}
		}
	}
	return largest
}

// fileSizeKey is the map key for the fileSize lookup table.
type fileSizeKey struct {
	slot, fileId uint64
}

// buildFileSizeMap builds a flat map from (slot, fileId) to fileSize from the manifests.
func buildFileSizeMap(manifests ...*SnapshotManifest) map[fileSizeKey]uint64 {
	m := make(map[fileSizeKey]uint64)
	for _, manifest := range manifests {
		if manifest == nil {
			continue
		}
		for _, slotAcctVecs := range manifest.AccountsDb.Storages {
			for _, av := range slotAcctVecs.AcctVecs {
				m[fileSizeKey{slotAcctVecs.Slot, av.Id}] = av.FileSize
			}
		}
	}
	return m
}

// Ingest SSTs into a fresh pebble DB and return it.
func ingestSSTFiles(indexDir, logsDir string) (*pebble.DB, error) {
	db, err := pebble.Open(indexDir, &pebble.Options{})
	if err != nil {
		return nil, fmt.Errorf("pebble.Open(%s): %w", indexDir, err)
	}

	glob := filepath.Join(logsDir, "*.sst")
	sstFiles, err := filepath.Glob(glob)
	if err != nil {
		return nil, fmt.Errorf("filepath.Glob(%s): %w", glob, err)
	}
	if len(sstFiles) == 0 {
		return nil, fmt.Errorf("filepath.Glob(%s): unexpectedly globbed 0 SST files!", glob)
	}

	err = db.Ingest(sstFiles)
	if err != nil {
		return nil, fmt.Errorf("ingesting SSTs: %w", err)
	}
	return db, nil
}

