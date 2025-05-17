package snapshot

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/statsd"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"github.com/panjf2000/ants/v2"

	"github.com/Overclock-Validator/fastcache"
)

// This struct effectively acts as a mutex on a per-pubkey basis.
// The purpose of this is to prevent entries for the same pubkey being
// processed at the same time to ensure that we end up with the account
// entry for the most recent slot.

type pubkeyLock struct {
	pubkeyMap sync.Map
}

func (pp *pubkeyLock) LockPubkey(pubkey solana.PublicKey) {
	for {
		_, shouldWait := pp.pubkeyMap.LoadOrStore(pubkey, 1)
		if !shouldWait {
			return
		}
	}
}

func (pp *pubkeyLock) UnlockPubkey(pubkey solana.PublicKey) {
	pp.pubkeyMap.Delete(pubkey)
}

// identify appendvec files, whose path is of the form "accounts/SLOT.ID"
func isAppendVec(filename string) bool {
	return strings.Contains(filename, "accounts/") && strings.Contains(filename, ".")
}

const numShards = 256

func BuildAccountsDb(snapshotFile string, accountsDbDir string) (*accountsdb.AccountsDb, *SnapshotManifest, error) {
	manifest, file, err := UnmarshalManifestFromSnapshot(snapshotFile, accountsDbDir)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()

	tarReader, err := newSnapshotReader(file)
	if err != nil {
		return nil, nil, err
	}

	start := time.Now()

	appendVecsOutputDir := fmt.Sprintf("%s/accounts", accountsDbDir)
	if err = os.MkdirAll(appendVecsOutputDir, 0775); err != nil {
		return nil, nil, err
	}

	defer ants.Release()

	var largestFileId atomic.Uint64
	wg := sync.WaitGroup{}

	dbFn := fmt.Sprintf("%s/mithril_db", accountsDbDir)
	db, err := fastcache.NewCache(fastcache.GB*512, &fastcache.Config{
		Shards: 256,
		//MaxElementLen: 2000000000,
		MemoryType: fastcache.MMAP,
		MemoryKey:  dbFn,
	})
	if err != nil {
		panic(err)
	}

	var pp pubkeyLock

	indexEntryCommiterPool, _ := ants.NewPoolWithFunc(20, func(i interface{}) {
		start := time.Now()
		defer wg.Done()
		task := i.(indexEntryCommitterTask)
		writer := new(bytes.Buffer)

		for idx, entry := range task.IndexEntries {
			writer.Reset()
			encoder := bin.NewBinEncoder(writer)
			err = entry.MarshalWithEncoder(encoder)

			shouldSet := true
			var dst []byte

			pp.LockPubkey(task.Pubkeys[idx])
			dst, err = db.Get(task.Pubkeys[idx][:])
			if err == nil {
				if len(dst) >= 8 {
					currentSlot := binary.LittleEndian.Uint64(dst[:8])
					if currentSlot >= entry.Slot {
						shouldSet = false
					}
				}
			}

			if shouldSet {
				err = db.Set(task.Pubkeys[idx][:], writer.Bytes())
				if err != nil {
					mlog.Log.Infof("failed to set value for %s: %s", task.Pubkeys[idx], err)
				}
			}
			pp.UnlockPubkey(task.Pubkeys[idx])
		}
		statsd.Timing("tasks.index_entry_committer.latency", time.Since(start), nil, 1)
	})

	indexEntryBuilderPool, _ := ants.NewPoolWithFunc(500, func(i interface{}) {
		start := time.Now()
		defer wg.Done()
		task := i.(indexEntryBuilderTask)
		pubkeys, entries, err := accountsdb.BuildIndexEntriesFromAppendVecs(task.Data, task.FileSize, task.Slot, task.FileId)
		if err != nil {
			mlog.Log.Errorf("%s\n", err)
			return
		}

		commitTask := indexEntryCommitterTask{IndexEntries: entries, Pubkeys: pubkeys}
		wg.Add(1)
		statsd.Timing("tasks.index_entry_builder.latency", time.Since(start), nil, 1)
		err = indexEntryCommiterPool.Invoke(commitTask)
		if err != nil {
			mlog.Log.Errorf("error calling indexEntryCommiterPool.Invoke\n")
		}
	})

	appendVecCopyingPool, _ := ants.NewPoolWithFunc(500, func(i interface{}) {
		start := time.Now()
		defer wg.Done()
		task := i.(appendVecCopyingTask)
		filename := task.Filename
		writer := task.TarBuffer

		if !isAppendVec(filename) {
			return
		}

		outFile, err := os.Create(fmt.Sprintf("%s/%s", accountsDbDir, filename))
		if err != nil {
			mlog.Log.Errorf("err creating new: %s\n", err)
			return
		}

		appendVecBytes := writer.Bytes()
		_, err = io.Copy(outFile, bytes.NewReader(appendVecBytes))
		if err != nil {
			mlog.Log.Errorf("err copying file out: %s\n", err)
			return
		}

		// parse slot and file ID out of filename
		_, after, found := strings.Cut(filename, "/")
		if !found {
			panic(fmt.Sprintf("invalid appendvec path format: %s", filename))
		}

		slotStr, idStr, found := strings.Cut(after, ".")
		slot, err := strconv.ParseUint(slotStr, 10, 64)
		if err != nil {
			mlog.Log.Errorf("invalid snapshot - unable to convert string to slot\n")
			panic("")
		}

		fileId, err := strconv.ParseUint(idStr, 10, 64)
		if err != nil {
			panic("invalid snapshot - unable to convert string to file id\n")
		}

		if fileId > largestFileId.Load() {
			largestFileId.Store(fileId)
		}

		// find the relevant appendvec storage info
		var fileSize uint64
		for _, av := range manifest.AccountsDb.Storages[slot].AcctVecs {
			if av.Id == fileId {
				fileSize = av.FileSize
				break
			}
		}

		if fileSize == 0 {
			panic("programming error - fileSize for appendvec was 0")
		}

		nextTask := indexEntryBuilderTask{Data: appendVecBytes, FileSize: fileSize, Slot: slot, FileId: fileId}
		wg.Add(1)
		statsd.Timing("tasks.append_vec_copying.latency", time.Since(start), nil, 1)
		err = indexEntryBuilderPool.Invoke(nextTask)
		if err != nil {
			mlog.Log.Errorf("error calling indexEntryBuilderPool.Invoke\n")
		}
	})

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			mlog.Log.Errorf("err reading next tar: %s\n", err)
			return nil, nil, err
		}

		writer := new(bytes.Buffer)
		_, err = io.Copy(writer, tarReader)
		if err != nil {
			mlog.Log.Errorf("err copying data to reader: %s\n", err)
			return nil, nil, err
		}

		task := appendVecCopyingTask{TarBuffer: writer, Filename: header.Name}
		wg.Add(1)
		err = appendVecCopyingPool.Invoke(task)
		if err != nil {
			mlog.Log.Errorf("error calling appendVecCopyingPool.Invoke\n")
		}
	}

	mlog.Log.Infof("done in %s. waiting for all tasks to complete.\n", time.Since(start))
	wg.Wait()
	mlog.Log.Infof("snapshot processed in %s.\n", time.Since(start))

	largestFileIdFile, err := os.Create(fmt.Sprintf("%s/largest_file_id", accountsDbDir))
	if err != nil {
		mlog.Log.Errorf("err creating new: %s\n", err)
		return nil, nil, err
	}

	largestFileIdBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(largestFileIdBytes, largestFileId.Load())

	numBytesWritten, err := largestFileIdFile.Write(largestFileIdBytes[:])
	if err != nil {
		mlog.Log.Errorf("error writing largest file ID to file: %s\n", err)
		return nil, nil, err
	} else if numBytesWritten != 8 {
		mlog.Log.Errorf("error writing largest file ID to file\n")
		return nil, nil, fmt.Errorf("error writing largest file ID to file, wrote %d bytes", numBytesWritten)
	}

	largestFileIdFile.Close()

	bankHashOutputFileName := fmt.Sprintf("%s/bank_hash", accountsDbDir)
	bankHashFile, err := os.Create(bankHashOutputFileName)
	if err != nil {
		mlog.Log.Errorf("err creating new: %s\n", err)
		return nil, nil, err
	}

	numBytesWritten, err = bankHashFile.Write(manifest.Bank.Hash[:])
	if err != nil {
		mlog.Log.Errorf("error writing bank hash to file: %s\n", err)
		return nil, nil, err
	} else if numBytesWritten != 32 {
		mlog.Log.Errorf("error writing bank hash to file\n")
		return nil, nil, fmt.Errorf("error writing bank hash to file, wrote %d bytes", numBytesWritten)
	}

	bankHashFile.Close()
	indexEntryCommiterPool.Release()
	appendVecCopyingPool.Release()
	appendVecCopyingPool.Release()

	accountsDb := &accountsdb.AccountsDb{Index: db, AcctsDir: appendVecsOutputDir}
	accountsDb.LargestFileId.Store(largestFileId.Load())
	copy(accountsDb.BankHashBytes[:], manifest.Bank.Hash[:])

	return accountsDb, manifest, nil
}
