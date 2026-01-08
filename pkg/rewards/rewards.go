package rewards

import (
	"fmt"
	"math"
	"runtime"
	"sort"
	"strings"
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
	// When enabled, panics if local partition count differs from RPC, preventing corrupted state.
	// Default: true (for debugging). Set to false for production if confident in local computation.
	validatePartitionCount = true
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

// fetchRpcPartitionCountWithBackups fetches numRewardPartitions from RPC with failover.
// Used for pre-commit validation at epoch boundary.
func fetchRpcPartitionCountWithBackups(rpcc *rpcclient.RpcClient, backups []string, firstSlotInEpoch uint64) (uint64, error) {
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
// RefreshStakeCacheCreditsObserved refreshes credits_observed from AccountsDB and returns
// a snapshot of all valid stake accounts for use during distribution.
// The returned map contains clones of stake accounts that can be used instead of re-reading
// from AccountsDB, which is critical because GetAccount ignores the slot parameter.
func RefreshStakeCacheCreditsObserved(acctsDb *accountsdb.AccountsDb, slot uint64) (refreshed int, errors int, snapshots map[solana.PublicKey]*accounts.Account) {
	stakeCache := global.StakeCache()
	total := len(stakeCache)
	mlog.Log.Infof("refreshing stake cache credits_observed from AccountsDB (slot=%d): %d accounts", slot, total)

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

	var refreshedCount, errorCount int
	var tombstoneCount, notFoundCount, unmarshalErrCount, notStakeCount int
	var processed int

	// Pre-allocate the snapshots map
	snapshots = make(map[solana.PublicKey]*accounts.Account, total)

	for pubkey, delegation := range stakeCache {
		processed++
		if processed%100000 == 0 {
			mlog.Log.Infof("  refresh progress: %d/%d (%.1f%%)", processed, total, float64(processed)*100/float64(total))
		}

		// Read the actual stake account from AccountsDB
		// NOTE: GetAccount currently ignores the slot parameter and returns current state.
		// This means if an account was valid at boundarySlot but closed afterward,
		// we'll incorrectly see it as closed/tombstone here.
		stakeAcct, err := acctsDb.GetAccount(slot, pubkey)
		if err != nil {
			// Account not found in AccountsDB - remove from cache
			mlog.Log.Debugf("STAKE_CACHE_REFRESH_REMOVE: slot=%d pubkey=%s reason=not_found err=%v",
				slot, pubkey.String(), err)
			global.DeleteStakeCacheItem(pubkey)
			notFoundCount++
			errorCount++
			continue
		}

		// Check for tombstone: account exists but has 0 lamports and empty/minimal data
		// This indicates the account was closed (withdrawn to 0)
		if stakeAcct.Lamports == 0 && len(stakeAcct.Data) == 0 {
			mlog.Log.Debugf("STAKE_CACHE_REFRESH_REMOVE: slot=%d pubkey=%s reason=tombstone lamports=0 data_len=0",
				slot, pubkey.String())
			global.DeleteStakeCacheItem(pubkey)
			tombstoneCount++
			errorCount++
			continue
		}

		// Decode the stake state
		stakeState, err := sealevel.UnmarshalStakeState(stakeAcct.Data)
		if err != nil {
			// Unmarshal failed - could be corrupted data or wrong format
			// Remove from cache to avoid issues during rewards
			mlog.Log.Debugf("STAKE_CACHE_REFRESH_REMOVE: slot=%d pubkey=%s reason=unmarshal_error lamports=%d data_len=%d err=%v",
				slot, pubkey.String(), stakeAcct.Lamports, len(stakeAcct.Data), err)
			global.DeleteStakeCacheItem(pubkey)
			unmarshalErrCount++
			errorCount++
			continue
		}

		// Only update if it's a Stake state (not Initialized or Uninitialized)
		if stakeState.Status != sealevel.StakeStateV2StatusStake {
			// Remove non-stake accounts from cache
			mlog.Log.Debugf("STAKE_CACHE_REFRESH_REMOVE: slot=%d pubkey=%s reason=not_stake_state status=%d lamports=%d",
				slot, pubkey.String(), stakeState.Status, stakeAcct.Lamports)
			global.DeleteStakeCacheItem(pubkey)
			notStakeCount++
			errorCount++
			continue
		}

		// Update CreditsObserved from the actual stake account
		oldCredits := delegation.CreditsObserved
		newCredits := stakeState.Stake.Stake.CreditsObserved

		if oldCredits != newCredits {
			delegation.CreditsObserved = newCredits
			refreshedCount++
		}

		// Store a clone of the stake account for use during distribution
		// This ensures we use the exact same state that was used for rewards calculation,
		// avoiding issues with GetAccount returning different state later
		snapshots[pubkey] = stakeAcct.Clone()
	}

	mlog.Log.Infof("stake cache refresh complete: %d updated, %d errors/removed, %d snapshots captured", refreshedCount, errorCount, len(snapshots))
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

// minimumStakeDelegationFromFeatures returns the minimum stake delegation based on features.
// This is a feature-only version that doesn't require SlotCtx.
func minimumStakeDelegationFromFeatures(f *features.Features) uint64 {
	if !f.IsActive(features.StakeMinimumDelegationForRewards) {
		return 0
	}

	if f.IsActive(features.StakeRaiseMinimumDelegationTo1Sol) {
		return 1000000000
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
		rpcNumPartitions, err := fetchRpcPartitionCountWithBackups(validationRpcClient, validationRpcBackups, firstSlotInEpoch)
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
	ants.Release()

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

// AggregateVoteRewardsFromStakingRewards aggregates the VoterRewards portion from stake rewards
// by vote account. This sums the voter (commission) portion from all stake accounts delegated
// to each vote account.
func AggregateVoteRewardsFromStakingRewards(stakingRewards map[solana.PublicKey]*CalculatedStakeRewards) map[solana.PublicKey]uint64 {
	voteRewards := make(map[solana.PublicKey]uint64)
	stakeCache := global.StakeCache()

	for stakePubkey, reward := range stakingRewards {
		if reward == nil || reward.VoterRewards == 0 {
			continue
		}

		// Get the vote account this stake is delegated to
		delegation := stakeCache[stakePubkey]
		if delegation == nil {
			mlog.Log.Debugf("AggregateVoteRewards: stake %s not in cache, skipping voter reward %d",
				stakePubkey, reward.VoterRewards)
			continue
		}

		voteRewards[delegation.VoterPubkey] += reward.VoterRewards
	}

	return voteRewards
}

// CompareVoteRewardsWithRPC compares locally computed vote rewards against RPC block rewards.
// Logs detailed breakdown including:
// - Total comparison
// - Per-account differences
// - Top/bottom vote accounts by reward
// - Accounts only in local or only in RPC
// - Per-vote-pubkey commission summary (only on mismatch)
//
// If stakingRewards is provided and there's a mismatch, logs per-vote-pubkey summary
// with commission, voter_rewards_sum, staker_rewards_sum, and stake_accounts_count.
func CompareVoteRewardsWithRPC(localVoteRewards map[solana.PublicKey]uint64, rpcRewards []rpc.BlockReward, stakingRewards map[solana.PublicKey]*CalculatedStakeRewards) {
	// Build RPC vote rewards map and staking rewards total
	rpcVoteRewards := make(map[solana.PublicKey]uint64)
	var rpcVoteTotal uint64
	var rpcVoteCount int
	var rpcStakingTotal uint64
	var rpcStakingCount int
	for _, reward := range rpcRewards {
		if reward.Lamports > 0 {
			if string(reward.RewardType) == RewardTypeVoting {
				rpcVoteRewards[reward.Pubkey] = uint64(reward.Lamports)
				rpcVoteTotal += uint64(reward.Lamports)
				rpcVoteCount++
			} else if string(reward.RewardType) == RewardTypeStaking {
				rpcStakingTotal += uint64(reward.Lamports)
				rpcStakingCount++
			}
		}
	}

	// Calculate local vote total
	var localVoteTotal uint64
	for _, reward := range localVoteRewards {
		localVoteTotal += reward
	}

	// Calculate local staking totals (voter portion + staker portion from stakingRewards)
	var localStakerTotal uint64
	var localVoterFromStaking uint64
	if stakingRewards != nil {
		for _, sr := range stakingRewards {
			if sr != nil {
				localStakerTotal += sr.StakerRewards
				localVoterFromStaking += sr.VoterRewards
			}
		}
	}

	// Header with both Vote and Staking totals
	mlog.Log.Infof("================================================================================")
	mlog.Log.Infof("EPOCH REWARDS TOTALS COMPARISON: [LOCAL] vs [RPC]")
	mlog.Log.Infof("================================================================================")
	mlog.Log.Infof("")
	mlog.Log.Infof("VOTE REWARDS (commission portion to validators):")
	mlog.Log.Infof("  [LOCAL] Vote Total: %d lamports across %d vote accounts", localVoteTotal, len(localVoteRewards))
	mlog.Log.Infof("  [RPC]   Vote Total: %d lamports across %d vote accounts", rpcVoteTotal, rpcVoteCount)
	mlog.Log.Infof("  DIFF (LOCAL - RPC): %+d lamports", int64(localVoteTotal)-int64(rpcVoteTotal))
	mlog.Log.Infof("")
	mlog.Log.Infof("STAKING REWARDS (delegator portion to stake accounts):")
	mlog.Log.Infof("  [LOCAL] Staking Total: %d lamports across %d stake accounts", localStakerTotal, len(stakingRewards))
	mlog.Log.Infof("  [RPC]   Staking Total: %d lamports across %d stake accounts", rpcStakingTotal, rpcStakingCount)
	mlog.Log.Infof("  DIFF (LOCAL - RPC): %+d lamports", int64(localStakerTotal)-int64(rpcStakingTotal))
	mlog.Log.Infof("")
	mlog.Log.Infof("COMBINED TOTALS (vote + staking):")
	localCombined := localVoteTotal + localStakerTotal
	rpcCombined := rpcVoteTotal + rpcStakingTotal
	mlog.Log.Infof("  [LOCAL] Combined: %d lamports", localCombined)
	mlog.Log.Infof("  [RPC]   Combined: %d lamports", rpcCombined)
	mlog.Log.Infof("  DIFF (LOCAL - RPC): %+d lamports", int64(localCombined)-int64(rpcCombined))
	mlog.Log.Infof("")
	mlog.Log.Infof("SANITY CHECK (local voter from staking vs local vote rewards):")
	mlog.Log.Infof("  Voter rewards from stakingRewards map: %d lamports", localVoterFromStaking)
	mlog.Log.Infof("  Voter rewards from localVoteRewards map: %d lamports", localVoteTotal)
	if localVoterFromStaking != localVoteTotal {
		mlog.Log.Warnf("  MISMATCH: These should be equal! Diff=%+d", int64(localVoterFromStaking)-int64(localVoteTotal))
	} else {
		mlog.Log.Infof("  OK: Values match")
	}
	mlog.Log.Infof("")

	// Find accounts only in local
	var localOnlyAccounts []solana.PublicKey
	var localOnlyTotal uint64
	for pk := range localVoteRewards {
		if _, exists := rpcVoteRewards[pk]; !exists {
			localOnlyAccounts = append(localOnlyAccounts, pk)
			localOnlyTotal += localVoteRewards[pk]
		}
	}

	// Find accounts only in RPC
	var rpcOnlyAccounts []solana.PublicKey
	var rpcOnlyTotal uint64
	for pk := range rpcVoteRewards {
		if _, exists := localVoteRewards[pk]; !exists {
			rpcOnlyAccounts = append(rpcOnlyAccounts, pk)
			rpcOnlyTotal += rpcVoteRewards[pk]
		}
	}

	// Find accounts in both with different values
	type mismatchEntry struct {
		pubkey    solana.PublicKey
		local     uint64
		rpc       uint64
		diff      int64
	}
	var mismatches []mismatchEntry
	var mismatchTotal int64
	for pk, localReward := range localVoteRewards {
		if rpcReward, exists := rpcVoteRewards[pk]; exists && localReward != rpcReward {
			diff := int64(localReward) - int64(rpcReward)
			mismatches = append(mismatches, mismatchEntry{pk, localReward, rpcReward, diff})
			mismatchTotal += diff
		}
	}

	// Log local-only accounts
	if len(localOnlyAccounts) > 0 {
		mlog.Log.Infof("[LOCAL-ONLY] Vote accounts in LOCAL but NOT in RPC (%d accounts, %d lamports):", len(localOnlyAccounts), localOnlyTotal)
		for i, pk := range localOnlyAccounts {
			if i >= 10 {
				mlog.Log.Infof("  ... and %d more", len(localOnlyAccounts)-10)
				break
			}
			mlog.Log.Infof("  %s: [LOCAL]=%d lamports", pk.String(), localVoteRewards[pk])
		}
		mlog.Log.Infof("")
	}

	// Log RPC-only accounts
	if len(rpcOnlyAccounts) > 0 {
		mlog.Log.Infof("[RPC-ONLY] Vote accounts in RPC but NOT in LOCAL (%d accounts, %d lamports):", len(rpcOnlyAccounts), rpcOnlyTotal)
		for i, pk := range rpcOnlyAccounts {
			if i >= 10 {
				mlog.Log.Infof("  ... and %d more", len(rpcOnlyAccounts)-10)
				break
			}
			mlog.Log.Infof("  %s: [RPC]=%d lamports", pk.String(), rpcVoteRewards[pk])
		}
		mlog.Log.Infof("")
	}

	// Log mismatched amounts (accounts in both but with different values)
	if len(mismatches) > 0 {
		mlog.Log.Infof("MISMATCHED: Accounts in BOTH but with different amounts (%d accounts, net diff=%d lamports):", len(mismatches), mismatchTotal)
		// Sort by absolute difference (largest first)
		for i := 0; i < len(mismatches)-1; i++ {
			for j := i + 1; j < len(mismatches); j++ {
				if abs(mismatches[j].diff) > abs(mismatches[i].diff) {
					mismatches[i], mismatches[j] = mismatches[j], mismatches[i]
				}
			}
		}
		for i, m := range mismatches {
			if i >= 20 {
				mlog.Log.Infof("  ... and %d more", len(mismatches)-20)
				break
			}
			mlog.Log.Infof("  %s: [LOCAL]=%d [RPC]=%d DIFF=%+d", m.pubkey.String(), m.local, m.rpc, m.diff)
		}
		mlog.Log.Infof("")
	}

	// Log top 10 vote accounts by local reward
	type rewardEntry struct {
		pubkey solana.PublicKey
		reward uint64
	}
	var sortedLocal []rewardEntry
	for pk, reward := range localVoteRewards {
		sortedLocal = append(sortedLocal, rewardEntry{pk, reward})
	}
	// Sort descending by reward
	for i := 0; i < len(sortedLocal)-1; i++ {
		for j := i + 1; j < len(sortedLocal); j++ {
			if sortedLocal[j].reward > sortedLocal[i].reward {
				sortedLocal[i], sortedLocal[j] = sortedLocal[j], sortedLocal[i]
			}
		}
	}
	mlog.Log.Infof("TOP 10 vote accounts by [LOCAL] reward:")
	for i := 0; i < 10 && i < len(sortedLocal); i++ {
		e := sortedLocal[i]
		rpcVal := rpcVoteRewards[e.pubkey]
		diff := int64(e.reward) - int64(rpcVal)
		mlog.Log.Infof("  %d. %s: [LOCAL]=%d [RPC]=%d DIFF=%+d", i+1, e.pubkey.String(), e.reward, rpcVal, diff)
	}
	mlog.Log.Infof("")

	// Log top 10 vote accounts by RPC reward
	var sortedRPC []rewardEntry
	for pk, reward := range rpcVoteRewards {
		sortedRPC = append(sortedRPC, rewardEntry{pk, reward})
	}
	// Sort descending by reward
	for i := 0; i < len(sortedRPC)-1; i++ {
		for j := i + 1; j < len(sortedRPC); j++ {
			if sortedRPC[j].reward > sortedRPC[i].reward {
				sortedRPC[i], sortedRPC[j] = sortedRPC[j], sortedRPC[i]
			}
		}
	}
	mlog.Log.Infof("TOP 10 vote accounts by [RPC] reward:")
	for i := 0; i < 10 && i < len(sortedRPC); i++ {
		e := sortedRPC[i]
		localVal := localVoteRewards[e.pubkey]
		diff := int64(localVal) - int64(e.reward)
		mlog.Log.Infof("  %d. %s: [LOCAL]=%d [RPC]=%d DIFF=%+d", i+1, e.pubkey.String(), localVal, e.reward, diff)
	}
	mlog.Log.Infof("")

	// Log bottom 10 vote accounts by local reward (non-zero)
	mlog.Log.Infof("BOTTOM 10 vote accounts by [LOCAL] reward:")
	start := len(sortedLocal) - 10
	if start < 0 {
		start = 0
	}
	for i := len(sortedLocal) - 1; i >= start && i >= 0; i-- {
		e := sortedLocal[i]
		if e.reward == 0 {
			continue
		}
		rpcVal := rpcVoteRewards[e.pubkey]
		diff := int64(e.reward) - int64(rpcVal)
		mlog.Log.Infof("  %d. %s: [LOCAL]=%d [RPC]=%d DIFF=%+d", len(sortedLocal)-i, e.pubkey.String(), e.reward, rpcVal, diff)
	}
	mlog.Log.Infof("")

	// Log bottom 10 vote accounts by RPC reward (non-zero)
	mlog.Log.Infof("BOTTOM 10 vote accounts by [RPC] reward:")
	startRPC := len(sortedRPC) - 10
	if startRPC < 0 {
		startRPC = 0
	}
	for i := len(sortedRPC) - 1; i >= startRPC && i >= 0; i-- {
		e := sortedRPC[i]
		if e.reward == 0 {
			continue
		}
		localVal := localVoteRewards[e.pubkey]
		diff := int64(localVal) - int64(e.reward)
		mlog.Log.Infof("  %d. %s: [LOCAL]=%d [RPC]=%d DIFF=%+d", len(sortedRPC)-i, e.pubkey.String(), localVal, e.reward, diff)
	}
	mlog.Log.Infof("")

	// Summary
	hasMismatch := localVoteTotal != rpcVoteTotal || len(localOnlyAccounts) > 0 || len(rpcOnlyAccounts) > 0 || len(mismatches) > 0
	if !hasMismatch {
		mlog.Log.Infof("RESULT: MATCH - [LOCAL] vote rewards identical to [RPC]")
	} else {
		mlog.Log.Warnf("RESULT: MISMATCH - [LOCAL] differs from [RPC] by %d lamports", int64(localVoteTotal)-int64(rpcVoteTotal))

		// On mismatch, log per-vote-pubkey commission summary if stakingRewards provided
		if stakingRewards != nil && len(stakingRewards) > 0 {
			mlog.Log.Infof("")
			mlog.Log.Infof("PER-VOTE-PUBKEY COMMISSION SUMMARY (for mismatched vote accounts):")
			mlog.Log.Infof("  Commission source: current vote account state (delay_commission_updates NOT active)")
			mlog.Log.Infof("")

			// Build per-vote-pubkey aggregation
			type voteAggregation struct {
				votePubkey       solana.PublicKey
				commission       uint8
				voterRewardsSum  uint64
				stakerRewardsSum uint64
				stakeAcctCount   int
			}
			voteAggregations := make(map[solana.PublicKey]*voteAggregation)

			for _, sr := range stakingRewards {
				if sr == nil {
					continue
				}
				agg, exists := voteAggregations[sr.VotePubkey]
				if !exists {
					agg = &voteAggregation{
						votePubkey: sr.VotePubkey,
						commission: sr.Commission,
					}
					voteAggregations[sr.VotePubkey] = agg
				}
				agg.voterRewardsSum += sr.VoterRewards
				agg.stakerRewardsSum += sr.StakerRewards
				agg.stakeAcctCount++
			}

			// Only log vote pubkeys that have a mismatch
			mismatchedVotes := make(map[solana.PublicKey]bool)
			for _, m := range mismatches {
				mismatchedVotes[m.pubkey] = true
			}
			for _, pk := range localOnlyAccounts {
				mismatchedVotes[pk] = true
			}
			for _, pk := range rpcOnlyAccounts {
				mismatchedVotes[pk] = true
			}

			// Sort by voter rewards sum descending
			type sortableAgg struct {
				agg     *voteAggregation
				rpcVal  uint64
				diff    int64
			}
			var sortedAggs []sortableAgg
			for votePubkey, agg := range voteAggregations {
				if mismatchedVotes[votePubkey] {
					rpcVal := rpcVoteRewards[votePubkey]
					diff := int64(agg.voterRewardsSum) - int64(rpcVal)
					sortedAggs = append(sortedAggs, sortableAgg{agg, rpcVal, diff})
				}
			}

			// Sort by absolute diff descending
			for i := 0; i < len(sortedAggs)-1; i++ {
				for j := i + 1; j < len(sortedAggs); j++ {
					if abs(sortedAggs[j].diff) > abs(sortedAggs[i].diff) {
						sortedAggs[i], sortedAggs[j] = sortedAggs[j], sortedAggs[i]
					}
				}
			}

			// Log top 20 mismatched vote pubkeys
			mlog.Log.Infof("  vote_pubkey                                       comm%%  voter_sum      staker_sum     stake_accts  [RPC]          DIFF")
			mlog.Log.Infof("  ------------------------------------------------  -----  -------------  -------------  -----------  -------------  -------------")
			for i, sa := range sortedAggs {
				if i >= 20 {
					mlog.Log.Infof("  ... and %d more mismatched vote pubkeys", len(sortedAggs)-20)
					break
				}
				mlog.Log.Infof("  %s  %3d%%   %-13d  %-13d  %-11d  %-13d  %+d",
					sa.agg.votePubkey.String(), sa.agg.commission, sa.agg.voterRewardsSum, sa.agg.stakerRewardsSum, sa.agg.stakeAcctCount, sa.rpcVal, sa.diff)
			}
			mlog.Log.Infof("")
		}
	}
	mlog.Log.Infof("================================================================================")
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// DebugDumpPerVoteAccountBreakdown logs detailed per-vote-account breakdown of stake, points, and rewards.
// This helps identify if divergence is coming from stake calculation, credits, or commission.
// pointsPerStake: map from stake pubkey -> CalculatedStakePoints
// stakingRewards: map from stake pubkey -> CalculatedStakeRewards
// rpcVoteRewards: vote rewards from RPC (for comparison)
func DebugDumpPerVoteAccountBreakdown(
	pointsPerStake map[solana.PublicKey]*CalculatedStakePoints,
	stakingRewards map[solana.PublicKey]*CalculatedStakeRewards,
	rpcVoteRewards map[solana.PublicKey]uint64,
	totalPoints wide.Uint128,
	totalRewards uint64,
) {
	mlog.Log.Infof("")
	mlog.Log.Infof("================================================================================")
	mlog.Log.Infof("PER-VOTE-ACCOUNT BREAKDOWN (stake, points, rewards)")
	mlog.Log.Infof("================================================================================")

	// Aggregate per vote account
	type voteAcctStats struct {
		votePubkey      solana.PublicKey
		totalStake      uint64   // sum of effective stake delegated to this vote account
		totalPoints     wide.Uint128 // sum of points from all delegations
		voterRewards    uint64   // commission portion (from stakingRewards)
		stakerRewards   uint64   // delegator portion (from stakingRewards)
		numDelegations  int      // number of stake accounts
		commission      uint8    // commission rate
		avgCredits      uint64   // average new_credits_observed across delegations
	}

	voteStats := make(map[solana.PublicKey]*voteAcctStats)

	// First pass: aggregate from pointsPerStake (stake and points)
	stakeCache := global.StakeCache()
	for stakePk, pts := range pointsPerStake {
		if pts == nil {
			continue
		}
		delegation := stakeCache[stakePk]
		if delegation == nil {
			continue
		}
		votePk := delegation.VoterPubkey

		stats, exists := voteStats[votePk]
		if !exists {
			stats = &voteAcctStats{votePubkey: votePk}
			voteStats[votePk] = stats
		}
		stats.totalStake += delegation.StakeLamports
		stats.totalPoints = stats.totalPoints.Add(pts.Points)
		stats.avgCredits += pts.NewCreditsObserved
		stats.numDelegations++
	}

	// Second pass: add rewards from stakingRewards
	for _, sr := range stakingRewards {
		if sr == nil {
			continue
		}
		stats, exists := voteStats[sr.VotePubkey]
		if exists {
			stats.voterRewards += sr.VoterRewards
			stats.stakerRewards += sr.StakerRewards
			stats.commission = sr.Commission
		}
	}

	// Compute average credits
	for _, stats := range voteStats {
		if stats.numDelegations > 0 {
			stats.avgCredits /= uint64(stats.numDelegations)
		}
	}

	// Sort by total stake descending
	type sortableStats struct {
		stats    *voteAcctStats
		rpcVoter uint64
		diff     int64
	}
	var sorted []sortableStats
	for _, stats := range voteStats {
		rpcVoter := rpcVoteRewards[stats.votePubkey]
		diff := int64(stats.voterRewards) - int64(rpcVoter)
		sorted = append(sorted, sortableStats{stats, rpcVoter, diff})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].stats.totalStake > sorted[j].stats.totalStake
	})

	// Summary stats
	var totalLocalStake uint64
	var totalLocalPoints wide.Uint128
	var totalLocalVoter, totalLocalStaker uint64
	var totalRpcVoter uint64
	for _, ss := range sorted {
		totalLocalStake += ss.stats.totalStake
		totalLocalPoints = totalLocalPoints.Add(ss.stats.totalPoints)
		totalLocalVoter += ss.stats.voterRewards
		totalLocalStaker += ss.stats.stakerRewards
		totalRpcVoter += ss.rpcVoter
	}

	mlog.Log.Infof("TOTALS:")
	mlog.Log.Infof("  Vote accounts:    %d", len(sorted))
	mlog.Log.Infof("  Total stake:      %d lamports (%.2f SOL)", totalLocalStake, float64(totalLocalStake)/1e9)
	mlog.Log.Infof("  Total points:     %s (from aggregation)", totalLocalPoints.String())
	mlog.Log.Infof("  Total points:     %s (from calculation)", totalPoints.String())
	mlog.Log.Infof("  Total rewards:    %d (inflation pool)", totalRewards)
	mlog.Log.Infof("  Voter rewards:    LOCAL=%d RPC=%d DIFF=%+d", totalLocalVoter, totalRpcVoter, int64(totalLocalVoter)-int64(totalRpcVoter))
	mlog.Log.Infof("  Staker rewards:   %d", totalLocalStaker)
	mlog.Log.Infof("  Combined:         %d (voter+staker)", totalLocalVoter+totalLocalStaker)
	mlog.Log.Infof("")

	// Top 20 by stake
	mlog.Log.Infof("TOP 20 VOTE ACCOUNTS BY STAKE:")
	mlog.Log.Infof("  %-44s %6s %15s %20s %12s %12s %12s %+12s", "vote_pubkey", "comm%", "stake(SOL)", "points", "voter_local", "voter_rpc", "staker", "diff")
	mlog.Log.Infof("  %s", strings.Repeat("-", 160))
	for i, ss := range sorted {
		if i >= 20 {
			break
		}
		s := ss.stats
		mlog.Log.Infof("  %-44s %5d%% %15.2f %20s %12d %12d %12d %+12d",
			s.votePubkey.String(), s.commission, float64(s.totalStake)/1e9, s.totalPoints.String(),
			s.voterRewards, ss.rpcVoter, s.stakerRewards, ss.diff)
	}
	mlog.Log.Infof("")

	// Top 20 mismatches by absolute diff
	var mismatches []sortableStats
	for _, ss := range sorted {
		if ss.diff != 0 {
			mismatches = append(mismatches, ss)
		}
	}
	sort.Slice(mismatches, func(i, j int) bool {
		absI := mismatches[i].diff
		if absI < 0 { absI = -absI }
		absJ := mismatches[j].diff
		if absJ < 0 { absJ = -absJ }
		return absI > absJ
	})

	if len(mismatches) > 0 {
		mlog.Log.Infof("TOP 20 MISMATCHES BY ABSOLUTE DIFF:")
		mlog.Log.Infof("  %-44s %6s %15s %12s %12s %+12s %8s", "vote_pubkey", "comm%", "stake(SOL)", "voter_local", "voter_rpc", "diff", "delegs")
		mlog.Log.Infof("  %s", strings.Repeat("-", 120))
		for i, ss := range mismatches {
			if i >= 20 {
				mlog.Log.Infof("  ... and %d more mismatched vote accounts", len(mismatches)-20)
				break
			}
			s := ss.stats
			mlog.Log.Infof("  %-44s %5d%% %15.2f %12d %12d %+12d %8d",
				s.votePubkey.String(), s.commission, float64(s.totalStake)/1e9,
				s.voterRewards, ss.rpcVoter, ss.diff, s.numDelegations)
		}
	}
	mlog.Log.Infof("================================================================================")
}

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
func DistributeStakingRewardsForPartition(acctsDb *accountsdb.AccountsDb, partition *Partition, stakingRewards map[solana.PublicKey]*CalculatedStakeRewards, stakeAccountSnapshots map[solana.PublicKey]*accounts.Account, writeSlot uint64) ([]*accounts.Account, []*accounts.Account, uint64, error) {
	var distributedLamports atomic.Uint64
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
			// Account not in snapshots - this means it was filtered out during refresh
			// (not found, tombstone, unmarshal error, or not in Stake state).
			// This is expected and matches the behavior during rewards calculation.
			mlog.Log.Debugf("rewards distribution: stake account %s not in cached snapshots - skipping", stakePk)
			return
		}

		// Clone the cached account so we can modify it without affecting the cache
		stakeAcct := cachedAcct.Clone()

		parentAccts[idx] = cachedAcct.Clone()

		// Deserialize the stake state (we know it's valid from the refresh check)
		stakeState, err := sealevel.UnmarshalStakeState(stakeAcct.Data)
		if err != nil {
			// This should not happen since we validated during refresh, but handle it gracefully
			mlog.Log.Warnf("rewards distribution: stake account %s cannot be deserialized (unexpected) - skipping", stakePk)
			mlog.Log.Warnf("  error: %v", err)
			return
		}

		// Verify it's still in Stake state (should always be true from refresh check)
		if stakeState.Status != sealevel.StakeStateV2StatusStake {
			mlog.Log.Warnf("rewards distribution: stake account %s is not in Stake state (status=%d, unexpected) - skipping", stakePk, stakeState.Status)
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
	ants.Release()

	// Check for worker errors
	if errPtr := workerErr.Load(); errPtr != nil {
		return nil, nil, 0, *errPtr
	}

	err := acctsDb.StoreAccounts(accts, writeSlot)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("error updating accounts for partitioned epoch rewards in writeSlot %d: %w", writeSlot, err)
	}

	return accts, parentAccts, distributedLamports.Load(), nil
}

func minimumStakeDelegation(slotCtx *sealevel.SlotCtx) uint64 {
	if !slotCtx.Features.IsActive(features.StakeMinimumDelegationForRewards) {
		return 0
	}

	if slotCtx.Features.IsActive(features.StakeRaiseMinimumDelegationTo1Sol) {
		return 1000000000
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
	ants.Release()

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
	ants.Release()

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
	ants.Release()

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
	ants.Release()

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
	ants.Release()

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
