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
	// EahStartOffsetSlot and EahStopOffsetSlot are legacy fields for Epoch Accounts Hash.
	// EAH was deprecated after AccountsLtHash activation (~Nov 2024). These fields are only
	// relevant for replaying historical pre-AccountsLtHash slots and are not used in current flow.
	EahStartOffsetSlot  uint64
	EahStopOffsetSlot   uint64
	NumRewardPartitions uint64
	Credits             map[solana.PublicKey]CalculatedStakePoints
	RewardPartitions    Partitions
	StakingRewards      map[solana.PublicKey]*CalculatedStakeRewards
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
		pubkey     solana.PublicKey
		points     wide.Uint128
		commission uint8 // vote account commission (0-100)
	}
	var accountsWithPoints []accountPoints
	var totalPoints wide.Uint128

	// Accounts counted via force_credits_update_with_skipped_reward (get 0 rewards but still counted)
	var forceCreditsUpdateCount uint64
	// Detailed breakdown for debugging
	var creditsLessThanStakeCount uint64
	var activationMatchCount uint64
	var activationWithCreditsLess uint64
	var activationWithCreditsEqual uint64
	var activationWithCreditsGreater uint64
	// Track ALL accounts with activation_epoch == rewarded_epoch (before any filters)
	var totalActivationMatchRaw uint64

	// PASS 1: Calculate total_points and collect accounts
	// Also identify force_credits_update accounts (credits < stake OR activation == rewarded epoch)
	var processed uint64
	for pubkey, delegation := range stakeCache {
		processed++
		if processed%100000 == 0 {
			mlog.Log.Infof("  pass 1 progress: %d/%d accounts (%.1f%%)", processed, total, float64(processed)*100/float64(total))
		}

		// RAW diagnostic: count ALL accounts with activation_epoch == rewarded before any filtering
		if delegation.ActivationEpoch == rewardedEpoch {
			totalActivationMatchRaw++
		}

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
		// In Firedancer, this sets force_credits_update_with_skipped_reward = true in calculate_stake_points_and_credits
		// These accounts are COUNTED but get 0 rewards, and have 0 points (no contribution to total_points)
		if creditsInVote < creditsInStake {
			forceCreditsUpdateCount++
			creditsLessThanStakeCount++
			continue // Counted separately, 0 points so no contribution to total_points
		}

		// FORCE_CREDITS_UPDATE CHECK #2: activation_epoch == rewarded_epoch
		// CRITICAL: This must be checked BEFORE the noCredits check!
		// An account with activation_epoch == rewarded AND credits == stake is still counted via force_credits_update.
		// In FD, activation check (line 380-382 in redeem_rewards) happens after calculate_stake_points_and_credits,
		// but OVERRIDES the points=0 case when activation_epoch matches.
		if delegation.ActivationEpoch == rewardedEpoch {
			forceCreditsUpdateCount++
			activationMatchCount++
			// Track credits relationship for diagnostics
			if creditsInVote < creditsInStake {
				// Already counted above, shouldn't reach here (but track just in case)
				activationWithCreditsLess++
			} else if creditsInVote == creditsInStake {
				activationWithCreditsEqual++
			} else {
				activationWithCreditsGreater++
			}
			// Calculate points (FD does this first), add to total if > 0
			if creditsInVote > creditsInStake && len(epochCredits) > 0 {
				calculatedPoints := calculateStakePointsAndCredits(pubkey, stakeHistory, delegation, voteState, newRateActivationEpoch)
				if !calculatedPoints.Points.Eq(wide.Uint128FromUint64(0)) {
					totalPoints = totalPoints.Add(calculatedPoints.Points)
				}
			}
			continue // Counted separately, don't go through rewards/split checks
		}

		// If credits_in_vote == credits_in_stake (no new credits), skip
		// This is NOT a force_credits_update case - it returns error in Firedancer
		// (Unless activation_epoch == rewarded_epoch, which is handled above)
		if creditsInVote == creditsInStake || len(epochCredits) == 0 {
			noCredits++
			continue
		}

		// Normal case: credits_in_vote > credits_in_stake and activation_epoch != rewarded_epoch
		// Calculate points
		calculatedPoints := calculateStakePointsAndCredits(pubkey, stakeHistory, delegation, voteState, newRateActivationEpoch)
		zero128 := wide.Uint128FromUint64(0)

		if calculatedPoints.Points.Eq(zero128) {
			zeroPoints++
			continue
		}

		// This account has points > 0, add to our list with commission
		accountsWithPoints = append(accountsWithPoints, accountPoints{
			pubkey:     pubkey,
			points:     calculatedPoints.Points,
			commission: commission,
		})
		totalPoints = totalPoints.Add(calculatedPoints.Points)
	}

	mlog.Log.Infof("  pass 1 complete: accounts_with_points=%d force_credits_update=%d total_points=%s",
		len(accountsWithPoints), forceCreditsUpdateCount, totalPoints.String())
	mlog.Log.Infof("  force_credits_update breakdown: credits_less_than_stake=%d activation_match=%d",
		creditsLessThanStakeCount, activationMatchCount)
	mlog.Log.Infof("  activation_match breakdown: credits<stake=%d credits==stake=%d credits>stake=%d",
		activationWithCreditsLess, activationWithCreditsEqual, activationWithCreditsGreater)
	mlog.Log.Infof("  RAW activation_epoch==rewarded (before ANY filters): %d (if 0, stake cache missing activation_epoch)",
		totalActivationMatchRaw)

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

	// Diagnostic counters
	var rewardsGtZero uint64 // accounts where rewards > 0 (before split check)

	for i, acct := range accountsWithPoints {
		if (i+1)%100000 == 0 {
			mlog.Log.Infof("  pass 2 progress: %d/%d accounts (%.1f%%)", i+1, len(accountsWithPoints), float64(i+1)*100/float64(len(accountsWithPoints)))
		}

		// rewards = points * total_rewards / total_points
		// Use 128-bit arithmetic to avoid overflow
		numerator := acct.points.Mul(totalRewards128)
		rewards := numerator.Div(totalPoints)

		if rewards.Eq(wide.Uint128FromUint64(0)) {
			zeroRewards++
			continue
		}

		rewardsGtZero++ // Count for diagnostic bucket 2

		// Commission split check (Firedancer fd_rewards.c lines 400-404)
		// is_split = commission > 0 && commission < 100
		// If is_split AND (voter_portion == 0 OR staker_portion == 0), skip
		commission := acct.commission
		if commission > 100 {
			commission = 100 // Cap at 100 like Firedancer does
		}

		isSplit := commission > 0 && commission < 100
		if isSplit {
			rewardsU64 := rewards.IsUint64()
			var rewardsVal uint64
			if rewardsU64 {
				rewardsVal = rewards.Uint64()
			} else {
				// Rewards is > uint64 max, which means both portions will be non-zero
				// This is extremely unlikely but handle it gracefully
				rewardsVal = ^uint64(0)
			}

			// voter_portion = rewards * commission / 100
			voterPortion := rewardsVal * uint64(commission) / 100
			// staker_portion = rewards * (100 - commission) / 100
			stakerPortion := rewardsVal * uint64(100-commission) / 100

			if voterPortion == 0 || stakerPortion == 0 {
				zeroSplit++
				continue
			}
		}

		eligible++
	}

	// Log diagnostic buckets
	pointsGtZero := uint64(len(accountsWithPoints))
	normalEligible := eligible - forceCreditsUpdateCount
	mlog.Log.Infof("  pass 2 complete: eligible=%d (force_credits_update=%d + normal=%d)",
		eligible, forceCreditsUpdateCount, normalEligible)
	mlog.Log.Infof("DIAGNOSTIC BUCKETS:")
	mlog.Log.Infof("  force_credits_update (credits<stake OR activation==rewarded): %d", forceCreditsUpdateCount)
	mlog.Log.Infof("  points > 0:                    %d", pointsGtZero)
	mlog.Log.Infof("  rewards > 0:                   %d", rewardsGtZero)
	mlog.Log.Infof("  rewards > 0 AND split valid:   %d", normalEligible)
	mlog.Log.Infof("  TOTAL ELIGIBLE:                %d  <-- used for partition count", eligible)
	mlog.Log.Infof("ELIGIBILITY FILTER RESULT: eligible=%d total=%d (excluded: below_min=%d no_vote=%d no_credits=%d zero_points=%d zero_rewards=%d zero_split=%d)",
		eligible, total, belowMin, noVote, noCredits, zeroPoints, zeroRewards, zeroSplit)

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
		calculatedPoints := calculateStakePointsAndCredits(pubkey, stakeHistory, delegation, voteState, newRateActivationEpoch)
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
				// CRITICAL: Partition count mismatch will cause bankhash divergence.
				// Return error to allow clean shutdown with state file writing.
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
		}
	}

	return &PartitionedRewardDistributionInfo{
		TotalStakingRewards:    totalStakingRewards,
		FirstStakingRewardSlot: firstStakingRewardSlot,
		LastStakingRewardSlot:  lastStakingRewardSlot,
		EahStartOffsetSlot:     eahCalcSlot,
		EahStopOffsetSlot:      eahInclusionSlot,
		NumRewardPartitions:    numRewardPartitions,
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

type idxAndPubkey struct {
	idx    int
	pubkey solana.PublicKey
}

func DistributeStakingRewardsForPartition(acctsDb *accountsdb.AccountsDb, partition *Partition, stakingRewards map[solana.PublicKey]*CalculatedStakeRewards, slot uint64) ([]*accounts.Account, []*accounts.Account, uint64, error) {
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

		stakeAcct, err := acctsDb.GetAccount(slot, stakePk)
		if err != nil {
			errVal := fmt.Errorf("unable to get acct %s from acctsdb for partitioned epoch rewards distribution in slot %d: %w", stakePk, slot, err)
			workerErr.CompareAndSwap(nil, &errVal)
			return
		}
		parentAccts[idx] = stakeAcct.Clone()

		// update the delegation in the stake account state
		stakeState, err := sealevel.UnmarshalStakeState(stakeAcct.Data)
		if err != nil {
			errVal := fmt.Errorf("unable to deserialize stake account %s in distributing partitioned rewards: %w", stakePk, err)
			workerErr.CompareAndSwap(nil, &errVal)
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
			errVal := fmt.Errorf("overflow in partitioned epoch rewards distribution in slot %d to acct %s: %w", slot, stakePk, err)
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

	err := acctsDb.StoreAccounts(accts, slot)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("error updating accounts for partitioned epoch rewards in slot %d: %w", slot, err)
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

func CalculateStakeRewardsForAcct(pubkey solana.PublicKey, stakePointsResult *CalculatedStakePoints, delegation *sealevel.Delegation, voteState *sealevel.VoteStateVersions, rewardedEpoch uint64, pointValue PointValue, newRateActivationEpoch *uint64) *CalculatedStakeRewards {
	if pointValue.Rewards == 0 || delegation.ActivationEpoch == rewardedEpoch {
		stakePointsResult.ForceCreditsUpdateWithSkippedReward = true
	}

	if stakePointsResult.ForceCreditsUpdateWithSkippedReward {
		result := &CalculatedStakeRewards{NewCreditsObserved: stakePointsResult.NewCreditsObserved}
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

	result := &CalculatedStakeRewards{StakerRewards: splitResult.StakerPortion,
		VoterRewards: splitResult.VoterPortion, NewCreditsObserved: stakePointsResult.NewCreditsObserved}

	//mlog.Log.Debugf("returning CalculatedStakeRewards for %s. %+v", stakePubkey, result)

	return result
}

type CommissionSplit struct {
	VoterPortion  uint64
	StakerPortion uint64
	IsSplit       bool
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
	result := CommissionSplit{}

	switch commissionRate {
	case 0:
		// no commission, all rewards go to staker
		result.StakerPortion = rewards
	case 100:
		// 100% commission, all rewards go to validator
		result.VoterPortion = rewards
	default:
		// TODO: refactor to use 128-bit math here
		on := rewards
		mine := (on * commissionRate) / 100
		theirs := (on * (100 - commissionRate)) / 100

		result.VoterPortion = mine
		result.StakerPortion = theirs
		result.IsSplit = true
	}

	return result
}

type delegationAndPubkey struct {
	delegation *sealevel.Delegation
	pubkey     solana.PublicKey
}

func CalculateTotalPointsAndPartitions(
	acctsDb *accountsdb.AccountsDb,
	slotCtx *sealevel.SlotCtx,
	slot uint64,
	numPartitions uint64,
	stakeHistory *sealevel.SysvarStakeHistory,
	newWarmupCooldownRateEpoch *uint64,
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

		pcs := calculateStakePointsAndCredits(t.pubkey, stakeHistory, d, voteState, newWarmupCooldownRateEpoch)
		pointsAccum.Add(t.pubkey, pcs)

		if numPartitions != 0 {
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

func calculateStakePointsAndCredits(
	pubkey solana.PublicKey,
	stakeHistory *sealevel.SysvarStakeHistory,
	delegation *sealevel.Delegation,
	voteState *sealevel.VoteStateVersions,
	newRateActivationEpoch *uint64,
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
