package rewards

import (
	"fmt"
	"math"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/Overclock-Validator/mithril/pkg/safemath"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/wide"
	"github.com/dgryski/go-sip13"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/panjf2000/ants/v2"
)

// PartitionMismatchError represents a partition count mismatch between local computation and RPC.
// This is a critical error that will cause bankhash divergence if not handled.
type PartitionMismatchError struct {
	Epoch             uint64
	LocalPartitions   uint64
	RpcPartitions     uint64
	EligibleStakeAcct uint64
	TotalStakeAcct    uint64
	BelowMinAcct      uint64
	NoVoteAcct        uint64
	NoCreditsAcct     uint64
	ZeroPointsAcct    uint64
	ZeroRewardsAcct   uint64
	ZeroSplitAcct     uint64
}

func (e *PartitionMismatchError) Error() string {
	// Eligibility uses full Firedancer/Agave filter: points > 0 AND rewards > 0 AND split valid.
	// Filter breakdown: below_min, no_vote, no_credits, zero_points, zero_rewards, zero_split are mutually exclusive.
	return fmt.Sprintf("PARTITION COUNT MISMATCH: epoch=%d local=%d rpc=%d eligible=%d total=%d "+
		"(excluded: below_min=%d no_vote=%d no_credits=%d zero_points=%d zero_rewards=%d zero_split=%d).",
		e.Epoch, e.LocalPartitions, e.RpcPartitions, e.EligibleStakeAcct, e.TotalStakeAcct,
		e.BelowMinAcct, e.NoVoteAcct, e.NoCreditsAcct, e.ZeroPointsAcct, e.ZeroRewardsAcct, e.ZeroSplitAcct)
}

// Package-level validation settings for epoch boundary
var (
	// validationRpcClient is used for pre-commit validation at epoch boundary.
	// Set via SetValidationRpcClient() before replay starts.
	validationRpcClient *rpcclient.RpcClient

	// validationRpcBackups are backup RPC endpoints for validation failover.
	validationRpcBackups []string

	// validatePartitionCount enables partition count validation against RPC at epoch boundary.
	// When enabled, returns error if local partition count differs from RPC.
	// Default: false (no RPC calls beyond getBlock). Enable for debugging if needed.
	validatePartitionCount = false
)

// SetValidationRpcClient configures the RPC client used for epoch boundary validation.
// Call this before replay starts to enable partition count validation.
func SetValidationRpcClient(client *rpcclient.RpcClient, backups []string) {
	validationRpcClient = client
	validationRpcBackups = backups
}

// SetValidatePartitionCount enables or disables partition count validation at epoch boundary.
func SetValidatePartitionCount(enabled bool) {
	validatePartitionCount = enabled
}

// IsValidationEnabled returns whether partition count validation is enabled.
func IsValidationEnabled() bool {
	return validatePartitionCount
}

// GetValidationRpcClient returns the RPC client configured for validation.
func GetValidationRpcClient() *rpcclient.RpcClient {
	return validationRpcClient
}

// GetValidationRpcBackups returns the backup RPC endpoints for validation failover.
func GetValidationRpcBackups() []string {
	return validationRpcBackups
}

// FetchRpcPartitionCountWithBackups fetches numRewardPartitions from RPC with failover.
// Used for pre-commit validation at epoch boundary.
func FetchRpcPartitionCountWithBackups(rpcc *rpcclient.RpcClient, backups []string, firstSlotInEpoch uint64) (uint64, error) {
	// Try primary first
	numPartitions, err := rpcc.GetNumRewardPartitions(firstSlotInEpoch)
	if err == nil {
		return numPartitions, nil
	}

	lastErr := err
	mlog.Log.Warnf("partition validation: primary RPC failed: %v", err)

	// Try backup endpoints
	for i, endpoint := range backups {
		mlog.Log.Infof("partition validation: trying backup #%d: %s", i+1, endpoint)
		backupClient := rpcclient.NewRpcClient(endpoint)
		numPartitions, err := backupClient.GetNumRewardPartitions(firstSlotInEpoch)
		if err == nil {
			return numPartitions, nil
		}
		lastErr = err
		mlog.Log.Warnf("partition validation: backup #%d failed: %v", i+1, err)
	}

	return 0, fmt.Errorf("all endpoints failed, last error: %w", lastErr)
}

// FetchRpcEpochRewardsWithBackups fetches the EpochRewards sysvar from RPC with failover.
// Used for debugging/comparison at epoch boundary.
func FetchRpcEpochRewardsWithBackups(rpcc *rpcclient.RpcClient, backups []string, slot uint64) (*sealevel.SysvarEpochRewards, error) {
	// Try primary first
	epochRewards, err := fetchEpochRewardsFromClient(rpcc)
	if err == nil {
		return epochRewards, nil
	}

	lastErr := err
	mlog.Log.Debugf("epoch rewards fetch: primary RPC failed: %v", err)

	// Try backup endpoints
	for i, endpoint := range backups {
		backupClient := rpcclient.NewRpcClient(endpoint)
		epochRewards, err := fetchEpochRewardsFromClient(backupClient)
		if err == nil {
			return epochRewards, nil
		}
		lastErr = err
		mlog.Log.Debugf("epoch rewards fetch: backup #%d failed: %v", i+1, err)
	}

	return nil, fmt.Errorf("all endpoints failed, last error: %w", lastErr)
}

// fetchEpochRewardsFromClient fetches and decodes EpochRewards sysvar from a single RPC client.
func fetchEpochRewardsFromClient(rpcc *rpcclient.RpcClient) (*sealevel.SysvarEpochRewards, error) {
	data, err := rpcc.GetEpochRewardsSysvar()
	if err != nil {
		return nil, err
	}
	if len(data) < sealevel.SysvarEpochRewardsStructLen {
		return nil, fmt.Errorf("EpochRewards data too short: %d < %d", len(data), sealevel.SysvarEpochRewardsStructLen)
	}
	var epochRewards sealevel.SysvarEpochRewards
	decoder := bin.NewBinDecoder(data)
	if err := epochRewards.UnmarshalWithDecoder(decoder); err != nil {
		return nil, fmt.Errorf("failed to decode EpochRewards: %w", err)
	}
	return &epochRewards, nil
}

const (
	RewardTypeFee     string = "Fee"
	RewardTypeRent    string = "Rent"
	RewardTypeVoting  string = "Voting"
	RewardTypeStaking string = "Staking"

	// MaxRewardsPerBlock is the maximum number of stake accounts to process per block.
	// Matches Agave's MAX_REWARDS_PER_BLOCK constant.
	MaxRewardsPerBlock uint64 = 4096
)

// RefreshStakeCacheCreditsObserved updates CreditsObserved in the stake cache from AccountsDB.
//
// The snapshot manifest's epoch_stakes contains stake delegation entries, but the credits_observed
// field is NOT updated after rewards distribution - it only reflects the value when the stake was
// created or last modified (delegate/undelegate/merge operations).
//
// The actual stake accounts in AccountsDB have the correct credits_observed values (updated after
// each epoch's rewards distribution). This function refreshes the stake cache to use those values.
//
// This matches Firedancer's fd_stake_delegations_refresh() which is called before rewards calculation.
// See: src/flamenco/stakes/fd_stake_delegations.h lines 46-50:
//
//	"The reason we can't populate the stake accounts from the cache is because the cache in the
//	manifest is partially incomplete: all of the expected keys are there, but the values are not.
//	Notably, the credits_observed field is not available until all of the accounts are loaded
//	into the database."
// refreshTask holds input data for a stake cache refresh worker
type refreshTask struct {
	idx        int
	pubkey     solana.PublicKey
	delegation *sealevel.Delegation
}

// refreshResult holds output data from a stake cache refresh worker
type refreshResult struct {
	idx            int
	pubkey         solana.PublicKey
	delegation     *sealevel.Delegation
	shouldDelete   bool
	deleteReason   string // for debug logging
	newCredits     uint64
	creditsUpdated bool
	snapshot       *accounts.Account // nil if should delete
}

// RefreshStakeCacheCreditsObserved refreshes credits_observed from AccountsDB and returns
// a snapshot of all valid stake accounts for use during distribution.
// The returned map contains clones of stake accounts that can be used instead of re-reading
// from AccountsDB, which is critical because GetAccount ignores the slot parameter.
//
// This function is parallelized to improve performance on large stake caches (~1M accounts).
// Uses a 3-phase approach to avoid concurrent map read/write:
//   Phase 1: Snapshot stake cache into slice (single-threaded)
//   Phase 2: Workers read AccountsDB, compute results (no mutations)
//   Phase 3: Apply mutations after all workers finish (single-threaded)
func RefreshStakeCacheCreditsObserved(acctsDb *accountsdb.AccountsDb, slot uint64) (refreshed int, errors int, snapshots map[solana.PublicKey]*accounts.Account) {
	stakeCache := global.StakeCache()
	total := len(stakeCache)
	mlog.Log.Infof("refreshing stake cache credits_observed from AccountsDB (slot=%d): %d accounts (parallelized)", slot, total)

	// Calculate BEFORE totals
	var beforeTotalStake uint64
	var beforeTotalCredits uint64
	beforeVoteAccts := make(map[solana.PublicKey]bool)
	for _, delegation := range stakeCache {
		if delegation != nil {
			beforeTotalStake += delegation.StakeLamports
			beforeTotalCredits += delegation.CreditsObserved
			beforeVoteAccts[delegation.VoterPubkey] = true
		}
	}
	mlog.Log.Infof("  BEFORE refresh: stake=%.2f SOL, total_credits=%d, vote_accounts=%d",
		float64(beforeTotalStake)/1e9, beforeTotalCredits, len(beforeVoteAccts))

	// PHASE 1: Snapshot stake cache into slice (no concurrent iteration issues)
	tasks := make([]refreshTask, 0, total)
	for pubkey, delegation := range stakeCache {
		tasks = append(tasks, refreshTask{
			idx:        len(tasks),
			pubkey:     pubkey,
			delegation: delegation,
		})
	}

	// Pre-allocate results slice (indexed by task.idx for lock-free writes)
	results := make([]refreshResult, len(tasks))

	// Atomic counter for progress logging
	var processed atomic.Uint64

	var wg sync.WaitGroup

	// PHASE 2: Workers read AccountsDB, compute results (no mutations to global state)
	poolSize := runtime.GOMAXPROCS(0) * 8
	workerPool, poolErr := ants.NewPoolWithFunc(poolSize, func(i interface{}) {
		defer wg.Done()

		task := i.(*refreshTask)
		result := &results[task.idx]
		result.idx = task.idx
		result.pubkey = task.pubkey
		result.delegation = task.delegation

		// Progress logging
		p := processed.Add(1)
		if p%100000 == 0 {
			mlog.Log.Infof("  refresh progress: %d/%d (%.1f%%)", p, total, float64(p)*100/float64(total))
		}

		// Read the actual stake account from AccountsDB
		stakeAcct, err := acctsDb.GetAccount(slot, task.pubkey)
		if err != nil {
			result.shouldDelete = true
			result.deleteReason = "not_found"
			return
		}

		// Check for tombstone
		if stakeAcct.Lamports == 0 && len(stakeAcct.Data) == 0 {
			result.shouldDelete = true
			result.deleteReason = "tombstone"
			return
		}

		// Decode the stake state
		stakeState, err := sealevel.UnmarshalStakeState(stakeAcct.Data)
		if err != nil {
			result.shouldDelete = true
			result.deleteReason = "unmarshal_error"
			return
		}

		// Only keep if it's a Stake state (not Initialized or Uninitialized)
		if stakeState.Status != sealevel.StakeStateV2StatusStake {
			result.shouldDelete = true
			result.deleteReason = "not_stake_state"
			return
		}

		// Record new credits (will be applied in phase 3)
		result.newCredits = stakeState.Stake.Stake.CreditsObserved
		result.creditsUpdated = task.delegation.CreditsObserved != result.newCredits
		result.snapshot = stakeAcct.Clone()
	})

	// Fallback to sequential if pool creation fails
	if poolErr != nil {
		mlog.Log.Warnf("failed to create worker pool, falling back to sequential: %v", poolErr)
		return refreshStakeCacheSequential(acctsDb, slot, tasks, beforeTotalStake, beforeTotalCredits, len(beforeVoteAccts))
	}
	defer workerPool.Release()

	// Submit all tasks to the worker pool
	for i := range tasks {
		wg.Add(1)
		if err := workerPool.Invoke(&tasks[i]); err != nil {
			// Invoke failed - decrement wg to avoid deadlock
			wg.Done()
			mlog.Log.Warnf("worker pool invoke failed for pubkey %s: %v", tasks[i].pubkey.String(), err)
		}
	}

	// Wait for all workers to complete
	wg.Wait()

	// PHASE 3: Apply mutations (single-threaded, no race conditions)
	var refreshedCount, errorCount int
	var tombstoneCount, notFoundCount, unmarshalErrCount, notStakeCount int

	snapshots = make(map[solana.PublicKey]*accounts.Account, total)
	var toDelete []solana.PublicKey

	for i := range results {
		result := &results[i]
		if result.shouldDelete {
			toDelete = append(toDelete, result.pubkey)
			errorCount++
			switch result.deleteReason {
			case "not_found":
				notFoundCount++
			case "tombstone":
				tombstoneCount++
			case "unmarshal_error":
				unmarshalErrCount++
			case "not_stake_state":
				notStakeCount++
			}
			mlog.Log.Debugf("STAKE_CACHE_REFRESH_REMOVE: slot=%d pubkey=%s reason=%s",
				slot, result.pubkey.String(), result.deleteReason)
		} else {
			// Apply credits update
			if result.creditsUpdated {
				result.delegation.CreditsObserved = result.newCredits
				refreshedCount++
			}
			// Store snapshot
			if result.snapshot != nil {
				snapshots[result.pubkey] = result.snapshot
			}
		}
	}

	// Batch delete all entries that need removal
	for _, pk := range toDelete {
		global.DeleteStakeCacheItem(pk)
	}

	mlog.Log.Infof("stake cache refresh complete: %d updated, %d errors/removed, %d snapshots captured",
		refreshedCount, errorCount, len(snapshots))
	mlog.Log.Infof("  breakdown: not_found=%d tombstone=%d unmarshal_err=%d not_stake=%d",
		notFoundCount, tombstoneCount, unmarshalErrCount, notStakeCount)

	// Calculate AFTER totals
	afterStakeCache := global.StakeCache()
	var afterTotalStake uint64
	var afterTotalCredits uint64
	afterVoteAccts := make(map[solana.PublicKey]bool)
	for _, delegation := range afterStakeCache {
		if delegation != nil {
			afterTotalStake += delegation.StakeLamports
			afterTotalCredits += delegation.CreditsObserved
			afterVoteAccts[delegation.VoterPubkey] = true
		}
	}
	mlog.Log.Infof("  AFTER refresh:  stake=%.2f SOL, total_credits=%d, vote_accounts=%d",
		float64(afterTotalStake)/1e9, afterTotalCredits, len(afterVoteAccts))
	mlog.Log.Infof("  DIFF:           stake=%.2f SOL, credits=%+d, vote_accounts=%+d",
		float64(afterTotalStake-beforeTotalStake)/1e9,
		int64(afterTotalCredits)-int64(beforeTotalCredits),
		len(afterVoteAccts)-len(beforeVoteAccts))

	return refreshedCount, errorCount, snapshots
}

// refreshStakeCacheSequential is the fallback when worker pool creation fails
func refreshStakeCacheSequential(acctsDb *accountsdb.AccountsDb, slot uint64, tasks []refreshTask, beforeTotalStake, beforeTotalCredits uint64, beforeVoteAcctCount int) (refreshed int, errors int, snapshots map[solana.PublicKey]*accounts.Account) {
	total := len(tasks)
	snapshots = make(map[solana.PublicKey]*accounts.Account, total)

	var refreshedCount, errorCount int
	var tombstoneCount, notFoundCount, unmarshalErrCount, notStakeCount int

	for i, task := range tasks {
		if (i+1)%100000 == 0 {
			mlog.Log.Infof("  refresh progress: %d/%d (%.1f%%)", i+1, total, float64(i+1)*100/float64(total))
		}

		stakeAcct, err := acctsDb.GetAccount(slot, task.pubkey)
		if err != nil {
			global.DeleteStakeCacheItem(task.pubkey)
			notFoundCount++
			errorCount++
			continue
		}

		if stakeAcct.Lamports == 0 && len(stakeAcct.Data) == 0 {
			global.DeleteStakeCacheItem(task.pubkey)
			tombstoneCount++
			errorCount++
			continue
		}

		stakeState, err := sealevel.UnmarshalStakeState(stakeAcct.Data)
		if err != nil {
			global.DeleteStakeCacheItem(task.pubkey)
			unmarshalErrCount++
			errorCount++
			continue
		}

		if stakeState.Status != sealevel.StakeStateV2StatusStake {
			global.DeleteStakeCacheItem(task.pubkey)
			notStakeCount++
			errorCount++
			continue
		}

		newCredits := stakeState.Stake.Stake.CreditsObserved
		if task.delegation.CreditsObserved != newCredits {
			task.delegation.CreditsObserved = newCredits
			refreshedCount++
		}
		snapshots[task.pubkey] = stakeAcct.Clone()
	}

	mlog.Log.Infof("stake cache refresh complete (sequential): %d updated, %d errors/removed, %d snapshots",
		refreshedCount, errorCount, len(snapshots))
	mlog.Log.Infof("  breakdown: not_found=%d tombstone=%d unmarshal_err=%d not_stake=%d",
		notFoundCount, tombstoneCount, unmarshalErrCount, notStakeCount)

	// Calculate AFTER totals
	afterStakeCache := global.StakeCache()
	var afterTotalStake uint64
	var afterTotalCredits uint64
	afterVoteAccts := make(map[solana.PublicKey]bool)
	for _, delegation := range afterStakeCache {
		if delegation != nil {
			afterTotalStake += delegation.StakeLamports
			afterTotalCredits += delegation.CreditsObserved
			afterVoteAccts[delegation.VoterPubkey] = true
		}
	}
	mlog.Log.Infof("  AFTER refresh:  stake=%.2f SOL, total_credits=%d, vote_accounts=%d",
		float64(afterTotalStake)/1e9, afterTotalCredits, len(afterVoteAccts))
	mlog.Log.Infof("  DIFF:           stake=%.2f SOL, credits=%+d, vote_accounts=%+d",
		float64(afterTotalStake-beforeTotalStake)/1e9,
		int64(afterTotalCredits)-int64(beforeTotalCredits),
		len(afterVoteAccts)-beforeVoteAcctCount)

	return refreshedCount, errorCount, snapshots
}

// ComputeNumRewardPartitions calculates the number of reward partitions based on stake account count.
// Matches Agave's get_rewards_num_partitions formula with warmup check and max blocks clamping.
//
// Logic:
//  1. If in warmup period (epoch < firstNormalEpoch): return 1
//  2. numChunks = ceil(numStakeAccounts / MaxRewardsPerBlock)
//  3. maxBlocks = max(slotsPerEpoch / 10, 1)
//  4. return clamp(numChunks, 1, maxBlocks)
func ComputeNumRewardPartitions(epoch uint64, slotsPerEpoch uint64, numStakeAccounts uint64, firstNormalEpoch uint64) uint64 {
	// During warmup, use single partition
	if epoch < firstNormalEpoch {
		return 1
	}

	// Calculate number of chunks needed (ceiling division)
	var numChunks uint64
	if numStakeAccounts == 0 {
		numChunks = 1
	} else {
		numChunks = (numStakeAccounts + MaxRewardsPerBlock - 1) / MaxRewardsPerBlock
	}

	// Calculate max blocks allowed (10% of epoch)
	maxBlocks := slotsPerEpoch / 10
	if maxBlocks == 0 {
		maxBlocks = 1
	}

	// Clamp to [1, maxBlocks]
	if numChunks < 1 {
		return 1
	}
	if numChunks > maxBlocks {
		return maxBlocks
	}
	return numChunks
}

type PartitionedRewardDistributionInfo struct {
	TotalStakingRewards    uint64
	FirstStakingRewardSlot uint64
	LastStakingRewardSlot  uint64
	// BoundarySlot is the last slot of the previous epoch (firstSlotInEpoch - 1).
	// This is the slot from which stake account state should be read for rewards calculation.
	// Accounts that exist at BoundarySlot should receive rewards, even if they were closed
	// in subsequent slots during the distribution period.
	BoundarySlot uint64
	// EahStartOffsetSlot and EahStopOffsetSlot are legacy fields for Epoch Accounts Hash.
	// EAH was deprecated after AccountsLtHash activation (~Nov 2024). These fields are only
	// relevant for replaying historical pre-AccountsLtHash slots and are not used in current flow.
	EahStartOffsetSlot  uint64
	EahStopOffsetSlot   uint64
	NumRewardPartitions uint64
	// EligibleCount is the number of stake accounts expected to receive rewards (or have credits updated).
	// This is computed during DeterminePartitionedStakingRewardsInfoLocal and should match len(StakingRewards)
	// after CalculateStakeRewards. A mismatch indicates the stake cache changed between the two passes.
	EligibleCount       uint64
	Credits             map[solana.PublicKey]CalculatedStakePoints
	RewardPartitions    Partitions
	StakingRewards      map[solana.PublicKey]*CalculatedStakeRewards
	// StakeAccountSnapshots stores stake account data captured during RefreshStakeCacheCreditsObserved.
	// This allows distribution to use cached data instead of re-reading from AccountsDB,
	// which is critical because GetAccount ignores the slot parameter and returns current state.
	// Without this cache, accounts that existed at boundary but were closed afterward would
	// fail to receive rewards, causing divergence from Agave/Firedancer.
	StakeAccountSnapshots map[solana.PublicKey]*accounts.Account
}

type CalculatedStakePoints struct {
	Points                              wide.Uint128
	NewCreditsObserved                  uint64
	ForceCreditsUpdateWithSkippedReward bool
}

func SlotInYearForInflation(epochSchedule *sealevel.SysvarEpochSchedule, slotsPerYear float64, epoch uint64, f *features.Features) float64 {
	numSlots := GetInflationNumSlots(epochSchedule, epoch, f)
	return float64(numSlots) / slotsPerYear
}

func GetInflationNumSlots(epochSchedule *sealevel.SysvarEpochSchedule, epoch uint64, f *features.Features) uint64 {
	inflationActivationSlot := GetInflationStartSlot(f)
	inflationStartSlot := epochSchedule.FirstSlotInEpoch(safemath.SaturatingSubU64(epochSchedule.GetEpoch(inflationActivationSlot), 1))
	return epochSchedule.FirstSlotInEpoch(epoch) - inflationStartSlot
}

func GetInflationStartSlot(f *features.Features) uint64 {
	fullInflationFeatures := f.FullInflationFeaturesEnabled()
	var activationSlots []uint64

	for _, inflationFeature := range fullInflationFeatures {
		activationSlot, _ := f.ActivationSlot(inflationFeature)
		activationSlots = append(activationSlots, activationSlot)
	}

	sort.Slice(activationSlots, func(i, j int) bool {
		return activationSlots[i] < activationSlots[j]
	})

	if len(activationSlots) == 0 {
		picoActivationSlot, isActivated := f.ActivationSlot(features.PicoInflation)
		if !isActivated {
			return 0
		} else {
			return picoActivationSlot
		}
	} else {
		return activationSlots[0]
	}
}

func CalculatePreviousEpochInflationRewards(epochSchedule *sealevel.SysvarEpochSchedule, inflation *Inflation, prevEpochCapitalization, epoch, prevEpoch uint64, slotsPerYear float64, f *features.Features) uint64 {
	slotInYear := SlotInYearForInflation(epochSchedule, slotsPerYear, epoch, f)
	validatorRate := inflation.Validator(slotInYear)
	prevEpochDurationInYears := float64(epochSchedule.SlotsInEpoch(prevEpoch)) / slotsPerYear

	validatorRewards := validatorRate * float64(prevEpochCapitalization) * prevEpochDurationInYears
	return uint64(validatorRewards)
}

// RewardPoolInputs contains the intermediate values used in reward pool calculation.
// This is useful for debugging reward discrepancies.
type RewardPoolInputs struct {
	SlotInYear               float64
	ValidatorRate            float64
	PrevEpochDurationInYears float64
	SlotsPerYear             float64
}

// GetRewardPoolInputs returns the intermediate values used in the reward pool calculation.
// These values can be compared with reference implementations to identify discrepancies.
func GetRewardPoolInputs(epochSchedule *sealevel.SysvarEpochSchedule, inflation *Inflation, epoch, prevEpoch uint64, slotsPerYear float64, f *features.Features) RewardPoolInputs {
	slotInYear := SlotInYearForInflation(epochSchedule, slotsPerYear, epoch, f)
	validatorRate := inflation.Validator(slotInYear)
	prevEpochDurationInYears := float64(epochSchedule.SlotsInEpoch(prevEpoch)) / slotsPerYear

	return RewardPoolInputs{
		SlotInYear:               slotInYear,
		ValidatorRate:            validatorRate,
		PrevEpochDurationInYears: prevEpochDurationInYears,
		SlotsPerYear:             slotsPerYear,
	}
}

func IsWithinRewardsPeriod(epoch uint64, slot uint64, epochSchedule *sealevel.SysvarEpochSchedule) bool {
	firstSlotInEpoch := epochSchedule.FirstSlotInEpoch(epoch)
	if slot < (firstSlotInEpoch + 243) {
		return true
	} else {
		return false
	}
}

// DeterminePartitionedStakingRewardsInfo fetches reward partition info from RPC with failover support.
// It tries the primary RPC first with retries, then falls back to backup endpoints.
func DeterminePartitionedStakingRewardsInfo(rpcc *rpcclient.RpcClient, rpcBackups []string, epochSchedule *sealevel.SysvarEpochSchedule, inflation *Inflation, prevEpochCapitalization uint64, epoch uint64, prevEpoch uint64, slot uint64, slotsPerYear float64, f *features.Features) *PartitionedRewardDistributionInfo {
	firstSlotInEpoch := epochSchedule.FirstSlotInEpoch(epoch)

	// Try to fetch reward partition info with failover
	numRewardPartitions, rewardSlots, err := fetchRewardPartitionInfoWithBackups(rpcc, rpcBackups, firstSlotInEpoch)
	if err != nil {
		panic(fmt.Sprintf("failed to fetch reward partition info from all RPC endpoints: %v", err))
	}

	if numRewardPartitions > 500 {
		panic(fmt.Sprintf("num_reward_partitions returned by RPC node too large: %d", numRewardPartitions))
	}

	if len(rewardSlots) == 0 {
		panic("RPC node returned empty reward blocks response")
	}

	finalStakingRewardSlot := rewardSlots[len(rewardSlots)-1]
	totalStakingRewards := CalculatePreviousEpochInflationRewards(epochSchedule, inflation, prevEpochCapitalization, epoch, prevEpoch, slotsPerYear, f)

	eahCalcSlot := firstSlotInEpoch + (432000 / 4)
	eahInclusionSlot := firstSlotInEpoch + ((432000 / 4) * 3)

	return &PartitionedRewardDistributionInfo{TotalStakingRewards: totalStakingRewards, FirstStakingRewardSlot: firstSlotInEpoch + 1,
		LastStakingRewardSlot: finalStakingRewardSlot, EahStartOffsetSlot: eahCalcSlot, EahStopOffsetSlot: eahInclusionSlot, NumRewardPartitions: numRewardPartitions}
}

// fetchRewardPartitionInfoWithBackups tries the primary RPC first with retries, then backup endpoints.
func fetchRewardPartitionInfoWithBackups(rpcc *rpcclient.RpcClient, rpcBackups []string, firstSlotInEpoch uint64) (uint64, []uint64, error) {
	// Try primary first with retries
	numPartitions, slots, err := fetchRewardPartitionInfoWithRetry(rpcc, firstSlotInEpoch, 5)
	if err == nil {
		return numPartitions, slots, nil
	}

	lastErr := err
	mlog.Log.Errorf("reward partition fetch failed on primary %s: %v", rpcc.Endpoint(), err)

	// Try backup endpoints
	for i, endpoint := range rpcBackups {
		mlog.Log.Infof("trying backup RPC endpoint #%d for reward partitions: %s", i+1, endpoint)
		backupClient := rpcclient.NewRpcClient(endpoint)
		numPartitions, slots, err := fetchRewardPartitionInfoWithRetry(backupClient, firstSlotInEpoch, 3)
		if err == nil {
			mlog.Log.Infof("reward partition info fetched from backup endpoint %s", endpoint)
			return numPartitions, slots, nil
		}
		lastErr = err
		mlog.Log.Errorf("reward partition fetch failed on backup %s: %v", endpoint, err)
	}

	return 0, nil, fmt.Errorf("all endpoints failed, last error: %w", lastErr)
}

// fetchRewardPartitionInfoWithRetry attempts to fetch reward partition info with exponential backoff.
func fetchRewardPartitionInfoWithRetry(rpcc *rpcclient.RpcClient, firstSlotInEpoch uint64, maxAttempts int) (uint64, []uint64, error) {
	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		// First get num partitions
		numRewardPartitions, err := rpcc.GetNumRewardPartitions(firstSlotInEpoch)
		if err != nil {
			lastErr = err
			if attempt < maxAttempts-1 {
				waitTime := time.Duration(1<<attempt) * time.Second
				if waitTime > 30*time.Second {
					waitTime = 30 * time.Second
				}
				mlog.Log.Infof("GetNumRewardPartitions from %s failed, retrying in %v (attempt %d/%d): %v",
					rpcc.Endpoint(), waitTime, attempt+1, maxAttempts, err)
				time.Sleep(waitTime)
			}
			continue
		}

		// Then get reward slots
		rewardSlots, err := rpcc.GetStakingRewardSlots(firstSlotInEpoch, numRewardPartitions)
		if err != nil {
			lastErr = err
			if attempt < maxAttempts-1 {
				waitTime := time.Duration(1<<attempt) * time.Second
				if waitTime > 30*time.Second {
					waitTime = 30 * time.Second
				}
				mlog.Log.Infof("GetStakingRewardSlots from %s failed, retrying in %v (attempt %d/%d): %v",
					rpcc.Endpoint(), waitTime, attempt+1, maxAttempts, err)
				time.Sleep(waitTime)
			}
			continue
		}

		// Both succeeded
		return numRewardPartitions, rewardSlots, nil
	}

	return 0, nil, fmt.Errorf("failed after %d attempts: %w", maxAttempts, lastErr)
}

// StakeAccountStats holds detailed statistics about stake account filtering.
type StakeAccountStats struct {
	Total        uint64 // Total stake accounts in cache
	Eligible     uint64 // Accounts that will earn rewards
	BelowMin     uint64 // Accounts below minimum stake delegation
	NoVote       uint64 // Accounts with no vote state in cache
	NoCredits    uint64 // Accounts with vote but no new credits to earn (creditsInVote <= creditsInStake)
	ZeroPoints   uint64 // Accounts with vote+credits but zero effective stake (warmup/cooldown) → points=0
	ZeroStake    uint64 // Accounts with StakeLamports == 0 (subset of BelowMin when min > 0)
	ZeroWithVote uint64 // Zero-stake accounts that DO have vote cache (potential overcounting)
	MinimumStake uint64 // The minimum stake delegation threshold

	// Boundary analysis - helps debug partition count mismatches
	MinEligibleStake      uint64 // Minimum stake among eligible accounts
	MaxIneligibleWithVote uint64 // Max stake among accounts with vote but below min (boundary cases)

	// Histogram buckets (accounts with vote cache, by stake range)
	// This helps identify where Agave's cutoff might differ
	StakeZero    uint64 // stake == 0
	Stake1To999  uint64 // 1-999 lamports
	Stake1KTo1M  uint64 // 1,000 - 999,999 lamports (< 0.001 SOL)
	Stake1MTo1B  uint64 // 1,000,000 - 999,999,999 lamports (0.001 - 1 SOL)
	StakeAbove1B uint64 // >= 1,000,000,000 lamports (>= 1 SOL)
}

// CountEligibleStakeAccounts counts stake accounts that will actually earn rewards for the given epoch.
// This matches Agave's filtering logic: points > 0 (effective stake > 0 AND new credits to earn).
// IMPORTANT: This count is used for partition calculation, not the raw stake cache size.
// rewardedEpoch is the epoch being rewarded (typically prevEpoch at epoch boundary).
func CountEligibleStakeAccounts(rewardedEpoch uint64, f *features.Features, stakeHistory *sealevel.SysvarStakeHistory, newRateActivationEpoch *uint64) (eligible uint64, total uint64, belowMin uint64, noVote uint64, noCredits uint64, zeroPoints uint64) {
	stats := CountEligibleStakeAccountsDetailed(rewardedEpoch, f, stakeHistory, newRateActivationEpoch)
	return stats.Eligible, stats.Total, stats.BelowMin, stats.NoVote, stats.NoCredits, stats.ZeroPoints
}

// CountEligibleStakeAccountsWithRewardsFilter counts stake accounts using the FULL Firedancer/Agave filter:
// 1. stake >= minimum
// 2. has vote cache entry
// 3. SPECIAL CASE: force_credits_update (credits < stake OR activation == rewarded) → count immediately
// 4. If credits == stake: skip (no credits earned)
// 5. points > 0 (has credits to earn)
// 6. rewards > 0 after integer division (points * total_rewards / total_points > 0)
// 7. commission split yields non-zero for both voter AND staker (when commission is 1-99%)
//
// This is a two-pass algorithm:
// - Pass 1: Calculate total_points, identify force_credits_update accounts (counted separately)
// - Pass 2: Count accounts where rewards > 0 AND commission split is valid
//
// Returns: eligible, total, belowMin, noVote, noCredits, zeroPoints, zeroRewards, zeroSplit
// Also logs diagnostic buckets for debugging
func CountEligibleStakeAccountsWithRewardsFilter(
	rewardedEpoch uint64,
	f *features.Features,
	stakeHistory *sealevel.SysvarStakeHistory,
	newRateActivationEpoch *uint64,
	totalRewards uint64,
) (eligible uint64, total uint64, belowMin uint64, noVote uint64, noCredits uint64, zeroPoints uint64, zeroRewards uint64, zeroSplit uint64) {
	minimum := minimumStakeDelegationFromFeatures(f)
	stakeCache := global.StakeCache()
	total = uint64(len(stakeCache))

	mlog.Log.Infof("counting eligible stake accounts (with rewards filter): total=%d rewarded_epoch=%d total_rewards=%d (looking for activation_epoch==%d)",
		total, rewardedEpoch, totalRewards, rewardedEpoch)

	// Structure to hold account info for second pass
	type accountPoints struct {
		pubkey          solana.PublicKey
		voterPubkey     solana.PublicKey
		points          wide.Uint128
		commission      uint8  // vote account commission (0-100)
		creditsInStake  uint64 // for sampling
		creditsInVote   uint64 // for sampling
		activationEpoch uint64 // for sampling
		stake           uint64 // for sampling
	}
	var accountsWithPoints []accountPoints
	var totalPoints wide.Uint128

	// Accounts counted via force_credits_update_with_skipped_reward (get 0 rewards but still counted)
	var forceCreditsUpdateCount uint64
	var activationMatchCount uint64

	// EFFECTIVE STAKE TRACKING: sum of stake for all accounts contributing to total_points
	var effectiveStake uint64          // sum of stake for accounts with points > 0
	var totalNewCredits uint64         // sum of all new credits from accounts with points > 0
	var activationMatchWithPoints uint64    // count of activation_epoch==rewarded accounts with points > 0
	var activationMatchStake uint64    // stake from activation_epoch==rewarded accounts with points > 0
	var activationMatchCredits uint64  // new credits from activation_epoch==rewarded accounts with points > 0
	var activationMatchPoints wide.Uint128 // points from activation_epoch==rewarded accounts

	// PASS 1: Calculate total_points and collect accounts
	for pubkey, delegation := range stakeCache {
		stake := delegation.StakeLamports
		voteState := global.VoteCacheItem(delegation.VoterPubkey)
		hasVote := voteState != nil

		if stake < minimum {
			belowMin++
			continue
		}

		if !hasVote {
			noVote++
			continue
		}

		// Get credits and commission from vote state
		creditsInStake := delegation.CreditsObserved
		var epochCredits []sealevel.EpochCredits
		var commission uint8
		switch voteState.Type {
		case sealevel.VoteStateVersionCurrent:
			epochCredits = voteState.Current.EpochCredits
			commission = voteState.Current.Commission
		case sealevel.VoteStateVersionV0_23_5:
			epochCredits = voteState.V0_23_5.EpochCredits
			commission = voteState.V0_23_5.Commission
		case sealevel.VoteStateVersionV1_14_11:
			epochCredits = voteState.V1_14_11.EpochCredits
			commission = voteState.V1_14_11.Commission
		}

		var creditsInVote uint64
		if len(epochCredits) > 0 {
			creditsInVote = epochCredits[len(epochCredits)-1].Credits
		}

		// FORCE_CREDITS_UPDATE CHECK #1: credits_in_vote < credits_in_stake
		if creditsInVote < creditsInStake {
			forceCreditsUpdateCount++
			continue
		}

		// FORCE_CREDITS_UPDATE CHECK #2: activation_epoch == rewarded_epoch
		if delegation.ActivationEpoch == rewardedEpoch {
			forceCreditsUpdateCount++
			activationMatchCount++
			// Calculate points if applicable
			if creditsInVote > creditsInStake && len(epochCredits) > 0 {
				calculatedPoints := calculateStakePointsAndCredits(pubkey, stakeHistory, delegation, voteState, newRateActivationEpoch, nil)
				if !calculatedPoints.Points.Eq(wide.Uint128FromUint64(0)) {
					totalPoints = totalPoints.Add(calculatedPoints.Points)
					activationMatchWithPoints++
					activationMatchStake += stake
					activationMatchCredits += creditsInVote - creditsInStake
					activationMatchPoints = activationMatchPoints.Add(calculatedPoints.Points)
				}
			}
			continue
		}

		// No new credits - skip
		if creditsInVote == creditsInStake || len(epochCredits) == 0 {
			noCredits++
			continue
		}

		// Normal case: calculate points
		calculatedPoints := calculateStakePointsAndCredits(pubkey, stakeHistory, delegation, voteState, newRateActivationEpoch, nil)
		zero128 := wide.Uint128FromUint64(0)

		if calculatedPoints.Points.Eq(zero128) {
			zeroPoints++
			continue
		}

		// This account has points > 0, add to our list with commission
		accountsWithPoints = append(accountsWithPoints, accountPoints{
			pubkey:          pubkey,
			voterPubkey:     delegation.VoterPubkey,
			points:          calculatedPoints.Points,
			commission:      commission,
			creditsInStake:  creditsInStake,
			creditsInVote:   creditsInVote,
			activationEpoch: delegation.ActivationEpoch,
			stake:           stake,
		})
		totalPoints = totalPoints.Add(calculatedPoints.Points)

		// Track effective stake and credits
		effectiveStake += stake
		if creditsInVote > creditsInStake {
			totalNewCredits += creditsInVote - creditsInStake
		}
	}

	// Key comparison values for RPC validation
	mlog.Log.Infof("COMPARE WITH RPC: total_points=%s total_rewards=%d", totalPoints.String(), totalRewards)

	// EFFECTIVE STAKE SUMMARY: These totals help identify if we have different eligible accounts than RPC
	totalEffectiveStake := effectiveStake + activationMatchStake
	totalEffectiveCredits := totalNewCredits + activationMatchCredits
	totalAccountsWithPoints := uint64(len(accountsWithPoints)) + activationMatchWithPoints
	mlog.Log.Infof("┌─────────────────────────────────────────────────────────────────────────────┐")
	mlog.Log.Infof("│ ELIGIBLE ACCOUNT SUMMARY (ALL accounts contributing to total_points)       │")
	mlog.Log.Infof("├─────────────────────────────────────────────────────────────────────────────┤")
	mlog.Log.Infof("│ NORMAL accounts (points > 0):                                              │")
	mlog.Log.Infof("│   count:               %d", len(accountsWithPoints))
	mlog.Log.Infof("│   effective_stake:     %d lamports (%.2f SOL)", effectiveStake, float64(effectiveStake)/1e9)
	mlog.Log.Infof("│   new_credits:         %d", totalNewCredits)
	mlog.Log.Infof("│                                                                             │")
	mlog.Log.Infof("│ ACTIVATION_EPOCH==REWARDED accounts (with points > 0):                      │")
	mlog.Log.Infof("│   count:               %d (of %d total activation matches)", activationMatchWithPoints, activationMatchCount)
	mlog.Log.Infof("│   effective_stake:     %d lamports (%.2f SOL)", activationMatchStake, float64(activationMatchStake)/1e9)
	mlog.Log.Infof("│   new_credits:         %d", activationMatchCredits)
	mlog.Log.Infof("│   points:              %s", activationMatchPoints.String())
	mlog.Log.Infof("│                                                                             │")
	mlog.Log.Infof("│ COMBINED TOTALS:                                                            │")
	mlog.Log.Infof("│   total_accounts:      %d", totalAccountsWithPoints)
	mlog.Log.Infof("│   total_stake:         %d lamports (%.2f SOL)", totalEffectiveStake, float64(totalEffectiveStake)/1e9)
	mlog.Log.Infof("│   total_new_credits:   %d", totalEffectiveCredits)
	mlog.Log.Infof("│   total_points:        %s", totalPoints.String())
	if totalAccountsWithPoints > 0 {
		avgStake := totalEffectiveStake / totalAccountsWithPoints
		avgCredits := totalEffectiveCredits / totalAccountsWithPoints
		mlog.Log.Infof("│   avg_stake:           %d lamports (%.4f SOL)", avgStake, float64(avgStake)/1e9)
		mlog.Log.Infof("│   avg_new_credits:     %d", avgCredits)
	}
	mlog.Log.Infof("│                                                                             │")
	mlog.Log.Infof("│ If LOCAL total_points < RPC total_points:                                   │")
	mlog.Log.Infof("│   → LOCAL has fewer eligible accounts OR lower credits_observed             │")
	mlog.Log.Infof("│   → This makes LOCAL rewards_per_point HIGHER → all rewards inflate         │")
	mlog.Log.Infof("│                                                                             │")
	mlog.Log.Infof("│ Root cause hypothesis: If LOCAL total_points is ~0.004%% lower:             │")
	mlog.Log.Infof("│   → ~%d accounts excluded OR ~%d credits missing", totalAccountsWithPoints*4/100000, totalEffectiveCredits*4/100000)
	mlog.Log.Infof("└─────────────────────────────────────────────────────────────────────────────┘")

	// PASS 2: Count accounts where rewards > 0 AND commission split is valid
	// Formula: rewards = points * total_rewards / total_points
	// Commission split: voter_portion = rewards * commission / 100
	//                   staker_portion = rewards * (100 - commission) / 100
	// is_split = commission > 0 && commission < 100
	// Skip if: rewards == 0 OR (is_split AND (voter_portion == 0 OR staker_portion == 0))

	// Start with force_credits_update accounts (they're counted but got 0 rewards)
	eligible = forceCreditsUpdateCount

	if totalPoints.Eq(wide.Uint128FromUint64(0)) {
		// No points at all from normal accounts
		mlog.Log.Infof("  pass 2: total_points=0, only force_credits_update accounts eligible")
		mlog.Log.Infof("ELIGIBILITY FILTER RESULT: eligible=%d (force_credits_update=%d + normal=0)",
			eligible, forceCreditsUpdateCount)
		return eligible, total, belowMin, noVote, noCredits, zeroPoints, uint64(len(accountsWithPoints)), 0
	}

	totalRewards128 := wide.Uint128FromUint64(totalRewards)

	for _, acct := range accountsWithPoints {
		// rewards = points * total_rewards / total_points
		numerator := acct.points.Mul(totalRewards128)
		rewards := numerator.Div(totalPoints)

		if rewards.Eq(wide.Uint128FromUint64(0)) {
			zeroRewards++
			continue
		}

		// Commission split check (Firedancer fd_rewards.c lines 400-404)
		// is_split = commission > 0 && commission < 100
		// If is_split AND (voter_portion == 0 OR staker_portion == 0), skip
		commission := acct.commission
		if commission > 100 {
			commission = 100 // Cap at 100 like Firedancer does
		}

		isSplit := commission > 0 && commission < 100
		if isSplit {
			// Use 128-bit arithmetic to match Firedancer exactly (fd_rewards.c lines 334-337)
			// This ensures identical rounding behavior for the commission split
			commission128 := wide.Uint128FromUint64(uint64(commission))
			complement128 := wide.Uint128FromUint64(uint64(100 - commission))
			hundred128 := wide.Uint128FromUint64(100)

			// voter_portion = rewards * commission / 100
			voterPortion := rewards.Mul(commission128).Div(hundred128)
			// staker_portion = rewards * (100 - commission) / 100
			stakerPortion := rewards.Mul(complement128).Div(hundred128)

			zero128 := wide.Uint128FromUint64(0)
			if voterPortion.Eq(zero128) || stakerPortion.Eq(zero128) {
				zeroSplit++
				continue
			}
		}

		eligible++
	}

	// Diagnostic buckets summary
	pointsGtZero := uint64(len(accountsWithPoints))
	normalEligible := eligible - forceCreditsUpdateCount
	mlog.Log.Infof("FILTER PIPELINE: total=%d -> below_min=%d -> no_vote=%d -> force_credits=%d -> no_credits=%d -> zero_points=%d -> points>0=%d",
		total, belowMin, noVote, forceCreditsUpdateCount, noCredits, zeroPoints, pointsGtZero)
	mlog.Log.Infof("ELIGIBILITY: %d (force_credits=%d + normal=%d) | zero_rewards=%d zero_split=%d filtered in pass2",
		eligible, forceCreditsUpdateCount, normalEligible, zeroRewards, zeroSplit)

	return eligible, total, belowMin, noVote, noCredits, zeroPoints, zeroRewards, zeroSplit
}

// CountEligibleStakeAccountsDetailed returns detailed statistics about stake account filtering.
// Use this for debugging partition count mismatches.
// Eligibility requires: stake >= min AND has vote cache AND points > 0 (any epoch).
// Uses all-epochs filter (matches Firedancer/Agave: accounts catching up on credits are included).
// Also logs a diagnostic comparing both predicates for verification.
func CountEligibleStakeAccountsDetailed(rewardedEpoch uint64, f *features.Features, stakeHistory *sealevel.SysvarStakeHistory, newRateActivationEpoch *uint64) StakeAccountStats {
	minimum := minimumStakeDelegationFromFeatures(f)
	stakeCache := global.StakeCache()
	stats := StakeAccountStats{
		Total:            uint64(len(stakeCache)),
		MinimumStake:     minimum,
		MinEligibleStake: ^uint64(0), // Start with max value
	}

	mlog.Log.Infof("counting eligible stake accounts: total=%d rewarded_epoch=%d", stats.Total, rewardedEpoch)

	// DIAGNOSTIC: Track both eligibility predicates to determine which matches RPC
	var eligibleAllEpochs uint64      // calculateStakePointsAndCredits (points > 0 across all epochs)
	var eligibleRewardedEpochOnly uint64 // points > 0 for rewarded epoch only

	var processed uint64
	for pubkey, delegation := range stakeCache {
		processed++
		if processed%100000 == 0 {
			mlog.Log.Infof("  progress: %d/%d accounts (%.1f%%)", processed, stats.Total, float64(processed)*100/float64(stats.Total))
		}
		stake := delegation.StakeLamports
		voteState := global.VoteCacheItem(delegation.VoterPubkey)
		hasVote := voteState != nil

		// Track zero-stake accounts explicitly
		if stake == 0 {
			stats.ZeroStake++
			if hasVote {
				stats.ZeroWithVote++
			}
		}

		// Histogram for accounts WITH vote cache (these are candidates for eligibility)
		if hasVote {
			switch {
			case stake == 0:
				stats.StakeZero++
			case stake < 1000:
				stats.Stake1To999++
			case stake < 1000000:
				stats.Stake1KTo1M++
			case stake < 1000000000:
				stats.Stake1MTo1B++
			default:
				stats.StakeAbove1B++
			}
		}

		if stake < minimum {
			stats.BelowMin++
			// Track max stake among accounts that have vote but are below minimum
			if hasVote && stake > stats.MaxIneligibleWithVote {
				stats.MaxIneligibleWithVote = stake
			}
			continue
		}

		if !hasVote {
			stats.NoVote++
			continue
		}

		// Check if this account will earn rewards (has new credits)
		// An account earns rewards only if creditsInVote > creditsInStake
		creditsInStake := delegation.CreditsObserved
		var epochCredits []sealevel.EpochCredits
		switch voteState.Type {
		case sealevel.VoteStateVersionCurrent:
			epochCredits = voteState.Current.EpochCredits
		case sealevel.VoteStateVersionV0_23_5:
			epochCredits = voteState.V0_23_5.EpochCredits
		case sealevel.VoteStateVersionV1_14_11:
			epochCredits = voteState.V1_14_11.EpochCredits
		}

		var creditsInVote uint64
		if len(epochCredits) > 0 {
			creditsInVote = epochCredits[len(epochCredits)-1].Credits
		}

		// No new credits to earn - this account won't earn rewards
		if creditsInVote <= creditsInStake || len(epochCredits) == 0 {
			stats.NoCredits++
			continue
		}

		// DIAGNOSTIC: Check BOTH eligibility predicates to determine which matches RPC
		// Method 1: All epochs (calculateStakePointsAndCredits) - sums points across all epochCredits
		calculatedPoints := calculateStakePointsAndCredits(pubkey, stakeHistory, delegation, voteState, newRateActivationEpoch, nil)
		zero128 := wide.Uint128FromUint64(0)
		hasPointsAllEpochs := !calculatedPoints.Points.Eq(zero128)

		// Method 2: Rewarded epoch only - check if THIS specific epoch has points > 0
		hasPointsRewardedEpoch := hasPositivePointsForEpoch(delegation, epochCredits, creditsInStake, rewardedEpoch, stakeHistory, newRateActivationEpoch)

		// Track both counts for diagnostic comparison
		if hasPointsAllEpochs {
			eligibleAllEpochs++
		}
		if hasPointsRewardedEpoch {
			eligibleRewardedEpochOnly++
		}

		// Use all-epochs method for actual eligibility (matches Firedancer/Agave behavior)
		// Accounts with points > 0 across ANY epoch in epochCredits are included
		// The diagnostic log shows both predicates for verification
		if !hasPointsAllEpochs {
			stats.ZeroPoints++
			continue
		}

		// This account is eligible (stake >= min, has vote, points > 0)
		stats.Eligible++
		if stake < stats.MinEligibleStake {
			stats.MinEligibleStake = stake
		}
	}

	// Fix MinEligibleStake if no eligible accounts found
	if stats.Eligible == 0 {
		stats.MinEligibleStake = 0
	}

	// VERIFICATION: Filter counts MUST add up to total.
	// If they don't, there's a bug in the filtering logic.
	filterSum := stats.Eligible + stats.BelowMin + stats.NoVote + stats.NoCredits + stats.ZeroPoints
	if filterSum != stats.Total {
		mlog.Log.Errorf("PARTITION FILTER BUG: filter counts don't add up! total=%d but sum=%d (eligible=%d below_min=%d no_vote=%d no_credits=%d zero_points=%d)",
			stats.Total, filterSum, stats.Eligible, stats.BelowMin, stats.NoVote, stats.NoCredits, stats.ZeroPoints)
	}

	// Log detailed stats for debugging partition mismatches
	mlog.Log.Infof("stake account stats: total=%d eligible=%d below_min=%d no_vote=%d no_credits=%d zero_points=%d zero_stake=%d zero_with_vote=%d min_stake=%d",
		stats.Total, stats.Eligible, stats.BelowMin, stats.NoVote, stats.NoCredits, stats.ZeroPoints, stats.ZeroStake, stats.ZeroWithVote, stats.MinimumStake)
	mlog.Log.Infof("  boundary analysis: min_eligible_stake=%d max_ineligible_with_vote=%d",
		stats.MinEligibleStake, stats.MaxIneligibleWithVote)
	mlog.Log.Infof("  histogram (with vote): zero=%d 1-999=%d 1K-1M=%d 1M-1B=%d >=1B=%d",
		stats.StakeZero, stats.Stake1To999, stats.Stake1KTo1M, stats.Stake1MTo1B, stats.StakeAbove1B)
	mlog.Log.Infof("  partition calc: eligible=%d -> partitions=%d (if max_rewards_per_block=4096)",
		stats.Eligible, (stats.Eligible+MaxRewardsPerBlock-1)/MaxRewardsPerBlock)

	// DIAGNOSTIC: Compare both eligibility predicates to determine which matches RPC
	// This removes cache/feature/timing confounds and tells us which predicate Agave uses
	partitionsAllEpochs := (eligibleAllEpochs + MaxRewardsPerBlock - 1) / MaxRewardsPerBlock
	partitionsRewardedOnly := (eligibleRewardedEpochOnly + MaxRewardsPerBlock - 1) / MaxRewardsPerBlock
	mlog.Log.Infof("ELIGIBILITY DIAGNOSTIC: all_epochs=%d (partitions=%d) | rewarded_epoch_only=%d (partitions=%d) | delta=%d",
		eligibleAllEpochs, partitionsAllEpochs, eligibleRewardedEpochOnly, partitionsRewardedOnly,
		int64(eligibleAllEpochs)-int64(eligibleRewardedEpochOnly))

	return stats
}

// hasPositivePoints checks if a delegation would earn any points (points > 0).
// This matches the logic in calculateStakePointsAndCredits.
// Returns true if there's at least one epoch where earnedCredits > 0 AND effectiveStake > 0.
func hasPositivePoints(
	delegation *sealevel.Delegation,
	epochCredits []sealevel.EpochCredits,
	creditsInStake uint64,
	stakeHistory *sealevel.SysvarStakeHistory,
	newRateActivationEpoch *uint64,
) bool {
	newObserved := creditsInStake

	for _, ec := range epochCredits {
		final := ec.Credits
		initial := ec.PrevCredits

		var earnedCredits uint64
		if creditsInStake < initial {
			earnedCredits = final - initial
		} else if creditsInStake < final {
			earnedCredits = final - newObserved
		}

		if earnedCredits != 0 {
			// Check effective stake for this epoch
			effectiveStake := delegation.StakeActivatingAndDeactivating(ec.Epoch, stakeHistory, newRateActivationEpoch).Effective
			if effectiveStake > 0 {
				// Found at least one epoch with earnedCredits > 0 AND effectiveStake > 0
				return true
			}
		}

		newObserved = max(newObserved, final)
	}

	return false
}

// hasPositivePointsForEpoch checks if a delegation would earn any points for a SPECIFIC epoch.
// This is used for diagnostic comparison - checking if the rewarded epoch filter matches RPC.
// Returns true if the rewarded epoch has earnedCredits > 0 AND effectiveStake > 0.
func hasPositivePointsForEpoch(
	delegation *sealevel.Delegation,
	epochCredits []sealevel.EpochCredits,
	creditsInStake uint64,
	rewardedEpoch uint64,
	stakeHistory *sealevel.SysvarStakeHistory,
	newRateActivationEpoch *uint64,
) bool {
	newObserved := creditsInStake

	for _, ec := range epochCredits {
		final := ec.Credits
		initial := ec.PrevCredits

		var earnedCredits uint64
		if creditsInStake < initial {
			earnedCredits = final - initial
		} else if creditsInStake < final {
			earnedCredits = final - newObserved
		}

		// Only check the specific rewarded epoch
		if ec.Epoch == rewardedEpoch && earnedCredits != 0 {
			// Check effective stake for this epoch
			effectiveStake := delegation.StakeActivatingAndDeactivating(ec.Epoch, stakeHistory, newRateActivationEpoch).Effective
			if effectiveStake > 0 {
				return true
			}
		}

		newObserved = max(newObserved, final)
	}

	return false
}

// minimumStakeDelegationFromFeatures returns the minimum stake for REWARDS eligibility.
// This is a feature-only version that doesn't require SlotCtx.
func minimumStakeDelegationFromFeatures(f *features.Features) uint64 {
	if !f.IsActive(features.StakeMinimumDelegationForRewards) {
		return 0
	}
	if f.IsActive(features.StakeRaiseMinimumDelegationTo1Sol) {
		return 1000000000 // 1 SOL
	}
	return 1
}

// DeterminePartitionedStakingRewardsInfoLocal computes reward partition info locally without RPC.
// Uses ELIGIBLE stake account count (filtered by rewards > 0 after integer division) for partition calculation.
// This matches Firedancer/Agave's behavior where only accounts that actually receive non-zero rewards are counted.
// Returns PartitionMismatchError if validation is enabled and local partition count doesn't match RPC.
func DeterminePartitionedStakingRewardsInfoLocal(
	epochSchedule *sealevel.SysvarEpochSchedule,
	inflation *Inflation,
	prevEpochCapitalization uint64,
	epoch uint64,
	prevEpoch uint64,
	slotsPerYear float64,
	f *features.Features,
	stakeHistory *sealevel.SysvarStakeHistory,
	newRateActivationEpoch *uint64,
) (*PartitionedRewardDistributionInfo, error) {
	firstSlotInEpoch := epochSchedule.FirstSlotInEpoch(epoch)

	// Calculate total_rewards FIRST (needed for rewards > 0 filter)
	totalStakingRewards := CalculatePreviousEpochInflationRewards(
		epochSchedule, inflation, prevEpochCapitalization, epoch, prevEpoch, slotsPerYear, f)

	// Count ELIGIBLE stake accounts using full Firedancer/Agave filter:
	// 1. points > 0 (has credits to earn)
	// 2. rewards > 0 after integer division (points * total_rewards / total_points > 0)
	// 3. commission split yields non-zero for both voter AND staker (when commission 1-99%)
	// prevEpoch is the epoch being rewarded (rewards for epoch N-1 are distributed at start of epoch N)
	eligibleAccounts, totalAccounts, belowMinAccounts, noVoteAccounts, noCreditsAccounts, zeroPointsAccounts, zeroRewardsAccounts, zeroSplitAccounts := CountEligibleStakeAccountsWithRewardsFilter(prevEpoch, f, stakeHistory, newRateActivationEpoch, totalStakingRewards)
	numStakeAccounts := eligibleAccounts

	// Get slots per epoch for this epoch
	slotsPerEpoch := epochSchedule.SlotsInEpoch(epoch)

	// Compute partitions using local formula
	numRewardPartitions := ComputeNumRewardPartitions(epoch, slotsPerEpoch, numStakeAccounts, epochSchedule.FirstNormalEpoch)

	// First reward slot is firstSlotInEpoch + 1
	firstStakingRewardSlot := firstSlotInEpoch + 1

	// Last reward slot is an upper bound (actual may be lower if slots skipped)
	// This is used for IsWithinRewardsPeriod bounds check
	lastStakingRewardSlot := firstSlotInEpoch + numRewardPartitions

	// EAH calculation slots (epoch accounts hash) - LEGACY, not used after AccountsLtHash (~Nov 2024)
	// Hardcoded 432000 is mainnet slots-per-epoch; doesn't matter since EAH is deprecated
	eahCalcSlot := firstSlotInEpoch + (432000 / 4)
	eahInclusionSlot := firstSlotInEpoch + ((432000 / 4) * 3)

	mlog.Log.Infof("local rewards partition: epoch=%d prev_epoch=%d first_slot=%d slots_per_epoch=%d",
		epoch, prevEpoch, firstSlotInEpoch, slotsPerEpoch)
	mlog.Log.Infof("  stake_accts: total=%d eligible=%d below_min=%d no_vote=%d no_credits=%d zero_points=%d zero_rewards=%d zero_split=%d",
		totalAccounts, eligibleAccounts, belowMinAccounts, noVoteAccounts, noCreditsAccounts, zeroPointsAccounts, zeroRewardsAccounts, zeroSplitAccounts)
	mlog.Log.Infof("  partitions=%d first_reward=%d last_reward=%d total_rewards=%d",
		numRewardPartitions, firstStakingRewardSlot, lastStakingRewardSlot, totalStakingRewards)

	// PRE-COMMIT VALIDATION: Validate partition count against RPC before processing epoch boundary.
	// This prevents committing corrupted state that would cause vote failures on subsequent blocks.
	if validatePartitionCount && validationRpcClient != nil {
		rpcNumPartitions, err := FetchRpcPartitionCountWithBackups(validationRpcClient, validationRpcBackups, firstSlotInEpoch)
		if err != nil {
			mlog.Log.Warnf("partition validation: RPC fetch failed (continuing anyway): %v", err)
		} else {
			mlog.Log.Infof("partition validation: local=%d rpc=%d", numRewardPartitions, rpcNumPartitions)
			if numRewardPartitions != rpcNumPartitions {
				// Fetch EpochRewards sysvar to get authoritative values
				epochRewardsData, err := validationRpcClient.GetEpochRewardsSysvar()
				if err != nil {
					mlog.Log.Warnf("partition validation: failed to fetch EpochRewards sysvar: %v", err)
					// Can't recover without RPC sysvar - return error
					return nil, &PartitionMismatchError{
						Epoch:             epoch,
						LocalPartitions:   numRewardPartitions,
						RpcPartitions:     rpcNumPartitions,
						EligibleStakeAcct: eligibleAccounts,
						TotalStakeAcct:    totalAccounts,
						BelowMinAcct:      belowMinAccounts,
						NoVoteAcct:        noVoteAccounts,
						NoCreditsAcct:     noCreditsAccounts,
						ZeroPointsAcct:    zeroPointsAccounts,
						ZeroRewardsAcct:   zeroRewardsAccounts,
						ZeroSplitAcct:     zeroSplitAccounts,
					}
				}

				var rpcEpochRewards sealevel.SysvarEpochRewards
				decoder := bin.NewBinDecoder(epochRewardsData)
				if err := rpcEpochRewards.UnmarshalWithDecoder(decoder); err != nil {
					mlog.Log.Warnf("partition validation: failed to decode EpochRewards sysvar: %v", err)
					return nil, &PartitionMismatchError{
						Epoch:             epoch,
						LocalPartitions:   numRewardPartitions,
						RpcPartitions:     rpcNumPartitions,
						EligibleStakeAcct: eligibleAccounts,
						TotalStakeAcct:    totalAccounts,
						BelowMinAcct:      belowMinAccounts,
						NoVoteAcct:        noVoteAccounts,
						NoCreditsAcct:     noCreditsAccounts,
						ZeroPointsAcct:    zeroPointsAccounts,
						ZeroRewardsAcct:   zeroRewardsAccounts,
						ZeroSplitAcct:     zeroSplitAccounts,
					}
				}

				// Successfully fetched RPC sysvar - use its authoritative values
				mlog.Log.Warnf("PARTITION MISMATCH RECOVERY: using RPC EpochRewards sysvar values instead of local computation")
				mlog.Log.Infof("  RPC sysvar: num_partitions=%d total_points=%s total_rewards=%d",
					rpcEpochRewards.NumPartitions, rpcEpochRewards.TotalPoints.String(), rpcEpochRewards.TotalRewards)
				mlog.Log.Infof("  LOCAL computed: partitions=%d total_rewards=%d", numRewardPartitions, totalStakingRewards)
				mlog.Log.Infof("  DIFF: partitions=%d rewards=%d",
					int64(rpcEpochRewards.NumPartitions)-int64(numRewardPartitions),
					int64(rpcEpochRewards.TotalRewards)-int64(totalStakingRewards))

				// Use RPC values for correct partition info
				// This matches how Firedancer handles the recalc path when epoch_rewards.active=true
				correctedNumPartitions := rpcEpochRewards.NumPartitions
				correctedTotalRewards := rpcEpochRewards.TotalRewards
				correctedLastRewardSlot := firstSlotInEpoch + correctedNumPartitions

				mlog.Log.Infof("  CORRECTED: partitions=%d first_reward=%d last_reward=%d total_rewards=%d",
					correctedNumPartitions, firstStakingRewardSlot, correctedLastRewardSlot, correctedTotalRewards)

				return &PartitionedRewardDistributionInfo{
					TotalStakingRewards:    correctedTotalRewards,
					FirstStakingRewardSlot: firstStakingRewardSlot,
					LastStakingRewardSlot:  correctedLastRewardSlot,
					EahStartOffsetSlot:     eahCalcSlot,
					EahStopOffsetSlot:      eahInclusionSlot,
					NumRewardPartitions:    correctedNumPartitions,
				}, nil
			}
		}
	}

	return &PartitionedRewardDistributionInfo{
		TotalStakingRewards:    totalStakingRewards,
		FirstStakingRewardSlot: firstStakingRewardSlot,
		LastStakingRewardSlot:  lastStakingRewardSlot,
		EahStartOffsetSlot:     eahCalcSlot,
		EahStopOffsetSlot:      eahInclusionSlot,
		NumRewardPartitions:    numRewardPartitions,
		EligibleCount:          eligibleAccounts,
	}, nil
}

type idxAndReward struct {
	idx    int
	reward rpc.BlockReward
}

func DistributeVotingRewards(acctsDb *accountsdb.AccountsDb, rewards []rpc.BlockReward, slot uint64) ([]*accounts.Account, []*accounts.Account, uint64, error) {
	var totalVotingRewards atomic.Uint64
	var workerErr atomic.Pointer[error]

	accts := make([]*accounts.Account, len(rewards))
	parentUpdatedAccts := make([]*accounts.Account, len(rewards))

	var wg sync.WaitGroup

	size := runtime.GOMAXPROCS(0) * 8
	workerPool, _ := ants.NewPoolWithFunc(size, func(i interface{}) {
		defer wg.Done()

		r := i.(idxAndReward)
		reward := r.reward
		idx := r.idx

		if string(reward.RewardType) == RewardTypeVoting /*&& reward.Lamports != 0*/ {
			stakeAcct, err := acctsDb.GetAccount(slot, reward.Pubkey)
			if err != nil {
				errVal := fmt.Errorf("unable to get acct %s from acctsdb for voting rewards distribution in slot %d: %w", reward.Pubkey, slot, err)
				workerErr.CompareAndSwap(nil, &errVal)
				return
			}

			// IDEMPOTENCY CHECK: If balance already matches PostBalance,
			// this reward was already applied in a previous partial run (crash recovery)
			if stakeAcct.Lamports == reward.PostBalance {
				// Already credited - include in result but don't modify or count again
				accts[idx] = stakeAcct
				return
			}

			parentUpdatedAccts[idx] = stakeAcct.Clone()

			stakeAcct.Lamports, err = safemath.CheckedAddU64(stakeAcct.Lamports, uint64(reward.Lamports))
			if err != nil {
				errVal := fmt.Errorf("overflow in voting rewards distribution in slot %d to acct %s: %w", slot, reward.Pubkey, err)
				workerErr.CompareAndSwap(nil, &errVal)
				return
			}

			if stakeAcct.Lamports != reward.PostBalance {
				errVal := fmt.Errorf("post-balance for acct %s in distributing voting rewards in slot %d did not match expected %d (actual %d)", reward.Pubkey, slot, reward.PostBalance, stakeAcct.Lamports)
				workerErr.CompareAndSwap(nil, &errVal)
				return
			}

			accts[idx] = stakeAcct

			new := totalVotingRewards.Add(uint64(reward.Lamports))
			if new < uint64(reward.Lamports) {
				errVal := fmt.Errorf("overflow in accumulating voting rewards in slot %d", slot)
				workerErr.CompareAndSwap(nil, &errVal)
				return
			}
		}
	})

	for idx, reward := range rewards {
		r := idxAndReward{idx: idx, reward: reward}
		wg.Add(1)
		workerPool.Invoke(r)
	}

	wg.Wait()
	workerPool.Release()

	// Check for worker errors
	if errPtr := workerErr.Load(); errPtr != nil {
		return nil, nil, 0, *errPtr
	}

	err := acctsDb.StoreAccounts(accts, slot)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("error updating accounts for voting rewards in slot %d: %w", slot, err)
	}

	return accts, parentUpdatedAccts, totalVotingRewards.Load(), nil
}

// DistributeVotingRewardsLocal distributes vote rewards using locally computed values
// instead of RPC-provided block rewards. This removes the RPC dependency for vote reward distribution.
//
// Parameters:
//   - acctsDb: the accounts database
//   - voteRewards: map of vote account pubkey -> lamports to distribute (from AggregateVoteRewardsFromStakingRewards)
//   - slot: the slot to write updates to
//
// Returns: (updatedAccts, parentUpdatedAccts, totalDistributed, error)
func DistributeVotingRewardsLocal(acctsDb *accountsdb.AccountsDb, voteRewards map[solana.PublicKey]uint64, slot uint64) ([]*accounts.Account, []*accounts.Account, uint64, error) {
	if len(voteRewards) == 0 {
		return nil, nil, 0, nil
	}

	var totalVotingRewards atomic.Uint64
	var workerErr atomic.Pointer[error]

	// Convert map to slice for parallel processing
	type voteRewardEntry struct {
		pubkey  solana.PublicKey
		lamports uint64
		idx     int
	}
	entries := make([]voteRewardEntry, 0, len(voteRewards))
	i := 0
	for pubkey, lamports := range voteRewards {
		if lamports > 0 {
			entries = append(entries, voteRewardEntry{pubkey: pubkey, lamports: lamports, idx: i})
			i++
		}
	}

	accts := make([]*accounts.Account, len(entries))
	parentUpdatedAccts := make([]*accounts.Account, len(entries))

	var wg sync.WaitGroup

	size := runtime.GOMAXPROCS(0) * 8
	workerPool, _ := ants.NewPoolWithFunc(size, func(task interface{}) {
		defer wg.Done()

		entry := task.(voteRewardEntry)

		voteAcct, err := acctsDb.GetAccount(slot, entry.pubkey)
		if err != nil {
			errVal := fmt.Errorf("unable to get vote acct %s from acctsdb for voting rewards distribution in slot %d: %w", entry.pubkey, slot, err)
			workerErr.CompareAndSwap(nil, &errVal)
			return
		}

		parentUpdatedAccts[entry.idx] = voteAcct.Clone()

		voteAcct.Lamports, err = safemath.CheckedAddU64(voteAcct.Lamports, entry.lamports)
		if err != nil {
			errVal := fmt.Errorf("overflow in voting rewards distribution in slot %d to acct %s: %w", slot, entry.pubkey, err)
			workerErr.CompareAndSwap(nil, &errVal)
			return
		}

		accts[entry.idx] = voteAcct

		new := totalVotingRewards.Add(entry.lamports)
		if new < entry.lamports {
			errVal := fmt.Errorf("overflow in accumulating voting rewards in slot %d", slot)
			workerErr.CompareAndSwap(nil, &errVal)
			return
		}
	})

	for _, entry := range entries {
		wg.Add(1)
		workerPool.Invoke(entry)
	}

	wg.Wait()
	workerPool.Release()

	// Check for worker errors
	if errPtr := workerErr.Load(); errPtr != nil {
		return nil, nil, 0, *errPtr
	}

	// Filter out nil entries (shouldn't happen, but defensive)
	var nonNilAccts []*accounts.Account
	for _, acct := range accts {
		if acct != nil {
			nonNilAccts = append(nonNilAccts, acct)
		}
	}

	err := acctsDb.StoreAccounts(nonNilAccts, slot)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("error updating accounts for voting rewards in slot %d: %w", slot, err)
	}

	return accts, parentUpdatedAccts, totalVotingRewards.Load(), nil
}

// AggregateVoteRewardsFromStakingRewards aggregates the VoterRewards portion from stake rewards
// by vote account. This sums the voter (commission) portion from all stake accounts delegated
// to each vote account.
func AggregateVoteRewardsFromStakingRewards(stakingRewards map[solana.PublicKey]*CalculatedStakeRewards) map[solana.PublicKey]uint64 {
	voteRewards := make(map[solana.PublicKey]uint64)
	stakeCache := global.StakeCache()

	var mismatchCount, notInCacheCount int

	for stakePubkey, reward := range stakingRewards {
		if reward == nil || reward.VoterRewards == 0 {
			continue
		}

		// Get the vote account this stake is delegated to
		delegation := stakeCache[stakePubkey]
		if delegation == nil {
			notInCacheCount++
			// Use reward.VotePubkey as fallback when not in cache
			voteRewards[reward.VotePubkey] += reward.VoterRewards
			continue
		}

		// Check for mismatch between reward's vote pubkey and stake cache
		if delegation.VoterPubkey != reward.VotePubkey {
			mismatchCount++
			mlog.Log.Warnf("VotePubkey mismatch: stake=%s cache=%s reward=%s lamports=%d",
				stakePubkey, delegation.VoterPubkey, reward.VotePubkey, reward.VoterRewards)
		}

		// Use reward.VotePubkey (from calculation time) not delegation.VoterPubkey (current cache)
		voteRewards[reward.VotePubkey] += reward.VoterRewards
	}

	if mismatchCount > 0 || notInCacheCount > 0 {
		mlog.Log.Infof("AggregateVoteRewards: %d mismatches, %d not-in-cache (using reward.VotePubkey for both)",
			mismatchCount, notInCacheCount)
	}

	return voteRewards
}

// NOTE: CompareVoteRewardsWithRPC and DebugDumpPerVoteAccountBreakdown have been removed.
// Detailed diagnostics are now written to a file by WriteEpochBoundaryDiagnostics in epoch_diag.go.
// Terminal output shows only a brief summary via LogEpochBoundarySummary.

type idxAndPubkey struct {
	idx    int
	pubkey solana.PublicKey
}


// DistributeStakingRewardsForPartition distributes staking rewards for a single partition.
// stakeAccountSnapshots: cached stake accounts captured during RefreshStakeCacheCreditsObserved
// writeSlot: the slot to write updated accounts to (current slot being processed)
//
// This function uses cached stake account data instead of re-reading from AccountsDB.
// This is critical because GetAccount ignores the slot parameter and returns current state,
// which can differ from boundary-slot state (accounts may have been closed, modified, etc.).
// Using cached accounts ensures we apply rewards to the exact state that was used for calculation.
func DistributeStakingRewardsForPartition(acctsDb *accountsdb.AccountsDb, partition *Partition, stakingRewards map[solana.PublicKey]*CalculatedStakeRewards, stakeAccountSnapshots map[solana.PublicKey]*accounts.Account, writeSlot uint64) ([]*accounts.Account, []*accounts.Account, uint64, uint64, error) {
	var distributedLamports atomic.Uint64
	var burnedLamports atomic.Uint64 // Track lamports that would have been distributed but account failed
	var workerErr atomic.Pointer[error]
	accts := make([]*accounts.Account, partition.NumPubkeys())
	parentAccts := make([]*accounts.Account, partition.NumPubkeys())

	var wg sync.WaitGroup

	size := runtime.GOMAXPROCS(0) * 8
	workerPool, _ := ants.NewPoolWithFunc(size, func(i interface{}) {
		defer wg.Done()

		ip := i.(idxAndPubkey)
		idx := ip.idx
		stakePk := ip.pubkey

		reward, ok := stakingRewards[stakePk]
		if !ok {
			//mlog.Log.Debugf("no staking rewards present in map for %s", stakePk)
			return
		}

		// Use cached stake account from snapshots instead of reading from AccountsDB.
		// This ensures we use the exact same state that was captured during refresh,
		// avoiding issues with GetAccount returning different (current) state.
		cachedAcct, hasCached := stakeAccountSnapshots[stakePk]
		if !hasCached {
			// Account not in snapshots - track as burned to match Firedancer/Agave behavior.
			// These lamports count toward DistributedRewards even though they're not distributed.
			burnedLamports.Add(reward.StakerRewards)
			mlog.Log.Debugf("rewards distribution: stake account %s not in cached snapshots - burning %d lamports", stakePk, reward.StakerRewards)
			return
		}

		// Clone the cached account so we can modify it without affecting the cache
		stakeAcct := cachedAcct.Clone()

		parentAccts[idx] = cachedAcct.Clone()

		// Deserialize the stake state (we know it's valid from the refresh check)
		stakeState, err := sealevel.UnmarshalStakeState(stakeAcct.Data)
		if err != nil {
			// This should not happen since we validated during refresh, but handle it gracefully.
			// Track as burned to match Firedancer/Agave behavior.
			burnedLamports.Add(reward.StakerRewards)
			mlog.Log.Warnf("rewards distribution: stake account %s cannot be deserialized - burning %d lamports", stakePk, reward.StakerRewards)
			mlog.Log.Warnf("  error: %v", err)
			return
		}

		// Verify it's still in Stake state (should always be true from refresh check)
		if stakeState.Status != sealevel.StakeStateV2StatusStake {
			// Track as burned to match Firedancer/Agave behavior.
			burnedLamports.Add(reward.StakerRewards)
			mlog.Log.Warnf("rewards distribution: stake account %s is not in Stake state (status=%d) - burning %d lamports", stakePk, stakeState.Status, reward.StakerRewards)
			return
		}

		stakeState.Stake.Stake.CreditsObserved = reward.NewCreditsObserved
		stakeState.Stake.Stake.Delegation.StakeLamports = safemath.SaturatingAddU64(stakeState.Stake.Stake.Delegation.StakeLamports, uint64(reward.StakerRewards))

		newStakeStateBytes, err := sealevel.MarshalStakeStake(stakeState)
		if err != nil {
			errVal := fmt.Errorf("unable to serialize new stake account state for %s in distributing partitioned rewards: %w", stakePk, err)
			workerErr.CompareAndSwap(nil, &errVal)
			return
		}
		copy(stakeAcct.Data, newStakeStateBytes)

		// update lamports in stake account
		stakeAcct.Lamports, err = safemath.CheckedAddU64(stakeAcct.Lamports, uint64(reward.StakerRewards))
		if err != nil {
			errVal := fmt.Errorf("overflow in partitioned epoch rewards distribution in writeSlot %d to acct %s: %w", writeSlot, stakePk, err)
			workerErr.CompareAndSwap(nil, &errVal)
			return
		}

		accts[idx] = stakeAcct
		distributedLamports.Add(reward.StakerRewards)
		//mlog.Log.Debugf("distributed partitioned rewards to %s, %d lamports", stakePk, reward.StakerRewards)
	})

	for idx, stakePk := range partition.Pubkeys() {
		ip := idxAndPubkey{idx: idx, pubkey: stakePk}
		wg.Add(1)
		workerPool.Invoke(ip)
	}
	wg.Wait()

	workerPool.Release()

	// Check for worker errors
	if errPtr := workerErr.Load(); errPtr != nil {
		return nil, nil, 0, 0, *errPtr
	}

	err := acctsDb.StoreAccounts(accts, writeSlot)
	if err != nil {
		return nil, nil, 0, 0, fmt.Errorf("error updating accounts for partitioned epoch rewards in writeSlot %d: %w", writeSlot, err)
	}

	return accts, parentAccts, distributedLamports.Load(), burnedLamports.Load(), nil
}

// minimumStakeDelegation returns the minimum stake for REWARDS eligibility.
func minimumStakeDelegation(slotCtx *sealevel.SlotCtx) uint64 {
	if !slotCtx.Features.IsActive(features.StakeMinimumDelegationForRewards) {
		return 0
	}
	if slotCtx.Features.IsActive(features.StakeRaiseMinimumDelegationTo1Sol) {
		return 1000000000 // 1 SOL
	}
	return 1
}

func CalculateRewardPartitionForPubkey(pubkey solana.PublicKey, blockhash [32]byte, numPartitions uint64) uint64 {
	var data [64]byte
	copy(data[:32], blockhash[:])
	copy(data[32:], pubkey[:])
	hash := sip13.Sum64(0, 0, data[:])

	ulongMaxPlus1 := wide.Uint128FromUint64(math.MaxUint64).Add(wide.Uint128FromUint64(1))
	partitionIdx := wide.Uint128FromUint64(numPartitions).Mul(wide.Uint128FromUint64(hash)).Div(ulongMaxPlus1)
	partitionIdx64 := partitionIdx.Uint64()

	//mlog.Log.Debugf("using blockhash %s in epoch rewards hasher, and num_partitions %d: hash = %d, partitionIdx = %d", solana.HashFromBytes(blockhash[:]), numPartitions, hash, partitionIdx64)

	return partitionIdx64
}

type PointValue struct {
	Rewards uint64
	Points  wide.Uint128
}

type CalculatedStakeRewards struct {
	StakerRewards      uint64
	VoterRewards       uint64
	NewCreditsObserved uint64
	Commission         uint8            // Commission rate used (0-100)
	VotePubkey         solana.PublicKey // Vote account this stake delegates to
}

func CalculateStakeRewards(pointsPerStakeAcct map[solana.PublicKey]*CalculatedStakePoints, slotCtx *sealevel.SlotCtx, stakeHistory *sealevel.SysvarStakeHistory, slot uint64, rewardedEpoch uint64, pointValue PointValue, newRateActivationEpoch *uint64, f *features.Features) map[solana.PublicKey]*CalculatedStakeRewards {
	stakeInfoResults := make(map[solana.PublicKey]*CalculatedStakeRewards, 1500000)
	minimumStakeDelegation := minimumStakeDelegation(slotCtx)
	var belowMinCount atomic.Uint64
	var noVoteCount atomic.Uint64
	var eligibleCount atomic.Uint64

	var mu sync.Mutex
	var wg sync.WaitGroup

	workerPool, _ := ants.NewPoolWithFunc(runtime.GOMAXPROCS(0)*8, func(i interface{}) {
		defer wg.Done()

		delegation := i.(*delegationAndPubkey)

		if delegation.delegation.StakeLamports < minimumStakeDelegation {
			belowMinCount.Add(1)
			return
		}

		voterPk := delegation.delegation.VoterPubkey
		voteStateVersioned := global.VoteCacheItem(voterPk)
		if voteStateVersioned == nil {
			noVoteCount.Add(1)
			return
		}
		eligibleCount.Add(1)

		pointsForStakeAcct := pointsPerStakeAcct[delegation.pubkey]
		calculatedStakeRewards := CalculateStakeRewardsForAcct(delegation.pubkey, pointsForStakeAcct, delegation.delegation, voteStateVersioned, rewardedEpoch, pointValue, newRateActivationEpoch)
		if calculatedStakeRewards != nil {
			mu.Lock()
			stakeInfoResults[delegation.pubkey] = calculatedStakeRewards
			mu.Unlock()
		}
	})

	stakeCache := global.StakeCache()
	total := uint64(len(stakeCache))
	mlog.Log.Infof("calculating stake rewards: processing %d stake accounts", total)

	var dispatched uint64
	for pk, delegation := range stakeCache {
		dispatched++
		if dispatched%100000 == 0 {
			mlog.Log.Infof("  stake rewards progress: %d/%d (%.1f%%)", dispatched, total, float64(dispatched)*100/float64(total))
		}
		d := &delegationAndPubkey{delegation: delegation, pubkey: pk}
		wg.Add(1)
		workerPool.Invoke(d)
	}
	wg.Wait()
	workerPool.Release()

	mlog.Log.Infof("stake rewards calculation complete: %d accounts with rewards", len(stakeInfoResults))

	mlog.Log.Debugf("rewards: stake filter counts (rewards): slot=%d epoch=%d min_stake=%d total=%d eligible=%d below_min=%d no_vote=%d",
		slot, rewardedEpoch, minimumStakeDelegation, len(global.StakeCache()),
		eligibleCount.Load(), belowMinCount.Load(), noVoteCount.Load())

	return stakeInfoResults
}

// CalculateStakeRewardsAndPartitions calculates stake rewards and then computes partitions
// based on the actual number of accounts that receive rewards (matching Firedancer's approach).
// This is the correct flow: compute partition count AFTER rewards calculation, not before.
func CalculateStakeRewardsAndPartitions(
	pointsPerStakeAcct map[solana.PublicKey]*CalculatedStakePoints,
	slotCtx *sealevel.SlotCtx,
	stakeHistory *sealevel.SysvarStakeHistory,
	slot uint64,
	rewardedEpoch uint64,
	pointValue PointValue,
	newRateActivationEpoch *uint64,
	f *features.Features,
	epochSchedule *sealevel.SysvarEpochSchedule,
	epoch uint64,
) (map[solana.PublicKey]*CalculatedStakeRewards, Partitions, uint64) {
	// First calculate rewards (same as CalculateStakeRewards)
	stakeInfoResults := make(map[solana.PublicKey]*CalculatedStakeRewards, 1500000)
	minimumStakeDelegation := minimumStakeDelegation(slotCtx)
	var belowMinCount atomic.Uint64
	var noVoteCount atomic.Uint64
	var eligibleCount atomic.Uint64

	var mu sync.Mutex
	var wg sync.WaitGroup

	workerPool, _ := ants.NewPoolWithFunc(runtime.GOMAXPROCS(0)*8, func(i interface{}) {
		defer wg.Done()

		delegation := i.(*delegationAndPubkey)

		if delegation.delegation.StakeLamports < minimumStakeDelegation {
			belowMinCount.Add(1)
			return
		}

		voterPk := delegation.delegation.VoterPubkey
		voteStateVersioned := global.VoteCacheItem(voterPk)
		if voteStateVersioned == nil {
			noVoteCount.Add(1)
			return
		}
		eligibleCount.Add(1)

		pointsForStakeAcct := pointsPerStakeAcct[delegation.pubkey]
		calculatedStakeRewards := CalculateStakeRewardsForAcct(delegation.pubkey, pointsForStakeAcct, delegation.delegation, voteStateVersioned, rewardedEpoch, pointValue, newRateActivationEpoch)
		if calculatedStakeRewards != nil {
			mu.Lock()
			stakeInfoResults[delegation.pubkey] = calculatedStakeRewards
			mu.Unlock()
		}
	})

	stakeCache := global.StakeCache()
	total := uint64(len(stakeCache))
	mlog.Log.Infof("calculating stake rewards: processing %d stake accounts", total)

	var dispatched uint64
	for pk, delegation := range stakeCache {
		dispatched++
		if dispatched%100000 == 0 {
			mlog.Log.Infof("  stake rewards progress: %d/%d (%.1f%%)", dispatched, total, float64(dispatched)*100/float64(total))
		}
		d := &delegationAndPubkey{delegation: delegation, pubkey: pk}
		wg.Add(1)
		workerPool.Invoke(d)
	}
	wg.Wait()
	workerPool.Release()

	mlog.Log.Infof("stake rewards calculation complete: %d accounts with rewards", len(stakeInfoResults))

	// Now compute partition count based on actual reward recipients (matching Firedancer)
	numRewardRecipients := uint64(len(stakeInfoResults))
	slotsPerEpoch := epochSchedule.SlotsInEpoch(epoch)
	numPartitions := ComputeNumRewardPartitions(epoch, slotsPerEpoch, numRewardRecipients, epochSchedule.FirstNormalEpoch)

	mlog.Log.Infof("computed partition count from reward recipients: recipients=%d partitions=%d (slots_per_epoch=%d)",
		numRewardRecipients, numPartitions, slotsPerEpoch)

	// Hash reward recipients into partitions
	partitions := NewPartitions(numPartitions)
	var wgPartition sync.WaitGroup

	type assign struct {
		idx uint64
		pk  solana.PublicKey
	}
	assigns := make(chan assign, 1<<20)

	wgPartition.Add(1)
	go func() {
		defer wgPartition.Done()
		for a := range assigns {
			partitions[a.idx].pubkeys = append(partitions[a.idx].pubkeys, a.pk)
		}
	}()

	workerPool2, _ := ants.NewPoolWithFunc(runtime.GOMAXPROCS(0)*8, func(i interface{}) {
		defer wg.Done()
		stakePk := i.(solana.PublicKey)
		// Use slotCtx.Blockhash for partition hashing. Since slotCtx is actually prevSlotCtx
		// (the SlotCtx from the previous slot, passed from handleEpochRewards), slotCtx.Blockhash
		// IS the parent blockhash of the first slot in the new epoch, matching Firedancer/Agave.
		idx := CalculateRewardPartitionForPubkey(stakePk, slotCtx.Blockhash, numPartitions)
		assigns <- assign{idx: idx, pk: stakePk}
	})

	for stakePk := range stakeInfoResults {
		wg.Add(1)
		workerPool2.Invoke(stakePk)
	}

	wg.Wait()
	workerPool2.Release()

	close(assigns)
	wgPartition.Wait()

	mlog.Log.Debugf("rewards: stake filter counts (rewards): slot=%d epoch=%d min_stake=%d total=%d eligible=%d below_min=%d no_vote=%d",
		slot, rewardedEpoch, minimumStakeDelegation, len(global.StakeCache()),
		eligibleCount.Load(), belowMinCount.Load(), noVoteCount.Load())

	return stakeInfoResults, partitions, numPartitions
}

func CalculateStakeRewardsForAcct(pubkey solana.PublicKey, stakePointsResult *CalculatedStakePoints, delegation *sealevel.Delegation, voteState *sealevel.VoteStateVersions, rewardedEpoch uint64, pointValue PointValue, newRateActivationEpoch *uint64) *CalculatedStakeRewards {
	if pointValue.Rewards == 0 || delegation.ActivationEpoch == rewardedEpoch {
		stakePointsResult.ForceCreditsUpdateWithSkippedReward = true
	}

	if stakePointsResult.ForceCreditsUpdateWithSkippedReward {
		result := &CalculatedStakeRewards{
			NewCreditsObserved: stakePointsResult.NewCreditsObserved,
			VotePubkey:         delegation.VoterPubkey,
		}
		return result
	}

	zero128 := wide.Uint128FromUint64(0)
	if stakePointsResult.Points.Eq(zero128) || pointValue.Points.Eq(zero128) {
		//mlog.Log.Debugf("CalculateStakeRewardsForAcct: returning nil for %s. stakePointsResult.Points = %d, pointValue.Points = %d", stakePubkey, stakePointsResult.Points.Uint64(), pointValue.Points.Uint64())
		return nil
	}

	rewards128 := stakePointsResult.Points.Mul(wide.Uint128FromUint64(pointValue.Rewards)).Div(pointValue.Points)
	if !rewards128.IsUint64() {
		//mlog.Log.Debugf("CalculateStakeRewardsForAcct: returning nil for %s. rewards128 not a uint64. %s", stakePubkey, rewards128)
		return nil
	}

	rewards := rewards128.Uint64()
	if rewards == 0 {
		//mlog.Log.Debugf("CalculateStakeRewardsForAcct: returning nil for %s. rewards == 0", stakePubkey)
		return nil
	}

	splitResult := voteCommissionSplit(voteState, rewards)
	if splitResult.IsSplit && (splitResult.VoterPortion == 0 || splitResult.StakerPortion == 0) {
		//mlog.Log.Debugf("CalculateStakeRewardsForAcct: returning nil for %s. IsSplit = %t, splitResult.VoterPortion = %d, splitResult.StakerPortion = %d", stakePubkey, splitResult.VoterPortion, splitResult.StakerPortion)
		return nil
	}

	result := &CalculatedStakeRewards{
		StakerRewards:      splitResult.StakerPortion,
		VoterRewards:       splitResult.VoterPortion,
		NewCreditsObserved: stakePointsResult.NewCreditsObserved,
		Commission:         splitResult.Commission,
		VotePubkey:         delegation.VoterPubkey,
	}

	//mlog.Log.Debugf("returning CalculatedStakeRewards for %s. %+v", stakePubkey, result)

	return result
}

type CommissionSplit struct {
	VoterPortion  uint64
	StakerPortion uint64
	IsSplit       bool
	Commission    uint8 // Commission rate used (0-100)
}

func voteCommissionSplit(voteState *sealevel.VoteStateVersions, rewards uint64) CommissionSplit {
	var commission byte

	switch voteState.Type {
	case sealevel.VoteStateVersionCurrent:
		commission = voteState.Current.Commission
	case sealevel.VoteStateVersionV0_23_5:
		commission = voteState.V0_23_5.Commission
	case sealevel.VoteStateVersionV1_14_11:
		commission = voteState.V1_14_11.Commission
	}

	commissionRate := uint64(min(commission, 100))
	result := CommissionSplit{Commission: uint8(commissionRate)}

	switch commissionRate {
	case 0:
		// no commission, all rewards go to staker
		result.StakerPortion = rewards
	case 100:
		// 100% commission, all rewards go to validator
		result.VoterPortion = rewards
	default:
		// Use mulDivPercent to match Agave's calculation exactly.
		// This splits the division to avoid overflow and computes both portions
		// independently using the same formula.
		result.VoterPortion = mulDivPercent(rewards, commissionRate)
		result.StakerPortion = mulDivPercent(rewards, 100-commissionRate)
		result.IsSplit = true
	}

	return result
}

// mulDivPercent computes (on * pct) / 100 using a split approach to avoid overflow.
// This matches Agave's commission split calculation exactly.
func mulDivPercent(on uint64, pct uint64) uint64 {
	// pct must be 0..100
	q := on / 100
	r := on % 100
	return q*pct + (r*pct)/100
}

type delegationAndPubkey struct {
	delegation *sealevel.Delegation
	pubkey     solana.PublicKey
}

// CalculateTotalPointsAndPartitions computes total stake points and assigns stake accounts to partitions.
// When maxEpoch is non-nil (recalc mode), epoch credits beyond maxEpoch are ignored to freeze
// the view of vote credits at epoch boundary, matching Firedancer's prev_vote_credits behavior.
func CalculateTotalPointsAndPartitions(
	acctsDb *accountsdb.AccountsDb,
	slotCtx *sealevel.SlotCtx,
	slot uint64,
	numPartitions uint64,
	stakeHistory *sealevel.SysvarStakeHistory,
	newWarmupCooldownRateEpoch *uint64,
	maxEpoch *uint64,
) (map[solana.PublicKey]*CalculatedStakePoints, wide.Uint128, Partitions) {
	/*old := debug.SetGCPercent(200)
	defer debug.SetGCPercent(old)*/

	minimum := minimumStakeDelegation(slotCtx)
	var belowMinCount atomic.Uint64
	var noVoteCount atomic.Uint64
	var eligibleCount atomic.Uint64

	n := len(global.StakeCache())
	pks := make([]solana.PublicKey, 0, n)
	for pk := range global.StakeCache() {
		pks = append(pks, pk)
	}

	pointsAccum := NewCalculatedStakePointsAccumulator(pks)
	partitions := NewPartitions(numPartitions)

	type assign struct {
		idx uint64
		pk  solana.PublicKey
	}
	var wgMerge sync.WaitGroup
	assigns := make(chan assign, 1<<20)
	if numPartitions != 0 {
		wgMerge.Add(1)
		go func() {
			defer wgMerge.Done()
			for a := range assigns {
				partitions[a.idx].pubkeys = append(partitions[a.idx].pubkeys, a.pk)
			}
		}()
	}

	var wg sync.WaitGroup

	size := runtime.GOMAXPROCS(0) * 8
	workerPool, _ := ants.NewPoolWithFunc(size, func(i interface{}) {
		defer wg.Done()

		t := i.(*delegationAndPubkey)
		d := t.delegation
		if d.StakeLamports < minimum {
			belowMinCount.Add(1)
			return
		}

		voterPk := d.VoterPubkey
		voteState := global.VoteCacheItem(voterPk)
		if voteState == nil {
			noVoteCount.Add(1)
			return
		}
		eligibleCount.Add(1)

		pcs := calculateStakePointsAndCredits(t.pubkey, stakeHistory, d, voteState, newWarmupCooldownRateEpoch, maxEpoch)
		pointsAccum.Add(t.pubkey, pcs)

		if numPartitions != 0 {
			// Use slotCtx.Blockhash for partition hashing. Since slotCtx is actually prevSlotCtx
			// (the SlotCtx from the previous slot, passed from handleEpochRewards), slotCtx.Blockhash
			// IS the parent blockhash of the first slot in the new epoch, matching Firedancer/Agave.
			idx := CalculateRewardPartitionForPubkey(t.pubkey, slotCtx.Blockhash, numPartitions)
			assigns <- assign{idx: idx, pk: t.pubkey}
		}
	})

	for pk, delegation := range global.StakeCache() {
		wg.Add(1)
		workerPool.Invoke(&delegationAndPubkey{delegation: delegation, pubkey: pk})
	}

	wg.Wait()
	workerPool.Release()

	if numPartitions != 0 {
		close(assigns)
		wgMerge.Wait()
	}

	mlog.Log.Debugf("rewards: stake filter counts (points): slot=%d min_stake=%d total=%d eligible=%d below_min=%d no_vote=%d",
		slot, minimum, n, eligibleCount.Load(), belowMinCount.Load(), noVoteCount.Load())

	return pointsAccum.CalculatedStakePoints(), pointsAccum.TotalPoints(), partitions
}

// CalculateTotalPoints computes total stake points WITHOUT partition assignment.
// This is used in the new flow where partition count is computed AFTER rewards calculation.
// When maxEpoch is non-nil (recalc mode), epoch credits beyond maxEpoch are ignored.
func CalculateTotalPoints(
	slotCtx *sealevel.SlotCtx,
	slot uint64,
	stakeHistory *sealevel.SysvarStakeHistory,
	newWarmupCooldownRateEpoch *uint64,
	maxEpoch *uint64,
) (map[solana.PublicKey]*CalculatedStakePoints, wide.Uint128) {
	minimum := minimumStakeDelegation(slotCtx)
	var belowMinCount atomic.Uint64
	var noVoteCount atomic.Uint64
	var eligibleCount atomic.Uint64

	n := len(global.StakeCache())
	pks := make([]solana.PublicKey, 0, n)
	for pk := range global.StakeCache() {
		pks = append(pks, pk)
	}

	pointsAccum := NewCalculatedStakePointsAccumulator(pks)

	var wg sync.WaitGroup

	size := runtime.GOMAXPROCS(0) * 8
	workerPool, _ := ants.NewPoolWithFunc(size, func(i interface{}) {
		defer wg.Done()

		t := i.(*delegationAndPubkey)
		d := t.delegation
		if d.StakeLamports < minimum {
			belowMinCount.Add(1)
			return
		}

		voterPk := d.VoterPubkey
		voteState := global.VoteCacheItem(voterPk)
		if voteState == nil {
			noVoteCount.Add(1)
			return
		}
		eligibleCount.Add(1)

		pcs := calculateStakePointsAndCredits(t.pubkey, stakeHistory, d, voteState, newWarmupCooldownRateEpoch, maxEpoch)
		pointsAccum.Add(t.pubkey, pcs)
	})

	for pk, delegation := range global.StakeCache() {
		wg.Add(1)
		workerPool.Invoke(&delegationAndPubkey{delegation: delegation, pubkey: pk})
	}

	wg.Wait()
	workerPool.Release()

	mlog.Log.Debugf("rewards: stake filter counts (points): slot=%d min_stake=%d total=%d eligible=%d below_min=%d no_vote=%d",
		slot, minimum, n, eligibleCount.Load(), belowMinCount.Load(), noVoteCount.Load())

	return pointsAccum.CalculatedStakePoints(), pointsAccum.TotalPoints()
}

// calculateStakePointsAndCredits computes stake points for a single stake account.
// When maxEpoch is non-nil, epoch credits entries beyond maxEpoch are ignored.
// This is used in recalc mode to freeze the view of vote credits at epoch boundary,
// matching Firedancer's prev_vote_credits behavior.
func calculateStakePointsAndCredits(
	pubkey solana.PublicKey,
	stakeHistory *sealevel.SysvarStakeHistory,
	delegation *sealevel.Delegation,
	voteState *sealevel.VoteStateVersions,
	newRateActivationEpoch *uint64,
	maxEpoch *uint64,
) CalculatedStakePoints {
	creditsInStake := delegation.CreditsObserved

	var epochCredits []sealevel.EpochCredits
	switch voteState.Type {
	case sealevel.VoteStateVersionCurrent:
		epochCredits = voteState.Current.EpochCredits
	case sealevel.VoteStateVersionV0_23_5:
		epochCredits = voteState.V0_23_5.EpochCredits
	case sealevel.VoteStateVersionV1_14_11:
		epochCredits = voteState.V1_14_11.EpochCredits
	default:
		panic("invalid vote state - should be impossible")
	}

	// When maxEpoch is set (recalc mode), filter out epoch credits beyond maxEpoch.
	// This ensures we use credits as they were at epoch boundary, not snapshot time.
	if maxEpoch != nil {
		filtered := make([]sealevel.EpochCredits, 0, len(epochCredits))
		for _, ec := range epochCredits {
			if ec.Epoch <= *maxEpoch {
				filtered = append(filtered, ec)
			}
		}
		epochCredits = filtered
	}

	var creditsInVote uint64
	if len(epochCredits) != 0 {
		creditsInVote = epochCredits[len(epochCredits)-1].Credits
	}

	if creditsInVote < creditsInStake {
		return CalculatedStakePoints{
			NewCreditsObserved:                  creditsInVote,
			ForceCreditsUpdateWithSkippedReward: true,
		}
	}

	if creditsInVote == creditsInStake || len(epochCredits) == 0 {
		return CalculatedStakePoints{NewCreditsObserved: creditsInVote}
	}

	/*start := sort.Search(len(epochCredits), func(i int) bool {
		return epochCredits[i].Credits > creditsInStake
	})
	if start >= len(epochCredits) {
		return CalculatedStakePoints{NewCreditsObserved: creditsInVote}
	}*/

	var points wide.Uint128
	newObserved := creditsInStake

	for _, ec := range epochCredits {
		final := ec.Credits
		initial := ec.PrevCredits

		var earnedCredits uint64
		if creditsInStake < initial {
			earnedCredits = final - initial
		} else if creditsInStake < final {
			earnedCredits = final - newObserved
		}

		if earnedCredits != 0 {
			stakeAmt := delegation.StakeActivatingAndDeactivating(ec.Epoch, stakeHistory, newRateActivationEpoch).Effective
			earnedPoints := wide.Uint128FromUint64(stakeAmt).Mul(wide.Uint128FromUint64(earnedCredits))
			points = points.Add(earnedPoints)

		}

		newObserved = max(newObserved, final)
	}

	return CalculatedStakePoints{
		Points:             points,
		NewCreditsObserved: newObserved,
	}
}

// CombinedRefreshPointsAndPartitions performs stake cache refresh, points calculation, and partition
// assignment in a single pass, optimizing I/O by only reading from AccountsDB for accounts that
// pass initial eligibility checks (stake >= minimum AND has vote account in vote cache).
//
// This combines:
// - RefreshStakeCacheCreditsObserved (updates credits_observed, removes invalid entries)
// - CalculateTotalPointsAndPartitions (calculates points, assigns to partitions)
// - Snapshot capture (for use during distribution)
//
// The optimization is significant:
// - Old flow: Read ALL ~1M stake accounts from AccountsDB (even ineligible ones)
// - New flow: Only read ~500K accounts that pass initial eligibility checks
//
// Returns:
// - pointsPerStake: Points for each eligible stake account
// - totalPoints: Sum of all points
// - partitions: Partition assignment for each eligible stake account
// - snapshots: Captured stake account data for use during distribution
// - eligibleCount: Number of eligible stake accounts (used for partition count computation)
// - statsOut: Breakdown of why accounts were filtered out (for diagnostics)
type CombinedStats struct {
	TotalAccounts    int
	BelowMinimum     int
	NoVoteInCache    int
	NotFoundInDB     int
	Tombstones       int
	UnmarshalErrors  int
	NotStakeState    int
	ZeroPoints       int
	Eligible         int
	CreditsUpdated   int
}

func CombinedRefreshPointsAndPartitions(
	acctsDb *accountsdb.AccountsDb,
	slotCtx *sealevel.SlotCtx,
	slot uint64,
	numPartitions uint64,
	stakeHistory *sealevel.SysvarStakeHistory,
	newWarmupCooldownRateEpoch *uint64,
	maxEpoch *uint64,
) (map[solana.PublicKey]*CalculatedStakePoints, wide.Uint128, Partitions, map[solana.PublicKey]*accounts.Account, uint64, CombinedStats) {
	minimum := minimumStakeDelegation(slotCtx)
	stakeCache := global.StakeCache()
	total := len(stakeCache)

	mlog.Log.Infof("combined refresh+points+partitions: slot=%d accounts=%d min_stake=%d", slot, total, minimum)

	// PHASE 1: Snapshot stake cache into slice (avoid concurrent map iteration issues)
	type combinedTask struct {
		idx        int
		pubkey     solana.PublicKey
		delegation *sealevel.Delegation
	}
	tasks := make([]combinedTask, 0, total)
	for pk, delegation := range stakeCache {
		tasks = append(tasks, combinedTask{
			idx:        len(tasks),
			pubkey:     pk,
			delegation: delegation,
		})
	}

	// Result struct for each task
	type combinedResult struct {
		pubkey         solana.PublicKey
		delegation     *sealevel.Delegation
		points         *CalculatedStakePoints // nil if not eligible for points
		snapshot       *accounts.Account      // nil if not captured
		shouldDelete   bool                   // should be removed from stake cache
		filterReason   string                 // why filtered (for stats)
		creditsUpdated bool
		dbCredits      uint64 // credits_observed from AccountsDB (authoritative for cache update)
		partitionIdx   uint64
		processed      bool // true if worker processed this task (false if Invoke failed)
	}
	results := make([]combinedResult, len(tasks))

	// Track failed invocations for sequential retry
	var invokeFailures []int
	var invokeFailuresMu sync.Mutex

	// Atomic counters
	var belowMinCount, noVoteCount, notFoundCount, tombstoneCount atomic.Uint64
	var unmarshalErrCount, notStakeCount, zeroPointsCount, eligibleCount, creditsUpdatedCount atomic.Uint64
	var processed atomic.Uint64

	var wg sync.WaitGroup
	poolSize := runtime.GOMAXPROCS(0) * 8
	workerPool, poolErr := ants.NewPoolWithFunc(poolSize, func(i interface{}) {
		defer wg.Done()
		task := i.(*combinedTask)
		result := &results[task.idx]
		result.pubkey = task.pubkey
		result.delegation = task.delegation

		// Progress logging
		p := processed.Add(1)
		if p%100000 == 0 {
			mlog.Log.Infof("  combined progress: %d/%d (%.1f%%)", p, total, float64(p)*100/float64(total))
		}

		// Check #1: Minimum stake (from cache, no I/O)
		if task.delegation.StakeLamports < minimum {
			belowMinCount.Add(1)
			result.filterReason = "below_minimum"
			result.processed = true
			return
		}

		// Check #2: Vote account in cache (from cache, no I/O)
		voteState := global.VoteCacheItem(task.delegation.VoterPubkey)
		if voteState == nil {
			noVoteCount.Add(1)
			result.filterReason = "no_vote_in_cache"
			result.processed = true
			return
		}

		// At this point, account is POTENTIALLY eligible. Now read from AccountsDB.
		stakeAcct, err := acctsDb.GetAccount(slot, task.pubkey)
		if err != nil {
			notFoundCount.Add(1)
			result.shouldDelete = true
			result.filterReason = "not_found"
			result.processed = true
			return
		}

		// Check for tombstone
		if stakeAcct.Lamports == 0 && len(stakeAcct.Data) == 0 {
			tombstoneCount.Add(1)
			result.shouldDelete = true
			result.filterReason = "tombstone"
			result.processed = true
			return
		}

		// Decode stake state
		stakeState, err := sealevel.UnmarshalStakeState(stakeAcct.Data)
		if err != nil {
			unmarshalErrCount.Add(1)
			result.shouldDelete = true
			result.filterReason = "unmarshal_error"
			result.processed = true
			return
		}

		// Must be in Stake state
		if stakeState.Status != sealevel.StakeStateV2StatusStake {
			notStakeCount.Add(1)
			result.shouldDelete = true
			result.filterReason = "not_stake_state"
			result.processed = true
			return
		}

		// Get credits_observed from AccountsDB (authoritative source for cache refresh)
		dbCredits := stakeState.Stake.Stake.CreditsObserved
		result.dbCredits = dbCredits
		if task.delegation.CreditsObserved != dbCredits {
			result.creditsUpdated = true
			creditsUpdatedCount.Add(1)
		}

		// Calculate points using dbCredits (authoritative), not the potentially stale cache value.
		// Create a local copy with the correct credits_observed from AccountsDB.
		delegationWithDbCredits := *task.delegation
		delegationWithDbCredits.CreditsObserved = dbCredits
		pts := calculateStakePointsAndCredits(task.pubkey, stakeHistory, &delegationWithDbCredits, voteState, newWarmupCooldownRateEpoch, maxEpoch)

		// Check if points are actually > 0 (required for eligibility)
		// Also count ForceCreditsUpdateWithSkippedReward accounts as eligible
		if pts.Points.Eq(wide.Uint128FromUint64(0)) && !pts.ForceCreditsUpdateWithSkippedReward {
			zeroPointsCount.Add(1)
			result.filterReason = "zero_points"
			// Still capture snapshot for potential credits update during distribution
			result.snapshot = stakeAcct.Clone()
			result.points = &pts
			result.processed = true
			return
		}

		eligibleCount.Add(1)
		result.points = &pts
		result.snapshot = stakeAcct.Clone()
		result.processed = true

		// Calculate partition assignment for accounts with points > 0 OR ForceCreditsUpdateWithSkippedReward
		// (ForceCreditsUpdateWithSkippedReward accounts need to be in partitions so credits can be updated)
		if numPartitions != 0 {
			result.partitionIdx = CalculateRewardPartitionForPubkey(task.pubkey, slotCtx.Blockhash, numPartitions)
		}
	})

	if poolErr != nil {
		mlog.Log.Warnf("combined: failed to create worker pool, using sequential fallback: %v", poolErr)
		// Fall back to fully sequential processing
		return combinedRefreshPointsSequentialFull(acctsDb, slotCtx, slot, numPartitions, stakeHistory, newWarmupCooldownRateEpoch, maxEpoch, minimum)
	}
	defer workerPool.Release()

	// Submit all tasks
	for i := range tasks {
		wg.Add(1)
		if err := workerPool.Invoke(&tasks[i]); err != nil {
			wg.Done()
			// Track failed invocations for sequential retry
			invokeFailuresMu.Lock()
			invokeFailures = append(invokeFailures, i)
			invokeFailuresMu.Unlock()
			mlog.Log.Warnf("combined: invoke failed for %s: %v", tasks[i].pubkey.String(), err)
		}
	}
	wg.Wait()

	// Process any failed invocations sequentially
	if len(invokeFailures) > 0 {
		mlog.Log.Infof("combined: processing %d failed invocations sequentially", len(invokeFailures))
		for _, idx := range invokeFailures {
			task := &tasks[idx]
			result := &results[idx]
			result.pubkey = task.pubkey
			result.delegation = task.delegation

			// Same logic as worker, but sequential
			if task.delegation.StakeLamports < minimum {
				belowMinCount.Add(1)
				result.filterReason = "below_minimum"
				result.processed = true
				continue
			}

			voteState := global.VoteCacheItem(task.delegation.VoterPubkey)
			if voteState == nil {
				noVoteCount.Add(1)
				result.filterReason = "no_vote_in_cache"
				result.processed = true
				continue
			}

			stakeAcct, err := acctsDb.GetAccount(slot, task.pubkey)
			if err != nil {
				notFoundCount.Add(1)
				result.shouldDelete = true
				result.filterReason = "not_found"
				result.processed = true
				continue
			}

			if stakeAcct.Lamports == 0 && len(stakeAcct.Data) == 0 {
				tombstoneCount.Add(1)
				result.shouldDelete = true
				result.filterReason = "tombstone"
				result.processed = true
				continue
			}

			stakeState, err := sealevel.UnmarshalStakeState(stakeAcct.Data)
			if err != nil {
				unmarshalErrCount.Add(1)
				result.shouldDelete = true
				result.filterReason = "unmarshal_error"
				result.processed = true
				continue
			}

			if stakeState.Status != sealevel.StakeStateV2StatusStake {
				notStakeCount.Add(1)
				result.shouldDelete = true
				result.filterReason = "not_stake_state"
				result.processed = true
				continue
			}

			dbCredits := stakeState.Stake.Stake.CreditsObserved
			result.dbCredits = dbCredits
			if task.delegation.CreditsObserved != dbCredits {
				result.creditsUpdated = true
				creditsUpdatedCount.Add(1)
			}

			// Use dbCredits for points calculation (authoritative, not stale cache)
			delegationWithDbCredits := *task.delegation
			delegationWithDbCredits.CreditsObserved = dbCredits
			pts := calculateStakePointsAndCredits(task.pubkey, stakeHistory, &delegationWithDbCredits, voteState, newWarmupCooldownRateEpoch, maxEpoch)

			if pts.Points.Eq(wide.Uint128FromUint64(0)) && !pts.ForceCreditsUpdateWithSkippedReward {
				zeroPointsCount.Add(1)
				result.filterReason = "zero_points"
				result.snapshot = stakeAcct.Clone()
				result.points = &pts
				result.processed = true
				continue
			}

			eligibleCount.Add(1)
			result.points = &pts
			result.snapshot = stakeAcct.Clone()
			result.processed = true

			if numPartitions != 0 {
				result.partitionIdx = CalculateRewardPartitionForPubkey(task.pubkey, slotCtx.Blockhash, numPartitions)
			}
		}
	}

	// PHASE 3: Collect results and apply mutations (single-threaded)
	pointsMap := make(map[solana.PublicKey]*CalculatedStakePoints, eligibleCount.Load())
	snapshots := make(map[solana.PublicKey]*accounts.Account, eligibleCount.Load())
	partitions := NewPartitions(numPartitions)
	var toDelete []solana.PublicKey
	var totalPoints wide.Uint128

	for i := range results {
		result := &results[i]

		if result.shouldDelete {
			toDelete = append(toDelete, result.pubkey)
			continue
		}

		if result.points != nil {
			pointsMap[result.pubkey] = result.points
			totalPoints = totalPoints.Add(result.points.Points)
		}

		if result.snapshot != nil {
			snapshots[result.pubkey] = result.snapshot
		}

		// Update credits in stake cache with dbCredits (authoritative from AccountsDB)
		// This is the refresh step - use dbCredits, not NewCreditsObserved which is for reward application
		if result.creditsUpdated {
			result.delegation.CreditsObserved = result.dbCredits
		}

		// Add to partition if eligible (has points > 0 OR needs credits update via ForceCreditsUpdateWithSkippedReward)
		if result.points != nil && numPartitions != 0 {
			hasPoints := !result.points.Points.Eq(wide.Uint128FromUint64(0))
			needsCreditsUpdate := result.points.ForceCreditsUpdateWithSkippedReward
			if hasPoints || needsCreditsUpdate {
				partitions[result.partitionIdx].pubkeys = append(partitions[result.partitionIdx].pubkeys, result.pubkey)
			}
		}
	}

	// Apply deletions
	for _, pk := range toDelete {
		global.DeleteStakeCacheItem(pk)
	}

	stats := CombinedStats{
		TotalAccounts:   total,
		BelowMinimum:    int(belowMinCount.Load()),
		NoVoteInCache:   int(noVoteCount.Load()),
		NotFoundInDB:    int(notFoundCount.Load()),
		Tombstones:      int(tombstoneCount.Load()),
		UnmarshalErrors: int(unmarshalErrCount.Load()),
		NotStakeState:   int(notStakeCount.Load()),
		ZeroPoints:      int(zeroPointsCount.Load()),
		Eligible:        int(eligibleCount.Load()),
		CreditsUpdated:  int(creditsUpdatedCount.Load()),
	}

	mlog.Log.Infof("combined complete: total=%d eligible=%d snapshots=%d partitions=%d deleted=%d",
		total, stats.Eligible, len(snapshots), numPartitions, len(toDelete))
	mlog.Log.Infof("  filter breakdown: below_min=%d no_vote=%d not_found=%d tombstone=%d unmarshal=%d not_stake=%d zero_pts=%d",
		stats.BelowMinimum, stats.NoVoteInCache, stats.NotFoundInDB, stats.Tombstones, stats.UnmarshalErrors, stats.NotStakeState, stats.ZeroPoints)
	mlog.Log.Infof("  credits updated: %d (%.1f%%)", stats.CreditsUpdated, float64(stats.CreditsUpdated)*100/float64(total))

	return pointsMap, totalPoints, partitions, snapshots, uint64(stats.Eligible), stats
}

// combinedRefreshPointsSequentialFull is the fallback when worker pool creation fails.
// It processes the stake cache directly (not a pre-built tasks slice).
func combinedRefreshPointsSequentialFull(
	acctsDb *accountsdb.AccountsDb,
	slotCtx *sealevel.SlotCtx,
	slot uint64,
	numPartitions uint64,
	stakeHistory *sealevel.SysvarStakeHistory,
	newWarmupCooldownRateEpoch *uint64,
	maxEpoch *uint64,
	minimum uint64,
) (map[solana.PublicKey]*CalculatedStakePoints, wide.Uint128, Partitions, map[solana.PublicKey]*accounts.Account, uint64, CombinedStats) {
	stakeCache := global.StakeCache()
	total := len(stakeCache)

	pointsMap := make(map[solana.PublicKey]*CalculatedStakePoints, total)
	snapshots := make(map[solana.PublicKey]*accounts.Account, total)
	partitions := NewPartitions(numPartitions)
	var totalPoints wide.Uint128
	var stats CombinedStats
	stats.TotalAccounts = total

	var toDelete []solana.PublicKey
	i := 0
	for pubkey, delegation := range stakeCache {
		i++
		if i%100000 == 0 {
			mlog.Log.Infof("  combined (seq) progress: %d/%d", i, total)
		}

		// Check minimum stake
		if delegation.StakeLamports < minimum {
			stats.BelowMinimum++
			continue
		}

		// Check vote cache
		voteState := global.VoteCacheItem(delegation.VoterPubkey)
		if voteState == nil {
			stats.NoVoteInCache++
			continue
		}

		// Read from AccountsDB
		stakeAcct, err := acctsDb.GetAccount(slot, pubkey)
		if err != nil {
			toDelete = append(toDelete, pubkey)
			stats.NotFoundInDB++
			continue
		}

		if stakeAcct.Lamports == 0 && len(stakeAcct.Data) == 0 {
			toDelete = append(toDelete, pubkey)
			stats.Tombstones++
			continue
		}

		stakeState, err := sealevel.UnmarshalStakeState(stakeAcct.Data)
		if err != nil {
			toDelete = append(toDelete, pubkey)
			stats.UnmarshalErrors++
			continue
		}

		if stakeState.Status != sealevel.StakeStateV2StatusStake {
			toDelete = append(toDelete, pubkey)
			stats.NotStakeState++
			continue
		}

		// Update credits if needed
		dbCredits := stakeState.Stake.Stake.CreditsObserved
		if delegation.CreditsObserved != dbCredits {
			delegation.CreditsObserved = dbCredits
			stats.CreditsUpdated++
		}

		// Calculate points
		pts := calculateStakePointsAndCredits(pubkey, stakeHistory, delegation, voteState, newWarmupCooldownRateEpoch, maxEpoch)

		if pts.Points.Eq(wide.Uint128FromUint64(0)) && !pts.ForceCreditsUpdateWithSkippedReward {
			stats.ZeroPoints++
			snapshots[pubkey] = stakeAcct.Clone()
			pointsMap[pubkey] = &pts
			continue
		}

		stats.Eligible++
		pointsMap[pubkey] = &pts
		snapshots[pubkey] = stakeAcct.Clone()
		totalPoints = totalPoints.Add(pts.Points)

		// Add to partition if has points > 0 OR needs credits update
		if numPartitions != 0 {
			hasPoints := !pts.Points.Eq(wide.Uint128FromUint64(0))
			needsCreditsUpdate := pts.ForceCreditsUpdateWithSkippedReward
			if hasPoints || needsCreditsUpdate {
				idx := CalculateRewardPartitionForPubkey(pubkey, slotCtx.Blockhash, numPartitions)
				partitions[idx].pubkeys = append(partitions[idx].pubkeys, pubkey)
			}
		}
	}

	// Apply deletions
	for _, pk := range toDelete {
		global.DeleteStakeCacheItem(pk)
	}

	mlog.Log.Infof("combined (seq) complete: eligible=%d deleted=%d", stats.Eligible, len(toDelete))
	return pointsMap, totalPoints, partitions, snapshots, uint64(stats.Eligible), stats
}
