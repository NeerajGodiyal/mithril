package accountsdb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync/atomic"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/util"
	"github.com/Overclock-Validator/sniper"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
)

type AccountsDb struct {
	IndexDb       *sniper.Store
	AcctsDir      string
	IndexDir      string
	LargestFileId atomic.Uint64
	BankHashBytes [32]byte
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

	// attempt to open the index kv store
	indexDir := fmt.Sprintf("%s/index", accountsDbDir)
	db, err := sniper.Open(sniper.Dir(indexDir), sniper.ChunksCollision(32))
	if err != nil {
		mlog.Log.Infof("failed to open database: %s\n", err)
		return nil, err
	}

	accountsDb := &AccountsDb{IndexDb: db, AcctsDir: appendVecsDir, IndexDir: indexDir}
	accountsDb.LargestFileId.Store(largestFileId)
	copy(accountsDb.BankHashBytes[:], bankHashBytes)

	return accountsDb, nil
}

func (accountsDb *AccountsDb) CloseDb() {
	accountsDb.IndexDb.Close()
}

func (accountsDb *AccountsDb) GetAccount(slot uint64, pubkey solana.PublicKey) (*accounts.Account, error) {
	acctIdxEntryBytes, err := accountsDb.IndexDb.Get(pubkey[:])
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
	defer appendVecFile.Close()

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

	msg := util.PrettyPrintAcct(acct)
	mlog.Log.Debugf("SLOT %d - accountsdb.Get() found acct in %s for %s: %s", slot, appendVecFileName, pubkey, msg)

	return acct, err
}

func (accountsDb *AccountsDb) StoreAccounts(accts []*accounts.Account, slot uint64) error {
	fileId := accountsDb.LargestFileId.Add(1)

	appendVecFileName := fmt.Sprintf("%s/%d.%d", accountsDb.AcctsDir, slot, fileId)
	appendVecFile, err := os.OpenFile(appendVecFileName, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		mlog.Log.Debugf("unable to open appendvec file %s for writing to accountsdb", appendVecFileName)
		return err
	}
	defer appendVecFile.Close()

	// allocate required memory all at once to avoid constant reallocs
	//marshaledSize := appendVecAcctsMarshaledSize(accts)
	appendVecAcctsBuf := new(bytes.Buffer)
	//appendVecAcctsBuf.Grow(int(marshaledSize))

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

		err = accountsDb.IndexDb.SetIfSlotHigher(acct.Key[:], writer.Bytes(), 0)
		if err != nil {
			mlog.Log.Debugf("error calling SetIfSlotHigher on accountsdb for pubkey %s", acct.Key)
			return err
		}

		msg := util.PrettyPrintAcct(acct)
		mlog.Log.Debugf("SLOT %d - wrote account %s to %s in StoreAccounts: %s", slot, acct.Key, appendVecFileName, msg)

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
	keys := accountsDb.IndexDb.KeysBetweenPrefixes(startPrefix, endPrefix)

	keyObjs := make([]solana.PublicKey, 0)
	for _, key := range keys {
		keyObject := solana.PublicKeyFromBytes(key)
		keyObjs = append(keyObjs, keyObject)
	}

	return keyObjs
}

func (accountsDb *AccountsDb) AllKeys() [][]byte {
	keys := accountsDb.IndexDb.AllKeys()
	sort.SliceStable(keys, func(i, j int) bool {
		return util.PubkeyCmpByteSlice(keys[i], keys[j])
	})

	return keys
}

func (accountsDb *AccountsDb) BankHash() [32]byte {
	return accountsDb.BankHashBytes
}
