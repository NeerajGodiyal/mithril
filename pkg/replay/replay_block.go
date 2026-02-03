package replay

import (
	"context"
	"fmt"
	"sync"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/fees"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

// ReplayBlockResult contains the results of replaying a block.
type ReplayBlockResult struct {
	TotalFees   uint64
	Divergences int
}

// ReplayBlock replays a block using the provided AccountsDb.
// This is a simplified version of ProcessBlock for offline replay.
func ReplayBlock(db *accountsdb.AccountsDb, blk *b.Block, parallelism int) (*ReplayBlockResult, error) {
	// Set block's epoch if not already set (needed for clock sysvar update)
	if blk.Epoch == 0 {
		blk.Epoch = blk.Slot / 432000 // Approximate mainnet slots per epoch
	}

	// Build SlotCtx
	slotCtx, err := buildSlotCtxForReplay(db, blk)
	if err != nil {
		return nil, fmt.Errorf("failed to build SlotCtx: %w", err)
	}

	// Restore SysvarCache (must pass blk for sysvar updates)
	if err := restoreSysvarCacheForReplay(slotCtx, blk); err != nil {
		return nil, fmt.Errorf("failed to restore SysvarCache: %w", err)
	}

	// Run TxLoop
	dbgOpts := &DebugOptions{}
	var txFeeAccumulator fees.TxFeeInfoAccumulator
	var divergences int

	if parallelism > 0 {
		txFeeAccumulator, divergences = replayBlockParallel(slotCtx, blk, parallelism, dbgOpts)
	} else {
		txFeeAccumulator, divergences = replayBlockSequential(slotCtx, blk, dbgOpts)
	}

	return &ReplayBlockResult{
		TotalFees:   txFeeAccumulator.TotalFees,
		Divergences: divergences,
	}, nil
}

func buildSlotCtxForReplay(db *accountsdb.AccountsDb, blk *b.Block) (*sealevel.SlotCtx, error) {
	// Load accounts - simplified version without FeeRateGovernor logic
	accts, parentAccts, err := loadBlockAccountsForReplay(db, blk)
	if err != nil {
		return nil, fmt.Errorf("failed to load block accounts: %w", err)
	}

	// Get parent slot (assume it's slot - 1 for now, could be improved)
	parentSlot := blk.Slot - 1

	// Calculate epoch from slot (simplified - uses epoch schedule)
	epoch := blk.Slot / 432000 // Approximate mainnet slots per epoch

	// Initialize features with all features enabled at slot 0
	// (for recent mainnet blocks, all features should be active)
	f := features.NewFeaturesDefault()
	for _, gate := range features.AllFeatureGates {
		f.EnableFeature(gate, 0)
	}

	// Default FeeRateGovernor for replay (mainnet defaults)
	feeRateGovernor := &sealevel.FeeRateGovernor{
		TargetLamportsPerSignature: 10000,
		TargetSignaturesPerSlot:    20000,
		MinLamportsPerSignature:    5000,
		MaxLamportsPerSignature:    100000,
		BurnPercent:                50,
		LamportsPerSignature:       5000,
		PrevLamportsPerSignature:   5000,
	}

	slotCtx := &sealevel.SlotCtx{
		Accounts:               accts,
		ParentAccts:            parentAccts,
		AccountsDb:             db,
		FeeRateGovernor:        feeRateGovernor,
		Slot:                   blk.Slot,
		ParentSlot:             parentSlot,
		Epoch:                  epoch,
		AcctMapsMu:             &sync.Mutex{},
		ModifiedAccts:          make(map[solana.PublicKey]bool),
		WritableAccts:          make(map[solana.PublicKey]bool),
		NumSignatures:          0,
		Blockhash:              blk.Blockhash,
		LastBlockhash:          blk.LastBlockhash,
		Features:               f,
		VoteTimestampMu:        &sync.Mutex{},
		VoteTimestamps:         make(map[solana.PublicKey]sealevel.BlockTimestamp),
		StakeCache:             make(map[solana.PublicKey]*sealevel.Delegation),
		VoteAccts:              make(map[solana.PublicKey]uint64),
		Replay:                 true,
	}

	return slotCtx, nil
}

// loadBlockAccountsForReplay loads accounts needed for replay without the
// FeeRateGovernor and sysvar update logic from production.
func loadBlockAccountsForReplay(db *accountsdb.AccountsDb, blk *b.Block) (accounts.Accounts, accounts.Accounts, error) {
	// Extract and dedupe account pubkeys from the block
	dedupedAccts := extractAndDedupeBlockAccts(blk)

	// Fetch accounts from AccountsDb at the PARENT slot (state before this block)
	// The snapshot contains accounts at slot N, and we're replaying block N+1
	parentSlot := blk.Slot - 1
	ctx := context.Background()
	slotAccts, err := db.GetAccountsBatch(ctx, parentSlot, dedupedAccts)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to fetch accounts: %w", err)
	}

	// Build MemAccounts
	numAccts := uint64(len(slotAccts))
	accts := accounts.NewMemAccountsWithLen(numAccts)
	parentAccts := accounts.NewMemAccountsWithLen(numAccts)

	for _, acct := range slotAccts {
		if err := accts.SetAccountWithoutLock(acct.Key, acct); err != nil {
			return nil, nil, err
		}
		if err := parentAccts.SetAccountWithoutLock(acct.Key, acct); err != nil {
			return nil, nil, err
		}
	}

	return accts, parentAccts, nil
}

func restoreSysvarCacheForReplay(slotCtx *sealevel.SlotCtx, blk *b.Block) error {
	// Load sysvars from AccountsDb at parent slot (state before this block)
	parentSlot := slotCtx.ParentSlot

	// EpochSchedule (must be loaded first, needed by other updates)
	if acct, err := slotCtx.AccountsDb.GetAccount(parentSlot, sealevel.SysvarEpochScheduleAddr); err == nil {
		var epochSchedule sealevel.SysvarEpochSchedule
		decoder := bin.NewBinDecoder(acct.Data)
		epochSchedule.MustUnmarshalWithDecoder(decoder)
		sealevel.SysvarCache.EpochSchedule.Sysvar = &epochSchedule
		sealevel.SysvarCache.EpochSchedule.Acct = acct
	}

	// Rent
	if acct, err := slotCtx.AccountsDb.GetAccount(parentSlot, sealevel.SysvarRentAddr); err == nil {
		var rent sealevel.SysvarRent
		decoder := bin.NewBinDecoder(acct.Data)
		rent.MustUnmarshalWithDecoder(decoder)
		sealevel.SysvarCache.Rent.Sysvar = &rent
		sealevel.SysvarCache.Rent.Acct = acct
	}

	// Fees
	if acct, err := slotCtx.AccountsDb.GetAccount(parentSlot, sealevel.SysvarFeesAddr); err == nil {
		var fees sealevel.SysvarFees
		decoder := bin.NewBinDecoder(acct.Data)
		fees.MustUnmarshalWithDecoder(decoder)
		sealevel.SysvarCache.Fees.Sysvar = &fees
		sealevel.SysvarCache.Fees.Acct = acct
	}

	// Clock - load from parent slot, then update for current slot
	if acct, err := slotCtx.AccountsDb.GetAccount(parentSlot, sealevel.SysvarClockAddr); err == nil {
		var clock sealevel.SysvarClock
		decoder := bin.NewBinDecoder(acct.Data)
		clock.UnmarshalWithDecoder(decoder)

		// Update clock for the new slot (like production does in loadBlockAccountsAndUpdateSysvars)
		if err := updateClockSysvar(&clock, blk); err != nil {
			return fmt.Errorf("failed to update clock sysvar: %w", err)
		}

		sealevel.SysvarCache.Clock.Sysvar = &clock
		sealevel.SysvarCache.Clock.Acct = acct
	}

	// SlotHashes - load from parent slot, then update with parent bankhash
	if acct, err := slotCtx.AccountsDb.GetAccount(parentSlot, sealevel.SysvarSlotHashesAddr); err == nil {
		var slotHashes sealevel.SysvarSlotHashes
		decoder := bin.NewBinDecoder(acct.Data)
		slotHashes.UnmarshalWithDecoder(decoder)

		// Get parent bankhash from BankHashStore and update SlotHashes
		// (like production does in loadBlockAccountsAndUpdateSysvars)
		if parentBankhash, err := slotCtx.AccountsDb.GetBankHashForSlot(parentSlot); err == nil {
			var parentBankhashArr [32]byte
			copy(parentBankhashArr[:], parentBankhash)
			slotHashes.Update(blk.Slot, parentSlot, parentBankhashArr)
		}

		sealevel.SysvarCache.SlotHashes.Sysvar = &slotHashes
		sealevel.SysvarCache.SlotHashes.Acct = acct
	}

	// RecentBlockHashes
	if acct, err := slotCtx.AccountsDb.GetAccount(parentSlot, sealevel.SysvarRecentBlockHashesAddr); err == nil {
		var recentBlockhashes sealevel.SysvarRecentBlockhashes
		decoder := bin.NewBinDecoder(acct.Data)
		recentBlockhashes.UnmarshalWithDecoder(decoder)
		sealevel.SysvarCache.RecentBlockHashes.Sysvar = &recentBlockhashes
		sealevel.SysvarCache.RecentBlockHashes.Acct = acct
	}

	// EpochRewards
	if acct, err := slotCtx.AccountsDb.GetAccount(parentSlot, sealevel.SysvarEpochRewardsAddr); err == nil {
		var epochRewards sealevel.SysvarEpochRewards
		decoder := bin.NewBinDecoder(acct.Data)
		epochRewards.MustUnmarshalWithDecoder(decoder)
		sealevel.SysvarCache.EpochRewards.Sysvar = &epochRewards
		sealevel.SysvarCache.EpochRewards.Acct = acct
	}

	// StakeHistory (needed for stake program)
	if acct, err := slotCtx.AccountsDb.GetAccount(parentSlot, sealevel.SysvarStakeHistoryAddr); err == nil {
		var stakeHistory sealevel.SysvarStakeHistory
		decoder := bin.NewBinDecoder(acct.Data)
		stakeHistory.MustUnmarshalWithDecoder(decoder)
		sealevel.SysvarCache.StakeHistory.Sysvar = &stakeHistory
		sealevel.SysvarCache.StakeHistory.Acct = acct
	}

	return nil
}

func replayBlockSequential(slotCtx *sealevel.SlotCtx, blk *b.Block, dbgOpts *DebugOptions) (fees.TxFeeInfoAccumulator, int) {
	var txFeeAccumulator fees.TxFeeInfoAccumulator
	divergences := 0

	for idx, tx := range blk.Transactions {
		var txMeta *rpc.TransactionMeta
		if blk.TxMetas != nil && idx < len(blk.TxMetas) {
			txMeta = blk.TxMetas[idx]
		}

		// Skip signature verification for offline replay (pass nil sigverifyWg)
		txFeeInfo, txErr := ProcessTransaction(slotCtx, nil, tx, txMeta, dbgOpts, nil)

		// Check for divergences
		if txMeta != nil {
			if txErr == nil && txMeta.Err != nil {
				mlog.Log.Errorf("DIVERGENCE in slot %d: tx %s succeeded locally but failed onchain: %+v",
					slotCtx.Slot, tx.Signatures[0], txMeta.Err)
				divergences++
			} else if txErr != nil && txMeta.Err == nil {
				mlog.Log.Errorf("DIVERGENCE in slot %d: tx %s failed locally (%v) but succeeded onchain",
					slotCtx.Slot, tx.Signatures[0], txErr)
				divergences++
			}
		}

		if txFeeInfo != nil {
			txFeeAccumulator.Add(txFeeInfo)
		}
	}

	return txFeeAccumulator, divergences
}

func replayBlockParallel(slotCtx *sealevel.SlotCtx, blk *b.Block, parallelism int, dbgOpts *DebugOptions) (fees.TxFeeInfoAccumulator, int) {
	var txFeeAccumulator fees.TxFeeInfoAccumulator
	txFeeInfos := make([]*fees.TxFeeInfo, len(blk.Transactions))
	errs := make([]error, len(blk.Transactions))

	do := make(chan int, len(blk.Transactions))
	done := make(chan int, len(blk.Transactions))
	go TopsortPlannerStream(blk, do, done)

	wg := &sync.WaitGroup{}
	wg.Add(parallelism)
	for range parallelism {
		go func() {
			defer wg.Done()
			for idx := range do {
				tx := blk.Transactions[idx]
				var txMeta *rpc.TransactionMeta
				if blk.TxMetas != nil && idx < len(blk.TxMetas) {
					txMeta = blk.TxMetas[idx]
				}

				txFeeInfos[idx], errs[idx] = ProcessTransaction(slotCtx, nil, tx, txMeta, dbgOpts, nil)
				done <- idx
			}
		}()
	}

	wg.Wait()
	close(done)

	// Count divergences and accumulate fees
	divergences := 0
	for idx, txFeeInfo := range txFeeInfos {
		if blk.TxMetas != nil && idx < len(blk.TxMetas) {
			txMeta := blk.TxMetas[idx]
			txErr := errs[idx]
			tx := blk.Transactions[idx]

			if txMeta != nil {
				if txErr == nil && txMeta.Err != nil {
					mlog.Log.Errorf("DIVERGENCE in slot %d: tx %s succeeded locally but failed onchain: %+v",
						slotCtx.Slot, tx.Signatures[0], txMeta.Err)
					divergences++
				} else if txErr != nil && txMeta.Err == nil {
					mlog.Log.Errorf("DIVERGENCE in slot %d: tx %s failed locally (%v) but succeeded onchain",
						slotCtx.Slot, tx.Signatures[0], txErr)
					divergences++
				}
			}
		}

		if txFeeInfo != nil {
			txFeeAccumulator.Add(txFeeInfo)
		}
	}

	return txFeeAccumulator, divergences
}
