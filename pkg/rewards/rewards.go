package rewards

import (
	"fmt"
	"io"
	"math"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"

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
	"github.com/panjf2000/ants/v2"
)

const (
	RewardTypeFee     string = "Fee"
	RewardTypeRent    string = "Rent"
	RewardTypeVoting  string = "Voting"
	RewardTypeStaking string = "Staking"
)

type PartitionedRewardDistributionInfo struct {
	TotalStakingRewards          uint64
	FirstStakingRewardSlot       uint64
	NumRewardPartitionsRemaining uint64
	SpoolDir                     string // Base directory for per-partition spool files
	SpoolSlot                    uint64 // Slot for spool file naming
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
	totalStakingRewards := CalculatePreviousEpochInflationRewards(epochSchedule, inflation, prevEpochCapitalization, epoch, prevEpoch, slotsPerYear, f)
	return &PartitionedRewardDistributionInfo{TotalStakingRewards: totalStakingRewards, FirstStakingRewardSlot: firstSlotInEpoch + 1}
}

type idxAndRewardNew struct {
	idx     int
	reward  uint64
	voterPk solana.PublicKey
}

func DistributeVotingRewards(acctsDb *accountsdb.AccountsDb, validatorRewards map[solana.PublicKey]*atomic.Uint64, slot uint64) ([]*accounts.Account, []*accounts.Account, uint64) {
	var totalVotingRewards atomic.Uint64

	updatedAccts := make([]*accounts.Account, len(validatorRewards))
	parentUpdatedAccts := make([]*accounts.Account, len(validatorRewards))

	var wg sync.WaitGroup

	size := runtime.GOMAXPROCS(0) * 2
	workerPool, _ := ants.NewPoolWithFunc(size, func(i interface{}) {
		defer wg.Done()

		r := i.(idxAndRewardNew)
		reward := r.reward
		voterPk := r.voterPk
		idx := r.idx

		voteAcct, err := acctsDb.GetAccount(slot, voterPk)
		if err != nil {
			return
		}
		parentUpdatedAccts[idx] = voteAcct.Clone()

		voteAcct.Lamports, err = safemath.CheckedAddU64(voteAcct.Lamports, uint64(reward))
		if err != nil {
			panic(fmt.Sprintf("overflow in voting rewards distribution in slot %d to acct %s: %s", slot, voterPk, err))
		}

		updatedAccts[idx] = voteAcct

		new := totalVotingRewards.Add(uint64(reward))
		if new < uint64(reward) {
			panic(fmt.Sprintf("overflow in accumulating voting rewards in slot %d", slot))
		}
	})

	var idx int
	for votePk, reward := range validatorRewards {
		r := idxAndRewardNew{idx: idx, reward: reward.Load(), voterPk: votePk}
		wg.Add(1)
		workerPool.Invoke(r)
		idx++
	}

	wg.Wait()
	workerPool.Release()

	err := acctsDb.StoreAccounts(updatedAccts, slot, nil)
	if err != nil {
		panic(fmt.Sprintf("error updating accounts for voting rewards in slot %d: %s", slot, err))
	}

	return updatedAccts, parentUpdatedAccts, totalVotingRewards.Load()
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

	return partitionIdx64
}

type PointValue struct {
	Rewards uint64
	Points  wide.Uint128
}

type CalculatedStakeRewards struct {
	StakerRewards      uint64
	VoterRewards       uint64
	VoterPubkey        solana.PublicKey
	NewCreditsObserved uint64
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
		VoterRewards: splitResult.VoterPortion, NewCreditsObserved: stakePointsResult.NewCreditsObserved,
		VoterPubkey: delegation.VoterPubkey}

	//mlog.Log.Debugf("returning CalculatedStakeRewards for %s. %+v", stakePubkey, result)

	return result
}

type CommissionSplit struct {
	VoterPortion  uint64
	StakerPortion uint64
	IsSplit       bool
}

func mulDivPercent(on uint64, pct uint64) uint64 {
	// pct must be 0..100
	q := on / 100
	r := on % 100
	return q*pct + (r*pct)/100
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
		mine := mulDivPercent(rewards, commissionRate)
		theirs := mulDivPercent(rewards, 100-commissionRate)

		result.VoterPortion = mine
		result.StakerPortion = theirs
		result.IsSplit = true
	}

	return result
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

func CalculateNumRewardPartitions(numStakingRewards uint64) uint64 {
	numEligible := numStakingRewards
	target := uint64(4096)
	slotsInEpoch := uint64(432000)
	unclamped := (numEligible + (target - 1)) / target
	cap := slotsInEpoch / 10
	numRewardPartitions := min(unclamped, cap)

	return numRewardPartitions
}

// StreamingRewardsResult holds the results from streaming rewards calculation.
type StreamingRewardsResult struct {
	SpoolDir         string // Base directory for per-partition spool files
	SpoolSlot        uint64 // Slot for spool file naming
	TotalPoints      wide.Uint128
	ValidatorRewards map[solana.PublicKey]*atomic.Uint64
	NumStakeRewards  uint64
	NumPartitions    uint64
}

// spoolWriteRequest is sent to the single-writer goroutine for spool writes.
type spoolWriteRequest struct {
	record *SpoolRecord
}

// CalculateRewardsStreaming performs a three-phase streaming calculation of stake rewards.
// Phase 1: Stream stakes to calculate total points (no caching - flat RAM)
// Phase 2: Recompute points + calculate rewards + write to TEMP spool (no partition yet)
// Phase 3: Read temp spool, calculate partitions from ACTUAL count, write per-partition spools
// Uses channel-based single writer to capture spool write errors.
//
// CRITICAL FIX: numPartitions must be calculated from ACTUAL reward count (accounts where
// CalculateStakeRewardsForAcct returns non-nil), not from "eligible" count. The old code
// used eligibleCount which included accounts that later returned nil rewards, causing
// partition count mismatch with Solana and divergence.
func CalculateRewardsStreaming(
	acctsDb *accountsdb.AccountsDb,
	slot uint64,
	stakeHistory *sealevel.SysvarStakeHistory,
	newWarmupCooldownRateEpoch *uint64,
	voteCache map[solana.PublicKey]*sealevel.VoteStateVersions,
	pointValue PointValue,
	rewardedEpoch uint64,
	blockhash [32]byte,
	slotCtx *sealevel.SlotCtx,
	f *features.Features,
) (*StreamingRewardsResult, error) {
	minimum := minimumStakeDelegation(slotCtx)
	spoolDir := filepath.Join(acctsDb.AcctsDir, "..")

	mlog.Log.Infof("rewards: voteCache has %d entries, minimum stake delegation=%d", len(voteCache), minimum)

	// ==================== PHASE 1: Calculate total points ====================
	var totalPoints wide.Uint128
	var totalPointsMu sync.Mutex
	var phase1StakeCount atomic.Int64
	var phase1BelowMinimum atomic.Int64
	var phase1NoVoteState atomic.Int64
	var phase1ZeroPoints atomic.Int64
	var phase1TotalStakeLamports atomic.Uint64

	_, err := global.StreamStakeAccounts(acctsDb, slot,
		func(pk solana.PublicKey, delegation *sealevel.Delegation, creditsObs uint64) {
			if delegation.StakeLamports < minimum {
				phase1BelowMinimum.Add(1)
				return
			}

			voterPk := delegation.VoterPubkey
			voteState := voteCache[voterPk]
			if voteState == nil {
				phase1NoVoteState.Add(1)
				return
			}

			// Calculate points for this stake account
			delegWithCredits := *delegation
			delegWithCredits.CreditsObserved = creditsObs
			pcs := calculateStakePointsAndCredits(pk, stakeHistory, &delegWithCredits, voteState, newWarmupCooldownRateEpoch)

			// Track zero-point stakes
			zero128 := wide.Uint128FromUint64(0)
			if pcs.Points.Eq(zero128) {
				phase1ZeroPoints.Add(1)
			}

			// Accumulate total points
			totalPointsMu.Lock()
			totalPoints = totalPoints.Add(pcs.Points)
			totalPointsMu.Unlock()
			phase1StakeCount.Add(1)
			phase1TotalStakeLamports.Add(delegation.StakeLamports)
		})
	if err != nil {
		return nil, fmt.Errorf("phase 1 streaming stakes for points: %w", err)
	}

	mlog.Log.Infof("rewards phase 1: %d stakes (belowMin=%d, noVote=%d, zeroPoints=%d), totalStakeLamports=%d, totalPoints=%s, totalRewards=%d",
		phase1StakeCount.Load(), phase1BelowMinimum.Load(), phase1NoVoteState.Load(), phase1ZeroPoints.Load(),
		phase1TotalStakeLamports.Load(), totalPoints.String(), pointValue.Rewards)

	// Create point value with calculated total points
	pv := PointValue{Rewards: pointValue.Rewards, Points: totalPoints}

	// ==================== PHASE 2: Calculate rewards, write to TEMP spool ====================
	// Write to temp spool WITHOUT partition index - we don't know numPartitions yet
	tempWriter, err := NewTempSpoolWriter(spoolDir, slot)
	if err != nil {
		return nil, fmt.Errorf("creating temp spool: %w", err)
	}
	tempPath := tempWriter.Path()

	// Track validator rewards (for voting rewards distribution)
	validatorRewards := make(map[solana.PublicKey]*atomic.Uint64)
	var validatorRewardsMu sync.Mutex

	// Phase 2 stats for debugging
	var phase2SkippedZeroPoints atomic.Int64
	var phase2SkippedNilReward atomic.Int64
	var phase2TotalStakerRewards atomic.Uint64

	// Channel-based single writer pattern to capture write errors
	writeChan := make(chan spoolWriteRequest, 10000)
	var writeErr atomic.Pointer[error]
	var writerWg sync.WaitGroup
	writerWg.Add(1)

	go func() {
		defer writerWg.Done()
		for req := range writeChan {
			if writeErr.Load() != nil {
				continue // already failed, drain channel
			}
			if err := tempWriter.WriteRecord(req.record); err != nil {
				writeErr.Store(&err)
			}
		}
	}()

	_, err = global.StreamStakeAccounts(acctsDb, slot,
		func(pk solana.PublicKey, delegation *sealevel.Delegation, creditsObs uint64) {
			if delegation.StakeLamports < minimum {
				return
			}

			voterPk := delegation.VoterPubkey
			voteState := voteCache[voterPk]
			if voteState == nil {
				return
			}

			// Recompute points (same as Phase 1 - cheap)
			delegWithCredits := *delegation
			delegWithCredits.CreditsObserved = creditsObs
			pcs := calculateStakePointsAndCredits(pk, stakeHistory, &delegWithCredits, voteState, newWarmupCooldownRateEpoch)

			// Skip if no points and not forced update
			zero128 := wide.Uint128FromUint64(0)
			if pcs.Points.Eq(zero128) && !pcs.ForceCreditsUpdateWithSkippedReward {
				phase2SkippedZeroPoints.Add(1)
				return
			}

			// Calculate rewards using recomputed points
			calculatedRewards := CalculateStakeRewardsForAcct(pk, &pcs, &delegWithCredits, voteState, rewardedEpoch, pv, newWarmupCooldownRateEpoch)
			if calculatedRewards == nil {
				phase2SkippedNilReward.Add(1)
				return // Not counted - this is the key fix!
			}

			phase2TotalStakerRewards.Add(calculatedRewards.StakerRewards)

			// Write to temp spool WITHOUT partition index
			writeChan <- spoolWriteRequest{record: &SpoolRecord{
				StakePubkey:     pk,
				VotePubkey:      delegation.VoterPubkey,
				StakeLamports:   delegation.StakeLamports,
				CreditsObserved: calculatedRewards.NewCreditsObserved,
				RewardLamports:  calculatedRewards.StakerRewards,
				// PartitionIndex NOT set - will be assigned in Phase 3
			}}

			// Track validator rewards
			if calculatedRewards.VoterRewards > 0 {
				validatorRewardsMu.Lock()
				if _, exists := validatorRewards[voterPk]; !exists {
					validatorRewards[voterPk] = &atomic.Uint64{}
				}
				validatorRewards[voterPk].Add(calculatedRewards.VoterRewards)
				validatorRewardsMu.Unlock()
			}
		})

	// Close write channel and wait for writer to finish
	close(writeChan)
	writerWg.Wait()

	// Check for temp spool write errors
	if werr := writeErr.Load(); werr != nil {
		tempWriter.Close()
		CleanupTempSpoolFile(tempPath)
		return nil, fmt.Errorf("temp spool write failed: %w", *werr)
	}

	// Close temp spool and check for errors
	if err := tempWriter.Close(); err != nil {
		CleanupTempSpoolFile(tempPath)
		return nil, fmt.Errorf("temp spool close failed: %w", err)
	}

	if err != nil {
		CleanupTempSpoolFile(tempPath)
		return nil, fmt.Errorf("phase 2 streaming stakes for rewards: %w", err)
	}

	// ==================== Calculate numPartitions from ACTUAL count ====================
	actualRewardCount := uint64(tempWriter.Count())
	numPartitions := CalculateNumRewardPartitions(actualRewardCount)

	// Calculate total validator rewards for debugging
	var totalVotingRewards uint64
	for _, v := range validatorRewards {
		totalVotingRewards += v.Load()
	}
	mlog.Log.Infof("rewards phase 2: %d records (skippedZeroPoints=%d, skippedNilReward=%d), totalStakerRewards=%d, totalVotingRewards=%d, validatorCount=%d, numPartitions=%d",
		actualRewardCount, phase2SkippedZeroPoints.Load(), phase2SkippedNilReward.Load(),
		phase2TotalStakerRewards.Load(), totalVotingRewards, len(validatorRewards), numPartitions)

	// ==================== PHASE 3: Read temp spool, assign partitions, write per-partition spools ====================
	tempReader, err := NewTempSpoolReader(tempPath)
	if err != nil {
		CleanupTempSpoolFile(tempPath)
		return nil, fmt.Errorf("opening temp spool reader: %w", err)
	}

	partitionWriters := NewPartitionedSpoolWriters(spoolDir, slot, numPartitions)

	for {
		rec, err := tempReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			tempReader.Close()
			partitionWriters.Close()
			CleanupTempSpoolFile(tempPath)
			CleanupPartitionedSpoolFiles(spoolDir, slot, numPartitions)
			return nil, fmt.Errorf("reading temp spool: %w", err)
		}

		// Assign partition index using CORRECT numPartitions
		rec.PartitionIndex = uint32(CalculateRewardPartitionForPubkey(rec.StakePubkey, blockhash, numPartitions))

		if err := partitionWriters.WriteRecord(rec); err != nil {
			tempReader.Close()
			partitionWriters.Close()
			CleanupTempSpoolFile(tempPath)
			CleanupPartitionedSpoolFiles(spoolDir, slot, numPartitions)
			return nil, fmt.Errorf("partition spool write: %w", err)
		}
	}

	tempReader.Close()
	CleanupTempSpoolFile(tempPath)

	if err := partitionWriters.Close(); err != nil {
		CleanupPartitionedSpoolFiles(spoolDir, slot, numPartitions)
		return nil, fmt.Errorf("partition spool close failed: %w", err)
	}

	return &StreamingRewardsResult{
		SpoolDir:         spoolDir,
		SpoolSlot:        slot,
		TotalPoints:      totalPoints,
		ValidatorRewards: validatorRewards,
		NumStakeRewards:  actualRewardCount,
		NumPartitions:    numPartitions,
	}, nil
}

// spoolDistributionTask carries context for processing one spool record.
type spoolDistributionTask struct {
	rec         *SpoolRecord
	acctsDb     *accountsdb.AccountsDb
	slot        uint64
	accts       *[]*accounts.Account
	parentAccts *[]*accounts.Account
	mu          *sync.Mutex
	distributed *atomic.Uint64
	firstError  *atomic.Pointer[error]
}

// DistributeStakingRewardsFromSpool reads rewards from a per-partition spool file and distributes them.
// Uses streaming I/O - reads records one at a time to keep RAM flat.
// STRICT MODE: Any account read/unmarshal/marshal failure is fatal - we cannot diverge from consensus.
func DistributeStakingRewardsFromSpool(
	acctsDb *accountsdb.AccountsDb,
	spoolDir string,
	spoolSlot uint64,
	partitionIndex uint32,
	slot uint64,
) ([]*accounts.Account, []*accounts.Account, uint64, error) {
	// Open partition-specific spool file for sequential reading
	reader, err := NewPartitionReader(spoolDir, spoolSlot, partitionIndex)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("opening partition %d reader: %w", partitionIndex, err)
	}
	if reader == nil {
		// No records for this partition
		return nil, nil, 0, nil
	}
	defer reader.Close()

	var distributedLamports atomic.Uint64
	var firstError atomic.Pointer[error]
	var mu sync.Mutex

	// Dynamic slices - append as we process (no pre-allocation with nils)
	var accts []*accounts.Account
	var parentAccts []*accounts.Account

	var wg sync.WaitGroup
	size := runtime.GOMAXPROCS(0) * 8
	workerPool, err := ants.NewPoolWithFunc(size, func(i interface{}) {
		defer wg.Done()

		task := i.(*spoolDistributionTask)

		// Skip if we already have an error
		if task.firstError.Load() != nil {
			return
		}

		stakeAcct, err := task.acctsDb.GetAccount(task.slot, task.rec.StakePubkey)
		if err != nil {
			newErr := fmt.Errorf("GetAccount %s: %w", task.rec.StakePubkey, err)
			task.firstError.CompareAndSwap(nil, &newErr)
			return
		}
		parentAcct := stakeAcct.Clone()

		stakeState, err := sealevel.UnmarshalStakeState(stakeAcct.Data)
		if err != nil {
			newErr := fmt.Errorf("UnmarshalStakeState %s: %w", task.rec.StakePubkey, err)
			task.firstError.CompareAndSwap(nil, &newErr)
			return
		}

		// Apply reward
		stakeState.Stake.Stake.CreditsObserved = task.rec.CreditsObserved
		stakeState.Stake.Stake.Delegation.StakeLamports = safemath.SaturatingAddU64(
			stakeState.Stake.Stake.Delegation.StakeLamports, task.rec.RewardLamports)

		err = sealevel.MarshalStakeStakeInto(stakeState, stakeAcct.Data)
		if err != nil {
			newErr := fmt.Errorf("MarshalStakeStakeInto %s: %w", task.rec.StakePubkey, err)
			task.firstError.CompareAndSwap(nil, &newErr)
			return
		}

		stakeAcct.Lamports = safemath.SaturatingAddU64(stakeAcct.Lamports, task.rec.RewardLamports)
		task.distributed.Add(task.rec.RewardLamports)

		// Append to result slices under lock
		task.mu.Lock()
		*task.accts = append(*task.accts, stakeAcct)
		*task.parentAccts = append(*task.parentAccts, parentAcct)
		task.mu.Unlock()
	})
	if err != nil {
		return nil, nil, 0, fmt.Errorf("creating worker pool: %w", err)
	}
	defer workerPool.Release()

	// Stream records from spool file - one at a time (flat RAM)
	for {
		rec, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, 0, fmt.Errorf("reading partition %d record: %w", partitionIndex, err)
		}

		// Submit to worker pool - check for invoke errors
		wg.Add(1)
		if invokeErr := workerPool.Invoke(&spoolDistributionTask{
			rec:         rec,
			acctsDb:     acctsDb,
			slot:        slot,
			accts:       &accts,
			parentAccts: &parentAccts,
			mu:          &mu,
			distributed: &distributedLamports,
			firstError:  &firstError,
		}); invokeErr != nil {
			wg.Done() // balance the Add since worker won't run
			return nil, nil, 0, fmt.Errorf("worker pool invoke failed: %w", invokeErr)
		}
	}
	wg.Wait()

	// STRICT: Any failure is fatal - we cannot silently skip rewards and diverge from consensus
	if ferr := firstError.Load(); ferr != nil {
		return nil, nil, 0, fmt.Errorf("reward distribution partition %d failed: %w", partitionIndex, *ferr)
	}

	if len(accts) > 0 {
		err = acctsDb.StoreAccounts(accts, slot, nil)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("storing accounts: %w", err)
		}
	}

	return accts, parentAccts, distributedLamports.Load(), nil
}
