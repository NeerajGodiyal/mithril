package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	a "github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/arena"
	"github.com/Overclock-Validator/mithril/pkg/bankhash"
	"github.com/Overclock-Validator/mithril/pkg/base58"
	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/blockstream"
	"github.com/Overclock-Validator/mithril/pkg/epochstakes"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/fees"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/metrics"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/rent"
	"github.com/Overclock-Validator/mithril/pkg/rewards"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/Overclock-Validator/mithril/pkg/rpcserver"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/snapshot"
	"github.com/Overclock-Validator/mithril/pkg/statsd"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/panjf2000/ants/v2"
)

var SerializedParameterArena *arena.Arena[byte]

func resolveAddrTableLookups(accountsDb *accountsdb.AccountsDb, block *b.Block) error {
	tables := make(map[solana.PublicKey]solana.PublicKeySlice)

	for _, tx := range block.Transactions {
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

func extractAndDedupeBlockAccts(block *b.Block) []solana.PublicKey {
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

	for _, pk := range block.UpdatedAccts {
		pubkeyMap[pk] = struct{}{}
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
			panic("unable to get fees sysvar when caching sysvars")
		}
		var fees sealevel.SysvarFees
		decoder := bin.NewBinDecoder(acct.Data)
		fees.MustUnmarshalWithDecoder(decoder)
		sealevel.SysvarCache.Fees.Sysvar = &fees
		sealevel.SysvarCache.Fees.Acct = acct
	}

	{
		acct, err := acctsDb.GetAccount(0, sealevel.SysvarEpochRewardsAddr)
		if err != nil {
			panic("unable to get fees sysvar when caching sysvars")
		}
		var rewards sealevel.SysvarEpochRewards
		decoder := bin.NewBinDecoder(acct.Data)
		rewards.MustUnmarshalWithDecoder(decoder)
		sealevel.SysvarCache.EpochRewards.Sysvar = &rewards
		sealevel.SysvarCache.EpochRewards.Acct = acct
	}
}

func loadBlockAccountsAndUpdateSysvars(accountsDb *accountsdb.AccountsDb, block *b.Block) (accounts.Accounts, accounts.Accounts, error) {
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

	block.FeeRateGovernor = sealevel.NewFeeRateGovernorDerived(block.PrevFeeRateGovernor, block.PrevNumSignatures)
	if block.FeeRateGovernor.PrevLamportsPerSignature == 0 {
		block.FeeRateGovernor.PrevLamportsPerSignature = block.InitialPreviousLamportsPerSignature
	}

	// load sysvar accounts and assign them to the sysvar cache
	{
		// update and cache clock sysvar
		{
			clockAcct, err := accountsDb.GetAccount(block.Slot, sealevel.SysvarClockAddr)
			if err != nil {
				panic("unable to retrieve clock sysvar when updating clock")
			}

			err = parentAccts.SetAccountWithoutLock(sealevel.SysvarClockAddr, clockAcct.Clone())
			if err != nil {
				panic("unable to set clock sysvar to accts")
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
		}

		// update and cache SlotHashes sysvar
		{
			slotHashesAcct, err := accountsDb.GetAccount(block.Slot, sealevel.SysvarSlotHashesAddr)
			if err != nil {
				panic("unable to retrieve slothashes sysvar from acctsdb")
			}

			err = parentAccts.SetAccountWithoutLock(sealevel.SysvarSlotHashesAddr, slotHashesAcct.Clone())
			if err != nil {
				panic("unable to set slothashes sysvar to accountsdb")
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
		}

		// cache RecentBlockhashes sysvar
		{
			recentBlockhashesAcct, err := accountsDb.GetAccount(block.Slot, sealevel.SysvarRecentBlockHashesAddr)
			if err != nil {
				panic("unable to get recentblockhashes")
			}

			err = parentAccts.SetAccountWithoutLock(sealevel.SysvarRecentBlockHashesAddr, recentBlockhashesAcct.Clone())
			if err != nil {
				panic("unable to set recentblockhashes sysvar to accts")
			}

			if sealevel.SysvarCache.RecentBlockHashes.Sysvar == nil {
				decoder := bin.NewBinDecoder(recentBlockhashesAcct.Data)
				var recentBlockhashes sealevel.SysvarRecentBlockhashes
				recentBlockhashes.MustUnmarshalWithDecoder(decoder)
				sealevel.SysvarCache.RecentBlockHashes.Sysvar = &recentBlockhashes
				sealevel.SysvarCache.RecentBlockHashes.Acct = recentBlockhashesAcct
			}

			err = accts.SetAccountWithoutLock(sealevel.SysvarRecentBlockHashesAddr, recentBlockhashesAcct)
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

			err = parentAccts.SetAccountWithoutLock(sealevel.SysvarSlotHistoryAddr, slotHistoryAcct.Clone())
			if err != nil {
				panic("unable to set slothistory sysvar to accts")
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
		}

		// cache StakeHistory sysvar
		{
			stakeHistoryAcct, err := accountsDb.GetAccount(block.Slot, sealevel.SysvarStakeHistoryAddr)
			if err != nil {
				panic("unable to get stakehistory")
			}

			var setStakeHistoryParent bool
			if len(block.EpochUpdatedAccts) != 0 {
				for _, a := range block.ParentEpochUpdatedAccts {
					if a != nil {
						if a.Key == sealevel.SysvarStakeHistoryAddr {
							err = parentAccts.SetAccountWithoutLock(sealevel.SysvarStakeHistoryAddr, a.Clone())
							if err != nil {
								panic("unable to set stakehistory sysvar to accts")
							}
							setStakeHistoryParent = true
						}
					}
				}
			}

			if !setStakeHistoryParent {
				err = parentAccts.SetAccountWithoutLock(sealevel.SysvarStakeHistoryAddr, stakeHistoryAcct.Clone())
				if err != nil {
					panic("unable to set stakehistory sysvar to accts")
				}
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
		}

		// cache LastRestartSlot sysvar
		{
			lastRestartSlotAcct, err := accountsDb.GetAccount(block.Slot, sealevel.SysvarLastRestartSlotAddr)
			if err != nil {
				panic("unable to get last restart slot sysvar acct")
			}

			err = parentAccts.SetAccountWithoutLock(sealevel.SysvarLastRestartSlotAddr, lastRestartSlotAcct.Clone())
			if err != nil {
				panic("unable to set last restart slot sysvar to accts")
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
		}
	}

	for idx, acct := range block.EpochUpdatedAccts {
		if acct == nil {
			continue
		}

		err = accts.SetAccountWithoutLock(acct.Key, acct.Clone())
		if err != nil {
			panic("unable to setup epoch transition modified acct")
		}

		parentAcct := block.ParentEpochUpdatedAccts[idx].Clone()
		err = parentAccts.SetAccountWithoutLock(acct.Key, parentAcct)
		if err != nil {
			panic("unable to setup epoch transition modified acct")
		}
	}

	return accts, parentAccts, nil
}

func scanAndEnableFeatures(acctsDb *accountsdb.AccountsDb, slot uint64, startOfEpoch bool) (*features.Features, []*accounts.Account, []*accounts.Account) {
	parentNewlyActivatedFeatureAccts := make([]*accounts.Account, 0)
	newlyActivatedFeatureAccts := make([]*accounts.Account, 0)

	f := features.NewFeaturesDefault()

	for _, featureGate := range features.AllFeatureGates {
		acct, err := acctsDb.GetAccount(slot, featureGate.Address)
		if err == nil {
			if acct.Owner != a.FeatureAddr {
				continue
			}
			parentNewlyActivatedFeatureAccts = append(parentNewlyActivatedFeatureAccts, acct.Clone())

			featureAcct := features.UnmarshalFeatureAcct(acct.Data)

			// already activated
			if featureAcct.ActivatedAt != nil && slot >= *featureAcct.ActivatedAt {
				f.EnableFeature(featureGate, *featureAcct.ActivatedAt)
			}

			if featureAcct.ActivatedAt == nil && startOfEpoch {
				newFeatureAcct := &features.FeatureAcct{ActivatedAt: &slot}
				newFeatureAcctBytes, err := features.MarshalFeatureAcct(newFeatureAcct)
				if err != nil {
					panic(err)
				}

				acct.Data = newFeatureAcctBytes
				newlyActivatedFeatureAccts = append(newlyActivatedFeatureAccts, acct)

				f.EnableFeature(featureGate, slot)
			}
		}
	}

	if len(newlyActivatedFeatureAccts) != 0 {
		err := acctsDb.StoreAccounts(newlyActivatedFeatureAccts, slot)
		if err != nil {
			panic(err)
		}
	}

	return f, newlyActivatedFeatureAccts, parentNewlyActivatedFeatureAccts
}

func setupInitialVoteAcctsAndStakeAccts(acctsDb *accountsdb.AccountsDb, block *b.Block, snapshotManifest *snapshot.SnapshotManifest) {
	block.VoteTimestamps = make(map[solana.PublicKey]sealevel.BlockTimestamp)
	block.VoteAccts = make(map[solana.PublicKey]uint64)

	var wg sync.WaitGroup
	voteAcctWorkerPool, _ := ants.NewPoolWithFunc(1024, func(i interface{}) {
		defer wg.Done()

		pk := i.(solana.PublicKey)
		voteAcct, err := acctsDb.GetAccount(block.Slot, pk)
		if err == nil {
			versionedVoteState, err := sealevel.UnmarshalVersionedVoteState(voteAcct.Data)
			if err == nil {
				global.PutVoteCacheItem(pk, versionedVoteState)
			}
		}
	})

	stakeAcctWorkerPool, _ := ants.NewPoolWithFunc(1024, func(i interface{}) {
		defer wg.Done()

		sa := i.(snapshot.DelegationPair)
		var creditsObserved uint64

		stakeAcct, err := acctsDb.GetAccount(block.Slot, sa.Account)
		if err == nil {
			stakeState, err := sealevel.UnmarshalStakeState(stakeAcct.Data)
			if err == nil {
				creditsObserved = stakeState.Stake.Stake.CreditsObserved
			}
		}
		global.PutStakeCacheItem(sa.Account,
			&sealevel.Delegation{VoterPubkey: sa.Delegation.VoterPubkey,
				StakeLamports:      sa.Delegation.Stake,
				ActivationEpoch:    sa.Delegation.ActivationEpoch,
				DeactivationEpoch:  sa.Delegation.DeactivationEpoch,
				WarmupCooldownRate: sa.Delegation.WarmupCooldownRate,
				CreditsObserved:    creditsObserved})

	})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, va := range snapshotManifest.Bank.Stakes.VoteAccounts {
			ts := sealevel.BlockTimestamp{Slot: va.Value.LastTimestampSlot, Timestamp: va.Value.LastTimestampTs}
			block.VoteTimestamps[va.Key] = ts
			block.VoteAccts[va.Key] = va.Stake
			block.TotalEpochStake += va.Stake

			wg.Add(1)
			voteAcctWorkerPool.Invoke(va.Key)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, sa := range snapshotManifest.Bank.Stakes.Delegations {
			wg.Add(1)
			stakeAcctWorkerPool.Invoke(sa)
		}
	}()

	wg.Wait()
	stakeAcctWorkerPool.Release()
	ants.Release()
}

func configureInitialBlock(acctsDb *accountsdb.AccountsDb,
	block *b.Block,
	snapshotManifest *snapshot.SnapshotManifest,
	epochCtx *ReplayCtx,
	epochSchedule *sealevel.SysvarEpochSchedule,
	rpcClient *rpcclient.RpcClient) {

	block.ParentBankhash = snapshotManifest.Bank.Hash
	block.ParentSlot = snapshotManifest.Bank.Slot
	block.AcctsLtHash = snapshotManifest.LtHash
	block.EpochAcctsHash = epochCtx.EpochAcctsHash
	block.PrevFeeRateGovernor = &snapshotManifest.Bank.FeeRateGovernor
	block.PrevNumSignatures = snapshotManifest.Bank.SignatureCount
	block.InitialPreviousLamportsPerSignature = snapshotManifest.LamportsPerSignature

	setupInitialVoteAcctsAndStakeAccts(acctsDb, block, snapshotManifest)
	configureGlobalCtx(block)

	if global.ManageLeaderSchedule() {
		prepareLeaderSchedule(block.Epoch, epochSchedule, rpcClient)
		var exists bool
		block.Leader, exists = global.LeaderForSlot(block.Slot)
		if !exists {
			panic(fmt.Sprintf("unable to find leader for slot %d", block.Slot))
		}
	}

	if global.ManageBlockHeight() {
		block.BlockHeight = global.BlockHeight()
	}

	// we use the RecentBlockhashes sysvar to determine whether a tx has a blockhash of acceptable
	// age, but due to how Agave's BlockhashQueue is implemented, the latest 151 blockhashes
	// are valid, rather than 150. we therefore need the last blockhash that was evicted from
	// the RecentBlockhashes sysvar, and that's what the code below does.
	ages := snapshotManifest.Bank.BlockhashQueue.HashAndAge
	sort.Slice(ages, func(i, j int) bool { return ages[i].Val.HashIndex > ages[j].Val.HashIndex })
	block.LatestEvictedBlockhash = ages[150].Key

	snapshotManifest = nil
}

func configureBlock(block *b.Block, epochCtx *ReplayCtx, lastSlotCtx *sealevel.SlotCtx) {
	copy(block.ParentBankhash[:], lastSlotCtx.FinalBankhash)
	block.AcctsLtHash = lastSlotCtx.AcctsLtHash
	block.VoteTimestamps = lastSlotCtx.VoteTimestamps
	block.VoteAccts = lastSlotCtx.VoteAccts
	block.ParentSlot = lastSlotCtx.Slot
	block.LatestEvictedBlockhash = lastSlotCtx.LatestEvictedBlockhash
	block.EpochAcctsHash = epochCtx.EpochAcctsHash
	block.PrevFeeRateGovernor = lastSlotCtx.FeeRateGovernor
	block.PrevNumSignatures = lastSlotCtx.NumSignatures
	block.TotalEpochStake = lastSlotCtx.TotalEpochStake

	if global.ManageLeaderSchedule() {
		block.LastBlockhash = lastSlotCtx.Blockhash
	}

	configureGlobalCtx(block)

	if global.ManageLeaderSchedule() {
		var exists bool
		block.Leader, exists = global.LeaderForSlot(block.Slot)
		if !exists {
			panic(fmt.Sprintf("unable to find leader for slot %d", block.Slot))
		}
	}

	if global.ManageBlockHeight() {
		block.BlockHeight = global.BlockHeight()
	}
}

func configureGlobalCtx(block *b.Block) {
	global.SetSlot(block.Slot)
	global.SetEpoch(block.Epoch)
	global.SetLatestBlockHash(block.LastBlockhash)
	global.SetBlockHeight(block.BlockHeight)
}

func buildInitialEpochStakesCache(snapshotManifest *snapshot.SnapshotManifest) {
	for _, epochStake := range snapshotManifest.VersionedEpochStakes {
		if epochStake.Epoch == snapshotManifest.Bank.Epoch {
			for _, entry := range epochStake.Val.EpochAuthorizedVoters {
				global.PutEpochAuthorizedVoter(entry.Key, entry.Val)
			}
		}

		global.PutEpochTotalStake(epochStake.Epoch, epochStake.Val.TotalStake)
		for _, entry := range epochStake.Val.Stakes.VoteAccounts {
			voteAcct := &epochstakes.VoteAccount{Lamports: entry.Value.Lamports,
				NodePubkey:        entry.Value.NodePubkey,
				LastTimestampTs:   entry.Value.LastTimestampTs,
				LastTimestampSlot: entry.Value.LastTimestampSlot,
				Owner:             entry.Value.Owner,
				Executable:        entry.Value.Executable,
				RentEpoch:         entry.Value.RentEpoch}
			global.PutEpochStakesEntry(epochStake.Epoch, entry.Key, entry.Stake, voteAcct)
		}
	}
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
	isLive bool,
	useOvercast bool,
	dbgOpts *DebugOptions,
	metricsWriter io.Writer,
	rpcServer *rpcserver.RpcServer,
) error {
	rpcc := rpcclient.NewRpcClient(rpcEndpoint)
	cacheConstantSysvars(acctsDb)
	epochSchedule := sealevel.SysvarCache.EpochSchedule.Sysvar

	global.SetCalcUnixTimeForClockSysvar(true)
	global.SetManageBlockHeight(true)

	if isLive {
		global.SetManageLeaderSchedule(true)
	}

	var err error
	var currentSlot uint64
	currentEpoch := epochSchedule.GetEpoch(startSlot)
	var lastSlotCtx *sealevel.SlotCtx
	var partitionedEpochRewardsEnabled bool
	var partitionedRewardsInfo *rewards.PartitionedRewardDistributionInfo
	var featuresActivatedInFirstSlot []*accounts.Account
	var parentFeaturesActivatedInFirstSlot []*accounts.Account

	replayCtx := newReplayCtx(snapshotManifest)

	global.IncrTransactionCount(snapshotManifest.Bank.TransactionCount)
	isFirstSlotInEpoch := epochSchedule.FirstSlotInEpoch(currentEpoch) == startSlot
	replayCtx.CurrentFeatures, featuresActivatedInFirstSlot, parentFeaturesActivatedInFirstSlot = scanAndEnableFeatures(acctsDb, startSlot, isFirstSlotInEpoch)
	partitionedEpochRewardsEnabled = replayCtx.CurrentFeatures.IsActive(features.EnablePartitionedEpochReward) || replayCtx.CurrentFeatures.IsActive(features.EnablePartitionedEpochRewardsSuperfeature)

	buildInitialEpochStakesCache(snapshotManifest)
	//forkChoice, err := forkchoice.NewForkChoiceService(currentEpoch, global.EpochStakes(currentEpoch), global.EpochTotalStake(currentEpoch), global.EpochAuthorizedVoters(), 4)
	//forkChoice.Start()
	//global.SetForkChoice(forkChoice)

	var statsCounter uint64
	var timeAccumulator float64
	var justCrossedEpochBoundary bool

	var opts *blockstream.BlockSourceOpts
	if useOvercast {
		opts = &blockstream.BlockSourceOpts{
			SourceType: blockstream.BlockSourceOvercast,
			RpcClient:  rpcc,
			StartSlot:  startSlot,
			EndSlot:    endSlot,
			BlockDir:   blockDir,
		}
	} else {
		opts = &blockstream.BlockSourceOpts{
			SourceType: blockstream.BlockSourceRpc,
			RpcClient:  rpcc,
			StartSlot:  startSlot,
			EndSlot:    endSlot,
			BlockDir:   blockDir,
		}
	}
	blockStream := blockstream.NewBlockSource(opts)

	if !isLive {
		blockStream.DownloadInitialBlocks()
	}
	go blockStream.Start()

	for {
		block := blockStream.NextBlock()
		if block == nil {
			break
		}

		if ctx.Err() != nil {
			mlog.Log.Infof("context cancelled, stopping replay: %v", ctx.Err())
			break
		}
		start := time.Now()
		currentSlot = block.Slot
		block.Epoch = epochSchedule.GetEpoch(currentSlot)
		if currentSlot == startSlot {
			configureInitialBlock(acctsDb, block, snapshotManifest, replayCtx, epochSchedule, rpcc)
		} else {
			configureBlock(block, replayCtx, lastSlotCtx)
		}

		// epoch boundary
		if block.Epoch != currentEpoch {
			mlog.Log.Infof("epoch boundary, %d -> %d", currentEpoch, currentEpoch+1)

			var newlyActivatedFeatures, parentNewlyActivatedFeatures []*accounts.Account
			replayCtx.CurrentFeatures, newlyActivatedFeatures, parentNewlyActivatedFeatures = scanAndEnableFeatures(acctsDb, currentSlot, true)
			partitionedEpochRewardsEnabled = replayCtx.CurrentFeatures.IsActive(features.EnablePartitionedEpochReward) || replayCtx.CurrentFeatures.IsActive(features.EnablePartitionedEpochRewardsSuperfeature)
			partitionedRewardsInfo = handleEpochTransition(acctsDb, rpcc, partitionedEpochRewardsEnabled, lastSlotCtx, replayCtx, epochSchedule, replayCtx.CurrentFeatures, block, currentEpoch)
			currentEpoch = block.Epoch
			justCrossedEpochBoundary = true
			if len(newlyActivatedFeatures) != 0 {
				block.EpochUpdatedAccts = append(block.EpochUpdatedAccts, newlyActivatedFeatures...)
				block.ParentEpochUpdatedAccts = append(block.ParentEpochUpdatedAccts, parentNewlyActivatedFeatures...)
			}
			if len(featuresActivatedInFirstSlot) != 0 {
				block.EpochUpdatedAccts = append(block.EpochUpdatedAccts, featuresActivatedInFirstSlot...)
				block.ParentEpochUpdatedAccts = append(block.ParentEpochUpdatedAccts, parentFeaturesActivatedInFirstSlot...)
				featuresActivatedInFirstSlot = nil
				parentFeaturesActivatedInFirstSlot = nil
			}

			if global.ManageLeaderSchedule() {
				prepareLeaderSchedule(block.Epoch, epochSchedule, rpcc)
			}
		} else if currentSlot == startSlot && partitionedEpochRewardsEnabled {
			if rewards.IsWithinRewardsPeriod(block.Epoch, currentSlot, epochSchedule) {
				panic("bootstrapping during epoch rewards period is currently unsupported.")
			}
		}

		block.Features = replayCtx.CurrentFeatures

		if len(block.Rewards) > 1 && partitionedEpochRewardsEnabled && currentSlot >= partitionedRewardsInfo.FirstStakingRewardSlot && currentSlot <= partitionedRewardsInfo.LastStakingRewardSlot {
			distributedAccts, parentDistributedAccts := distributePartitionedEpochRewardsForSlot(acctsDb, replayCtx, partitionedRewardsInfo, currentSlot, block.BlockHeight, partitionedRewardsInfo.LastStakingRewardSlot)
			block.EpochUpdatedAccts = append(block.EpochUpdatedAccts, distributedAccts...)
			block.ParentEpochUpdatedAccts = append(block.ParentEpochUpdatedAccts, parentDistributedAccts...)
		}

		// workaround for skipping the soon-to-be obsolete EAH.
		// EAH is now obsolete as per the introduction of the accounts lattice hash.
		// This remains in place for old slots.
		if !block.Features.IsActive(features.AccountsLtHash) {
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
		}
		metrics.GlobalBlockReplay.PreprocessBlock.AddTimingSince(start)

		lastSlotCtx, err = ProcessBlock(acctsDb, block, txParallelism, dbgOpts)
		if err != nil {
			mlog.Log.Errorf("error encountered during block replay: %s\n", err)
			break
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

func compileWritableAndModifiedAccts(slotCtx *sealevel.SlotCtx, block *b.Block, rentAccts []*accounts.Account) ([]*accounts.Account, []*accounts.Account) {
	writableAccts := make([]*accounts.Account, 0, len(slotCtx.WritableAccts)+len(block.UpdatedAccts)+len(rentAccts)+4)
	modifiedAccts := make([]*accounts.Account, 0, len(slotCtx.ModifiedAccts)+len(block.UpdatedAccts)+len(rentAccts)+4)
	alreadyAdded := make(map[solana.PublicKey]bool)

	for pk := range slotCtx.WritableAccts {
		acct, _ := slotCtx.GetAccount(pk)
		writableAccts = append(writableAccts, acct)
		alreadyAdded[pk] = true
	}

	for pk := range slotCtx.ModifiedAccts {
		acct, err := slotCtx.GetAccount(pk)
		if err != nil {
			mlog.Log.Infof("compileWritableAndModifiedAccts: unable to get %s", acct.Key)
		}
		modifiedAccts = append(modifiedAccts, acct)
	}

	for _, eua := range block.EpochUpdatedAccts {
		if eua == nil {
			continue
		}
		if _, exists := alreadyAdded[eua.Key]; exists {
			continue
		}
		acct, err := slotCtx.GetAccount(eua.Key)
		if err != nil {
			acct, err = slotCtx.GetAccountFromAccountsDb(eua.Key)
			if err != nil {
				panic(fmt.Sprintf("unable to fetch %s from neither SlotCtx nor accountsdb for inclusion in bankhash in slot %d", eua.Key, slotCtx.Slot))
			}
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

func newSlotCtx(block *b.Block, accts accounts.Accounts, parentAccts accounts.Accounts, acctsDb *accountsdb.AccountsDb) *sealevel.SlotCtx {
	slotCtx := &sealevel.SlotCtx{
		Accounts:        accts,
		ParentAccts:     parentAccts,
		AccountsDb:      acctsDb,
		Slot:            block.Slot,
		ParentSlot:      block.ParentSlot,
		Epoch:           block.Epoch,
		FeeRateGovernor: block.FeeRateGovernor,
		NumSignatures:   block.NumSignatures,

		AcctMapsMu:    &sync.Mutex{},
		ModifiedAccts: make(map[solana.PublicKey]bool),
		WritableAccts: make(map[solana.PublicKey]bool),

		Blockhash:              block.Blockhash,
		LastBlockhash:          block.LastBlockhash,
		LatestEvictedBlockhash: block.LatestEvictedBlockhash,
		Replay:                 true,
		Features:               block.Features,
		AcctsLtHash:            block.AcctsLtHash,

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

func sequentialTxLoop(slotCtx *sealevel.SlotCtx, sigverifyWg *sync.WaitGroup, block *b.Block, dbgOpts *DebugOptions) fees.TxFeeInfoAccumulator {
	var txFeeAccumulator fees.TxFeeInfoAccumulator
	// process & execute each transaction in turn
	for idx, tx := range block.Transactions {
		var txMeta *rpc.TransactionMeta
		if block.TxMetas != nil {
			txMeta = block.TxMetas[idx]
		}
		txFeeInfo, txErr := ProcessTransaction(slotCtx, sigverifyWg, tx, txMeta, dbgOpts, nil)

		if txErr != nil {
			if txMeta.Err == nil && tx.IsVote() {
				panic(fmt.Sprintf("vote tx %s failed in slot %d => bankhash mismatch at slot %d", tx.Signatures[0], block.Slot, block.ParentSlot))
			}
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

func parallelTxLoop(slotCtx *sealevel.SlotCtx, sigverifyWg *sync.WaitGroup, block *b.Block, rblock *b.Block, txParallelism int, dbgOpts *DebugOptions) fees.TxFeeInfoAccumulator {
	var txFeeAccumulator fees.TxFeeInfoAccumulator
	txFeeInfos := make([]*fees.TxFeeInfo, len(block.Transactions))
	errs := make([]error, len(block.Transactions))
	txDurations := make([]time.Duration, txParallelism)

	if rblock.FromOvercast {
		wg := &sync.WaitGroup{}
		workerPool, _ := ants.NewPoolWithFunc(txParallelism, func(i interface{}) {
			defer wg.Done()
			idx := i.(uint64)
			txFeeInfos[idx], errs[idx] = ProcessTransaction(slotCtx, sigverifyWg, rblock.Transactions[idx], nil, dbgOpts, nil)
		})

		for _, entry := range rblock.Entries {
			for _, txIdx := range entry.Indices {
				wg.Add(1)
				workerPool.Invoke(txIdx)
			}
			wg.Wait()
		}
	} else {
		do := make(chan int, len(block.Transactions))
		done := make(chan int, len(block.Transactions))
		go TopsortPlannerStream(block, do, done)

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
					if rblock.TxMetas != nil && txErr == nil && rblock.TxMetas[idx].Err != nil {
						panic(fmt.Sprintf("tx %s return value divergence: txErr was nil, but onchain err was %+v", tx.Signatures[0], rblock.TxMetas[idx].Err))
					} else if rblock.TxMetas != nil && txErr != nil && rblock.TxMetas[idx].Err == nil {
						panic(fmt.Sprintf("tx %s return value divergence: txErr was %+v (%s), but onchain err was nil", tx.Signatures[0], txErr, txErr))
					}
					txDurations[i] += time.Since(txStart)
					done <- idx
				}

			}()
		}

		wg.Wait()
		close(done)
	}

	for _, txFeeInfo := range txFeeInfos {
		txFeeAccumulator.Add(txFeeInfo)
	}

	return txFeeAccumulator
}

func ProcessBlock(acctsDb *accountsdb.AccountsDb, block *b.Block, txParallelism int, dbgOpts *DebugOptions) (*sealevel.SlotCtx, error) {
	if SerializedParameterArena != nil {
		SerializedParameterArena.Reset()
	}

	var sigverifyWg sync.WaitGroup
	defer sigverifyWg.Wait()
	start := time.Now()
	unresolvedBlock := &b.Block{
		Transactions: make([]*solana.Transaction, len(block.Transactions)),
		TxMetas:      make([]*rpc.TransactionMeta, len(block.TxMetas)),
	}
	for i := range block.Transactions {
		unresolvedBlock.Transactions[i] = &solana.Transaction{}
		*(unresolvedBlock.Transactions[i]) = *block.Transactions[i]
		if unresolvedBlock.TxMetas != nil && !block.FromOvercast {
			unresolvedBlock.TxMetas[i] = &rpc.TransactionMeta{}
			*(unresolvedBlock.TxMetas[i]) = *block.TxMetas[i]
		}
	}

	start = time.Now()
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

	// distribute tx fees to the slot leader
	// skip leader handling if there are zero transactions in this block
	if !global.ManageLeaderSchedule() && block.BlockReward != nil && len(block.Transactions) > 0 {
		slotCtx.LamportsBurnt = fees.DistributeTxFeesToSlotLeader(acctsDb, slotCtx, block.BlockReward.Leader, &txFeeAccumulator)
		slotCtx.RecordModifiedAcct(block.BlockReward.Leader)
	} else if global.ManageLeaderSchedule() && len(block.Transactions) > 0 {
		slotCtx.LamportsBurnt = fees.DistributeTxFeesToSlotLeader(acctsDb, slotCtx, block.Leader, &txFeeAccumulator)
		slotCtx.RecordModifiedAcct(block.Leader)
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

	start = time.Now()
	slotCtx.FinalBankhash = bankhash.CalculateBankHash(slotCtx, writableAccts, modifiedAccts, block.ParentBankhash, block.NumSignatures, block.Blockhash)
	metrics.GlobalBlockReplay.BankHash.AddTimingSince(start)

	/*confirmed := global.BankhashConfirmedForSlot(slotCtx.Slot, solana.HashFromBytes(slotCtx.FinalBankhash))
	for confirmed == forkchoice.BankhashNeedWait {
		confirmed = global.BankhashConfirmedForSlot(slotCtx.Slot, solana.HashFromBytes(slotCtx.FinalBankhash))
	}*/

	// this slot should be skipped.
	/*if confirmed == forkchoice.BankhashNoSupermajority {
		// TODO: return signal that slot should be skipped
	}*/

	if len(modifiedAccts) > 0 {
		err = acctsDb.StoreAccounts(modifiedAccts, slotCtx.Slot)
	}
	metrics.GlobalBlockReplay.BlockUpdateAccounts.AddTimingSince(start)

	// EAH workaround
	if slotCtx.HasEahWorkaround {
		slotCtx.FinalBankhash = slotCtx.EahWorkaroundBankhash
		return slotCtx, err
	}

	err = acctsDb.StoreBankHashForSlot(slotCtx.Slot, slotCtx.FinalBankhash)
	if err != nil {
		mlog.Log.Infof("unable to store bankhash for slot %d", slotCtx.Slot)
	}

	global.IncrTransactionCount(uint64(len(block.Transactions)))
	return slotCtx, err
}
