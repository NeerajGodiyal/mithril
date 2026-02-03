package replay

import (
	"bytes"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/base58"
	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/fees"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

// AccountDivergence records a mismatch between expected and actual account state.
type AccountDivergence struct {
	Pubkey           solana.PublicKey
	Field            string // "lamports", "data", "owner", "executable", "rent_epoch"
	ExpectedValue    string
	ActualValue      string
}

// TxLoopReplayResult contains the results of replaying a TxLoop.
type TxLoopReplayResult struct {
	TxFeeAccumulator     fees.TxFeeInfoAccumulator
	Duration             time.Duration
	TxCount              int
	AccountDivergences   []AccountDivergence
	MissingAccounts      []solana.PublicKey // expected but not modified
	ExtraAccounts        []solana.PublicKey // modified but not expected
}

// ReplayTxLoop replays a recorded TxLoop offline.
// parallelism <= 0 means sequential execution.
func ReplayTxLoop(recordPath string, parallelism int) (*TxLoopReplayResult, error) {
	record, err := LoadRecordedTxLoop(recordPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load recorded txloop: %w", err)
	}

	mlog.Log.Infof("Replaying TxLoop for slot %d with %d transactions", record.Slot, len(record.Transactions))

	// Create temporary directory for AccountsDb
	tmpDir, err := os.MkdirTemp("", fmt.Sprintf("txloop-replay-%d-", record.Slot))
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Build list of accounts from the snapshot
	snapshotAccts := make([]*accounts.Account, 0, len(record.Accounts))
	for _, acct := range record.Accounts {
		snapshotAccts = append(snapshotAccts, acct)
	}

	// Create AccountsDb from snapshot accounts
	accountsDb, err := accountsdb.CreateAccountsDbFromSnapshot(snapshotAccts, record.Slot, tmpDir)
	if err != nil {
		return nil, fmt.Errorf("failed to create AccountsDb from snapshot: %w", err)
	}
	defer accountsDb.CloseDb()

	// Reconstruct SlotCtx
	slotCtx, err := reconstructSlotCtx(record, accountsDb)
	if err != nil {
		return nil, fmt.Errorf("failed to reconstruct SlotCtx: %w", err)
	}

	// Restore SysvarCache from accounts
	if err := restoreSysvarCache(slotCtx); err != nil {
		return nil, fmt.Errorf("failed to restore SysvarCache: %w", err)
	}

	// Run TxLoop (pass nil sigverifyWg to skip signature verification for offline replay)
	dbgOpts := &DebugOptions{}

	start := time.Now()
	var txFeeAccumulator fees.TxFeeInfoAccumulator

	if parallelism > 0 {
		if record.Block != nil {
			// Use the saved unresolved block directly for parallel replay
			txFeeAccumulator = replayParallelTxLoop(slotCtx, record.Block, parallelism, dbgOpts, record.AddressTables)
		} else {
			// Legacy: build block from transactions (requires sync for ALT resolution)
			block := &b.Block{
				Slot:         record.Slot,
				Transactions: record.Transactions,
				TxMetas:      record.TxMetas,
			}
			txFeeAccumulator = replayParallelTxLoop(slotCtx, block, parallelism, dbgOpts, record.AddressTables)
		}
	} else {
		// Re-resolve address table lookups for transactions
		// (needed because the 'resolved' flag isn't preserved through JSON serialization)
		if err := resolveTransactionALTs(record.Transactions, record.AddressTables); err != nil {
			return nil, fmt.Errorf("failed to resolve address table lookups: %w", err)
		}
		txFeeAccumulator = replaySequentialTxLoop(slotCtx, nil, record.Transactions, record.TxMetas, dbgOpts)
	}

	duration := time.Since(start)

	result := &TxLoopReplayResult{
		TxFeeAccumulator: txFeeAccumulator,
		Duration:         duration,
		TxCount:          len(record.Transactions),
	}

	// Verify modified accounts against expected state
	if record.ExpectedModifiedAccounts != nil {
		verifyModifiedAccounts(slotCtx, record.ExpectedModifiedAccounts, result)
	}

	return result, nil
}

func reconstructSlotCtx(record *RecordedTxLoop, accountsDb *accountsdb.AccountsDb) (*sealevel.SlotCtx, error) {
	// Parse blockhashes
	blockhash := base58.MustDecodeFromString(record.Blockhash)
	lastBlockhash := base58.MustDecodeFromString(record.LastBlockhash)
	latestEvictedBlockhash := base58.MustDecodeFromString(record.LatestEvictedBlockhash)

	// Reconstruct accounts (in-memory cache for fast access during TxLoop)
	accts := accountsFromMap(record.Accounts)

	// Reconstruct features
	features := featuresFromJSON(record.ActiveFeatures)

	feeRateGovernor := record.FeeRateGovernor

	slotCtx := &sealevel.SlotCtx{
		Accounts:               accts,
		ParentAccts:            nil, // Not needed for TxLoop replay
		AccountsDb:             accountsDb,
		FeeRateGovernor:        feeRateGovernor,
		Slot:                   record.Slot,
		ParentSlot:             record.ParentSlot,
		Epoch:                  record.Epoch,
		AcctMapsMu:             &sync.Mutex{},
		ModifiedAccts:          make(map[solana.PublicKey]bool),
		WritableAccts:          make(map[solana.PublicKey]bool),
		NumSignatures:          0,
		Blockhash:              blockhash,
		LastBlockhash:          lastBlockhash,
		LatestEvictedBlockhash: latestEvictedBlockhash,
		Features:               features,
		VoteTimestampMu:        &sync.Mutex{},
		VoteTimestamps:         make(map[solana.PublicKey]sealevel.BlockTimestamp),
		StakeCache:             make(map[solana.PublicKey]*sealevel.Delegation),
		VoteAccts:              make(map[solana.PublicKey]uint64),
		Replay:                 true,
	}

	return slotCtx, nil
}

func restoreSysvarCache(slotCtx *sealevel.SlotCtx) error {
	// Restore sysvars by unmarshaling from account data, following the pattern in block.go

	// EpochSchedule
	if acct, err := slotCtx.GetAccount(sealevel.SysvarEpochScheduleAddr); err == nil {
		var epochSchedule sealevel.SysvarEpochSchedule
		decoder := bin.NewBinDecoder(acct.Data)
		epochSchedule.MustUnmarshalWithDecoder(decoder)
		sealevel.SysvarCache.EpochSchedule.Sysvar = &epochSchedule
		sealevel.SysvarCache.EpochSchedule.Acct = acct
	}

	// Rent
	if acct, err := slotCtx.GetAccount(sealevel.SysvarRentAddr); err == nil {
		var rent sealevel.SysvarRent
		decoder := bin.NewBinDecoder(acct.Data)
		rent.MustUnmarshalWithDecoder(decoder)
		sealevel.SysvarCache.Rent.Sysvar = &rent
		sealevel.SysvarCache.Rent.Acct = acct
	}

	// Fees
	if acct, err := slotCtx.GetAccount(sealevel.SysvarFeesAddr); err == nil {
		var fees sealevel.SysvarFees
		decoder := bin.NewBinDecoder(acct.Data)
		fees.MustUnmarshalWithDecoder(decoder)
		sealevel.SysvarCache.Fees.Sysvar = &fees
		sealevel.SysvarCache.Fees.Acct = acct
	}

	// Clock
	if acct, err := slotCtx.GetAccount(sealevel.SysvarClockAddr); err == nil {
		var clock sealevel.SysvarClock
		decoder := bin.NewBinDecoder(acct.Data)
		clock.UnmarshalWithDecoder(decoder)
		sealevel.SysvarCache.Clock.Sysvar = &clock
		sealevel.SysvarCache.Clock.Acct = acct
	}

	// SlotHashes
	if acct, err := slotCtx.GetAccount(sealevel.SysvarSlotHashesAddr); err == nil {
		var slotHashes sealevel.SysvarSlotHashes
		decoder := bin.NewBinDecoder(acct.Data)
		slotHashes.UnmarshalWithDecoder(decoder)
		sealevel.SysvarCache.SlotHashes.Sysvar = &slotHashes
		sealevel.SysvarCache.SlotHashes.Acct = acct
	}

	// RecentBlockHashes
	if acct, err := slotCtx.GetAccount(sealevel.SysvarRecentBlockHashesAddr); err == nil {
		var recentBlockhashes sealevel.SysvarRecentBlockhashes
		decoder := bin.NewBinDecoder(acct.Data)
		recentBlockhashes.UnmarshalWithDecoder(decoder)
		sealevel.SysvarCache.RecentBlockHashes.Sysvar = &recentBlockhashes
		sealevel.SysvarCache.RecentBlockHashes.Acct = acct
	}

	// EpochRewards
	if acct, err := slotCtx.GetAccount(sealevel.SysvarEpochRewardsAddr); err == nil {
		var epochRewards sealevel.SysvarEpochRewards
		decoder := bin.NewBinDecoder(acct.Data)
		epochRewards.MustUnmarshalWithDecoder(decoder)
		sealevel.SysvarCache.EpochRewards.Sysvar = &epochRewards
		sealevel.SysvarCache.EpochRewards.Acct = acct
	}

	return nil
}

// replaySequentialTxLoop is a simplified version of sequentialTxLoop for offline replay.
func replaySequentialTxLoop(
	slotCtx *sealevel.SlotCtx,
	sigverifyWg *sync.WaitGroup,
	transactions []*solana.Transaction,
	txMetas []*rpc.TransactionMeta,
	dbgOpts *DebugOptions,
) fees.TxFeeInfoAccumulator {
	var txFeeAccumulator fees.TxFeeInfoAccumulator

	for idx, tx := range transactions {
		var txMeta *rpc.TransactionMeta
		if txMetas != nil && idx < len(txMetas) {
			txMeta = txMetas[idx]
		}

		txFeeInfo, txErr := ProcessTransaction(slotCtx, sigverifyWg, tx, txMeta, dbgOpts, nil)

		if txErr != nil {
			if txMeta != nil && txMeta.Err == nil && tx.IsVote() {
				mlog.Log.Errorf("DIVERGENCE in slot %d: vote tx %s failed locally but succeeded onchain",
					slotCtx.Slot, tx.Signatures[0])
			}
		}

		// Check for success-failure return value divergences
		if txMeta != nil {
			if txErr == nil && txMeta.Err != nil {
				mlog.Log.Errorf("DIVERGENCE in slot %d: tx %s succeeded locally but failed onchain: %+v",
					slotCtx.Slot, tx.Signatures[0], txMeta.Err)
			} else if txErr != nil && txMeta.Err == nil {
				mlog.Log.Errorf("DIVERGENCE in slot %d: tx %s failed locally (%v) but succeeded onchain",
					slotCtx.Slot, tx.Signatures[0], txErr)
			}
		}

		if txFeeInfo != nil {
			txFeeAccumulator.Add(txFeeInfo)
		}
	}

	return txFeeAccumulator
}

// replayParallelTxLoop runs transactions in parallel using topsort scheduling.
// resolveALTs is called after the dependency graph is built but before execution starts.
func replayParallelTxLoop(
	slotCtx *sealevel.SlotCtx,
	block *b.Block,
	parallelism int,
	dbgOpts *DebugOptions,
	addressTables map[solana.PublicKey]solana.PublicKeySlice,
) fees.TxFeeInfoAccumulator {
	var txFeeAccumulator fees.TxFeeInfoAccumulator
	txFeeInfos := make([]*fees.TxFeeInfo, len(block.Transactions))
	errs := make([]error, len(block.Transactions))

	do := make(chan int, len(block.Transactions))
	done := make(chan int, len(block.Transactions))
	graphReady := make(chan struct{})
	go TopsortPlannerStreamWithReady(block, do, done, graphReady)

	// Wait for dependency graph to be built before resolving ALTs
	<-graphReady
	resolveTransactionALTs(block.Transactions, addressTables)

	wg := &sync.WaitGroup{}
	wg.Add(parallelism)
	for range parallelism {
		go func() {
			defer wg.Done()
			for idx := range do {
				tx := block.Transactions[idx]
				var txMeta *rpc.TransactionMeta
				if block.TxMetas != nil && idx < len(block.TxMetas) {
					txMeta = block.TxMetas[idx]
				}

				txFeeInfos[idx], errs[idx] = ProcessTransaction(slotCtx, nil, tx, txMeta, dbgOpts, nil)
				txErr := errs[idx]

				// Check for success-failure return value divergences
				if txMeta != nil {
					if txErr == nil && txMeta.Err != nil {
						mlog.Log.Errorf("DIVERGENCE in slot %d: tx %s succeeded locally but failed onchain: %+v",
							block.Slot, tx.Signatures[0], txMeta.Err)
					} else if txErr != nil && txMeta.Err == nil {
						mlog.Log.Errorf("DIVERGENCE in slot %d: tx %s failed locally (%v) but succeeded onchain",
							block.Slot, tx.Signatures[0], txErr)
					}
				}

				done <- idx
			}
		}()
	}

	wg.Wait()
	close(done)

	for _, txFeeInfo := range txFeeInfos {
		if txFeeInfo != nil {
			txFeeAccumulator.Add(txFeeInfo)
		}
	}

	return txFeeAccumulator
}

// verifyModifiedAccounts compares actual modified accounts against expected state.
func verifyModifiedAccounts(
	slotCtx *sealevel.SlotCtx,
	expected map[solana.PublicKey]*accounts.Account,
	result *TxLoopReplayResult,
) {
	// Check for accounts that were expected but not modified
	for pk := range expected {
		if _, modified := slotCtx.ModifiedAccts[pk]; !modified {
			result.MissingAccounts = append(result.MissingAccounts, pk)
		}
	}

	// Check for accounts that were modified but not expected
	for pk := range slotCtx.ModifiedAccts {
		if _, ok := expected[pk]; !ok {
			result.ExtraAccounts = append(result.ExtraAccounts, pk)
		}
	}

	// Compare state of accounts that were both expected and modified
	for pk, expectedAcct := range expected {
		if _, modified := slotCtx.ModifiedAccts[pk]; !modified {
			continue
		}

		actualAcct, err := slotCtx.GetAccount(pk)
		if err != nil {
			result.AccountDivergences = append(result.AccountDivergences, AccountDivergence{
				Pubkey:        pk,
				Field:         "existence",
				ExpectedValue: "exists",
				ActualValue:   "not found",
			})
			continue
		}

		compareAccounts(pk, expectedAcct, actualAcct, result)
	}
}

// compareAccounts compares two accounts field by field and records divergences.
func compareAccounts(
	pk solana.PublicKey,
	expected *accounts.Account,
	actual *accounts.Account,
	result *TxLoopReplayResult,
) {
	if expected.Lamports != actual.Lamports {
		result.AccountDivergences = append(result.AccountDivergences, AccountDivergence{
			Pubkey:        pk,
			Field:         "lamports",
			ExpectedValue: fmt.Sprintf("%d", expected.Lamports),
			ActualValue:   fmt.Sprintf("%d", actual.Lamports),
		})
	}

	if expected.Owner != actual.Owner {
		result.AccountDivergences = append(result.AccountDivergences, AccountDivergence{
			Pubkey:        pk,
			Field:         "owner",
			ExpectedValue: expected.Owner.String(),
			ActualValue:   actual.Owner.String(),
		})
	}

	if expected.Executable != actual.Executable {
		result.AccountDivergences = append(result.AccountDivergences, AccountDivergence{
			Pubkey:        pk,
			Field:         "executable",
			ExpectedValue: fmt.Sprintf("%t", expected.Executable),
			ActualValue:   fmt.Sprintf("%t", actual.Executable),
		})
	}

	if expected.RentEpoch != actual.RentEpoch {
		result.AccountDivergences = append(result.AccountDivergences, AccountDivergence{
			Pubkey:        pk,
			Field:         "rent_epoch",
			ExpectedValue: fmt.Sprintf("%d", expected.RentEpoch),
			ActualValue:   fmt.Sprintf("%d", actual.RentEpoch),
		})
	}

	if !bytes.Equal(expected.Data, actual.Data) {
		result.AccountDivergences = append(result.AccountDivergences, AccountDivergence{
			Pubkey:        pk,
			Field:         "data",
			ExpectedValue: fmt.Sprintf("len=%d", len(expected.Data)),
			ActualValue:   fmt.Sprintf("len=%d", len(actual.Data)),
		})
	}
}

// resolveTransactionALTs re-applies address table lookups to transactions after JSON deserialization.
// This is needed because the Message.version and Message.resolved fields aren't serialized.
// The JSON snapshot contains fully resolved AccountKeys, but since resolved=false, the library
// will try to re-resolve and duplicate keys. We fix this by:
// 1. Trimming AccountKeys back to just the static keys
// 2. Setting version and address tables
// 3. Calling ResolveLookups to properly resolve (which also sets resolved=true internally)
func resolveTransactionALTs(transactions []*solana.Transaction, addressTables map[solana.PublicKey]solana.PublicKeySlice) error {
	if len(addressTables) == 0 {
		return nil
	}

	for _, tx := range transactions {
		// Check if this transaction has address table lookups (version field is lost in JSON)
		if tx.Message.AddressTableLookups == nil || tx.Message.AddressTableLookups.NumLookups() == 0 {
			continue
		}

		// Calculate number of static keys from header
		header := tx.Message.Header
		numStaticKeys := int(header.NumRequiredSignatures) +
			int(header.NumReadonlySignedAccounts) +
			int(header.NumReadonlyUnsignedAccounts)
		// Actually, the formula is: total static = all keys before lookup resolution
		// Which is: numRequiredSignatures + numReadonlyUnsigned from header perspective
		// But simpler: static keys = total signed + total unsigned from static accounts
		// The header gives us: NumRequiredSignatures (total signers), NumReadonlySignedAccounts (readonly signers),
		// NumReadonlyUnsignedAccounts (readonly non-signers)
		// Static keys count = len(AccountKeys) - keys_from_lookups
		keysFromLookups := 0
		for _, lookup := range tx.Message.AddressTableLookups {
			keysFromLookups += len(lookup.WritableIndexes) + len(lookup.ReadonlyIndexes)
		}
		numStaticKeys = len(tx.Message.AccountKeys) - keysFromLookups

		// Trim AccountKeys back to just static keys (remove the already-resolved lookup keys)
		tx.Message.AccountKeys = tx.Message.AccountKeys[:numStaticKeys]

		// Set version to V0 since it has address table lookups (version isn't preserved in JSON)
		tx.Message.SetVersion(solana.MessageVersionV0)

		// Set address tables and resolve
		if err := tx.Message.SetAddressTables(addressTables); err != nil {
			return fmt.Errorf("failed to set address tables: %w", err)
		}
		if err := tx.Message.ResolveLookups(); err != nil {
			return fmt.Errorf("failed to resolve lookups: %w", err)
		}
	}

	return nil
}
