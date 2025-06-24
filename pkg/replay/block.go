package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"runtime/pprof"
	"sync"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	a "github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/arena"
	"github.com/Overclock-Validator/mithril/pkg/base58"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/fees"
	"github.com/Overclock-Validator/mithril/pkg/metrics"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/rent"
	"github.com/Overclock-Validator/mithril/pkg/rewards"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/snapshot"
	"github.com/Overclock-Validator/mithril/pkg/statsd"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

var SerializedParameterArena *arena.Arena[byte]

type BlockRewardsInfo struct {
	Leader      solana.PublicKey
	Lamports    uint64
	PostBalance uint64
}

type Block struct {
	Slot                   uint64
	ParentSlot             uint64
	BlockHeight            uint64
	Epoch                  uint64
	Transactions           []*solana.Transaction
	BankHash               [32]byte
	EpochAcctsHash         []byte
	EahWorkaroundBankhash  []byte
	HasEahWorkaround       bool
	ParentBankhash         [32]byte
	NumSignatures          uint64
	Blockhash              [32]byte
	ExpectedBankhash       [32]byte
	TxMetas                []*rpc.TransactionMeta
	Leader                 solana.PublicKey
	BlockReward            *BlockRewardsInfo
	LastBlockhash          [32]byte
	UnixTimestamp          int64
	StakeAccts             map[solana.PublicKey]bool
	VoteAccts              map[solana.PublicKey]uint64
	VoteTimestamps         map[solana.PublicKey]sealevel.BlockTimestamp
	TotalEpochStake        uint64
	Features               *features.Features
	UpdatedAccts           []solana.PublicKey
	PartitionedRewardsInfo *rewards.PartitionedRewardDistributionInfo
	Rewards                []rpc.BlockReward
	NumRewardPartitions    uint64
}

func resolveAddrTableLookups(accountsDb *accountsdb.AccountsDb, block *Block) error {
	tables := make(map[solana.PublicKey]solana.PublicKeySlice)

	for _, tx := range block.Transactions {
		//mlog.Log.Debugf("resolveAddrTableLookups for transaction %d", idx)

		if !tx.Message.IsVersioned() {
			continue
		}

		var skipLookup bool
		for _, addrTableKey := range tx.Message.GetAddressTableLookups().GetTableIDs() {
			if _, alreadyLoaded := tables[addrTableKey]; alreadyLoaded {
				continue
			}

			acct, err := accountsDb.GetAccount(block.Slot, addrTableKey)
			if err != nil {
				//mlog.Log.Debugf("unable to get address lookup table account: %s", addrTableKey)
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
	var numPubkeys int
	for _, tx := range block.Transactions {
		numPubkeys += len(tx.Message.AccountKeys)
	}

	numPubkeys += len(block.UpdatedAccts)

	pubkeyMap := make(map[solana.PublicKey]struct{}, numPubkeys)

	for _, tx := range block.Transactions {
		for _, pk := range tx.Message.AccountKeys {
			pubkeyMap[pk] = struct{}{}
		}
	}

	pubkeys := make([]solana.PublicKey, len(pubkeyMap))
	i := 0
	for pk := range pubkeyMap {
		pubkeys[i] = pk
		i++
	}

	return pubkeys
}

func isNativeProgram(pubkey solana.PublicKey) bool {
	if pubkey == a.SystemProgramAddr || pubkey == a.BpfLoaderUpgradeableAddr ||
		pubkey == a.BpfLoader2Addr || pubkey == a.BpfLoaderDeprecatedAddr ||
		pubkey == a.VoteProgramAddr || pubkey == a.StakeProgramAddr ||
		pubkey == a.ConfigProgramAddr || pubkey == a.StakeProgramConfigAddr ||
		pubkey == a.NativeLoaderAddr {
		return true
	} else {
		return false
	}
}

func isSysvar(pubkey solana.PublicKey) bool {
	if pubkey == sealevel.SysvarClockAddr || pubkey == sealevel.SysvarEpochScheduleAddr ||
		pubkey == sealevel.SysvarFeesAddr || pubkey == sealevel.SysvarInstructionsAddr ||
		pubkey == sealevel.SysvarRecentBlockHashesAddr || pubkey == sealevel.SysvarRentAddr ||
		pubkey == a.SysvarRewardsAddr || pubkey == sealevel.SysvarSlotHashesAddr ||
		pubkey == sealevel.SysvarSlotHistoryAddr || pubkey == sealevel.SysvarStakeHistoryAddr {
		return true
	} else {
		return false
	}
}

func cacheConstantSysvars(acctsDb *accountsdb.AccountsDb) {
	{
		acct, err := acctsDb.GetAccount(0, sealevel.SysvarEpochScheduleAddr)
		if err != nil {
			panic("unable to get epochschedule when caching sysvars")
		}
		decoder := bin.NewBinDecoder(acct.Data)
		var epochSchedule sealevel.SysvarEpochSchedule
		epochSchedule.MustUnmarshalWithDecoder(decoder)
		sealevel.SysvarCache.EpochSchedule.Sysvar = &epochSchedule
		sealevel.SysvarCache.EpochSchedule.Acct = acct
	}

	{
		acct, err := acctsDb.GetAccount(0, sealevel.SysvarRentAddr)
		if err != nil {
			panic("unable to get rent sysvar when caching sysvars")
		}
		var rent sealevel.SysvarRent
		decoder := bin.NewBinDecoder(acct.Data)
		rent.MustUnmarshalWithDecoder(decoder)
		sealevel.SysvarCache.Rent.Sysvar = &rent
		sealevel.SysvarCache.Rent.Acct = acct
	}

	{
		acct, err := acctsDb.GetAccount(0, sealevel.SysvarFeesAddr)
		if err != nil {
			panic("nable to get fees sysvar when caching sysvars")
		}
		var fees sealevel.SysvarFees
		decoder := bin.NewBinDecoder(acct.Data)
		fees.MustUnmarshalWithDecoder(decoder)
		sealevel.SysvarCache.Fees.Sysvar = &fees
		sealevel.SysvarCache.Fees.Acct = acct
	}
}

func loadBlockAccountsAndUpdateSysvars(accountsDb *accountsdb.AccountsDb, block *Block) (accounts.Accounts, accounts.Accounts, error) {
	err := resolveAddrTableLookups(accountsDb, block)
	if err != nil {
		return nil, nil, err
	}

	dedupedAccts := extractAndDedupeBlockAccts(block)
	ctx := context.Background()
	slotAccts, err := accountsDb.GetAccountsBatch(ctx, block.Slot, dedupedAccts)
	if err != nil {
		return nil, nil, err
	}

	numAccts := uint64(len(slotAccts))
	accts := accounts.NewMemAccountsWithLen(numAccts)
	parentAccts := accounts.NewMemAccountsWithLen(numAccts)

	for _, acct := range slotAccts {
		err = accts.SetAccountWithoutLock(acct.Key, acct)
		if err != nil {
			return nil, nil, err
		}

		err = parentAccts.SetAccountWithoutLock(acct.Key, acct)
		if err != nil {
			return nil, nil, err
		}
	}

	// load sysvar accounts and assign them to the sysvar cache
	{
		// update and cache clock sysvar
		{
			clockAcct, err := accountsDb.GetAccount(block.Slot, sealevel.SysvarClockAddr)
			if err != nil {
				panic("unable to retrieve clock sysvar when updating clock")
			}
			decoder := bin.NewBinDecoder(clockAcct.Data)
			var clock sealevel.SysvarClock

			err = clock.UnmarshalWithDecoder(decoder)
			if err != nil {
				panic("unable to unmarshal clock sysvar")
			}

			err = updateClockSysvar(&clock, block)
			if err != nil {
				panic(fmt.Sprintf("failed to update clock sysvar: %s", err))
			}

			newClockBytes := clock.MustMarshal()
			copy(clockAcct.Data, newClockBytes)
			sealevel.SysvarCache.Clock.Sysvar = &clock
			sealevel.SysvarCache.Clock.Acct = clockAcct

			err = accts.SetAccountWithoutLock(sealevel.SysvarClockAddr, clockAcct)
			if err != nil {
				panic("unable to set clock sysvar to accts")
			}

			err = parentAccts.SetAccountWithoutLock(sealevel.SysvarClockAddr, clockAcct)
			if err != nil {
				panic("unable to set clock sysvar to accts")
			}
		}

		// update and cache SlotHashes sysvar
		{
			slotHashesAcct, err := accountsDb.GetAccount(block.Slot, sealevel.SysvarSlotHashesAddr)
			if err != nil {
				panic("unable to retrieve slothashes sysvar from acctsdb")
			}

			decoder := bin.NewBinDecoder(slotHashesAcct.Data)
			var slotHashes sealevel.SysvarSlotHashes

			err = slotHashes.UnmarshalWithDecoder(decoder)
			if err != nil {
				panic("unable to unmarshal slothashes sysvar")
			}

			slotHashes.Update(block.Slot, block.ParentSlot, block.ParentBankhash)
			newSlotHashesBytes := slotHashes.MustMarshal()
			copy(slotHashesAcct.Data, newSlotHashesBytes)
			sealevel.SysvarCache.SlotHashes.Sysvar = &slotHashes
			sealevel.SysvarCache.SlotHashes.Acct = slotHashesAcct

			err = accts.SetAccountWithoutLock(sealevel.SysvarSlotHashesAddr, slotHashesAcct)
			if err != nil {
				panic("unable to set slothashes sysvar to accountsdb")
			}

			err = parentAccts.SetAccountWithoutLock(sealevel.SysvarSlotHashesAddr, slotHashesAcct)
			if err != nil {
				panic("unable to set slothashes sysvar to accountsdb")
			}
		}

		// cache RecentBlockhashes sysvar
		{
			recentBlockhashesAcct, err := accountsDb.GetAccount(block.Slot, sealevel.SysvarRecentBlockHashesAddr)
			if err != nil {
				panic("unable to get recentblockhashes")
			}
			decoder := bin.NewBinDecoder(recentBlockhashesAcct.Data)
			var recentBlockhashes sealevel.SysvarRecentBlockhashes
			recentBlockhashes.MustUnmarshalWithDecoder(decoder)
			sealevel.SysvarCache.RecentBlockHashes.Sysvar = &recentBlockhashes
			sealevel.SysvarCache.RecentBlockHashes.Acct = recentBlockhashesAcct

			err = accts.SetAccountWithoutLock(sealevel.SysvarRecentBlockHashesAddr, recentBlockhashesAcct)
			if err != nil {
				panic("unable to set recentblockhashes sysvar to accts")
			}

			err = parentAccts.SetAccountWithoutLock(sealevel.SysvarRecentBlockHashesAddr, recentBlockhashesAcct)
			if err != nil {
				panic("unable to set recentblockhashes sysvar to accts")
			}
		}

		// cache SlotHistory sysvar
		{
			slotHistoryAcct, err := accountsDb.GetAccount(block.Slot, sealevel.SysvarSlotHistoryAddr)
			if err != nil {
				panic("unable to get slothistory")
			}
			decoder := bin.NewBinDecoder(slotHistoryAcct.Data)
			var slotHistory sealevel.SysvarSlotHistory
			slotHistory.MustUnmarshalWithDecoder(decoder)
			sealevel.SysvarCache.SlotHistory.Sysvar = &slotHistory
			sealevel.SysvarCache.SlotHistory.Acct = slotHistoryAcct

			err = accts.SetAccountWithoutLock(sealevel.SysvarSlotHistoryAddr, slotHistoryAcct)
			if err != nil {
				panic("unable to set clock sysvar to accts")
			}

			err = parentAccts.SetAccountWithoutLock(sealevel.SysvarSlotHistoryAddr, slotHistoryAcct)
			if err != nil {
				panic("unable to set clock sysvar to accts")
			}
		}

		// cache StakeHistory sysvar
		{
			stakeHistoryAcct, err := accountsDb.GetAccount(block.Slot, sealevel.SysvarStakeHistoryAddr)
			if err != nil {
				panic("unable to get stakehistory")
			}
			decoder := bin.NewBinDecoder(stakeHistoryAcct.Data)
			var stakeHistory sealevel.SysvarStakeHistory
			stakeHistory.MustUnmarshalWithDecoder(decoder)
			sealevel.SysvarCache.StakeHistory.Sysvar = &stakeHistory
			sealevel.SysvarCache.StakeHistory.Acct = stakeHistoryAcct

			err = accts.SetAccountWithoutLock(sealevel.SysvarStakeHistoryAddr, stakeHistoryAcct)
			if err != nil {
				panic("unable to set stakehistory sysvar to accts")
			}

			err = parentAccts.SetAccountWithoutLock(sealevel.SysvarStakeHistoryAddr, stakeHistoryAcct)
			if err != nil {
				panic("unable to set stakehistory sysvar to accts")
			}
		}

		// cache LastRestartSlot sysvar
		{
			lastRestartSlotAcct, err := accountsDb.GetAccount(block.Slot, sealevel.SysvarLastRestartSlotAddr)
			if err != nil {
				panic("unable to get last restart slot sysvar acct")
			}
			decoder := bin.NewBinDecoder(lastRestartSlotAcct.Data)
			var lastRestartSlot sealevel.SysvarLastRestartSlot
			lastRestartSlot.MustUnmarshalWithDecoder(decoder)
			sealevel.SysvarCache.LastRestartSlot.Sysvar = &lastRestartSlot
			sealevel.SysvarCache.LastRestartSlot.Acct = lastRestartSlotAcct

			err = accts.SetAccountWithoutLock(sealevel.SysvarLastRestartSlotAddr, lastRestartSlotAcct)
			if err != nil {
				panic("unable to set last restart slot sysvar to accts")
			}

			err = accts.SetAccountWithoutLock(sealevel.SysvarLastRestartSlotAddr, lastRestartSlotAcct)
			if err != nil {
				panic("unable to set last restart slot sysvar to accts")
			}
		}
	}

	return accts, parentAccts, nil
}

func scanAndEnableFeatures(acctsDb *accountsdb.AccountsDb, slot uint64, startOfEpoch bool) (*features.Features, []solana.PublicKey) {
	newlyActivatedFeatures := make([]solana.PublicKey, 0)
	acctsToStore := make([]*accounts.Account, 0)

	f := features.NewFeaturesDefault()

	for _, featureGate := range features.AllFeatureGates {
		acct, err := acctsDb.GetAccount(slot, featureGate.Address)
		if err == nil {
			if acct.Owner != a.FeatureAddr {
				continue
			}

			featureAcct := features.UnmarshalFeatureAcct(acct.Data)

			// already activated
			if featureAcct.ActivatedAt != nil && slot >= *featureAcct.ActivatedAt {
				f.EnableFeature(featureGate, *featureAcct.ActivatedAt)
				//mlog.Log.Debugf("enabled *already* enabled feature: %s, %s", featureGate.Name, solana.PublicKeyFromBytes(featureGate.Address[:]))
			}

			if featureAcct.ActivatedAt == nil && startOfEpoch {
				newFeatureAcct := &features.FeatureAcct{ActivatedAt: &slot}
				newFeatureAcctBytes, err := features.MarshalFeatureAcct(newFeatureAcct)
				if err != nil {
					panic(err)
				}

				acct.Data = newFeatureAcctBytes
				acctsToStore = append(acctsToStore, acct)

				newlyActivatedFeatures = append(newlyActivatedFeatures, featureGate.Address)
				f.EnableFeature(featureGate, slot)
				//mlog.Log.Debugf("enabled pending feature: %s, %s", featureGate.Name, solana.PublicKeyFromBytes(featureGate.Address[:]))
			}
		}
	}

	if len(acctsToStore) != 0 {
		err := acctsDb.StoreAccounts(acctsToStore, slot)
		if err != nil {
			panic(err)
		}
	}

	/*mlog.Log.Debugf("scanAndEnableFeatures, modified features:\n")
	for _, feat := range newlyActivatedFeatures {
		mlog.Log.Debugf("feature: %s", feat)
	}*/

	return f, newlyActivatedFeatures
}

func blockRewardRewards(rewards []rpc.BlockReward) *rpc.BlockReward {
	for _, reward := range rewards {
		if string(reward.RewardType) == "Fee" {
			return &reward
		}
	}

	return nil
}

func NewBlockFromBlockResult(blockResult *rpc.GetBlockResult, slot uint64, rpcc *rpcclient.RpcClient) (*Block, error) {
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
	block.LastBlockhash = blockResult.PreviousBlockhash
	block.UnixTimestamp = int64(*blockResult.BlockTime)
	block.BlockHeight = *blockResult.BlockHeight
	block.Rewards = blockResult.Rewards

	if blockResult.NumRewardPartitions != nil {
		block.NumRewardPartitions = *blockResult.NumRewardPartitions
	} else {
		block.NumRewardPartitions = math.MaxUint64
	}

	blockReward := blockRewardRewards(blockResult.Rewards)
	if blockReward != nil {
		block.BlockReward = &BlockRewardsInfo{Leader: blockReward.Pubkey, Lamports: uint64(blockReward.Lamports), PostBalance: blockReward.PostBalance}
	} else {
		if rpcc != nil {
			leaderForSlot, err := rpcc.GetLeaderForSlot(slot)
			if err != nil {
				panic(fmt.Sprintf("unable to get blockreward for slot %d", slot))
			} else {
				block.BlockReward = &BlockRewardsInfo{Leader: leaderForSlot}
			}
		}
	}

	for _, tx := range block.Transactions {
		block.NumSignatures += uint64(tx.Message.Header.NumRequiredSignatures)
	}

	return block, nil
}

func setupInitialVoteAcctsAndStakeAccts(block *Block, snapshotManifest *snapshot.SnapshotManifest) {
	block.VoteTimestamps = make(map[solana.PublicKey]sealevel.BlockTimestamp)
	block.StakeAccts = make(map[solana.PublicKey]bool)
	block.VoteAccts = make(map[solana.PublicKey]uint64)

	for _, va := range snapshotManifest.Bank.Stakes.VoteAccounts {
		ts := sealevel.BlockTimestamp{Slot: va.Value.LastTimestampSlot, Timestamp: va.Value.LastTimestampTs}
		block.VoteTimestamps[va.Key] = ts
		block.VoteAccts[va.Key] = va.Stake
		block.TotalEpochStake += va.Stake
	}

	for _, sa := range snapshotManifest.Bank.Stakes.Delegations {
		block.StakeAccts[sa.Account] = true
	}
}

func configureInitialBlock(block *Block, snapshotManifest *snapshot.SnapshotManifest, epochCtx *ReplayCtx) {
	block.ParentBankhash = snapshotManifest.Bank.Hash
	block.ParentSlot = snapshotManifest.Bank.Slot
	block.EpochAcctsHash = epochCtx.EpochAcctsHash
	setupInitialVoteAcctsAndStakeAccts(block, snapshotManifest)
	snapshotManifest = nil
}

func configureBlock(block *Block, epochCtx *ReplayCtx, lastSlotCtx *sealevel.SlotCtx) {
	copy(block.ParentBankhash[:], lastSlotCtx.FinalBankhash)
	block.StakeAccts = lastSlotCtx.StakeAccts
	block.VoteTimestamps = lastSlotCtx.VoteTimestamps
	block.VoteAccts = lastSlotCtx.VoteAccts
	block.ParentSlot = lastSlotCtx.Slot
	block.EpochAcctsHash = epochCtx.EpochAcctsHash
}

func ReplayBlocks(
	ctx context.Context,
	acctsDb *accountsdb.AccountsDb,
	acctsDbPath string,
	snapshotManifest *snapshot.SnapshotManifest,
	startSlot, endSlot uint64,
	rpcEndpoint string,
	blockDir string,
	txParallelism int,
	dbgOpts *DebugOptions,
	metricsWriter io.Writer,
	cpuprofWriter io.Writer,
) error {
	rpcc := rpcclient.NewRpcClient(rpcEndpoint)
	cacheConstantSysvars(acctsDb)
	epochSchedule := sealevel.SysvarCache.EpochSchedule.Sysvar

	var err error
	var currentSlot uint64
	currentEpoch := epochSchedule.GetEpoch(startSlot)
	var lastSlotCtx *sealevel.SlotCtx
	var partitionedEpochRewardsEnabled bool
	var partitionedRewardsInfo *rewards.PartitionedRewardDistributionInfo
	var featuresActivatedInFirstSlot []solana.PublicKey

	replayCtx := newReplayCtx(snapshotManifest)

	isFirstSlotInEpoch := epochSchedule.FirstSlotInEpoch(currentEpoch) == startSlot
	replayCtx.CurrentFeatures, featuresActivatedInFirstSlot = scanAndEnableFeatures(acctsDb, startSlot, isFirstSlotInEpoch)
	partitionedEpochRewardsEnabled = replayCtx.CurrentFeatures.IsActive(features.EnablePartitionedEpochReward) || replayCtx.CurrentFeatures.IsActive(features.EnablePartitionedEpochRewardsSuperfeature)

	var statsCounter uint64
	var timeAccumulator float64
	var justCrossedEpochBoundary bool

	blockBuffer := 25
	streamChan := make(chan *Block, blockBuffer)
	blockStream := NewBlockStream(rpcc, streamChan, startSlot, endSlot, uint64(blockBuffer), blockDir)
	blockStream.downloadInitialBlocks()
	go blockStream.startAsyncBlockStream()

	if cpuprofWriter != nil {
		pprof.StartCPUProfile(cpuprofWriter)
		defer pprof.StopCPUProfile()
	}
	for block := range streamChan {
		if ctx.Err() != nil {
			mlog.Log.Infof("context cancelled, stopping replay: %v", ctx.Err())
			break
		}
		start := time.Now()

		currentSlot = block.Slot
		if currentSlot == startSlot {
			configureInitialBlock(block, snapshotManifest, replayCtx)
		} else {
			configureBlock(block, replayCtx, lastSlotCtx)
		}

		block.Epoch = epochSchedule.GetEpoch(currentSlot)

		// epoch boundary
		if block.Epoch != currentEpoch {
			mlog.Log.Infof("epoch boundary")

			var newlyActivatedFeatures []solana.PublicKey
			replayCtx.CurrentFeatures, newlyActivatedFeatures = scanAndEnableFeatures(acctsDb, currentSlot, true)
			partitionedEpochRewardsEnabled = replayCtx.CurrentFeatures.IsActive(features.EnablePartitionedEpochReward) || replayCtx.CurrentFeatures.IsActive(features.EnablePartitionedEpochRewardsSuperfeature)

			var updatedPks []solana.PublicKey
			partitionedRewardsInfo, updatedPks, block.VoteAccts = handleEpochTransition(acctsDb, rpcc, partitionedEpochRewardsEnabled, lastSlotCtx, replayCtx, epochSchedule, replayCtx.CurrentFeatures, block, currentEpoch)
			if partitionedEpochRewardsEnabled {
				block.UpdatedAccts = append(block.UpdatedAccts, updatedPks...)
			}
			block.UpdatedAccts = append(block.UpdatedAccts, newlyActivatedFeatures...)
			currentEpoch = block.Epoch
			justCrossedEpochBoundary = true
		} else if currentSlot == startSlot && partitionedEpochRewardsEnabled {
			partitionedRewardsInfo = rewards.DeterminePartitionedStakingRewardsInfo(rpcc, epochSchedule, &replayCtx.Inflation, replayCtx.Capitalization, block.Epoch, block.Epoch-1, currentSlot, replayCtx.SlotsPerYear, replayCtx.CurrentFeatures)
			if startSlot <= partitionedRewardsInfo.LastStakingRewardSlot {
				calculatePartitionedEpochRewardsDuringRewardsWindow(partitionedRewardsInfo, acctsDb, block, epochSchedule, startSlot, currentEpoch, replayCtx.CurrentFeatures)
			}
		}

		block.Features = replayCtx.CurrentFeatures
		block.PartitionedRewardsInfo = partitionedRewardsInfo

		if len(block.Rewards) > 1 && partitionedEpochRewardsEnabled && currentSlot >= partitionedRewardsInfo.FirstStakingRewardSlot && currentSlot <= partitionedRewardsInfo.LastStakingRewardSlot {
			rewardPks := distributePartitionedEpochRewardsForSlot(acctsDb, replayCtx, partitionedRewardsInfo, currentSlot, block.BlockHeight, partitionedRewardsInfo.LastStakingRewardSlot)
			block.UpdatedAccts = append(block.UpdatedAccts, rewardPks...)
		}

		if len(featuresActivatedInFirstSlot) != 0 {
			block.UpdatedAccts = append(block.UpdatedAccts, featuresActivatedInFirstSlot...)
			featuresActivatedInFirstSlot = make([]solana.PublicKey, 0)
		}

		// workaround for skipping the soon-to-be obsolete EAH
		if partitionedEpochRewardsEnabled && block.Slot == partitionedRewardsInfo.EahStopOffsetSlot {
			if replayCtx.HasEpochAcctsHash {
				block.EpochAcctsHash = replayCtx.EpochAcctsHash
			} else {
				block.EahWorkaroundBankhash, err = fetchBankhashForSlot(rpcc, block.Slot)
				if err != nil {
					panic(fmt.Sprintf("unable to fetch bankhash for EAH workaround for slot %d", block.Slot))
				}
				block.HasEahWorkaround = true
			}
		}
		metrics.GlobalBlockReplay.PreprocessBlock.AddTimingSince(start)

		lastSlotCtx, err = ProcessBlock(acctsDb, block, txParallelism, dbgOpts)
		if err != nil {
			mlog.Log.Errorf("error encountered during block replay: %s\n", err)
			break
		} else {
			//mlog.Log.Debugf("block replayed successfully.\n")
		}
		replayCtx.Capitalization -= lastSlotCtx.LamportsBurnt

		slotReplayDuration := time.Since(start)
		mlog.Log.Infof("replayed slot %d - bankhash: %s  (slot replay time: %fs)", block.Slot, base58.Encode(lastSlotCtx.FinalBankhash), slotReplayDuration.Seconds())
		statsd.Count("slot_replays", 1, nil, 1)
		statsd.Distribution("slot_replay_duration_ms.distribution", float64(slotReplayDuration.Nanoseconds())/1e6, nil, 1)
		statsd.Gauge("epoch", float64(block.Epoch), nil, 1)
		statsd.Gauge("slot", float64(block.Slot), nil, 1)
		statsd.Distribution("txs_per_block", float64(len(block.Transactions)), nil, 1)
		if !justCrossedEpochBoundary {
			statsCounter++
			timeAccumulator += slotReplayDuration.Seconds()

			if statsCounter == 100 {
				mlog.Log.Infof("(average slot replay time over 100 slots: %f)\n", timeAccumulator/float64(statsCounter))
				timeAccumulator = 0
				statsCounter = 0
			}
		} else {
			justCrossedEpochBoundary = false
		}
		{
			metrics.GlobalBlockReplay.Slot = block.Slot
			if metricsWriter != nil {
				encoder := json.NewEncoder(metricsWriter)
				err := encoder.Encode(metrics.GlobalBlockReplay)
				if err != nil {
					mlog.Log.Errorf("Error marshaling latencies: %v", err)
				}
			}
			statsd.SendBlockReplayMetrics(metrics.GlobalBlockReplay)
			metrics.GlobalBlockReplay = metrics.BlockReplay{}
		}

	}

	return nil
}

func runIncinerator(slotCtx *sealevel.SlotCtx) {
	incineratorAcct, err := slotCtx.GetAccount(a.IncineratorAddr)
	if err != nil {
		return
	}
	newIncineratorAcct := &accounts.Account{Key: a.IncineratorAddr, Owner: a.SystemProgramAddr, RentEpoch: math.MaxUint64}
	slotCtx.SetAccount(a.IncineratorAddr, newIncineratorAcct)
	slotCtx.LamportsBurnt += incineratorAcct.Lamports
}

func compileWritableAndModifiedAccts(slotCtx *sealevel.SlotCtx, block *Block, rentAccts []*accounts.Account) ([]*accounts.Account, []*accounts.Account) {
	writableAccts := make([]*accounts.Account, 0, len(slotCtx.WritableAccts)+len(block.UpdatedAccts)+len(rentAccts)+4)
	modifiedAccts := make([]*accounts.Account, 0, len(slotCtx.ModifiedAccts)+len(block.UpdatedAccts)+len(rentAccts)+4)

	for pk := range slotCtx.WritableAccts {
		acct, _ := slotCtx.GetAccount(pk)
		writableAccts = append(writableAccts, acct)
	}

	for pk := range slotCtx.ModifiedAccts {
		acct, _ := slotCtx.GetAccount(pk)
		modifiedAccts = append(modifiedAccts, acct)
	}

	for _, pk := range block.UpdatedAccts {
		////mlog.Log.Debugf("adding updated acct for bankhash: %s", pk)
		acct, err := slotCtx.GetAccount(pk)
		if err != nil {
			panic(fmt.Sprintf("unable to fetch %s from accountsdb for inclusion in bankhash", pk))
		}
		writableAccts = append(writableAccts, acct)
		modifiedAccts = append(modifiedAccts, acct)
	}

	writableAccts = append(writableAccts, rentAccts...)
	modifiedAccts = append(modifiedAccts, rentAccts...)

	sysvarAccts := collectAndUpdateSysvarAcctsForAdh(slotCtx)
	writableAccts = append(writableAccts, sysvarAccts...)
	modifiedAccts = append(modifiedAccts, sysvarAccts...)

	return writableAccts, modifiedAccts
}

func newSlotCtx(block *Block, accts accounts.Accounts, parentAccts accounts.Accounts, acctsDb *accountsdb.AccountsDb) *sealevel.SlotCtx {
	slotCtx := &sealevel.SlotCtx{
		Accounts:    accts,
		ParentAccts: parentAccts,
		AccountsDb:  acctsDb,
		Slot:        block.Slot,
		ParentSlot:  block.ParentSlot,
		Epoch:       block.Epoch,

		AcctMapsMu:    &sync.Mutex{},
		ModifiedAccts: make(map[solana.PublicKey]bool),
		WritableAccts: make(map[solana.PublicKey]bool),

		Blockhash:     block.Blockhash,
		LastBlockhash: block.LastBlockhash,
		Replay:        true,
		Features:      block.Features,
		StakeAccts:    block.StakeAccts,

		VoteAccts:       block.VoteAccts,
		VoteTimestampMu: &sync.Mutex{},
		VoteTimestamps:  block.VoteTimestamps,
		TotalEpochStake: block.TotalEpochStake,

		EpochsAcctHash:        block.EpochAcctsHash,
		EahWorkaroundBankhash: block.EahWorkaroundBankhash,

		HasEahWorkaround: block.HasEahWorkaround,

		SerializedParameterArena: SerializedParameterArena,
	}

	return slotCtx
}

func sequentialTxLoop(slotCtx *sealevel.SlotCtx, sigverifyWg *sync.WaitGroup, block *Block, dbgOpts *DebugOptions) fees.TxFeeInfoAccumulator {
	var txFeeAccumulator fees.TxFeeInfoAccumulator
	// process & execute each transaction in turn
	for idx, tx := range block.Transactions {
		//mlog.Log.Debugf("[+] executing transaction %d (slot %d, epoch %d), %s", idx+1, block.Slot, block.Epoch, tx.Signatures[0])

		txMeta := block.TxMetas[idx]
		txFeeInfo, txErr := ProcessTransaction(slotCtx, sigverifyWg, tx, txMeta, dbgOpts, nil)

		if txErr != nil {
			if txMeta.Err == nil && tx.IsVote() {
				panic(fmt.Sprintf("vote tx %s failed in slot %d => bankhash mismatch at slot %d", tx.Signatures[0], block.Slot, block.ParentSlot))
			}
			//mlog.Log.Debugf("tx %d returned error: %s\n", idx+1, txErr)
		}

		// check for success-failure return value divergences
		if txErr == nil && txMeta.Err != nil {
			panic(fmt.Sprintf("tx %s return value divergence: txErr was nil, but onchain err was %+v", tx.Signatures[0], block.TxMetas[idx].Err))
		} else if txErr != nil && txMeta.Err == nil {
			panic(fmt.Sprintf("tx %s return value divergence: txErr was %+v (%s), but onchain err was nil", tx.Signatures[0], txErr, txErr))
		}

		txFeeAccumulator.Add(txFeeInfo)
	}
	return txFeeAccumulator
}

func parallelTxLoop(slotCtx *sealevel.SlotCtx, sigverifyWg *sync.WaitGroup, block *Block, rblock *Block, txParallelism int, dbgOpts *DebugOptions) fees.TxFeeInfoAccumulator {
	do := make(chan int, len(block.Transactions))
	done := make(chan int, len(block.Transactions))
	go TopsortPlannerStream(block, do, done)

	var txFeeAccumulator fees.TxFeeInfoAccumulator
	txFeeInfos := make([]*fees.TxFeeInfo, len(block.Transactions))
	errs := make([]error, len(block.Transactions))
	txDurations := make([]time.Duration, txParallelism)

	//start := time.Now()

	wg := &sync.WaitGroup{}
	wg.Add(txParallelism)
	for i := range txParallelism {
		go func() {
			defer wg.Done()
			for idx := range do {
				txStart := time.Now()
				tx := block.Transactions[idx]
				txFeeInfos[idx], errs[idx] = ProcessTransaction(slotCtx, sigverifyWg, rblock.Transactions[idx], rblock.TxMetas[idx], dbgOpts, sealevel.BorrowedAccountArenas[i])
				txErr := errs[idx]
				// check for success-failure return value divergences
				if txErr == nil && rblock.TxMetas[idx].Err != nil {
					panic(fmt.Sprintf("tx %s return value divergence: txErr was nil, but onchain err was %+v", tx.Signatures[0], rblock.TxMetas[idx].Err))
				} else if txErr != nil && rblock.TxMetas[idx].Err == nil {
					panic(fmt.Sprintf("tx %s return value divergence: txErr was %+v (%s), but onchain err was nil", tx.Signatures[0], txErr, txErr))
				}
				txDurations[i] += time.Since(txStart)
				done <- idx
			}

		}()
	}

	wg.Wait()
	close(done)
	for _, txFeeInfo := range txFeeInfos {
		txFeeAccumulator.Add(txFeeInfo)
	}
	/*wallDuration := time.Since(start)
	var txDuration time.Duration
	for _, workerTxDuration := range txDurations {
		txDuration += workerTxDuration
	}*/
	//mlog.Log.Infof("completed %s parallel tx execution time in %s wall time.", txDuration, wallDuration)

	return txFeeAccumulator
}

func ProcessBlock(acctsDb *accountsdb.AccountsDb, block *Block, txParallelism int, dbgOpts *DebugOptions) (*sealevel.SlotCtx, error) {
	if SerializedParameterArena != nil {
		SerializedParameterArena.Reset()
	}

	var sigverifyWg sync.WaitGroup
	// Each Transaction's sigverify is done asynchronously. Make sure they're all done before we finish this block.
	defer sigverifyWg.Wait()
	start := time.Now()
	//mlog.Log.Debugf("replaying slot %d, epoch %d", block.Slot, block.Epoch)
	unresolvedBlock := &Block{
		Transactions: make([]*solana.Transaction, len(block.Transactions)),
		TxMetas:      make([]*rpc.TransactionMeta, len(block.TxMetas)),
	}
	for i := range block.Transactions {
		unresolvedBlock.Transactions[i] = &solana.Transaction{}
		*(unresolvedBlock.Transactions[i]) = *block.Transactions[i]
		unresolvedBlock.TxMetas[i] = &rpc.TransactionMeta{}
		*(unresolvedBlock.TxMetas[i]) = *block.TxMetas[i]
	}

	start = time.Now()
	// gather up all accounts referenced in the block
	accts, parentAccts, err := loadBlockAccountsAndUpdateSysvars(acctsDb, block)
	if err != nil {
		panic(fmt.Sprintf("unable to load slot accounts and update sysvars: %s", err))
	}
	metrics.GlobalBlockReplay.LoadBlockAccounts.AddTimingSince(start)

	slotCtx := newSlotCtx(block, accts, parentAccts, acctsDb)

	var txFeeAccumulator fees.TxFeeInfoAccumulator
	start = time.Now()
	if txParallelism > 0 {
		txFeeAccumulator = parallelTxLoop(slotCtx, &sigverifyWg, unresolvedBlock, block, txParallelism, dbgOpts)
	} else {
		txFeeAccumulator = sequentialTxLoop(slotCtx, &sigverifyWg, block, dbgOpts)
	}
	metrics.GlobalBlockReplay.TxLoop.AddTimingSince(start)

	start = time.Now()

	// skip leader handling if there are zero transactions in this block
	if block.BlockReward != nil && len(block.Transactions) > 0 {
		// distribute tx fees to the slot leader
		slotCtx.LamportsBurnt = fees.DistributeTxFeesToSlotLeader(acctsDb, slotCtx, block.BlockReward.Leader, &txFeeAccumulator)
		slotCtx.RecordModifiedAcct(block.BlockReward.Leader)
		//mlog.Log.Debugf("from RPC fees for leader: %d, post-balance: %d (%s)", block.BlockReward.Lamports, block.BlockReward.PostBalance, block.BlockReward.Leader)
	}
	metrics.GlobalBlockReplay.Reward.AddTimingSince(start)

	start = time.Now()
	epochSchedule := sealevel.SysvarCache.EpochSchedule.Sysvar
	rentSysvar := sealevel.SysvarCache.Rent.Sysvar
	rentAccts := rent.CollectRentEagerly(slotCtx, rentSysvar, epochSchedule)
	metrics.GlobalBlockReplay.Rent.AddTimingSince(start)

	start = time.Now()
	runIncinerator(slotCtx)
	metrics.GlobalBlockReplay.RunIncinerator.AddTimingSince(start)

	start = time.Now()
	writableAccts, modifiedAccts := compileWritableAndModifiedAccts(slotCtx, block, rentAccts)
	if len(modifiedAccts) > 0 {
		//mlog.Log.Debugf("updating accountsdb")
		err = acctsDb.StoreAccounts(modifiedAccts, slotCtx.Slot)
	}
	metrics.GlobalBlockReplay.BlockUpdateAccounts.AddTimingSince(start)

	//mlog.Log.Debugf("\ncalculating accts delta hash for %d eligible accounts. len of rentAccts = %d", len(writableAccts), len(rentAccts))

	// EAH workaround
	if slotCtx.HasEahWorkaround {
		slotCtx.FinalBankhash = slotCtx.EahWorkaroundBankhash
		return slotCtx, err
	}

	// calculate ADH and bankhash
	start = time.Now()
	acctDeltaHash := calculateAcctsDeltaHash(writableAccts)
	metrics.GlobalBlockReplay.AccountsDeltaHash.AddTimingSince(start)
	start = time.Now()
	slotCtx.FinalBankhash = calculateBankHash(slotCtx, acctDeltaHash, block.ParentBankhash, block.NumSignatures, block.Blockhash)
	metrics.GlobalBlockReplay.BankHash.AddTimingSince(start)

	return slotCtx, err
}
