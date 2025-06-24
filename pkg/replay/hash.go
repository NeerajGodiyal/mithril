package replay

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"runtime"
	"slices"
	"sync"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/Overclock-Validator/mithril/pkg/safemath"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/zeebo/blake3"
)

type acctHash struct {
	Pubkey solana.PublicKey
	Hash   [32]byte
}

func newAcctHash(pubkey solana.PublicKey, hash []byte) acctHash {
	pair := acctHash{Pubkey: pubkey}
	copy(pair.Hash[:], hash)
	return pair
}

func calculateSingleAcctHash(acct accounts.Account) acctHash {
	hasher := blake3.New()

	var lamportBytes [8]byte
	binary.LittleEndian.PutUint64(lamportBytes[:], acct.Lamports)
	_, _ = hasher.Write(lamportBytes[:])

	var rentEpochBytes [8]byte
	binary.LittleEndian.PutUint64(rentEpochBytes[:], acct.RentEpoch)
	_, _ = hasher.Write(rentEpochBytes[:])

	_, _ = hasher.Write(acct.Data)

	if acct.Executable {
		_, _ = hasher.Write([]byte{1})
	} else {
		_, _ = hasher.Write([]byte{0})
	}

	_, _ = hasher.Write(acct.Owner[:])
	_, _ = hasher.Write(acct.Key[:])

	/*h := sha256.New()
	h.Write(acct.Data)

	fmt.Printf("acct: pubkey %s, lamports %d, owner %s, rent_epoch %d, data hash: %s\n", acct.Key, acct.Lamports, solana.PublicKeyFromBytes(acct.Owner[:]), acct.RentEpoch, solana.HashFromBytes(h.Sum(nil)))*/

	return newAcctHash(acct.Key, hasher.Sum(nil))
}

func calculateSingleAcctHashOnly(acct accounts.Account) []byte {
	hasher := blake3.New()

	var lamportBytes [8]byte
	binary.LittleEndian.PutUint64(lamportBytes[:], acct.Lamports)
	_, _ = hasher.Write(lamportBytes[:])

	var rentEpochBytes [8]byte
	binary.LittleEndian.PutUint64(rentEpochBytes[:], acct.RentEpoch)
	_, _ = hasher.Write(rentEpochBytes[:])

	_, _ = hasher.Write(acct.Data)

	if acct.Executable {
		_, _ = hasher.Write([]byte{1})
	} else {
		_, _ = hasher.Write([]byte{0})
	}

	_, _ = hasher.Write(acct.Owner[:])
	_, _ = hasher.Write(acct.Key[:])

	return hasher.Sum(nil)
}

func calculateAccountHashes(accts []*accounts.Account) []acctHash {
	if len(accts) == 0 {
		return []acctHash{}
	}

	numWorkers := runtime.NumCPU()
	if numWorkers > len(accts) {
		numWorkers = len(accts)
	}

	pairs := make([]acctHash, len(accts))
	chunkSize := (len(accts) + numWorkers - 1) / numWorkers

	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			start := workerID * chunkSize
			end := start + chunkSize
			if end > len(accts) {
				end = len(accts)
			}

			for j := start; j < end; j++ {
				acct := accts[j]
				if acct.Lamports == 0 {
					pairs[j] = newAcctHash(acct.Key, nil)
				} else {
					pairs[j] = calculateSingleAcctHash(*acct)
				}
			}
		}(i)
	}

	wg.Wait()
	return pairs
}

const maxMerkleHeight = 16
const merkleFanout = 16

func divCeil(x uint64, y uint64) uint64 {
	result := x / y
	if (x % y) != 0 {
		result++
	}
	return result
}

func computeMerkleRootLoop(acctHashes [][]byte) []byte {
	if len(acctHashes) == 0 {
		return nil
	}

	totalHashes := uint64(len(acctHashes))
	chunks := divCeil(totalHashes, merkleFanout)

	results := make([][]byte, chunks)

	for i := uint64(0); i < chunks; i++ {
		startIdx := i * merkleFanout
		endIdx := min(startIdx+merkleFanout, totalHashes)

		hasher := sha256.New()
		a := acctHashes[startIdx:endIdx]

		for _, h := range a {
			hasher.Write(h)
		}

		results[i] = hasher.Sum(nil)
	}

	if len(results) == 1 {
		return results[0]
	} else {
		return computeMerkleRootLoop(results)
	}
}

func calculateAcctsDeltaHash(accts []*accounts.Account) []byte {
	acctHashes := calculateAccountHashes(accts)

	// sort by pubkey
	slices.SortFunc(acctHashes, func(a, b acctHash) int {
		return bytes.Compare(a.Pubkey[:], b.Pubkey[:])
	})

	/*mlog.Log.Debugf("accounts modified, sorted by pubkey:\n")
	for _, ah := range acctHashes {
		mlog.Log.Debugf("pubkey: %s, hash: %s\n", ah.Pubkey, solana.PublicKeyFromBytes(ah.Hash[:]))
	}*/

	hashes := make([][]byte, len(acctHashes))
	for idx, ah := range acctHashes {
		hashes[idx] = make([]byte, 32)
		copy(hashes[idx], ah.Hash[:])
	}

	return computeMerkleRootLoop(hashes)
}

func calculateEpochAcctsHash(acctsDb *accountsdb.AccountsDb) []byte {
	mlog.Log.Infof("computing EAH")

	// get all pubkeys in acctsdb
	allKeys := acctsDb.AllKeys()

	// compute acct hashes
	hashes := make([][]byte, len(allKeys))
	for i, pk := range allKeys {
		pkObj := solana.PublicKeyFromBytes(pk)

		acct, err := acctsDb.GetAccount(0, pkObj)
		if err != nil {
			panic(fmt.Sprintf("unable to fetch acct in EAH calculation: %s", pkObj))
		}
		if acct.Lamports != 0 {
			hashes[i] = calculateSingleAcctHashOnly(*acct)
		}
	}

	// merkel root loop
	return computeMerkleRootLoop(hashes)
}

const maxLockoutHistory = 31
const calculateIntervalBuffer = 150
const minimumCalculationInterval = maxLockoutHistory + calculateIntervalBuffer

func isEnabledThisEpoch(epochSchedule *sealevel.SysvarEpochSchedule, epoch uint64) bool {
	slotsPerEpoch := epochSchedule.SlotsInEpoch(epoch)
	calculationOffsetStart := slotsPerEpoch / 4
	calculationOffsetStop := (slotsPerEpoch / 4) * 3
	calculationInterval := safemath.SaturatingSubU64(calculationOffsetStop, calculationOffsetStart)

	return calculationInterval >= minimumCalculationInterval
}

func shouldIncludeEah(epochSchedule *sealevel.SysvarEpochSchedule, slotCtx *sealevel.SlotCtx) bool {
	if !isEnabledThisEpoch(epochSchedule, slotCtx.Epoch) {
		return false
	}

	slotsPerEpoch := epochSchedule.SlotsInEpoch(slotCtx.Epoch)
	calculationOffsetStop := (slotsPerEpoch / 4) * 3
	firstSlotInEpoch := epochSchedule.FirstSlotInEpoch(slotCtx.Epoch)
	stopSlot := safemath.SaturatingAddU64(firstSlotInEpoch, calculationOffsetStop)

	return slotCtx.ParentSlot < stopSlot && slotCtx.Slot >= stopSlot
}

func calculateBankHash(slotCtx *sealevel.SlotCtx, acctsDeltaHash []byte, parentBankHash [32]byte, numSigs uint64, blockHash [32]byte) []byte {
	hasher := sha256.New()
	hasher.Write(parentBankHash[:])
	hasher.Write(acctsDeltaHash[:])

	var numSigsBytes [8]byte
	binary.LittleEndian.PutUint64(numSigsBytes[:], numSigs)

	hasher.Write(numSigsBytes[:])
	hasher.Write(blockHash[:])

	bankHash := hasher.Sum(nil)

	// EAH must be worked into the bankhash for the slot that is 3/4 through the epoch
	epochSchedule := sealevel.SysvarCache.EpochSchedule.Sysvar
	if shouldIncludeEah(epochSchedule, slotCtx) {
		mlog.Log.Infof("EAH required for this bankhash")
		hasher := sha256.New()
		hasher.Write(bankHash)
		hasher.Write(slotCtx.EpochsAcctHash)
		bankHash = hasher.Sum(nil)
	}

	return bankHash
}

var maxBlockfetchAttempts = 10

func fetchBankhashForSlot(rpcc *rpcclient.RpcClient, slot uint64) ([]byte, error) {
	var blockResult *rpc.GetBlockResult
	var err error
	var errCount uint64

	slotToFetch := slot + 1
	for {
		blockResult, err = rpcc.GetBlockFinalized(uint64(slotToFetch))
		if err == nil {
			break
		} else if err == rpcclient.SlotSkipped {
			slotToFetch++
		} else {
			if errCount == 10 {
				return nil, fmt.Errorf("unable to get block: %s", err)
			}
			errCount++
		}
	}

	block, err := NewBlockFromBlockResult(blockResult, slot, rpcc)
	if err != nil {
		panic(fmt.Sprintf("error creating block from BlockResult: %s\n", err))
	}

	var count uint64
	for _, tx := range block.Transactions {
		if tx.IsVote() {

			// skip first 400 votes. most of the first load of votes in a slot usually pertain to two slots back rather than
			// the most recent parent slot.
			count++
			if count < 400 {
				continue
			}

			if len(tx.Message.Instructions) < 1 {
				continue
			}

			instrData := tx.Message.Instructions[0].Data
			decoder := bin.NewBinDecoder(instrData)
			instructionType, err := decoder.ReadUint32(bin.LE)
			if err != nil {
				continue
			}

			if instructionType != sealevel.VoteProgramInstrTypeTowerSync && instructionType != sealevel.VoteProgramInstrTypeTowerSyncSwitch {
				continue
			}

			var towerSyncInstr *sealevel.VoteInstrTowerSync

			if instructionType == sealevel.VoteProgramInstrTypeTowerSync {
				var vote sealevel.VoteInstrTowerSync
				err = vote.UnmarshalWithDecoder(decoder)
				if err != nil {
					continue
				}
				towerSyncInstr = &vote

			} else if instructionType == sealevel.VoteProgramInstrTypeTowerSyncSwitch {
				var vote sealevel.VoteInstrTowerSyncSwitch
				err = vote.UnmarshalWithDecoder(decoder)
				if err != nil {
					continue
				}
				towerSyncInstr = &vote.TowerSync
			}

			lockoutsLen := towerSyncInstr.Lockouts.Len()
			if lockoutsLen == 0 {
				continue
			}

			lockout := towerSyncInstr.Lockouts.PopBack()
			if lockout.Slot == slot {
				return towerSyncInstr.Hash[:], nil
			}
		}
	}

	panic("unable to find a vote for the relevant slot")
}
