package replay

import (
	"fmt"
	"math"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/base58"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/fees"
	"github.com/Overclock-Validator/mithril/pkg/rent"
	"github.com/Overclock-Validator/mithril/pkg/rewards"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/snapshot"
	"github.com/Overclock-Validator/mithril/pkg/util"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"k8s.io/klog/v2"
)

type BlockRewardsInfo struct {
	Leader      solana.PublicKey
	Lamports    uint64
	PostBalance uint64
}

type Block struct {
	Slot                         uint64
	Epoch                        uint64
	Transactions                 []*solana.Transaction
	BankHash                     [32]byte
	EpochAcctsHash               []byte
	ParentBankhash               [32]byte
	NumSignatures                uint64
	Blockhash                    [32]byte
	ExpectedBankhash             [32]byte
	TxMetas                      []*rpc.TransactionMeta
	Leader                       solana.PublicKey
	Reward                       BlockRewardsInfo
	RecentBlockhash              [32]byte
	UnixTimestamp                int64
	StakeAccts                   map[solana.PublicKey]bool
	VoteTimestamps               map[solana.PublicKey]sealevel.BlockTimestamp
	Features                     *features.Features
	PartitionedRewardsUpdatedPks []solana.PublicKey
	PartitionedRewardsInfo       *rewards.PartitionedRewardDistributionInfo
}

func resolveAddrTableLookups(accountsDb *accountsdb.AccountsDb, block *Block) error {
	tables := make(map[solana.PublicKey]solana.PublicKeySlice)

	for idx, tx := range block.Transactions {
		klog.Infof("resolveAddrTableLookups for transaction %d", idx)

		if !tx.Message.IsVersioned() {
			continue
		}

		var skipLookup bool
		for _, addrTableKey := range tx.Message.GetAddressTableLookups().GetTableIDs() {
			acct, err := accountsDb.GetAccount(block.Slot, addrTableKey)
			if err != nil {
				klog.Infof("unable to get address lookup table account: %s", addrTableKey)
				skipLookup = true
				break
			}

			addrLookupTable, err := sealevel.UnmarshalAddressLookupTable(acct.Data)
			if err != nil {
				return err
			}

			tables[addrTableKey] = addrLookupTable.Addresses
		}

		if skipLookup {
			continue
		}

		err := tx.Message.SetAddressTables(tables)
		if err != nil {
			return err
		}

		err = tx.Message.ResolveLookups()
		if err != nil {
			return err
		}
	}

	return nil
}

func extractAndDedupeBlockAccts(block *Block) []solana.PublicKey {
	pubkeys := make([]solana.PublicKey, 0)

	for _, tx := range block.Transactions {
		for _, pubkey := range tx.Message.AccountKeys {
			pubkeys = append(pubkeys, pubkey)
		}
	}

	for _, pubkey := range block.PartitionedRewardsUpdatedPks {
		pubkeys = append(pubkeys, pubkey)
	}

	pubkeys = util.DedupePubkeys(pubkeys)
	return pubkeys
}

func isNativeProgram(pubkey solana.PublicKey) bool {
	if pubkey == sealevel.SystemProgramAddr || pubkey == sealevel.BpfLoaderUpgradeableAddr ||
		pubkey == sealevel.BpfLoader2Addr || pubkey == sealevel.BpfLoaderDeprecatedAddr ||
		pubkey == sealevel.VoteProgramAddr || pubkey == sealevel.StakeProgramAddr ||
		pubkey == sealevel.ConfigProgramAddr || pubkey == sealevel.StakeProgramConfigAddr ||
		pubkey == sealevel.NativeLoaderAddr {
		return true
	} else {
		return false
	}
}

func isSysvar(pubkey solana.PublicKey) bool {
	if pubkey == sealevel.SysvarClockAddr || pubkey == sealevel.SysvarEpochScheduleAddr ||
		pubkey == sealevel.SysvarFeesAddr || pubkey == sealevel.SysvarInstructionsAddr ||
		pubkey == sealevel.SysvarRecentBlockHashesAddr || pubkey == sealevel.SysvarRentAddr ||
		pubkey == sealevel.SysvarRewardsAddr || pubkey == sealevel.SysvarSlotHashesAddr ||
		pubkey == sealevel.SysvarSlotHistoryAddr || pubkey == sealevel.SysvarStakeHistoryAddr {
		return true
	} else {
		return false
	}
}

func loadBlockAccountsAndUpdateSysvars(accountsDb *accountsdb.AccountsDb, block *Block) (accounts.Accounts, error) {
	err := resolveAddrTableLookups(accountsDb, block)
	if err != nil {
		return nil, err
	}

	dedupedAccts := extractAndDedupeBlockAccts(block)
	accts := accounts.NewMemAccounts()

	for _, pk := range dedupedAccts {

		// retrieve account from accountsdb
		acct, err := accountsDb.GetAccount(block.Slot, pk)

		// add the account to the slice, add a 'blank' account if the account doesn't exist,
		// or return an error
		if err == accountsdb.ErrNoAccount {
			if isNativeProgram(pk) {
				acct = &accounts.Account{Key: pk, Owner: sealevel.NativeLoaderAddr, Executable: true, Lamports: 1}
				klog.Infof("no account: %s, using empty owned by Native Loader\n", pk)
			} else {
				acct = &accounts.Account{Key: pk, Owner: sealevel.SystemProgramAddr, RentEpoch: math.MaxUint64}
				klog.Infof("no account: %s, using empty owned by System program\n", pk)
			}
		} else if err != nil {
			return nil, err
		} else {
			klog.Infof("found account in loadBlockAccounts for: %s\n", acct.Key)
		}

		var pkBytes [32]byte
		copy(pkBytes[:], pk.Bytes())

		err = accts.SetAccount(&pkBytes, acct)
		if err != nil {
			return nil, err
		}
	}

	// load sysvar accounts
	{
		sysvarAddrs := []solana.PublicKey{sealevel.SysvarClockAddr /*sealevel.SysvarEpochRewardsAddr,*/, sealevel.SysvarEpochScheduleAddr,
			sealevel.SysvarFeesAddr, sealevel.SysvarRecentBlockHashesAddr, sealevel.SysvarRentAddr, sealevel.SysvarSlotHashesAddr,
			sealevel.SysvarSlotHistoryAddr, sealevel.SysvarStakeHistoryAddr, sealevel.SysvarLastRestartSlotAddr}

		for _, sysvarAddr := range sysvarAddrs {
			sysvarAcct, err := accountsDb.GetAccount(block.Slot, sysvarAddr)
			if err != nil {
				panic(fmt.Sprintf("unable to retrieve sysvar %s from accountsdb", sysvarAddr))
			}

			if sysvarAcct.Key == sealevel.SysvarSlotHashesAddr {
				decoder := bin.NewBinDecoder(sysvarAcct.Data)
				var slotHashes sealevel.SysvarSlotHashes

				err = slotHashes.UnmarshalWithDecoder(decoder)
				if err != nil {
					panic(fmt.Sprintf("unable to unmarshal slothashes sysvar"))
				}

				slotHashes.Update(block.Slot, block.ParentBankhash)
				newSlotHashesBytes := slotHashes.MustMarshal()
				copy(sysvarAcct.Data, newSlotHashesBytes)
			} else if sysvarAcct.Key == sealevel.SysvarClockAddr {
				decoder := bin.NewBinDecoder(sysvarAcct.Data)
				var clock sealevel.SysvarClock

				err = clock.UnmarshalWithDecoder(decoder)
				if err != nil {
					panic(fmt.Sprintf("unable to unmarshal clock sysvar"))
				}

				err = updateClockSysvar(&clock, accountsDb, block)
				if err != nil {
					panic(fmt.Sprintf("failed to update clock sysvar: %s", err))
				}

				newClockBytes := clock.MustMarshal()
				copy(sysvarAcct.Data, newClockBytes)
			}

			var sysvarPkBytes [32]byte
			copy(sysvarPkBytes[:], sysvarAddr.Bytes())
			err = accts.SetAccount(&sysvarPkBytes, sysvarAcct)
			if err != nil {
				panic(fmt.Sprintf("unable to set sysvar %s to accountsdb", sysvarAddr))
			}
		}
	}

	return accts, nil
}

func scanAndEnableFeatures(acctsDb *accountsdb.AccountsDb, slot uint64) *features.Features {
	f := features.NewFeaturesDefault()
	for _, featureGate := range features.AllFeatureGates {
		_, err := acctsDb.GetAccount(slot, featureGate.Address)
		if err == nil {
			klog.Infof("enabled feature: %s, %s", featureGate.Name, solana.PublicKeyFromBytes(featureGate.Address[:]))
			f.EnableFeature(featureGate, slot)
		}
	}
	return f
}

func newBlockFromBlockResult(blockResult *rpc.GetBlockResult) (*Block, error) {
	block := new(Block)

	for _, tx := range blockResult.Transactions {
		txParsed, err := tx.GetTransaction()
		if err != nil {
			return nil, err
		}
		block.Transactions = append(block.Transactions, txParsed)
		block.TxMetas = append(block.TxMetas, tx.Meta)
	}

	block.Blockhash = blockResult.Blockhash
	block.RecentBlockhash = blockResult.PreviousBlockhash
	block.UnixTimestamp = int64(*blockResult.BlockTime)

	for _, tx := range block.Transactions {
		block.NumSignatures += uint64(tx.Message.Header.NumRequiredSignatures)
	}

	return block, nil
}

func setupInitialVoteAcctsAndStakeAccts(block *Block, snapshotManifest *snapshot.SnapshotManifest) {
	block.VoteTimestamps = make(map[solana.PublicKey]sealevel.BlockTimestamp)
	block.StakeAccts = make(map[solana.PublicKey]bool)

	for _, va := range snapshotManifest.Bank.Stakes.VoteAccounts {
		ts := sealevel.BlockTimestamp{Slot: va.Value.LastTimestampSlot, Timestamp: va.Value.LastTimestampTs}
		block.VoteTimestamps[va.Key] = ts
	}

	for _, sa := range snapshotManifest.Bank.Stakes.StakeDelegations {
		block.StakeAccts[sa.Account] = true
	}
}

func ReplayBlocks(acctsDb *accountsdb.AccountsDb, acctsDbPath string, snapshotManifest *snapshot.SnapshotManifest, startSlot, endSlot uint64, updateAcctsDb bool) error {
	rpcc := rpcclient.NewRpcClient("https://api.mainnet-beta.solana.com")

	epochScheduleAcct, err := acctsDb.GetAccount(startSlot, sealevel.SysvarEpochScheduleAddr)
	if err != nil {
		panic("unable to retrieve epoch schedule sysvar acct when updating clock sysvar")
	}

	decoder := bin.NewBinDecoder(epochScheduleAcct.Data)
	var epochSchedule sealevel.SysvarEpochSchedule
	err = epochSchedule.UnmarshalWithDecoder(decoder)
	if err != nil {
		panic(fmt.Sprintf("unable to unmarshal epoch schedule sysvar when updating clock sysvar"))
	}

	currentEpoch := epochSchedule.GetEpoch(startSlot)
	var currentFeatures *features.Features
	var lastSlotCtx *sealevel.SlotCtx
	var partitionedEpochRewardsEnabled bool
	var partitionedRewardsInfo *rewards.PartitionedRewardDistributionInfo

	currentFeatures = scanAndEnableFeatures(acctsDb, startSlot)
	partitionedEpochRewardsEnabled = currentFeatures.IsActive(features.EnablePartitionedEpochReward) || currentFeatures.IsActive(features.EnablePartitionedEpochRewardsSuperfeature)

	for currentSlot := startSlot; currentSlot <= endSlot; currentSlot++ {
		blockResult, err := rpcc.GetBlockFinalized(uint64(currentSlot))
		if err != nil {
			klog.Fatalf("error fetching block: %s\n", err)
		}

		block, err := newBlockFromBlockResult(blockResult)
		if err != nil {
			klog.Fatalf("error creating block from BlockResult: %s\n", err)
		}

		leader, err := rpcc.GetLeaderForSlot(uint64(currentSlot))
		if err != nil {
			klog.Fatalf("error fetching leader for slot: %s\n", err)
		}

		block.Leader = leader
		block.Reward = BlockRewardsInfo{Leader: blockResult.Rewards[0].Pubkey, Lamports: uint64(blockResult.Rewards[0].Lamports), PostBalance: blockResult.Rewards[0].PostBalance}
		block.Slot = currentSlot

		if currentSlot == startSlot {
			block.ParentBankhash = snapshotManifest.Bank.Hash
			setupInitialVoteAcctsAndStakeAccts(block, snapshotManifest)

		} else {
			copy(block.ParentBankhash[:], lastSlotCtx.FinalBankhash)
			block.StakeAccts = lastSlotCtx.StakeAccts
			block.VoteTimestamps = lastSlotCtx.VoteTimestamps
		}

		block.Epoch = epochSchedule.GetEpoch(currentSlot)

		// epoch boundary
		if block.Epoch != currentEpoch {
			klog.Infof("epoch boundary")

			currentFeatures = scanAndEnableFeatures(acctsDb, currentSlot)
			partitionedEpochRewardsEnabled = currentFeatures.IsActive(features.EnablePartitionedEpochReward) || currentFeatures.IsActive(features.EnablePartitionedEpochRewardsSuperfeature)

			var updatedPks []solana.PublicKey
			partitionedRewardsInfo, updatedPks = handleEpochTransition(acctsDb, rpcc, partitionedEpochRewardsEnabled, lastSlotCtx, &epochSchedule, blockResult, currentEpoch)
			if partitionedEpochRewardsEnabled {
				block.PartitionedRewardsUpdatedPks = append(block.PartitionedRewardsUpdatedPks, updatedPks...)
			}

			currentEpoch = block.Epoch
		} else if currentSlot == startSlot && partitionedEpochRewardsEnabled {
			partitionedRewardsInfo = rewards.RetrievePartitionedStakingRewardsInfo(rpcc, &epochSchedule, block.Epoch, currentSlot)
		}

		block.Features = currentFeatures
		block.PartitionedRewardsInfo = partitionedRewardsInfo

		if currentSlot == partitionedRewardsInfo.EahCalcSlot {
			// calculate accounts hash for *all* on-chain accounts
			block.EpochAcctsHash = calculateEpochAcctsHash(acctsDb)
			klog.Infof("epoch accts hash: %s", base58.Encode(block.EpochAcctsHash))
		}

		if len(blockResult.Rewards) > 1 && partitionedEpochRewardsEnabled && currentSlot <= partitionedRewardsInfo.LastStakingRewardSlot {
			rewardPks := distributePartitionedEpochRewardsForSlot(acctsDb, blockResult.Rewards, currentSlot, partitionedRewardsInfo.LastStakingRewardSlot)
			block.PartitionedRewardsUpdatedPks = append(block.PartitionedRewardsUpdatedPks, rewardPks...)
		}

		lastSlotCtx, err = ProcessBlock(acctsDb, block, updateAcctsDb)
		if err != nil {
			klog.Errorf("error encountered during block replay: %s\n", err)
			break
		} else {
			klog.Infof("block replayed successfully.\n")
		}
	}

	return nil
}

func ProcessBlock(acctsDb *accountsdb.AccountsDb, block *Block, updateAcctsDb bool) (*sealevel.SlotCtx, error) {

	klog.Infof("replaying slot %d, epoch %d", block.Slot, block.Epoch)

	// gather up all accounts referenced in the block
	accts, err := loadBlockAccountsAndUpdateSysvars(acctsDb, block)
	if err != nil {
		panic(fmt.Sprintf("unable to load slot accounts and update sysvars: %s", err))
	}

	slotCtx := &sealevel.SlotCtx{Slot: block.Slot, Epoch: block.Epoch, ParentSlot: block.Slot - 1,
		Blockhash: block.Blockhash, RecentBlockhash: block.RecentBlockhash, EpochsAcctHash: block.EpochAcctsHash,
		Accounts: accts, AccountsDb: acctsDb, Replay: true, Features: block.Features, StakeAccts: block.StakeAccts,
		VoteTimestamps: block.VoteTimestamps, EpochAcctHashInclusionSlot: math.MaxUint64}
	slotCtx.ModifiedAccts = make(map[solana.PublicKey]bool)

	if block.PartitionedRewardsInfo != nil {
		slotCtx.EpochAcctHashInclusionSlot = block.PartitionedRewardsInfo.EahInclusionSlot
	}

	acctIsWritable := make(map[solana.PublicKey]bool)
	var txFeeAccumulator fees.TxFeeInfoAccumulator

	// process & execute each transaction in turn
	for idx, tx := range block.Transactions {
		klog.Infof("[+] executing transaction %d (slot %d, epoch %d), %s", idx+1, block.Slot, block.Epoch, tx.Signatures[0])
		txFeeInfo, wpks, txErr := ProcessTransaction(slotCtx, tx, block.TxMetas[idx])
		if txErr != nil {
			klog.Infof("tx %d returned error: %s\n", idx+1, txErr)
		}

		// check for success-failure return value divergences
		if txErr == nil && block.TxMetas[idx].Err != nil {
			klog.Infof("tx %s return value divergence: txErr was nil, but onchain err was %+v", tx.Signatures[0], block.TxMetas[idx].Err)
		} else if txErr != nil && block.TxMetas[idx].Err == nil {
			klog.Infof("tx %s return value divergence: txErr was %+v, but onchain err was nil", tx.Signatures[0], txErr)
		}

		for _, pk := range wpks {
			acctIsWritable[pk] = true
		}

		txFeeAccumulator.Add(txFeeInfo)
	}

	// distribute tx fees to the leader by calculating 50% of the tx fees and adding the sum
	// to the slot leader's lamports balance, subsequently including it in the accounts delta hash.
	fees.DistributeTxFeesToSlotLeader(acctsDb, slotCtx, block.Leader, &txFeeAccumulator)

	klog.Infof("from RPC fees for leader: %d, post-balance: %d (%s)", block.Reward.Lamports, block.Reward.PostBalance, block.Reward.Leader)

	epochScheduleAcct, err := slotCtx.Accounts.GetAccount(&sealevel.SysvarEpochScheduleAddr)
	if err != nil {
		panic("unable to fetch EpochSchedule sysvar account")
	}

	dec := bin.NewBinDecoder(epochScheduleAcct.Data)
	var epochSchedule sealevel.SysvarEpochSchedule
	err = epochSchedule.UnmarshalWithDecoder(dec)
	if err != nil {
		panic("unable to deserialize EpochSchedule sysvar")
	}

	rentSysvarAcct, err := slotCtx.Accounts.GetAccount(&sealevel.SysvarRentAddr)
	if err != nil {
		panic("unable to fetch EpochSchedule sysvar account")
	}

	dec = bin.NewBinDecoder(rentSysvarAcct.Data)
	var rentSysvar sealevel.SysvarRent
	err = rentSysvar.UnmarshalWithDecoder(dec)
	if err != nil {
		panic("unable to deserialize Rent sysvar")
	}

	// XXX: disabling addition of rent accounts into bankhash for speed during testing
	rentAccts := rent.CollectRentEagerly(slotCtx, &rentSysvar, &epochSchedule)

	acctIsWritable[block.Leader] = true

	eligibleAccts := make([]*accounts.Account, 0)
	for pk := range acctIsWritable {
		acct, _ := slotCtx.GetAccount(pk)
		eligibleAccts = append(eligibleAccts, acct)
	}

	for _, pk := range block.PartitionedRewardsUpdatedPks {
		acct, _ := slotCtx.GetAccount(pk)
		eligibleAccts = append(eligibleAccts, acct)
	}

	eligibleAccts = append(eligibleAccts, rentAccts...)
	sysvarAccts := collectAndUpdateSysvarAcctsForAdh(slotCtx)
	eligibleAccts = append(eligibleAccts, sysvarAccts...)

	if len(eligibleAccts) > 0 && updateAcctsDb {
		klog.Infof("updating accountsdb")
		err = acctsDb.StoreAccounts(eligibleAccts, slotCtx.Slot)
		for _, acctToStore := range eligibleAccts {
			fmt.Printf("updated account: %s\n", util.PrettyPrintAcct(acctToStore))
		}
	} else {
		klog.Infof("accountsdb not updated")
	}

	klog.Infof("calculating accts delta hash for %d eligible accounts. len of rentAccts = %d", len(eligibleAccts), len(rentAccts))

	acctDeltaHash := calculateAcctsDeltaHash(eligibleAccts)

	// calculate bankhash
	slotCtx.FinalBankhash = calculateBankHash(slotCtx, acctDeltaHash, block.ParentBankhash, block.NumSignatures, block.Blockhash)
	klog.Infof("calculated bankhash for slot %d was %s", block.Slot, base58.Encode(slotCtx.FinalBankhash))

	return slotCtx, err
}
