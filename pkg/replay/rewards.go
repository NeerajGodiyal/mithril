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

func beginPartitionedEpochRewardsDistribution(acctsDb *accountsdb.AccountsDb, slotCtx *sealevel.SlotCtx, stakeHistory *sealevel.SysvarStakeHistory, epochCtx *ReplayCtx, epochSchedule *sealevel.SysvarEpochSchedule, block *block.Block, f *features.Features, epoch uint64, slot uint64, transitionCtx *EpochTransitionContext) (*rewards.PartitionedRewardDistributionInfo, []*accounts.Account, []*accounts.Account, error) {
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

	// Calculate total staking rewards from inflation (doesn't need stake cache)
	totalStakingRewards := rewards.CalculatePreviousEpochInflationRewards(
		epochSchedule, &epochCtx.Inflation, epochCtx.Capitalization, epoch, epoch-1, epochCtx.SlotsPerYear, f)

	// OPTIMIZED: Combined pass for refresh + points + snapshots.
	// This reduces I/O by only reading from AccountsDB for accounts that pass initial eligibility checks
	// (stake >= minimum AND has vote account in cache). Ineligible accounts are skipped entirely.
	//
	// First pass with numPartitions=0 to get points map (no partition assignment yet).
	// This replaces the old flow of:
	// 1. RefreshStakeCacheCreditsObserved (read ALL accounts)
	// 2. CountEligibleStakeAccountsWithRewardsFilter (iterate stake cache)
	// 3. CalculateTotalPointsAndPartitions (iterate stake cache again)
	//
	// CRITICAL: Pass rewardedEpoch as maxEpoch to clamp vote credits to the epoch being rewarded.
	// This prevents "post-boundary credits leak" where credits from the new epoch (906) would
	// incorrectly inflate rewards for the previous epoch (905).
	rewardedEpoch := epoch - 1
	pointsPerStakeAcct, points, _, stakeAccountSnapshots, _, _ := rewards.CombinedRefreshPointsAndPartitions(
		acctsDb, slotCtx, boundarySlot, 0, stakeHistory, newWarmupCooldownRateEpoch, &rewardedEpoch)

	// Calculate individual stake rewards BEFORE computing partition count.
	// This matches dev-calc behavior: partition count is based on len(stakingRewards),
	// which is the POST-reward count of accounts that will actually receive rewards
	// (or need ForceCreditsUpdate).
	pointValue := rewards.PointValue{Rewards: totalStakingRewards, Points: points}
	stakingRewards := rewards.CalculateStakeRewards(pointsPerStakeAcct, slotCtx, stakeHistory, slot, epoch-1, pointValue, newWarmupCooldownRateEpoch, slotCtx.Features)

	// Compute partition count from stakingRewards count (POST-reward recipient count)
	// This matches dev-calc which uses len(stakeInfoResults) after reward calculation
	slotsPerEpoch := epochSchedule.SlotsInEpoch(epoch)
	numRewardPartitions := rewards.ComputeNumRewardPartitions(epoch, slotsPerEpoch, uint64(len(stakingRewards)), epochSchedule.FirstNormalEpoch)

	firstSlotInEpoch := epochSchedule.FirstSlotInEpoch(epoch)
	firstStakingRewardSlot := firstSlotInEpoch + 1
	lastStakingRewardSlot := firstSlotInEpoch + numRewardPartitions

	mlog.Log.Infof("rewards partition: epoch=%d recipients=%d partitions=%d first=%d last=%d total_rewards=%d",
		epoch, len(stakingRewards), numRewardPartitions, firstStakingRewardSlot, lastStakingRewardSlot, totalStakingRewards)

	// Build partitions from stakingRewards keys (accounts that will receive rewards or need credits update)
	// This matches dev-calc behavior where partitions are built from stakeInfoResults keys.
	partitions := rewards.NewPartitions(numRewardPartitions)
	for pubkey := range stakingRewards {
		if numRewardPartitions != 0 {
			partitionIdx := rewards.CalculateRewardPartitionForPubkey(pubkey, slotCtx.Blockhash, numRewardPartitions)
			partitions.AddPubkey(partitionIdx, pubkey)
		}
	}

	// Log RPC partition count for comparison (informational only, never blocks processing)
	if rewards.GetValidationRpcClient() != nil {
		rpcNumPartitions, err := rewards.FetchRpcPartitionCountWithBackups(rewards.GetValidationRpcClient(), rewards.GetValidationRpcBackups(), firstSlotInEpoch)
		if err != nil {
			mlog.Log.Warnf("partition comparison: RPC fetch failed: %v", err)
		} else if numRewardPartitions == rpcNumPartitions {
			mlog.Log.Infof("partition comparison: local=%d rpc=%d ✓", numRewardPartitions, rpcNumPartitions)
		} else {
			mlog.Log.Warnf("partition comparison: local=%d rpc=%d MISMATCH (diff=%d)",
				numRewardPartitions, rpcNumPartitions, int64(numRewardPartitions)-int64(rpcNumPartitions))
		}
	}

	// Build partitioned rewards info
	partitionedRewardsInfo := &rewards.PartitionedRewardDistributionInfo{
		TotalStakingRewards:    totalStakingRewards,
		FirstStakingRewardSlot: firstStakingRewardSlot,
		LastStakingRewardSlot:  lastStakingRewardSlot,
		BoundarySlot:           boundarySlot,
		NumRewardPartitions:    numRewardPartitions,
		EligibleCount:          uint64(len(stakingRewards)),
		RewardPartitions:       partitions,
		StakeAccountSnapshots:  stakeAccountSnapshots,
		StakingRewards:         stakingRewards,
	}

	// Aggregate vote rewards from locally computed staking rewards (removes RPC dependency)
	localVoteRewards := rewards.AggregateVoteRewardsFromStakingRewards(partitionedRewardsInfo.StakingRewards)

	// Build diagnostics data BEFORE vote distribution so we always get the file even if distribution fails
	var localStakerTotal, localVoterTotal uint64
	for _, sr := range partitionedRewardsInfo.StakingRewards {
		if sr != nil {
			localStakerTotal += sr.StakerRewards
			localVoterTotal += sr.VoterRewards
		}
	}

	var rpcVoteTotal uint64
	var rpcVoteCount int
	for _, r := range block.Rewards {
		if string(r.RewardType) == "Voting" && r.Lamports > 0 {
			rpcVoteTotal += uint64(r.Lamports)
			rpcVoteCount++
		}
	}

	// Calculate stake cache breakdown
	var activeStake, activatingStake, deactivatingStake, deactivatedStake uint64
	prevEpoch := epoch - 1
	for _, delegation := range global.StakeCache() {
		if delegation != nil {
			isActivating := delegation.ActivationEpoch >= prevEpoch
			isDeactivating := delegation.DeactivationEpoch != ^uint64(0) && delegation.DeactivationEpoch <= prevEpoch
			isDeactivated := delegation.DeactivationEpoch != ^uint64(0) && delegation.DeactivationEpoch < prevEpoch

			if isDeactivated {
				deactivatedStake += delegation.StakeLamports
			} else if isDeactivating {
				deactivatingStake += delegation.StakeLamports
			} else if isActivating {
				activatingStake += delegation.StakeLamports
			} else {
				activeStake += delegation.StakeLamports
			}
		}
	}

	// Get reward pool inputs for debugging total rewards mismatch
	rewardPoolInputs := rewards.GetRewardPoolInputs(epochSchedule, &epochCtx.Inflation, epoch, epoch-1, epochCtx.SlotsPerYear, f)


	// Build and write diagnostics (RPC data kept only for comparison/debugging)
	diag := &rewards.EpochBoundaryDiagnostics{
		PrevEpoch:           epoch - 1,
		NewEpoch:            epoch,
		Slot:                slot,
		LocalPartitions:     partitionedRewardsInfo.NumRewardPartitions,
		RpcPartitions:       block.NumRewardPartitions,
		EligibleCount:       partitionedRewardsInfo.EligibleCount,
		TotalPoints:         points,
		TotalStakingRewards: partitionedRewardsInfo.TotalStakingRewards,
		LocalStakerTotal:    localStakerTotal,
		LocalVoterTotal:     localVoterTotal,
		RpcVoteTotal:        rpcVoteTotal,
		// Reward pool inputs
		Capitalization:           epochCtx.Capitalization,
		SlotInYear:               rewardPoolInputs.SlotInYear,
		ValidatorRate:            rewardPoolInputs.ValidatorRate,
		PrevEpochDurationInYears: rewardPoolInputs.PrevEpochDurationInYears,
		SlotsPerYear:             rewardPoolInputs.SlotsPerYear,
		// Stake cache stats
		StakeCacheSize:      global.StakeCacheSize(),
		ActiveStake:         activeStake,
		ActivatingStake:     activatingStake,
		DeactivatingStake:   deactivatingStake,
		DeactivatedStake:    deactivatedStake,
		LocalVoteAccounts:   len(localVoteRewards),
		RpcVoteAccounts:     rpcVoteCount,
		PointsPerStake:      pointsPerStakeAcct,
		StakingRewards:      partitionedRewardsInfo.StakingRewards,
		RpcRewards:          block.Rewards,
	}

	// Add leader schedule validation info if available
	if transitionCtx != nil {
		diag.LeaderScheduleMatched = transitionCtx.LeaderScheduleMatched
		diag.LocalScheduleHash = transitionCtx.LocalScheduleHash
		diag.RpcScheduleHash = transitionCtx.RpcScheduleHash
		diag.LocalValidatorCount = transitionCtx.LocalValidatorCount
		diag.LocalScheduleStake = transitionCtx.LocalTotalStake
	}

	// Fix RpcPartitions if it's MaxUint64 (means not available)
	if diag.RpcPartitions == ^uint64(0) {
		diag.RpcPartitions = 0
	}

	diagPath := rewards.WriteEpochBoundaryDiagnostics(diag)
	rewards.LogEpochBoundarySummary(diag, diagPath)

	// DEBUG MODE: Exit after diagnostics without committing any changes.
	// This allows inspecting the diagnostic file and restarting from a clean state.
	// Set to false when ready to actually process the epoch boundary.
	const epochBoundaryDebugMode = false
	if epochBoundaryDebugMode {
		mlog.Log.Infof("")
		mlog.Log.Infof("================================================================================")
		mlog.Log.Infof("EPOCH BOUNDARY DEBUG MODE - EXITING WITHOUT COMMITTING CHANGES")
		mlog.Log.Infof("================================================================================")
		mlog.Log.Infof("  Diagnostics written to: %s", diagPath)
		mlog.Log.Infof("  Local partitions:       %d", partitionedRewardsInfo.NumRewardPartitions)
		mlog.Log.Infof("  RPC partitions:         %d", block.NumRewardPartitions)
		mlog.Log.Infof("  Eligible stake accts:   %d", partitionedRewardsInfo.EligibleCount)
		mlog.Log.Infof("  Total points:           %s", points.String())
		mlog.Log.Infof("  Total staking rewards:  %d lamports (%.4f SOL)", totalStakingRewards, float64(totalStakingRewards)/1e9)
		mlog.Log.Infof("  Local staker rewards:   %d lamports", localStakerTotal)
		mlog.Log.Infof("  Local voter rewards:    %d lamports", localVoterTotal)
		mlog.Log.Infof("  Vote accounts to pay:   %d", len(localVoteRewards))
		mlog.Log.Infof("")
		mlog.Log.Infof("  NO CHANGES COMMITTED TO ACCOUNTSDB")
		mlog.Log.Infof("  You can safely restart from the same snapshot slot.")
		mlog.Log.Infof("  Set epochBoundaryDebugMode=false to proceed with actual processing.")
		mlog.Log.Infof("================================================================================")
		return nil, nil, nil, fmt.Errorf("epoch boundary debug mode: exiting after diagnostics (no changes committed)")
	}

	// Distribute vote rewards using locally computed values
	updatedAccts, parentUpdatedAccts, voteRewardsDistributed, err := rewards.DistributeVotingRewardsLocal(acctsDb, localVoteRewards, slot)
	if err != nil {
		mlog.Log.Errorf("vote rewards distribution failed after diagnostics written to: %s", diagPath)
		return nil, nil, nil, fmt.Errorf("vote rewards distribution failed: %w", err)
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
		TotalRewards: totalStakingRewards, DistributedRewards: voteRewardsDistributed, TotalPoints: points, Active: true}

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

	// Save boundary vote cache for resume support.
	// If Mithril crashes during rewards distribution and resumes later, the vote cache
	// may have post-boundary commission values. This persisted cache ensures we use
	// the exact commission rates from the boundary slot for correct reward calculations.
	if err := global.SaveBoundaryVoteCache(acctsDb.AcctsDir, boundarySlot, epoch); err != nil {
		mlog.Log.Warnf("failed to save boundary vote cache: %v (continuing anyway)", err)
	} else {
		mlog.Log.Infof("saved boundary vote cache: slot=%d epoch=%d entries=%d",
			boundarySlot, epoch, len(global.VoteCache()))
	}

	numStakeAccounts := uint64(len(global.StakeCache()))
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

	// VOTE CACHE RESTORATION:
	// Load the boundary vote cache saved when rewards distribution started.
	// This ensures we use the exact commission rates from the boundary slot,
	// not post-boundary values that may have changed during the crash/resume window.
	//
	// The maxEpoch filter handles epoch_credits correctly (filters to epochs <= rewardedEpoch),
	// but commission has no such filter - it's read directly from the vote state.
	// Loading the boundary cache ensures commission values are correct.
	loadedVotes, loadedEpoch, err := global.LoadBoundaryVoteCache(acctsDb.AcctsDir, boundarySlot)
	if err != nil {
		mlog.Log.Warnf("rewards resume: failed to load boundary vote cache: %v (using current vote cache with maxEpoch filter)", err)
		mlog.Log.Infof("rewards resume: using existing vote cache with maxEpoch=%d filter (non-historical read protection)", epoch-1)
	} else if loadedVotes == 0 {
		// No boundary vote cache file exists - this is expected for old state or first time
		mlog.Log.Infof("rewards resume: no boundary vote cache found (using current vote cache with maxEpoch=%d filter)", epoch-1)
	} else {
		mlog.Log.Infof("rewards resume: loaded boundary vote cache with %d entries from epoch %d", loadedVotes, loadedEpoch)
	}

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

	// DEBUG: Log EpochRewards state as read from AccountsDB
	mlog.Log.Debugf("distribution slot=%d: read EpochRewards - start_height=%d distributed=%d total=%d active=%v",
		currentSlot, epochRewards.DistributionStartingBlockHeight, epochRewards.DistributedRewards,
		epochRewards.TotalRewards, epochRewards.Active)

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

		// Clean up boundary vote cache (rewards distribution complete via early exit)
		if err := global.DeleteBoundaryVoteCache(acctsDb.AcctsDir); err != nil {
			mlog.Log.Warnf("failed to delete boundary vote cache: %v", err)
		}

		return nil, nil, nil
	}

	partitionSize := partitionedEpochRewardsInfo.RewardPartitions.Partition(partitionIdx).NumPubkeys()
	// Use cached stake account snapshots instead of reading from AccountsDB.
	// This ensures we use the exact state captured during refresh, avoiding issues
	// with GetAccount returning current state instead of boundary-slot state.
	distributedAccts, parentDistributedAccts, distributedLamports, burnedLamports, err := rewards.DistributeStakingRewardsForPartition(
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

	// Log burned lamports for diagnostics (matches Firedancer/Agave behavior)
	if burnedLamports > 0 {
		mlog.Log.Warnf("partition %d: burned %d lamports from failed distributions", partitionIdx+1, burnedLamports)
	}

	// Update EpochRewards with BOTH distributed and burned lamports.
	// This matches Firedancer/Agave behavior where failed distributions still count toward progress.
	epochRewards.Distribute(distributedLamports + burnedLamports)

	// Log partition progress at INFO level for diagnostics
	mlog.Log.Infof("rewards partition %d/%d: slot=%d stake_accts=%d lamports=%d cumulative=%d",
		partitionIdx+1, partitionedEpochRewardsInfo.NumRewardPartitions,
		currentSlot, partitionSize, distributedLamports, epochRewards.DistributedRewards)

	// DEBUG: Log EpochRewards sysvar state for comparison with RPC
	mlog.Log.Debugf("  EpochRewards: start_height=%d num_partitions=%d total_rewards=%d distributed=%d active=%v",
		epochRewards.DistributionStartingBlockHeight, epochRewards.NumPartitions,
		epochRewards.TotalRewards, epochRewards.DistributedRewards, epochRewards.Active)

	// Stop distribution when we've processed the last partition (partition-based, not slot-based)
	if partitionIdx >= partitionedEpochRewardsInfo.NumRewardPartitions-1 {
		epochRewards.Active = false
		mlog.Log.Infof("rewards distribution complete: slot=%d block_height=%d",
			currentSlot, currentBlockHeight)
		mlog.Log.Infof("  partition_idx=%d num_partitions=%d total_distributed=%d total_rewards=%d",
			partitionIdx+1, partitionedEpochRewardsInfo.NumRewardPartitions,
			epochRewards.DistributedRewards, partitionedEpochRewardsInfo.TotalStakingRewards)

		// Clean up boundary vote cache now that rewards distribution is complete.
		// This file is only needed during the rewards period for resume support.
		if err := global.DeleteBoundaryVoteCache(acctsDb.AcctsDir); err != nil {
			mlog.Log.Warnf("failed to delete boundary vote cache: %v", err)
		} else {
			mlog.Log.Infof("deleted boundary vote cache (rewards distribution complete)")
		}
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
