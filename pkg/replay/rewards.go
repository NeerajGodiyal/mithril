package replay

import (
	"bytes"
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/rewards"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/wide"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
)

func newWarmupCooldownRateEpoch(epochSchedule *sealevel.SysvarEpochSchedule, f *features.Features) *uint64 {
	slot, existed := f.ActivationSlot(features.ReduceStakeWarmupCooldown)
	if !existed {
		return nil
	}
	epoch := epochSchedule.GetEpoch(slot)
	return &epoch
}

func beginPartitionedEpochRewardsDistribution(acctsDb *accountsdb.AccountsDb, slotCtx *sealevel.SlotCtx, stakeHistory *sealevel.SysvarStakeHistory, epochCtx *ReplayCtx, epochSchedule *sealevel.SysvarEpochSchedule, block *block.Block, f *features.Features, epoch uint64, slot uint64) (*rewards.PartitionedRewardDistributionInfo, []*accounts.Account, []*accounts.Account, error) {
	// SAFEGUARD: Check if epoch_rewards.active is already true, which would indicate
	// we're trying to reprocess an already-processed epoch boundary.
	// Like Firedancer, when active=true we should NOT recompute - use existing sysvar values.
	epochRewardsAcct, err := acctsDb.GetAccount(slot, sealevel.SysvarEpochRewardsAddr)
	if err == nil && len(epochRewardsAcct.Data) >= sealevel.SysvarEpochRewardsStructLen {
		var existingEpochRewards sealevel.SysvarEpochRewards
		decoder := bin.NewBinDecoder(epochRewardsAcct.Data)
		if decErr := existingEpochRewards.UnmarshalWithDecoder(decoder); decErr == nil && existingEpochRewards.Active {
			mlog.Log.Warnf("EPOCH REWARDS ALREADY ACTIVE: skipping fresh computation and using existing sysvar values")
			mlog.Log.Infof("  existing sysvar: num_partitions=%d total_points=%s total_rewards=%d distributed=%d",
				existingEpochRewards.NumPartitions, existingEpochRewards.TotalPoints.String(),
				existingEpochRewards.TotalRewards, existingEpochRewards.DistributedRewards)

			// Return an error to signal caller should use recalculatePartitionedRewardsForResume instead
			return nil, nil, nil, fmt.Errorf("epoch rewards already active - use resume path instead")
		}
	}

	// Compute warmup/cooldown rate epoch first - needed for partition count calculation
	newWarmupCooldownRateEpoch := newWarmupCooldownRateEpoch(epochSchedule, f)

	// Calculate boundary slot = last slot of previous epoch = slot - 1
	// (slot is the first slot of the new epoch)
	// This is the slot from which stake account state should be read for rewards calculation.
	boundarySlot := slot - 1

	// CRITICAL: Refresh stake cache credits_observed from AccountsDB BEFORE partition count calculation.
	// The refresh removes stale entries (tombstones, unmarshal errors, etc.) from the stake cache.
	// This MUST happen before DeterminePartitionedStakingRewardsInfoLocal to ensure the partition count
	// is computed on the same stake cache that will be used for actual rewards calculation.
	// This matches Firedancer's fd_stake_delegations_refresh() call in init_after_snapshot.
	// Also capture stake account snapshots for use during distribution (avoids re-reading from AccountsDB).
	_, _, stakeAccountSnapshots := rewards.RefreshStakeCacheCreditsObserved(acctsDb, boundarySlot)

	// IMPORTANT: Validate partition count BEFORE any account writes.
	// If validation fails, we want to exit cleanly without having modified AccountsDB.
	// NOTE: This now uses the refreshed stake cache, ensuring consistency with rewards calculation.
	partitionedRewardsInfo, err := rewards.DeterminePartitionedStakingRewardsInfoLocal(epochSchedule, &epochCtx.Inflation, epochCtx.Capitalization, epoch, epoch-1, epochCtx.SlotsPerYear, f, stakeHistory, newWarmupCooldownRateEpoch)
	if err != nil {
		return nil, nil, nil, err
	}

	// Set the boundary slot and stake snapshots for use during distribution
	partitionedRewardsInfo.BoundarySlot = boundarySlot
	partitionedRewardsInfo.StakeAccountSnapshots = stakeAccountSnapshots

	// Now safe to distribute vote rewards since validation passed
	updatedAccts, parentUpdatedAccts, voteRewardsDistributed, err := rewards.DistributeVotingRewards(acctsDb, block.Rewards, slot)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("vote rewards distribution failed: %w", err)
	}
	totalRewards := partitionedRewardsInfo.TotalStakingRewards

	var points wide.Uint128
	var pointsPerStakeAcct map[solana.PublicKey]*rewards.CalculatedStakePoints
	// Use locally computed NumRewardPartitions, NOT block.NumRewardPartitions (which comes from RPC and may be MaxUint64 if missing)
	// Pass nil for maxEpoch - fresh compute uses current vote credits (correct at epoch boundary)
	// Use boundarySlot for stake account reads
	pointsPerStakeAcct, points, partitionedRewardsInfo.RewardPartitions = rewards.CalculateTotalPointsAndPartitions(acctsDb, slotCtx, boundarySlot, partitionedRewardsInfo.NumRewardPartitions, stakeHistory, newWarmupCooldownRateEpoch, nil)
	pointValue := rewards.PointValue{Rewards: totalRewards, Points: points}
	partitionedRewardsInfo.StakingRewards = rewards.CalculateStakeRewards(pointsPerStakeAcct, slotCtx, stakeHistory, slot, epoch-1, pointValue, newWarmupCooldownRateEpoch, slotCtx.Features)

	// DEBUG: Log comprehensive epoch rewards diagnostics
	// This includes points, total rewards, and vote rewards comparison
	mlog.Log.Infof("")
	mlog.Log.Infof("================================================================================")
	mlog.Log.Infof("EPOCH REWARDS DIAGNOSTICS - Epoch %d -> %d (slot %d)", epoch-1, epoch, slot)
	mlog.Log.Infof("================================================================================")

	// Calculate local totals from staking rewards
	var localStakerTotal, localVoterTotal uint64
	for _, sr := range partitionedRewardsInfo.StakingRewards {
		if sr != nil {
			localStakerTotal += sr.StakerRewards
			localVoterTotal += sr.VoterRewards
		}
	}

	// Count RPC vote rewards
	var rpcVoteTotal uint64
	var rpcVoteCount int
	for _, r := range block.Rewards {
		if string(r.RewardType) == "Voting" && r.Lamports > 0 {
			rpcVoteTotal += uint64(r.Lamports)
			rpcVoteCount++
		}
	}

	// Aggregate local vote rewards by vote account (for accurate count comparison)
	localVoteRewards := rewards.AggregateVoteRewardsFromStakingRewards(partitionedRewardsInfo.StakingRewards)
	localVoteAccountCount := len(localVoteRewards)

	// Derive RPC eligible count from partitions (partitions = ceil(eligible / 4096))
	// If partitions match, eligible counts are implicitly the same
	// Note: block.NumRewardPartitions may be MaxUint64 if RPC didn't provide it
	var rpcEligibleEstimate string
	if block.NumRewardPartitions != ^uint64(0) { // not MaxUint64
		// Estimate: eligible is between (partitions-1)*4096+1 and partitions*4096
		minEligible := (block.NumRewardPartitions-1)*4096 + 1
		maxEligible := block.NumRewardPartitions * 4096
		rpcEligibleEstimate = fmt.Sprintf("%d-%d", minEligible, maxEligible)
	} else {
		rpcEligibleEstimate = "(not available)"
	}

	// Fetch RPC EpochRewards sysvar for total_points and total_rewards comparison
	// NOTE: RPC returns the sysvar at whatever slot/epoch the RPC node is currently at.
	// When replaying historical data, the RPC sysvar may be from a different epoch entirely.
	// We log the RPC DistributionStartingBlockHeight to help identify mismatches.
	rpcEpochRewards, rpcSysvarErr := rewards.FetchRpcEpochRewardsSysvar()

	mlog.Log.Infof("")
	mlog.Log.Infof("                              [LOCAL]              [RPC]                 DIFF")
	mlog.Log.Infof("                              -------              -----                 ----")

	// Total points comparison
	if rpcSysvarErr == nil && rpcEpochRewards != nil {
		// Warn if RPC sysvar is from a different epoch (different starting block)
		// This comparison is only valid when replaying the same epoch the RPC is in
		if rpcEpochRewards.DistributionStartingBlockHeight != slot {
			mlog.Log.Warnf("  ⚠️  RPC sysvar may be from different epoch (RPC starting_slot=%d, LOCAL slot=%d)",
				rpcEpochRewards.DistributionStartingBlockHeight, slot)
		}
		// Compute diff: LOCAL points - RPC points
		// Both are Uint128, so we need to handle this carefully
		localPointsStr := points.String()
		rpcPointsStr := rpcEpochRewards.TotalPoints.String()
		// For display, we show both values; diff requires bigint math
		if points.Eq(rpcEpochRewards.TotalPoints) {
			mlog.Log.Infof("  Total points:               %-20s %-20s MATCH", localPointsStr, rpcPointsStr)
		} else if points.Gt(rpcEpochRewards.TotalPoints) {
			diff := points.Sub(rpcEpochRewards.TotalPoints)
			mlog.Log.Infof("  Total points:               %-20s %-20s +%s", localPointsStr, rpcPointsStr, diff.String())
		} else {
			diff := rpcEpochRewards.TotalPoints.Sub(points)
			mlog.Log.Infof("  Total points:               %-20s %-20s -%s", localPointsStr, rpcPointsStr, diff.String())
		}
	} else {
		mlog.Log.Infof("  Total points:               %-20s (RPC error: %v)", points.String(), rpcSysvarErr)
	}

	// Total rewards comparison from sysvar
	if rpcSysvarErr == nil && rpcEpochRewards != nil {
		localTotal := localStakerTotal + localVoterTotal
		rpcTotal := rpcEpochRewards.TotalRewards
		mlog.Log.Infof("  Total rewards (sysvar):     %-20d %-20d %+d", localTotal, rpcTotal, int64(localTotal)-int64(rpcTotal))
	}

	// Num partitions comparison
	if block.NumRewardPartitions != ^uint64(0) {
		mlog.Log.Infof("  Num partitions:             %-20d %-20d %+d", partitionedRewardsInfo.NumRewardPartitions, block.NumRewardPartitions, int64(partitionedRewardsInfo.NumRewardPartitions)-int64(block.NumRewardPartitions))
	} else if rpcSysvarErr == nil && rpcEpochRewards != nil {
		mlog.Log.Infof("  Num partitions:             %-20d %-20d %+d (from sysvar)", partitionedRewardsInfo.NumRewardPartitions, rpcEpochRewards.NumPartitions, int64(partitionedRewardsInfo.NumRewardPartitions)-int64(rpcEpochRewards.NumPartitions))
	} else {
		mlog.Log.Infof("  Num partitions:             %-20d (not available from RPC)", partitionedRewardsInfo.NumRewardPartitions)
	}
	mlog.Log.Infof("  Eligible stake accounts:    %-20d %-20s", partitionedRewardsInfo.EligibleCount, rpcEligibleEstimate)
	mlog.Log.Infof("  Vote accounts:              %-20d %-20d %+d", localVoteAccountCount, rpcVoteCount, localVoteAccountCount-rpcVoteCount)
	mlog.Log.Infof("  Vote rewards (lamports):    %-20d %-20d %+d", localVoterTotal, rpcVoteTotal, int64(localVoterTotal)-int64(rpcVoteTotal))
	mlog.Log.Infof("  Staker rewards (lamports):  %-20d (not available from RPC)", localStakerTotal)
	mlog.Log.Infof("  Combined (lamports):        %-20d (not available from RPC)", localStakerTotal+localVoterTotal)
	mlog.Log.Infof("")
	mlog.Log.Infof("  Eligibility: valid vote account, points > 0")
	mlog.Log.Infof("  Partition formula: ceil(eligible / 4096) = partitions")
	mlog.Log.Infof("")

	// Compare vote rewards with RPC
	if len(block.Rewards) > 0 {
		rewards.CompareVoteRewardsWithRPC(localVoteRewards, block.Rewards)
	}

	// SANITY CHECK: Verify eligible count matches actual rewards count.
	// A mismatch indicates the stake cache changed between partition count calculation and rewards calculation,
	// which would cause bank hash divergence. This check catches regressions early.
	actualRewardsCount := uint64(len(partitionedRewardsInfo.StakingRewards))
	if partitionedRewardsInfo.EligibleCount != actualRewardsCount {
		// Option C: Log detailed diagnostics BEFORE failing
		diff := int64(partitionedRewardsInfo.EligibleCount) - int64(actualRewardsCount)
		mlog.Log.Errorf("================================================================================")
		mlog.Log.Errorf("CRITICAL: ELIGIBLE COUNT MISMATCH - DIVERGENCE IMMINENT")
		mlog.Log.Errorf("================================================================================")
		mlog.Log.Errorf("  expected (from partition count pass): %d", partitionedRewardsInfo.EligibleCount)
		mlog.Log.Errorf("  actual (from rewards calculation):    %d", actualRewardsCount)
		mlog.Log.Errorf("  difference:                           %d", diff)
		mlog.Log.Errorf("")
		mlog.Log.Errorf("Context:")
		mlog.Log.Errorf("  epoch=%d slot=%d boundary_slot=%d", epoch, slot, boundarySlot)
		mlog.Log.Errorf("  stake_cache_size=%d", global.StakeCacheSize())
		mlog.Log.Errorf("  num_partitions=%d", partitionedRewardsInfo.NumRewardPartitions)
		mlog.Log.Errorf("")

		// Build sets for comparison
		eligibleSet := make(map[solana.PublicKey]bool)
		for pubkey := range pointsPerStakeAcct {
			eligibleSet[pubkey] = true
		}
		rewardsSet := make(map[solana.PublicKey]bool)
		for pubkey := range partitionedRewardsInfo.StakingRewards {
			rewardsSet[pubkey] = true
		}

		// Find accounts in eligible (pass 1) but not in rewards (pass 2)
		var inEligibleNotRewards []solana.PublicKey
		for pubkey := range eligibleSet {
			if !rewardsSet[pubkey] {
				inEligibleNotRewards = append(inEligibleNotRewards, pubkey)
			}
		}

		// Find accounts in rewards (pass 2) but not in eligible (pass 1)
		var inRewardsNotEligible []solana.PublicKey
		for pubkey := range rewardsSet {
			if !eligibleSet[pubkey] {
				inRewardsNotEligible = append(inRewardsNotEligible, pubkey)
			}
		}

		// Get current stake cache for lookups
		stakeCache := global.StakeCache()

		mlog.Log.Errorf("Accounts in eligible set but NOT in rewards: %d", len(inEligibleNotRewards))
		maxSample := 10
		for i, pubkey := range inEligibleNotRewards {
			if i >= maxSample {
				mlog.Log.Errorf("  ... and %d more", len(inEligibleNotRewards)-maxSample)
				break
			}
			// Try to get more info about this account
			if delegation, exists := stakeCache[pubkey]; exists {
				mlog.Log.Errorf("  [%d] %s (stake=%d voter=%s activation=%d credits=%d)",
					i, pubkey, delegation.StakeLamports, delegation.VoterPubkey, delegation.ActivationEpoch, delegation.CreditsObserved)
			} else {
				mlog.Log.Errorf("  [%d] %s (NOT IN STAKE CACHE - was it removed?)", i, pubkey)
			}
		}

		mlog.Log.Errorf("")
		mlog.Log.Errorf("Accounts in rewards set but NOT in eligible: %d", len(inRewardsNotEligible))
		for i, pubkey := range inRewardsNotEligible {
			if i >= maxSample {
				mlog.Log.Errorf("  ... and %d more", len(inRewardsNotEligible)-maxSample)
				break
			}
			if delegation, exists := stakeCache[pubkey]; exists {
				mlog.Log.Errorf("  [%d] %s (stake=%d voter=%s activation=%d credits=%d)",
					i, pubkey, delegation.StakeLamports, delegation.VoterPubkey, delegation.ActivationEpoch, delegation.CreditsObserved)
			} else {
				mlog.Log.Errorf("  [%d] %s (NOT IN STAKE CACHE)", i, pubkey)
			}
		}

		mlog.Log.Errorf("")
		mlog.Log.Errorf("This mismatch indicates the stake cache changed between partition count and rewards")
		mlog.Log.Errorf("calculation. This WILL cause bank hash divergence. Failing to prevent bad state.")
		mlog.Log.Errorf("================================================================================")

		return nil, nil, nil, fmt.Errorf("eligible count mismatch: expected %d, got %d (diff=%d) - see logs above for details",
			partitionedRewardsInfo.EligibleCount, actualRewardsCount, diff)
	}

	newEpochRewards := sealevel.SysvarEpochRewards{DistributionStartingBlockHeight: block.BlockHeight + 1,
		NumPartitions: partitionedRewardsInfo.NumRewardPartitions, ParentBlockhash: block.LastBlockhash,
		TotalRewards: totalRewards, DistributedRewards: voteRewardsDistributed, TotalPoints: points, Active: true}

	epochRewardsAcct, err = acctsDb.GetAccount(slot, sealevel.SysvarEpochRewardsAddr)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("unable to get EpochRewards from acctsdb: %w", err)
	}
	parentUpdatedAccts = append(parentUpdatedAccts, epochRewardsAcct.Clone())

	writer := new(bytes.Buffer)
	encoder := bin.NewBinEncoder(writer)
	newEpochRewards.MustMarshalWithEncoder(encoder)
	copy(epochRewardsAcct.Data, writer.Bytes())

	err = acctsDb.StoreAccounts([]*accounts.Account{epochRewardsAcct}, slot)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("unable to update EpochRewards sysvar to acctsdb: %w", err)
	}
	sealevel.SysvarCache.EpochRewards.Acct = epochRewardsAcct
	sealevel.SysvarCache.EpochRewards.Sysvar = &newEpochRewards

	numStakeAccounts := uint64(len(global.StakeCache()))
	firstSlotInEpoch := epochSchedule.FirstSlotInEpoch(epoch)
	slotsInEpoch := epochSchedule.SlotsInEpoch(epoch)

	mlog.Log.Infof("rewards distribution start: epoch=%d slot=%d block_height=%d",
		epoch, slot, block.BlockHeight)
	mlog.Log.Infof("  first_slot_in_epoch=%d slots_in_epoch=%d stake_accts=%d",
		firstSlotInEpoch, slotsInEpoch, numStakeAccounts)
	mlog.Log.Infof("  distribution_start_height=%d partitions=%d total_rewards=%d vote_rewards=%d",
		newEpochRewards.DistributionStartingBlockHeight, newEpochRewards.NumPartitions,
		newEpochRewards.TotalRewards, voteRewardsDistributed)
	mlog.Log.Infof("  first_reward_slot=%d last_reward_slot=%d total_points=%s",
		partitionedRewardsInfo.FirstStakingRewardSlot, partitionedRewardsInfo.LastStakingRewardSlot,
		points.String())

	updatedAccts = append(updatedAccts, epochRewardsAcct.Clone())
	epochCtx.Capitalization += voteRewardsDistributed

	return partitionedRewardsInfo, updatedAccts, parentUpdatedAccts, nil
}

// recalculatePartitionedRewardsForResume rebuilds the partitionedRewardsInfo when resuming
// during the epoch rewards period. This is needed because the in-memory partitionedRewardsInfo
// is lost on crash/restart, but we can reconstruct it from:
//   - EpochRewards sysvar: NumPartitions, TotalRewards, TotalPoints, ParentBlockhash
//   - Stake/Vote caches: populated from AccountsDB during resume
//
// This works because stake accounts cannot change during the rewards period (stake program
// rejects all operations when EpochRewards.Active == true), so the same inputs produce
// the same partition assignments and reward calculations.
//
// Returns (nil, nil) if rewards period is already complete (Active == false).
// Returns (nil, error) if EpochRewards sysvar cannot be loaded.
func recalculatePartitionedRewardsForResume(
	acctsDb *accountsdb.AccountsDb,
	stakeHistory *sealevel.SysvarStakeHistory,
	epochSchedule *sealevel.SysvarEpochSchedule,
	f *features.Features,
	epoch uint64,
	slot uint64,
) (*rewards.PartitionedRewardDistributionInfo, error) {
	// Try cache first, fall back to AccountsDB
	var epochRewards *sealevel.SysvarEpochRewards
	if sealevel.SysvarCache.EpochRewards.Sysvar != nil {
		epochRewards = sealevel.SysvarCache.EpochRewards.Sysvar
	} else {
		// Load from AccountsDB
		epochRewardsAcct, err := acctsDb.GetAccount(slot, sealevel.SysvarEpochRewardsAddr)
		if err != nil {
			return nil, fmt.Errorf("failed to load EpochRewards sysvar from AccountsDB: %w", err)
		}
		var er sealevel.SysvarEpochRewards
		decoder := bin.NewBinDecoder(epochRewardsAcct.Data)
		er.MustUnmarshalWithDecoder(decoder)
		epochRewards = &er
		// Update cache for future use
		sealevel.SysvarCache.EpochRewards.Acct = epochRewardsAcct
		sealevel.SysvarCache.EpochRewards.Sysvar = epochRewards
	}

	// Gate on Active flag - if rewards already complete, skip reconstruction
	if !epochRewards.Active {
		mlog.Log.Infof("rewards resume: EpochRewards.Active=false, rewards period already complete (distributed=%d)",
			epochRewards.DistributedRewards)
		return nil, nil
	}

	// Calculate boundary slot = last slot of previous epoch = firstSlotInEpoch - 1
	// This is the slot from which stake account state should be read for rewards calculation.
	firstSlotInEpoch := epochSchedule.FirstSlotInEpoch(epoch)
	boundarySlot := firstSlotInEpoch - 1

	numStakeAccounts := uint64(len(global.StakeCache()))
	mlog.Log.Infof("rewards resume: reconstructing partitionedRewardsInfo from stored state")
	mlog.Log.Infof("  epoch=%d slot=%d boundarySlot=%d active=%v distributed=%d/%d",
		epoch, slot, boundarySlot, epochRewards.Active, epochRewards.DistributedRewards, epochRewards.TotalRewards)
	mlog.Log.Infof("  num_partitions=%d distribution_start_height=%d stake_accts=%d",
		epochRewards.NumPartitions, epochRewards.DistributionStartingBlockHeight, numStakeAccounts)

	// Create a minimal SlotCtx with the stored ParentBlockhash
	// This blockhash is used by CalculateRewardPartitionForPubkey for deterministic partition assignment
	// NOTE: We set Blockhash (not LastBlockhash) because the rewards code receives prevSlotCtx,
	// and prevSlotCtx.Blockhash IS the parent blockhash of the first epoch slot.
	// The EpochRewards.ParentBlockhash stores exactly this value.
	mockSlotCtx := &sealevel.SlotCtx{
		Blockhash:  epochRewards.ParentBlockhash,
		Features:   f,
		AccountsDb: acctsDb,
	}

	// CRITICAL: Refresh stake cache credits_observed from AccountsDB using the BOUNDARY slot.
	// The manifest's epoch_stakes contains stake delegation entries, but credits_observed is NOT
	// updated after rewards distribution - it only reflects the value when the stake was created
	// or last modified. The actual stake accounts in AccountsDB have the correct values.
	// Using boundary slot ensures we see the same state that Agave used for rewards calculation.
	// This matches Firedancer's fd_stake_delegations_refresh() call before rewards calculation.
	// Also capture stake account snapshots for use during distribution (avoids re-reading from AccountsDB).
	_, _, stakeAccountSnapshots := rewards.RefreshStakeCacheCreditsObserved(acctsDb, boundarySlot)

	// Rebuild partition assignments and calculate points
	// Use rewardedEpoch (epoch-1) as maxEpoch to freeze vote credits at epoch boundary.
	// This matches Firedancer's prev_vote_credits behavior - we only consider credits
	// earned up to the rewarded epoch, ignoring any credits earned during distribution.
	newWarmupCooldownRateEpoch := newWarmupCooldownRateEpoch(epochSchedule, f)
	rewardedEpoch := epoch - 1
	pointsPerStakeAcct, points, partitions := rewards.CalculateTotalPointsAndPartitions(
		acctsDb, mockSlotCtx, boundarySlot, epochRewards.NumPartitions, stakeHistory, newWarmupCooldownRateEpoch, &rewardedEpoch)

	// When epoch_rewards.active == true, we're resuming mid-distribution.
	// Like Firedancer, we should NOT recompute total_points - use sysvar values directly.
	// The computed points may differ due to stale vote cache data from snapshot
	// (snapshot vote_credits can differ from epoch-boundary vote_credits).
	//
	// We MUST scale individual points to match sysvar total, otherwise rewards sum will be wrong.
	if !points.Eq(epochRewards.TotalPoints) {
		mlog.Log.Warnf("rewards resume: POINTS MISMATCH - computed=%s stored=%s (stake_accts=%d)",
			points.String(), epochRewards.TotalPoints.String(), numStakeAccounts)
		mlog.Log.Warnf("  this is expected when resuming from snapshot; using sysvar total_points per FD behavior")

		// Scale each stake account's points proportionally to match epochRewards.TotalPoints
		// This ensures: sum(scaled_points) ≈ epochRewards.TotalPoints
		// Individual proportions are preserved (even if slightly off due to non-uniform staleness)
		// Formula: scaled_points[i] = computed_points[i] * (stored_total / computed_total)
		zeroPoints := wide.Uint128FromUint64(0)
		if points.Cmp(zeroPoints) != 0 {
			for pubkey, calcPoints := range pointsPerStakeAcct {
				if calcPoints.Points.Cmp(zeroPoints) != 0 {
					// scaled = (original * stored_total) / computed_total
					// Use 128-bit multiplication then division
					numerator := calcPoints.Points.Mul(epochRewards.TotalPoints)
					scaledPoints := numerator.Div(points)
					pointsPerStakeAcct[pubkey].Points = scaledPoints
				}
			}
			mlog.Log.Infof("rewards resume: scaled %d stake account points to match stored total", len(pointsPerStakeAcct))
		}

		// Use stored total for subsequent calculations (as FD does)
		points = epochRewards.TotalPoints
	}

	// Rebuild reward calculations using stored total rewards and points
	pointValue := rewards.PointValue{
		Rewards: epochRewards.TotalRewards,
		Points:  epochRewards.TotalPoints, // Use stored points for consistency
	}
	stakingRewards := rewards.CalculateStakeRewards(
		pointsPerStakeAcct, mockSlotCtx, stakeHistory, slot, epoch-1,
		pointValue, newWarmupCooldownRateEpoch, f)

	// Build the partitioned rewards info struct
	// Note: firstSlotInEpoch and boundarySlot were calculated earlier in this function

	mlog.Log.Infof("rewards resume: reconstruction complete - partitions=%d stake_rewards=%d total_points=%s snapshots=%d",
		len(partitions), len(stakingRewards), epochRewards.TotalPoints.String(), len(stakeAccountSnapshots))

	return &rewards.PartitionedRewardDistributionInfo{
		TotalStakingRewards:       epochRewards.TotalRewards,
		FirstStakingRewardSlot:    firstSlotInEpoch + 1,
		LastStakingRewardSlot:     firstSlotInEpoch + epochRewards.NumPartitions,
		BoundarySlot:              boundarySlot, // Used for reading stake accounts during distribution
		NumRewardPartitions:       epochRewards.NumPartitions,
		RewardPartitions:          partitions,
		StakingRewards:            stakingRewards,
		StakeAccountSnapshots:     stakeAccountSnapshots, // Cached stake accounts for distribution
	}, nil
}

func distributePartitionedEpochRewardsForSlot(acctsDb *accountsdb.AccountsDb, epochCtx *ReplayCtx, partitionedEpochRewardsInfo *rewards.PartitionedRewardDistributionInfo, currentSlot uint64, currentBlockHeight uint64) ([]*accounts.Account, []*accounts.Account, error) {
	epochRewardsAcct, err := acctsDb.GetAccount(currentSlot, sealevel.SysvarEpochRewardsAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to get EpochRewards from acctsdb: %w", err)
	}

	var epochRewards sealevel.SysvarEpochRewards
	decoder := bin.NewBinDecoder(epochRewardsAcct.Data)
	epochRewards.MustUnmarshalWithDecoder(decoder)

	partitionIdx := currentBlockHeight - epochRewards.DistributionStartingBlockHeight

	// Bounds check: if partitionIdx is out of range, set inactive and return early.
	// This prevents panic from out-of-bounds Partition() access and handles any unexpected height drift.
	if partitionIdx >= partitionedEpochRewardsInfo.NumRewardPartitions {
		mlog.Log.Warnf("rewards distribution: partitionIdx %d >= numPartitions %d, setting inactive",
			partitionIdx, partitionedEpochRewardsInfo.NumRewardPartitions)
		epochRewards.Active = false

		writer := new(bytes.Buffer)
		encoder := bin.NewBinEncoder(writer)
		epochRewards.MustMarshalWithEncoder(encoder)
		copy(epochRewardsAcct.Data, writer.Bytes())

		err = acctsDb.StoreAccounts([]*accounts.Account{epochRewardsAcct}, currentSlot)
		if err != nil {
			return nil, nil, fmt.Errorf("unable to update EpochRewards sysvar to acctsdb: %w", err)
		}
		sealevel.SysvarCache.EpochRewards.Acct = epochRewardsAcct
		sealevel.SysvarCache.EpochRewards.Sysvar = &epochRewards

		return nil, nil, nil
	}

	partitionSize := partitionedEpochRewardsInfo.RewardPartitions.Partition(partitionIdx).NumPubkeys()
	// Use cached stake account snapshots instead of reading from AccountsDB.
	// This ensures we use the exact state captured during refresh, avoiding issues
	// with GetAccount returning current state instead of boundary-slot state.
	distributedAccts, parentDistributedAccts, distributedLamports, err := rewards.DistributeStakingRewardsForPartition(
		acctsDb,
		partitionedEpochRewardsInfo.RewardPartitions.Partition(partitionIdx),
		partitionedEpochRewardsInfo.StakingRewards,
		partitionedEpochRewardsInfo.StakeAccountSnapshots, // cached accounts from refresh
		currentSlot,                                       // writeSlot
	)
	if err != nil {
		return nil, nil, fmt.Errorf("staking rewards distribution failed for partition %d: %w", partitionIdx, err)
	}
	parentDistributedAccts = append(parentDistributedAccts, epochRewardsAcct.Clone())

	epochRewards.Distribute(distributedLamports)

	// Log partition progress at debug level (every partition)
	mlog.Log.Debugf("rewards partition %d/%d: slot=%d height=%d stake_accts=%d lamports=%d cumulative=%d",
		partitionIdx+1, partitionedEpochRewardsInfo.NumRewardPartitions,
		currentSlot, currentBlockHeight, partitionSize, distributedLamports, epochRewards.DistributedRewards)

	// Stop distribution when we've processed the last partition (partition-based, not slot-based)
	if partitionIdx >= partitionedEpochRewardsInfo.NumRewardPartitions-1 {
		epochRewards.Active = false
		mlog.Log.Infof("rewards distribution complete: slot=%d block_height=%d",
			currentSlot, currentBlockHeight)
		mlog.Log.Infof("  partition_idx=%d num_partitions=%d total_distributed=%d total_rewards=%d",
			partitionIdx+1, partitionedEpochRewardsInfo.NumRewardPartitions,
			epochRewards.DistributedRewards, partitionedEpochRewardsInfo.TotalStakingRewards)
	}

	writer := new(bytes.Buffer)
	encoder := bin.NewBinEncoder(writer)
	epochRewards.MustMarshalWithEncoder(encoder)
	copy(epochRewardsAcct.Data, writer.Bytes())

	err = acctsDb.StoreAccounts([]*accounts.Account{epochRewardsAcct}, currentSlot)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to update EpochRewards sysvar to acctsdb: %w", err)
	}
	sealevel.SysvarCache.EpochRewards.Acct = epochRewardsAcct
	sealevel.SysvarCache.EpochRewards.Sysvar = &epochRewards

	distributedAccts = append(distributedAccts, epochRewardsAcct.Clone())
	epochCtx.Capitalization += distributedLamports

	return distributedAccts, parentDistributedAccts, nil
}
