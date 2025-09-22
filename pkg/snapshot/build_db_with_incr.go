package snapshot

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Overclock-Validator/fastcache"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/blockstream"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/Overclock-Validator/mithril/pkg/snapshotdl"
	"github.com/Overclock-Validator/mithril/pkg/statsd"
	"github.com/panjf2000/ants/v2"
	"k8s.io/klog/v2"
)

func BuildAccountsDbWithIncr(
	fullSnapshotFile string,
	snapshotDownloadPath string,
	fullSnapshotSlot int,
	referenceSlot int,
	accountsDbDir string,
	rpcEndpoint string,
	rpcEndpointFile string,
	blockDir string,
	blockChan chan *block.Block,
) (*accountsdb.AccountsDb, *SnapshotManifest, error) {
	manifest, file, err := UnmarshalManifestFromSnapshot(fullSnapshotFile, accountsDbDir)
	if err != nil {
		return nil, nil, fmt.Errorf("reading snapshot manifest: %v", err)
	}
	mlog.Log.Infof("parsed manifest from snapshotFile=%s", fullSnapshotFile)
	defer file.Close()

	start := time.Now()

	appendVecsOutputDir := filepath.Join(accountsDbDir, "accounts")
	if err = os.MkdirAll(appendVecsOutputDir, 0775); err != nil {
		return nil, nil, err
	}

	defer ants.Release()

	var incrementalManifest *SnapshotManifest
	var incrementalFile *os.File
	var largestFileId atomic.Uint64
	wg := &sync.WaitGroup{}

	numShards := 256
	dbFn := filepath.Join(accountsDbDir, "mithril_db")
	db, err := fastcache.NewCache(fastcache.GB*256, &fastcache.Config{
		Shards:     uint32(numShards),
		MemoryType: fastcache.MMAP,
		MemoryKey:  dbFn,
	})
	if err != nil {
		panic(err)
	}

	ss := NewShardedSetter(db, numShards, 100)
	logsDir := filepath.Join(accountsDbDir, "mithril_db_log_shards")
	if err = os.MkdirAll(logsDir, 0775); err != nil {
		return nil, nil, err
	}
	sl := NewShardLogger(numShards, logsDir, ss)

	indexEntryCommiterPool, _ := ants.NewPoolWithFunc(maxIndexEntryCommitter, func(i interface{}) {
		tasks := indexEntryCommitterInProgress.Add(1)
		statsd.Gauge("snapshot.worker_pool.utilization", float64(tasks)/float64(maxIndexEntryCommitter), []string{"worker_pool:index_entry_committer"}, 1)
		start := time.Now()
		defer wg.Done()
		task := i.(indexEntryCommitterTask)

		for idx, entry := range task.IndexEntries {
			sl.EnqueueRequest(task.Pubkeys[idx], entry)
		}
		statsd.Timing("tasks.index_entry_committer.latency", time.Since(start), nil, 1)
		indexEntryCommitterInProgress.Add(-1)
	})

	indexEntryBuilderPool, _ := ants.NewPoolWithFunc(maxIndexEntryBuilder, func(i interface{}) {
		tasks := indexEntryBuilderInProgress.Add(1)
		statsd.Gauge("snapshot.worker_pool.utilization", float64(tasks)/float64(maxIndexEntryBuilder), []string{"worker_pool:index_entry_builder"}, 1)
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
		statsd.Timing("tasks.index_entry_builder.latency", time.Since(start), nil, 1)
		err = indexEntryCommiterPool.Invoke(commitTask)
		if err != nil {
			mlog.Log.Errorf("error calling indexEntryCommiterPool.Invoke\n")
		}
	})

	appendVecCopyingPool, _ := ants.NewPoolWithFunc(maxAppendVecCopying, func(i interface{}) {
		tasks := appendVecCopyingInProgress.Add(1)
		statsd.Gauge("snapshot.worker_pool.utilization", float64(tasks)/float64(maxAppendVecCopying), []string{"worker_pool:append_vec_copying"}, 1)
		start := time.Now()
		defer wg.Done()
		task := i.(appendVecCopyingTask)
		filename := task.Filename
		writer := task.TarBuffer

		outFilename := filepath.Join(accountsDbDir, filename)
		appendVecBytes := writer.Bytes()
		err := os.WriteFile(outFilename, appendVecBytes, 0644)
		if err != nil {
			mlog.Log.Errorf("err writing new file=%s: %v", outFilename, err)
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

		// find the relevant appendvec storage info. use the info from the incremental
		// snapshot manifest if this account entry is from the incremental snapshot.
		var fileSize uint64
		var usedIncrementalSnapshotVal bool
		if task.FromIncrementalSnapshot {
			if incrementalManifest != nil {
				for _, av := range incrementalManifest.AccountsDb.Storages[slot].AcctVecs {
					if av.Id == fileId {
						fileSize = av.FileSize
						usedIncrementalSnapshotVal = true
						break
					}
				}
			}
		}

		if !usedIncrementalSnapshotVal {
			for _, av := range manifest.AccountsDb.Storages[slot].AcctVecs {
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
		statsd.Timing("tasks.append_vec_copying.latency", time.Since(start), nil, 1)
		err = indexEntryBuilderPool.Invoke(nextTask)
		if err != nil {
			mlog.Log.Errorf("error calling indexEntryBuilderPool.Invoke\n")
		}
	})

	wg.Add(1)
	go func() {
		defer wg.Done()
		err = readTar(wg, file, appendVecCopyingPool)
	}()
	wg.Wait()
	mlog.Log.Infof("done processing full snapshot in %s.", time.Since(start))

	mlog.Log.Infof("Closing shard logger.")
	sl.Close()

	sl = NewShardLogger(numShards, logsDir, ss)
	if err != nil {
		mlog.Log.Errorf("processing snapshot: %v", err)
	}

	// download an incremental snapshot based on the full snapshot's slot number
	mlog.Log.Infof("downloading incremental snapshot (%d)...", referenceSlot)
	incrSnapshotDlStart := time.Now()
	incrementalSnapshotPath, _, incrSlot, err := snapshotdl.DownloadIncrementalSnapshot("https://api.mainnet-beta.solana.com", snapshotDownloadPath, referenceSlot, fullSnapshotSlot)
	if err != nil {
		klog.Fatalf("error downloading snapshot: %s", err)
	}
	mlog.Log.Infof("finished downloading incremental snapshot in %s to %s", time.Since(incrSnapshotDlStart), incrementalSnapshotPath)

	opts := blockstream.BlockSourceOpts{
		EndpointType: blockstream.BlockEndpointTypeRpc,
		OutDir:       blockDir,
		RpcPoolFile:  rpcEndpointFile,
		StartSlot:    uint64(incrSlot),
	}

	blockSubscriber := blockstream.NewBlockSource(opts)
	go blockSubscriber.Start()

	incrSnapshotStart := time.Now()
	incrementalManifest, incrementalFile, err = UnmarshalManifestFromSnapshot(incrementalSnapshotPath, accountsDbDir)
	if err != nil {
		return nil, nil, fmt.Errorf("reading incremental snapshot manifest: %v", err)
	}
	mlog.Log.Infof("parsed manifest from incrementalFile=%s", incrementalSnapshotPath)
	defer incrementalFile.Close()

	var incrementalErr error

	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		incrementalErr = readTarIncr(wg, incrementalFile, appendVecCopyingPool)
		mlog.Log.Infof("finished reading %s in %s", incrementalFile.Name(), time.Since(start))
	}()
	wg.Wait()
	mlog.Log.Infof("done processing incremental snapshot in %s.", time.Since(incrSnapshotStart))

	if err != nil || incrementalErr != nil {
		return nil, nil, err
	}

	mlog.Log.Infof("Closing shard logger for incremental snapshot.")
	sl.Close()
	mlog.Log.Infof("Stopping shard setter.")
	ss.Stop()

	mlog.Log.Infof("snapshots processed in %s.\n", time.Since(start))

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

	accountsDb := &accountsdb.AccountsDb{Index: db, AcctsDir: appendVecsOutputDir}
	accountsDb.LargestFileId.Store(largestFileId.Load())
	copy(accountsDb.BankHashBytes[:], manifest.Bank.Hash[:])

	if incrementalManifest != nil {
		accountsDb.CurrentSlot = incrementalManifest.Bank.Slot + 1
	} else {
		accountsDb.CurrentSlot = manifest.Bank.Slot + 1
	}

	rpcClient := rpcclient.NewRpcClient("https://api.mainnet-beta.solana.com/")
	latestSlot, _ := rpcClient.GetSlot()
	_, incrSlot = snapshotdl.ExtractIncrementalSnapshotSlots(incrementalSnapshotPath)

	mlog.Log.Infof("node currently at slot %d, whereas chain is at slot %d. currently %d slots behind.", incrSlot, latestSlot, latestSlot-uint64(incrSlot))

	return accountsDb, incrementalManifest, nil
}

func readTarIncr(wg *sync.WaitGroup, file *os.File, appendVecCopyingPool *ants.PoolWithFunc) error {
	tarReader, err := newSnapshotReader(file)
	if err != nil {
		return err
	}

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			mlog.Log.Errorf("reading next tar: %s\n", err)
			return err
		}

		if !isAppendVec(header.Name) {
			continue
		}

		writer := bytes.NewBuffer(make([]byte, 0, header.Size))
		tarBytesRead, err := io.Copy(writer, tarReader)
		if err != nil {
			mlog.Log.Errorf("err copying data to reader: %s\n", err)
			return err
		}
		statsd.Count("snapshot.tar_bytes_read", tarBytesRead, nil, 1)

		task := appendVecCopyingTask{TarBuffer: writer, Filename: header.Name, FromIncrementalSnapshot: true}
		wg.Add(1)
		err = appendVecCopyingPool.Invoke(task)
		if err != nil {
			mlog.Log.Errorf("error calling appendVecCopyingPool.Invoke: %v", err)
			return err
		}
	}

	return nil
}
