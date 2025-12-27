package snapshot

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
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
	"github.com/panjf2000/ants/v2"

	"github.com/Overclock-Validator/fastcache"
)

const (
	maxIndexEntryCommitter = 512
	maxIndexEntryBuilder   = 500
	maxAppendVecCopying    = 500
)

// cleanAccountsDbDir removes all artifacts from a previous incomplete snapshot run.
// This prevents corruption from Ctrl+C or partial downloads.
func cleanAccountsDbDir(accountsDbDir string) {
	// List of all files/directories that may be left from a previous incomplete run
	artifacts := []string{
		"accounts",
		"mithril_db",
		"mithril_db_log_shards",
		"bankhash_db",
		"largest_file_id",
		"bank_hash",
		"manifest",
	}
	for _, artifact := range artifacts {
		os.RemoveAll(filepath.Join(accountsDbDir, artifact))
	}
}

var (
	indexEntryCommitterInProgress = &atomic.Int64{}
	indexEntryBuilderInProgress   = &atomic.Int64{}
	appendVecCopyingInProgress    = &atomic.Int64{}
)

func BuildAccountsDb(
	ctx context.Context,
	snapshotFile string,
	incrementalSnapshotFile string,
	accountsDbDir string,
) (*accountsdb.AccountsDb, *SnapshotManifest, error) {
	// Clean any leftover artifacts from previous incomplete runs (e.g., Ctrl+C)
	cleanAccountsDbDir(accountsDbDir)

	manifest, err := UnmarshalManifestFromSnapshot(snapshotFile, accountsDbDir)
	if err != nil {
		return nil, nil, fmt.Errorf("reading snapshot manifest: %v", err)
	}
	mlog.Log.Infof("parsed manifest from snapshotFile=%s", snapshotFile)

	var incrementalManifest *SnapshotManifest
	if incrementalSnapshotFile != "" {
		incrementalManifest, err = UnmarshalManifestFromSnapshot(incrementalSnapshotFile, accountsDbDir)
		if err != nil {
			return nil, nil, fmt.Errorf("reading incremental snapshot manifest: %v", err)
		}
		mlog.Log.Infof("parsed manifest from incrementalSnapshotFile=%s", incrementalSnapshotFile)
	}

	start := time.Now()

	appendVecsOutputDir := filepath.Join(accountsDbDir, "accounts")
	if err = os.MkdirAll(appendVecsOutputDir, 0775); err != nil {
		return nil, nil, err
	}

	defer ants.Release()

	var largestFileId atomic.Uint64
	wg := &sync.WaitGroup{}

	numShards := 256
	dbFn := filepath.Join(accountsDbDir, "mithril_db")

	indexDb, err := fastcache.NewCache(fastcache.GB*256, &fastcache.Config{
		Shards:     uint32(numShards),
		MemoryType: fastcache.MMAP,
		MemoryKey:  dbFn,
	})
	if err != nil {
		panic(err)
	}

	ss := NewShardedSetter(indexDb, numShards, 100)
	logsDir := filepath.Join(accountsDbDir, "mithril_db_log_shards")
	if err = os.MkdirAll(logsDir, 0775); err != nil {
		return nil, nil, err
	}
	sl := NewShardLogger(numShards, logsDir, ss)

	indexEntryCommiterPool, _ := ants.NewPoolWithFunc(maxIndexEntryCommitter, func(i interface{}) {
		tasks := indexEntryCommitterInProgress.Add(1)
		statsd.Gauge(statsd.SnapshotWorkerPoolUtilization, float64(tasks)/float64(maxIndexEntryCommitter), []string{"index_entry_committer"})
		start := time.Now()
		defer wg.Done()
		task := i.(indexEntryCommitterTask)

		for idx, entry := range task.IndexEntries {
			sl.EnqueueRequest(task.Pubkeys[idx], entry)
		}
		statsd.Timing(statsd.TaskIndexEntryCommitterLatency, uint64(time.Since(start).Nanoseconds()), nil)
		indexEntryCommitterInProgress.Add(-1)
	})

	indexEntryBuilderPool, _ := ants.NewPoolWithFunc(maxIndexEntryBuilder, func(i interface{}) {
		tasks := indexEntryBuilderInProgress.Add(1)
		statsd.Gauge(statsd.SnapshotWorkerPoolUtilization, float64(tasks)/float64(maxIndexEntryBuilder), []string{"index_entry_builder"})
		start := time.Now()
		defer wg.Done()
		task := i.(indexEntryBuilderTask)
		pubkeys, entries, err := accountsdb.BuildIndexEntriesFromAppendVecs(task.Data, task.FileSize, task.Slot, task.FileId)
		if err != nil {
			mlog.Log.Errorf("%s\n", err)
			return
		}

		indexEntryBuilderInProgress.Add(-1)
		commitTask := indexEntryCommitterTask{IndexEntries: entries, Pubkeys: pubkeys}
		wg.Add(1)
		statsd.Timing(statsd.TasksIndexEntryBuilderLatency, uint64(time.Since(start)), nil)
		err = indexEntryCommiterPool.Invoke(commitTask)
		if err != nil {
			mlog.Log.Errorf("error calling indexEntryCommiterPool.Invoke\n")
		}
	})

	appendVecCopyingPool, _ := ants.NewPoolWithFunc(maxAppendVecCopying, func(i interface{}) {
		tasks := appendVecCopyingInProgress.Add(1)
		statsd.Gauge(statsd.SnapshotWorkerPoolUtilization, float64(tasks)/float64(maxAppendVecCopying), []string{"append_vec_copying"})
		start := time.Now()
		defer wg.Done()
		task := i.(appendVecCopyingTask)
		filename := task.Filename
		writer := task.TarBuffer

		outFilename := filepath.Join(accountsDbDir, filename)

		// validate that the path doesn't escape accountsDbDir (via '../' sequences)
		cleanPath := filepath.Clean(outFilename)
		if !strings.HasPrefix(cleanPath, filepath.Clean(accountsDbDir)+string(os.PathSeparator)) {
			panic(fmt.Sprintf("invalid path in tar archive: %s", filename))
		}

		appendVecBytes := writer.Bytes()
		err := os.WriteFile(cleanPath, appendVecBytes, 0644)
		if err != nil {
			mlog.Log.Errorf("err writing new file=%s: %v", cleanPath, err)
			appendVecCopyingInProgress.Add(-1)
			return
		}

		var slot, fileId uint64
		if n, err := fmt.Sscanf(filepath.Base(filename), "%d.%d", &slot, &fileId); n != 2 || err != nil {
			panic(fmt.Sprintf(
				"failed to parse slot and file from filename=%s basename=%s; parsed n=%d arguments (expected 2) and had err=%v",
				filename, filepath.Base(filename), n, err))
		}

		for {
			prevLargestFileId := largestFileId.Load()
			if fileId <= prevLargestFileId {
				break
			}
			swapped := largestFileId.CompareAndSwap(prevLargestFileId, fileId)
			if swapped {
				break
			}
		}

		// find the relevant appendvec storage info
		var fileSize uint64
		for _, av := range manifest.AccountsDb.Storages[slot].AcctVecs {
			if av.Id == fileId {
				fileSize = av.FileSize
				break
			}
		}

		if fileSize == 0 && incrementalManifest != nil {
			for _, av := range incrementalManifest.AccountsDb.Storages[slot].AcctVecs {
				if av.Id == fileId {
					fileSize = av.FileSize
					break
				}
			}
		}

		if fileSize == 0 {
			panic("programming error - fileSize for appendvec was 0")
		}

		appendVecCopyingInProgress.Add(-1)
		nextTask := indexEntryBuilderTask{Data: appendVecBytes, FileSize: fileSize, Slot: slot, FileId: fileId}
		wg.Add(1)
		statsd.Timing(statsd.TasksAppendVecCopyingLatency, uint64(time.Since(start)), nil)
		err = indexEntryBuilderPool.Invoke(nextTask)
		if err != nil {
			mlog.Log.Errorf("error calling indexEntryBuilderPool.Invoke\n")
		}
	})

	wg.Add(1)
	go func() {
		defer wg.Done()
		err = readTar(ctx, wg, snapshotFile, appendVecCopyingPool)
	}()

	var incrementalErr error
	if incrementalSnapshotFile != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			incrementalErr = readTar(ctx, wg, incrementalSnapshotFile, appendVecCopyingPool)
			mlog.Log.Infof("finished reading %s in %s", incrementalSnapshotFile, time.Since(start))
		}()
	}

	wg.Wait()
	if err := errors.Join(err, incrementalErr); err != nil {
		mlog.Log.Errorf("failed while processing snapshots: %v", err)
		return nil, nil, err
	}
	mlog.Log.Infof("Done unpacking and sharding snapshot in %s, closing shard logger", time.Since(start))

	// Show indexing progress for shard flush
	indexProgress := progress.NewIndexingProgress("Indexing")
	indexProgress.Start(numShards)
	err = sl.CloseWithProgress(ctx, func(completed, total int) {
		indexProgress.Update(completed, total)
	})
	if err != nil {
		return nil, nil, fmt.Errorf("closing shard logger: %w", err)
	}

	mlog.Log.Infof("Snapshot indexing complete in %s", time.Since(start))

	mlog.Log.Infof("Stopping shard setter.")
	ss.Stop()

	mlog.Log.Infof("snapshot processed in %s.\n", time.Since(start))

	var largestFileIdBytes [8]byte
	binary.LittleEndian.PutUint64(largestFileIdBytes[:], largestFileId.Load())

	path := filepath.Join(accountsDbDir, "largest_file_id")
	if err := os.WriteFile(path, largestFileIdBytes[:], 0644); err != nil {
		mlog.Log.Errorf("error while writing largest file ID=%d to %s: %s", largestFileId.Load(), path, err)
		return nil, nil, err
	}

	bankHashOutputFileName := filepath.Join(accountsDbDir, "bank_hash")
	if err := os.WriteFile(bankHashOutputFileName, manifest.Bank.Hash[:], 0644); err != nil {
		mlog.Log.Errorf("error writing bank hash=%x to file=%s: %s", manifest.Bank.Hash, bankHashOutputFileName, err)
		return nil, nil, err
	}

	indexEntryCommiterPool.Release()
	indexEntryBuilderPool.Release()
	appendVecCopyingPool.Release()

	accountsDb := &accountsdb.AccountsDb{Index: indexDb, AcctsDir: appendVecsOutputDir}
	accountsDb.LargestFileId.Store(largestFileId.Load())
	copy(accountsDb.BankHashBytes[:], manifest.Bank.Hash[:])

	bankHashDbFn := fmt.Sprintf("%s/bankhash_db", accountsDbDir)
	accountsDb.BankHashStore, err = fastcache.NewCache(fastcache.MB*128, &fastcache.Config{
		Shards: 256,
		//MaxElementLen: 2000000000,
		MemoryType: fastcache.MMAP,
		MemoryKey:  bankHashDbFn,
	})
	if err != nil {
		panic(err)
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

func readTar(ctx context.Context, wg *sync.WaitGroup, filename string, appendVecCopyingPool *ants.PoolWithFunc) error {
	return readTarWithSave(ctx, wg, filename, "", appendVecCopyingPool)
}

func readTarWithSave(ctx context.Context, wg *sync.WaitGroup, filename string, savePath string, appendVecCopyingPool *ants.PoolWithFunc) error {
	tarReader, closer, err := newSnapshotReaderWithSave(filename, savePath)
	if err != nil {
		return err
	}
	defer closer.Close()

	// cleanupPartial deletes the partial download file if it exists
	cleanupPartial := func(reason string) {
		if savePath != "" {
			if _, statErr := os.Stat(savePath); statErr == nil {
				mlog.Log.Infof("Deleting partial download %s (%s)", savePath, reason)
				if rmErr := os.Remove(savePath); rmErr != nil {
					mlog.Log.Errorf("Failed to delete partial download %s: %v", savePath, rmErr)
				}
			}
		}
	}

	for {
		if ctx.Err() != nil {
			mlog.Log.Infof("context cancelled, stopping snapshot unpack: %v", ctx.Err())
			cleanupPartial("cancelled")
			return ctx.Err()
		}
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			mlog.Log.Errorf("reading next tar: %s\n", err)
			cleanupPartial("read error")
			return err
		}

		if !isAppendVec(header.Name) {
			continue
		}

		writer := bytes.NewBuffer(make([]byte, 0, header.Size))
		tarBytesRead, err := io.Copy(writer, tarReader)
		if err != nil {
			mlog.Log.Errorf("err copying data to reader: %s\n", err)
			cleanupPartial("copy error")
			return err
		}
		statsd.Count(statsd.SnapshotTarBytesRead, tarBytesRead, nil)

		task := appendVecCopyingTask{TarBuffer: writer, Filename: header.Name}
		wg.Add(1)
		err = appendVecCopyingPool.Invoke(task)
		if err != nil {
			mlog.Log.Errorf("error calling appendVecCopyingPool.Invoke: %v", err)
			cleanupPartial("pool error")
			return err
		}
	}

	return nil
}

// readTarWithProgress is like readTarWithSave but reports progress to a DualProgress display.
// If dp is nil, it falls back to the standard behavior without progress reporting.
func readTarWithProgress(ctx context.Context, wg *sync.WaitGroup, filename string, savePath string, appendVecCopyingPool *ants.PoolWithFunc, dp *progress.DualProgress) error {
	tarReader, bmr, closer, err := newSnapshotReaderWithProgress(filename, savePath)
	if err != nil {
		return err
	}
	defer closer.Close()

	// Set up download progress callback
	if dp != nil && bmr != nil {
		downloadTotal := bmr.TotalSize()
		dp.Download.SetTotal(downloadTotal)
		bmr.SetProgressCallback(func(bytesRead, totalBytes int64) {
			dp.Download.Add(bytesRead - dp.Download.Current())
		})

		// Estimate build total based on typical zstd compression ratio (~3x expansion)
		// This gives a rough ETA; actual decompressed size varies by snapshot content
		estimatedBuildTotal := downloadTotal * 3
		dp.Build.SetTotal(estimatedBuildTotal)
	}

	// cleanupPartial deletes the partial download file if it exists
	cleanupPartial := func(reason string) {
		if savePath != "" {
			if _, statErr := os.Stat(savePath); statErr == nil {
				mlog.Log.Infof("Deleting partial download %s (%s)", savePath, reason)
				if rmErr := os.Remove(savePath); rmErr != nil {
					mlog.Log.Errorf("Failed to delete partial download %s: %v", savePath, rmErr)
				}
			}
		}
	}

	for {
		if ctx.Err() != nil {
			mlog.Log.Infof("context cancelled, stopping snapshot unpack: %v", ctx.Err())
			cleanupPartial("cancelled")
			return ctx.Err()
		}
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			mlog.Log.Errorf("reading next tar: %s\n", err)
			cleanupPartial("read error")
			return err
		}

		if !isAppendVec(header.Name) {
			continue
		}

		writer := bytes.NewBuffer(make([]byte, 0, header.Size))
		tarBytesRead, err := io.Copy(writer, tarReader)
		if err != nil {
			mlog.Log.Errorf("err copying data to reader: %s\n", err)
			cleanupPartial("copy error")
			return err
		}
		statsd.Count(statsd.SnapshotTarBytesRead, tarBytesRead, nil)

		// Update build progress
		if dp != nil {
			dp.Build.Add(tarBytesRead)
		}

		task := appendVecCopyingTask{TarBuffer: writer, Filename: header.Name}
		wg.Add(1)
		err = appendVecCopyingPool.Invoke(task)
		if err != nil {
			mlog.Log.Errorf("error calling appendVecCopyingPool.Invoke: %v", err)
			cleanupPartial("pool error")
			return err
		}
	}

	return nil
}
