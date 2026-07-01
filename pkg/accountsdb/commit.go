package accountsdb

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/cockroachdb/pebble"
)

// CommitSlotAtomic durably commits a slot's accounts crash-safely, each step durable
// before the next: stage redo → fresh appendvec + fsync → index + bankhash (Sync). A
// crash before the caller's checkpoint/DeleteRedo re-applies the redo idempotently.
func (accountsDb *AccountsDb) CommitSlotAtomic(accts []*accounts.Account, slot uint64, bankhash []byte) error {
	for _, a := range accts {
		if a != nil {
			a.Slot = slot
		}
	}
	if err := WriteRedo(accountsDb.AcctsDir, slot, bankhash, accts); err != nil {
		return fmt.Errorf("accountsdb: stage redo slot %d: %w", slot, err)
	}
	if err := accountsDb.applyAppendOnlySynced(accts, slot); err != nil {
		return fmt.Errorf("accountsdb: durable apply slot %d: %w", slot, err)
	}
	if err := accountsDb.storeBankHashSynced(slot, bankhash); err != nil {
		return fmt.Errorf("accountsdb: durable bankhash slot %d: %w", slot, err)
	}
	accountsDb.refreshReadCaches(accts)
	return nil
}

// CommitRootedSlot durably commits one rooted slot, then deletes its redo. A failed
// DeleteRedo is non-fatal: the redo is re-applied idempotently on next start.
func (accountsDb *AccountsDb) CommitRootedSlot(accts []*accounts.Account, slot uint64, bankhash []byte) error {
	if err := accountsDb.CommitSlotAtomic(accts, slot, bankhash); err != nil {
		return err
	}
	if derr := DeleteRedo(accountsDb.AcctsDir, slot); derr != nil {
		mlog.Log.Errorf("accountsdb: failed to delete redo for promoted slot %d (harmless, re-applied on restart): %v", slot, derr)
	}
	return nil
}

// ApplyPendingCommits roll-forwards commits interrupted before committedSlot advanced:
// redos <= committedSlot are discarded (re-applying would REGRESS), > committedSlot are
// re-applied idempotently and returned. A torn/unreadable redo is quarantined.
func (accountsDb *AccountsDb) ApplyPendingCommits(committedSlot uint64) ([]uint64, error) {
	slots, err := ListPendingRedo(accountsDb.AcctsDir)
	if err != nil {
		return nil, err
	}
	applied := make([]uint64, 0, len(slots))
	for _, slot := range slots {
		if slot <= committedSlot {
			if derr := DeleteRedo(accountsDb.AcctsDir, slot); derr != nil {
				return applied, derr
			}
			continue
		}

		accts, bankhash, err := ReadRedo(accountsDb.AcctsDir, slot)
		if err != nil {
			// Torn or otherwise unreadable: quarantine and keep recovering
			// rather than wedging startup.
			if qerr := quarantineRedo(accountsDb.AcctsDir, slot); qerr != nil {
				return applied, qerr
			}
			continue
		}
		if err := accountsDb.applyAppendOnlySynced(accts, slot); err != nil {
			return applied, err
		}
		if err := accountsDb.storeBankHashSynced(slot, bankhash); err != nil {
			return applied, err
		}
		accountsDb.refreshReadCaches(accts)
		applied = append(applied, slot)
	}
	return applied, nil
}

// applyAppendOnlySynced writes accounts to a fresh appendvec + fsync, then commits
// the index (Sync:true) AFTER the data it references — no in-place overwrite.
func (accountsDb *AccountsDb) applyAppendOnlySynced(accts []*accounts.Account, slot uint64) error {
	live := make([]*accounts.Account, 0, len(accts))
	for _, a := range accts {
		if a != nil {
			live = append(live, a)
		}
	}
	if len(live) == 0 {
		return nil
	}

	fileId := accountsDb.LargestFileId.Add(1)
	// Persist the high-water mark so a restart does not rewind it and reuse a
	// fileId, which (with O_TRUNC below) could clobber a committed appendvec.
	if err := accountsDb.persistLargestFileId(); err != nil {
		return err
	}
	name := fmt.Sprintf("%s/%d.%d", accountsDb.AcctsDir, slot, fileId)
	f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	batch := accountsDb.Index.NewBatch()
	defer batch.Close()
	var idxBuf [24]byte
	for _, acct := range live {
		entry := AccountIndexEntry{Slot: slot, FileId: fileId, Offset: uint64(buf.Len())}
		entry.Marshal(&idxBuf)
		ava := AppendVecAccount{
			DataLen:    uint64(len(acct.Data)),
			Pubkey:     acct.Key,
			Lamports:   acct.Lamports,
			RentEpoch:  acct.RentEpoch,
			Owner:      acct.Owner,
			Executable: acct.Executable,
			Data:       acct.Data,
		}
		if _, err := ava.MarshalReturningLength(&buf); err != nil {
			f.Close()
			return err
		}
		if err := batch.Set(acct.Key[:], idxBuf[:], nil); err != nil { // pebble copies key+value
			f.Close()
			return err
		}
	}

	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := fsyncDir(accountsDb.AcctsDir); err != nil {
		return err
	}
	return batch.Commit(&pebble.WriteOptions{Sync: true})
}

// persistLargestFileId durably records the appendvec file-id high-water mark so a
// restart cannot rewind it and reuse a fileId (which would clobber a committed appendvec).
func (accountsDb *AccountsDb) persistLargestFileId() error {
	path := filepath.Join(filepath.Dir(accountsDb.AcctsDir), "largest_file_id")
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], accountsDb.LargestFileId.Load())
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteAt(b[:], 0); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (accountsDb *AccountsDb) storeBankHashSynced(slot uint64, bankhash []byte) error {
	var slotBytes [8]byte
	binary.LittleEndian.PutUint64(slotBytes[:], slot)
	return accountsDb.BankHashStore.Set(slotBytes[:], bankhash, &pebble.WriteOptions{Sync: true})
}
