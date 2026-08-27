package accountsdb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/lthash"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/gagliardetto/solana-go"
)

const (
	// ponytail: this in-memory sort avoids reopening appendvecs; use an external
	// file-order sort if the verifier memory ceiling becomes material.
	snapshotVerificationBatchSize     = 5_000_000
	snapshotVerificationProgressEvery = 5_000_000
)

type snapshotFileID struct {
	slot   uint64
	fileID uint64
}

type snapshotStateRecord struct {
	pubkey solana.PublicKey
	entry  AccountIndexEntry
}

// CalculateSnapshotState recomputes the canonical AccountsLtHash and
// capitalization from the final snapshot index and appendvecs.
func (accountsDb *AccountsDb) CalculateSnapshotState(ctx context.Context) (_ *lthash.LtHash, capitalization uint64, count uint64, err error) {
	if accountsDb == nil || accountsDb.Index == nil {
		return nil, 0, 0, fmt.Errorf("snapshot AccountsDB is not open")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, 0, err
	}
	iter, err := accountsDb.Index.NewIter(nil)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("iterate snapshot account index: %w", err)
	}
	defer func() { err = errors.Join(err, iter.Close()) }()

	state := new(lthash.LtHash)
	started := time.Now()
	batch := make([]snapshotStateRecord, 0, snapshotVerificationBatchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		addedCapitalization, err := calculateSnapshotStateBatch(ctx, accountsDb.AcctsDir, batch, state)
		if err != nil {
			return err
		}
		if math.MaxUint64-capitalization < addedCapitalization {
			return fmt.Errorf("snapshot capitalization overflows uint64")
		}
		capitalization += addedCapitalization
		count += uint64(len(batch))
		if count%snapshotVerificationProgressEvery == 0 {
			mlog.Log.Infof("Snapshot canonical account state: verified %d accounts in %s.", count, time.Since(started).Round(time.Second))
		}
		batch = batch[:0]
		return nil
	}
	for valid := iter.First(); valid; valid = iter.Next() {
		if err := ctx.Err(); err != nil {
			return nil, 0, count, err
		}
		key := iter.Key()
		value := iter.Value()
		if len(key) != solana.PublicKeyLength {
			return nil, 0, count, fmt.Errorf("snapshot account index key has length %d, want %d", len(key), solana.PublicKeyLength)
		}
		if len(value) != 24 {
			return nil, 0, count, fmt.Errorf("snapshot account index value has length %d, want 24", len(value))
		}
		entry, err := UnmarshalAcctIdxEntry(value)
		if err != nil {
			return nil, 0, count, fmt.Errorf("decode snapshot account index entry: %w", err)
		}
		var pubkey solana.PublicKey
		copy(pubkey[:], key)
		batch = append(batch, snapshotStateRecord{pubkey: pubkey, entry: *entry})
		if len(batch) == cap(batch) {
			if err := flush(); err != nil {
				return nil, 0, count, err
			}
		}
	}
	if err := iter.Error(); err != nil {
		return nil, 0, count, fmt.Errorf("iterate snapshot account index: %w", err)
	}
	if err := flush(); err != nil {
		return nil, 0, count, err
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, count, err
	}
	return state, capitalization, count, nil
}

func calculateSnapshotStateBatch(ctx context.Context, dir string, batch []snapshotStateRecord, state *lthash.LtHash) (capitalization uint64, err error) {
	sort.Slice(batch, func(i, j int) bool {
		left, right := batch[i].entry, batch[j].entry
		if left.Slot != right.Slot {
			return left.Slot < right.Slot
		}
		if left.FileId != right.FileId {
			return left.FileId < right.FileId
		}
		return left.Offset < right.Offset
	})
	for first := 0; first < len(batch); {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		id := snapshotFileID{slot: batch[first].entry.Slot, fileID: batch[first].entry.FileId}
		last := first + 1
		for last < len(batch) && batch[last].entry.Slot == id.slot && batch[last].entry.FileId == id.fileID {
			last++
		}
		path := filepath.Join(dir, fmt.Sprintf("%d.%d", id.slot, id.fileID))
		file, err := os.Open(path)
		if err != nil {
			return 0, fmt.Errorf("open snapshot appendvec %d.%d: %w", id.slot, id.fileID, err)
		}
		groupCapitalization, readErr := readSnapshotStateGroup(ctx, file, id, batch[first:last], state)
		closeErr := file.Close()
		if readErr != nil || closeErr != nil {
			return 0, errors.Join(readErr, closeErr)
		}
		if math.MaxUint64-capitalization < groupCapitalization {
			return 0, fmt.Errorf("snapshot capitalization overflows uint64")
		}
		capitalization += groupCapitalization
		first = last
	}
	return capitalization, nil
}

func readSnapshotStateGroup(ctx context.Context, file *os.File, id snapshotFileID, records []snapshotStateRecord, state *lthash.LtHash) (uint64, error) {
	info, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat snapshot appendvec %d.%d: %w", id.slot, id.fileID, err)
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("snapshot appendvec %d.%d is not a regular file", id.slot, id.fileID)
	}
	var capitalization uint64
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		entry := record.entry
		if entry.Offset > math.MaxInt64 || int64(entry.Offset) >= info.Size() {
			return 0, fmt.Errorf("snapshot account offset %d is outside appendvec %d.%d", entry.Offset, id.slot, id.fileID)
		}
		var stored AppendVecAccount
		reader := io.NewSectionReader(file, int64(entry.Offset), info.Size()-int64(entry.Offset))
		if err := stored.Unmarshal(reader); err != nil {
			return 0, fmt.Errorf("read snapshot account at %d.%d:%d: %w", id.slot, id.fileID, entry.Offset, err)
		}
		if stored.Pubkey != record.pubkey {
			return 0, fmt.Errorf("snapshot account pubkey does not match its index key")
		}
		if math.MaxUint64-capitalization < stored.Lamports {
			return 0, fmt.Errorf("snapshot capitalization overflows uint64")
		}
		capitalization += stored.Lamports
		state.MixIn(new(lthash.LtHash).InitWithAcct(stored.ToAccount()))
	}
	return capitalization, nil
}
