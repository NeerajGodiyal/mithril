package accountsdb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync/atomic"

	"github.com/Overclock-Validator/fastcache"
	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/sbpf"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"github.com/maypok86/otter"
)

type AccountsDb struct {
	Index            fastcache.Cache
	AcctsDir         string
	LargestFileId    atomic.Uint64
	BankHashBytes    [32]byte
	VoteAcctCache    otter.Cache[solana.PublicKey, *accounts.Account]
	CommonAcctsCache otter.Cache[solana.PublicKey, *accounts.Account]
	ProgramCache     otter.Cache[solana.PublicKey, *ProgramCacheEntry]
}

var (
	ErrNoAccount = errors.New("ErrNoAccount")
)

func OpenDb(accountsDbDir string) (*AccountsDb, error) {
	// check for existence of the 'accounts' directory, which holds the appendvecs
	appendVecsDir := fmt.Sprintf("%s/accounts", accountsDbDir)
	_, err := os.Stat(appendVecsDir)
	if err != nil {
		return nil, err
	}

	// attempt to open largest_file_id file
	largestFileIdFn := fmt.Sprintf("%s/largest_file_id", accountsDbDir)
	lfi, err := os.Open(largestFileIdFn)
	if err != nil {
		mlog.Log.Infof("failed to open %s\n", largestFileIdFn)
		return nil, err
	}

	largestFileIdBytes := make([]byte, 8)
	bytesRead, err := lfi.Read(largestFileIdBytes)
	if err != nil {
		mlog.Log.Infof("error reading %s: %s\n", largestFileIdFn, err)
		return nil, err
	} else if bytesRead != 8 {
		mlog.Log.Infof("error reading %s: expected 8 bytes, got %d\n", largestFileIdFn, bytesRead)
		return nil, fmt.Errorf("only got %d bytes", bytesRead)
	}

	largestFileId := binary.LittleEndian.Uint64(largestFileIdBytes)
	mlog.Log.Infof("accountsdb.OpenDb: largestFileId=%d", largestFileId)

	bankHashFn := fmt.Sprintf("%s/bank_hash", accountsDbDir)
	bhf, err := os.Open(bankHashFn)
	if err != nil {
		mlog.Log.Infof("failed to open %s\n", bankHashFn)
		return nil, err
	}

	bankHashBytes := make([]byte, 32)
	bytesRead, err = bhf.Read(bankHashBytes)
	if err != nil {
		mlog.Log.Infof("error reading %s: %s\n", bankHashFn, err)
		return nil, err
	} else if bytesRead != 32 {
		mlog.Log.Infof("error reading %s: expected 8 bytes, got %d\n", bankHashFn, bytesRead)
		return nil, fmt.Errorf("only got %d bytes", bytesRead)
	}
	mlog.Log.Infof("accountsdb.OpenDb: bankHashBytes=%x", bankHashBytes)

	// attempt to open the index kv store
	dbFn := fmt.Sprintf("%s/mithril_db", accountsDbDir)
	db, err := fastcache.NewCache(fastcache.GB*256, &fastcache.Config{
		Shards: 256,
		//MaxElementLen: 2000000000,
		MemoryType: fastcache.MMAP,
		MemoryKey:  dbFn,
	})
	if err != nil {
		panic(err)
	}

	accountsDb := &AccountsDb{Index: db, AcctsDir: appendVecsDir}
	accountsDb.LargestFileId.Store(largestFileId)
	copy(accountsDb.BankHashBytes[:], bankHashBytes)

	return accountsDb, nil
}

func (accountsDb *AccountsDb) CloseDb() {
	accountsDb.Index.Close()
}

func (accountsDb *AccountsDb) InitCaches() {
	var err error
	accountsDb.VoteAcctCache, err = otter.MustBuilder[solana.PublicKey, *accounts.Account](10_000).
		Cost(func(key solana.PublicKey, acct *accounts.Account) uint32 {
			return 1
		}).
		Build()
	if err != nil {
		panic(err)
	}

	// TODO: review size of program cache
	accountsDb.ProgramCache, err = otter.MustBuilder[solana.PublicKey, *ProgramCacheEntry](10_000).
		Cost(func(key solana.PublicKey, progEntry *ProgramCacheEntry) uint32 {
			return 1
		}).
		Build()
	if err != nil {
		panic(err)
	}

	accountsDb.CommonAcctsCache, err = otter.MustBuilder[solana.PublicKey, *accounts.Account](100000).
		Cost(func(key solana.PublicKey, acct *accounts.Account) uint32 {
			return 1
		}).
		Build()
	if err != nil {
		panic(err)
	}
}

type ProgramCacheEntry struct {
	Program        *sbpf.Program
	DeploymentSlot uint64
}

func (accountsDb *AccountsDb) MaybeGetProgramFromCache(pubkey solana.PublicKey) (*ProgramCacheEntry, bool) {
	return accountsDb.ProgramCache.Get(pubkey)
}

func (accountsDb *AccountsDb) AddProgramToCache(pubkey solana.PublicKey, programEntry *ProgramCacheEntry) {
	accountsDb.ProgramCache.Set(pubkey, programEntry)
}

func (accountsDb *AccountsDb) RemoveProgramFromCache(pubkey solana.PublicKey) {
	accountsDb.ProgramCache.Delete(pubkey)
}

func (accountsDb *AccountsDb) GetAccount(slot uint64, pubkey solana.PublicKey) (*accounts.Account, error) {
	cachedAcct, hasAcct := accountsDb.VoteAcctCache.Get(pubkey)
	if hasAcct {
		return cachedAcct, nil
	}

	cachedAcct, hasAcct = accountsDb.CommonAcctsCache.Get(pubkey)
	if hasAcct {
		return cachedAcct, nil
	}

	acctIdxEntryBytes, err := accountsDb.Index.Get(pubkey[:])
	if err != nil {
		mlog.Log.Debugf("no account found in accountsdb for pubkey %s: %s", pubkey, err)
		return nil, ErrNoAccount
	}

	acctIdxEntry, err := unmarshalAcctIdxEntry(acctIdxEntryBytes)
	if err != nil {
		panic("failed to unmarshal AccountIndexEntry from index kv database")
	}

	appendVecFileName := fmt.Sprintf("%s/%d.%d", accountsDb.AcctsDir, acctIdxEntry.Slot, acctIdxEntry.FileId)
	appendVecFile, err := os.Open(appendVecFileName)
	if err != nil {
		mlog.Log.Debugf("failed to open appendvec file %s")
		return nil, err
	}

	offset, err := appendVecFile.Seek(int64(acctIdxEntry.Offset), 0)
	if err != nil {
		panic(fmt.Sprintf("file seek failed: %s\n", err))
	}
	if offset != int64(acctIdxEntry.Offset) {
		panic(fmt.Sprintf("file seek gave wrong idx (%d)\n", offset))
	}

	acct, err := unmarshalAcctFromAppendVecAcctHeader(appendVecFile)
	if err != nil {
		panic(fmt.Sprintf("failed to unmarshal account from appendvec file %s: %s", appendVecFileName, err))
	}

	if acct.Key != pubkey {
		panic(fmt.Sprintf("account unmarshaled from appendvec file %s has the wrong pubkey", appendVecFileName))
	}

	acct.Slot = acctIdxEntry.Slot
	accountsDb.CommonAcctsCache.Set(pubkey, acct)

	return acct, err
}

var voteAcct = solana.MustPublicKeyFromBase58("Vote111111111111111111111111111111111111111")

func (accountsDb *AccountsDb) StoreAccounts(accts []*accounts.Account, slot uint64) error {
	go accountsDb.storeAccountsInternal(accts, slot)

	for _, acct := range accts {
		acct.Slot = slot

		// if vote account, do not serialize up and write into accountsdb - just save it in cache.
		if solana.PublicKeyFromBytes(acct.Owner[:]) == voteAcct {
			accountsDb.VoteAcctCache.Set(acct.Key, acct)
		} else {
			accountsDb.CommonAcctsCache.Set(acct.Key, acct)
		}
	}

	return nil
}

func (accountsDb *AccountsDb) storeAccountsInternal(accts []*accounts.Account, slot uint64) error {
	fileId := accountsDb.LargestFileId.Add(1)

	appendVecFileName := fmt.Sprintf("%s/%d.%d", accountsDb.AcctsDir, slot, fileId)
	appendVecFile, err := os.OpenFile(appendVecFileName, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		mlog.Log.Debugf("unable to open appendvec file %s for writing to accountsdb", appendVecFileName)
		return err
	}
	defer appendVecFile.Close()

	appendVecAcctsBuf := new(bytes.Buffer)
	writer := new(bytes.Buffer)

	for _, acct := range accts {
		acct.Slot = slot

		// create index entry, encode it and write it to the index kv store
		// offset field is specified as the current num of bytes written to the appendvec buffer.
		writer.Reset()
		encoder := bin.NewBinEncoder(writer)

		indexEntry := AccountIndexEntry{Slot: slot, FileId: fileId, Offset: uint64(appendVecAcctsBuf.Len())}
		err = indexEntry.MarshalWithEncoder(encoder)
		if err != nil {
			mlog.Log.Debugf("error marshaling in Set on accountsdb for pubkey %s", acct.Key)
			return err
		}

		err = accountsDb.Index.Set(acct.Key[:], writer.Bytes())
		if err != nil {
			panic(fmt.Sprintf("unable to add acct for %s to acctsdb", acct.Key))
		}

		// marshal up the account as an appendvec style account and write it to the buffer
		appendVecAcct := AppendVecAccount{DataLen: uint64(len(acct.Data)), Pubkey: acct.Key, Lamports: acct.Lamports,
			RentEpoch: acct.RentEpoch, Owner: acct.Owner, Executable: acct.Executable, Data: acct.Data}

		err = appendVecAcct.Marshal(appendVecAcctsBuf)
		if err != nil {
			return err
		}
	}

	// write the appendvecs data into the file
	n, err := appendVecFile.Write(appendVecAcctsBuf.Bytes())
	if err != nil {
		return err
	} else if n != appendVecAcctsBuf.Len() {
		return fmt.Errorf("only wrote %d appendvec account bytes, rather than %d", n, appendVecAcctsBuf.Len())
	}

	return nil
}

func (accountsDb *AccountsDb) KeysBetweenPrefixes(startPrefix uint64, endPrefix uint64) []solana.PublicKey {
	return nil
	/*keys := accountsDb.IndexDb.KeysBetweenPrefixes(startPrefix, endPrefix)

	keyObjs := make([]solana.PublicKey, 0)
	for _, key := range keys {
		keyObject := solana.PublicKeyFromBytes(key)
		keyObjs = append(keyObjs, keyObject)
	}

	return keyObjs*/
}

func (accountsDb *AccountsDb) AllKeys() [][]byte {
	return nil
}

func (accountsDb *AccountsDb) BankHash() [32]byte {
	return accountsDb.BankHashBytes
}
