package rewards

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/epochstakes"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
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
	SpoolDir                     string
	SpoolSlot                    uint64
}

type InflationRewardVariant struct {
	SlotForRate              uint64
	SlotInYear               float64
	ValidatorRate            float64
	PrevEpochDurationInYears float64
	TotalRewards             uint64
}

type CalculatedStakePoints struct {
	Points                              wide.Uint128
	NewCreditsObserved                  uint64
	ForceCreditsUpdateWithSkippedReward bool
}

type phase1ReasonStats struct {
	count         atomic.Int64
	stakeLamports atomic.Uint64
	pointsMu      sync.Mutex
	points        wide.Uint128
}

func (s *phase1ReasonStats) Add(stakeLamports uint64, points wide.Uint128) {
	s.count.Add(1)
	s.stakeLamports.Add(stakeLamports)
	s.pointsMu.Lock()
	s.points = s.points.Add(points)
	s.pointsMu.Unlock()
}

func (s *phase1ReasonStats) Snapshot() (int64, uint64, wide.Uint128) {
	s.pointsMu.Lock()
	points := s.points
	s.pointsMu.Unlock()
	return s.count.Load(), s.stakeLamports.Load(), points
}

type phase1VoteSkipAggregate struct {
	VotePubkey    solana.PublicKey
	DelegationCnt int64
	StakeLamports uint64
}

type phase2VoteRewardAggregate struct {
	VotePubkey             solana.PublicKey
	RewardingDelegations   int64
	RewardingStakeLamports uint64
	TotalPoints            wide.Uint128
	TotalRawRewards        uint64
	TotalVoterRewards      uint64
	TotalStakerRewards     uint64
	SplitDust              uint64
}

func addPhase1VoteSkipAggregate(m map[solana.PublicKey]*phase1VoteSkipAggregate, votePk solana.PublicKey, stakeLamports uint64) {
	entry := m[votePk]
	if entry == nil {
		entry = &phase1VoteSkipAggregate{VotePubkey: votePk}
		m[votePk] = entry
	}
	entry.DelegationCnt++
	entry.StakeLamports += stakeLamports
}

func sortedPhase1VoteSkipAggregates(m map[solana.PublicKey]*phase1VoteSkipAggregate) []phase1VoteSkipAggregate {
	out := make([]phase1VoteSkipAggregate, 0, len(m))
	for _, entry := range m {
		out = append(out, *entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StakeLamports != out[j].StakeLamports {
			return out[i].StakeLamports > out[j].StakeLamports
		}
		if out[i].DelegationCnt != out[j].DelegationCnt {
			return out[i].DelegationCnt > out[j].DelegationCnt
		}
		return out[i].VotePubkey.String() < out[j].VotePubkey.String()
	})
	return out
}

func addPhase2VoteRewardAggregate(m map[solana.PublicKey]*phase2VoteRewardAggregate, votePk solana.PublicKey, stakeLamports uint64, points wide.Uint128, rawRewards uint64, voterRewards uint64, stakerRewards uint64) {
	entry := m[votePk]
	if entry == nil {
		entry = &phase2VoteRewardAggregate{VotePubkey: votePk}
		m[votePk] = entry
	}
	entry.RewardingDelegations++
	entry.RewardingStakeLamports += stakeLamports
	entry.TotalPoints = entry.TotalPoints.Add(points)
	entry.TotalRawRewards += rawRewards
	entry.TotalVoterRewards += voterRewards
	entry.TotalStakerRewards += stakerRewards
	entry.SplitDust += rawRewards - voterRewards - stakerRewards
}

func sortedPhase2VoteRewardAggregates(m map[solana.PublicKey]*phase2VoteRewardAggregate) []phase2VoteRewardAggregate {
	out := make([]phase2VoteRewardAggregate, 0, len(m))
	for _, entry := range m {
		out = append(out, *entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalVoterRewards != out[j].TotalVoterRewards {
			return out[i].TotalVoterRewards > out[j].TotalVoterRewards
		}
		if out[i].TotalRawRewards != out[j].TotalRawRewards {
			return out[i].TotalRawRewards > out[j].TotalRawRewards
		}
		if out[i].RewardingDelegations != out[j].RewardingDelegations {
			return out[i].RewardingDelegations > out[j].RewardingDelegations
		}
		return out[i].VotePubkey.String() < out[j].VotePubkey.String()
	})
	return out
}

func dumpPhase2VoteRewardSummaryCSV(slot uint64, voteAggregates map[solana.PublicKey]*phase2VoteRewardAggregate) {
	logDir := mlog.GetLogDir()
	if logDir == "" || len(voteAggregates) == 0 {
		return
	}

	sorted := sortedPhase2VoteRewardAggregates(voteAggregates)
	path := filepath.Join(logDir, fmt.Sprintf("rewards_phase2_vote_summary_slot%d.csv", slot))

	file, err := os.Create(path)
	if err != nil {
		mlog.Log.Warnf("Rewards Phase 2: unable to create vote summary csv %s: %v", path, err)
		return
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	if err := writer.Write([]string{
		"vote_pubkey",
		"rewarding_delegations",
		"rewarding_stake_lamports",
		"total_points",
		"total_raw_rewards",
		"total_voter_rewards",
		"total_staker_rewards",
		"split_dust",
	}); err != nil {
		mlog.Log.Warnf("Rewards Phase 2: unable to write csv header %s: %v", path, err)
		return
	}

	for _, entry := range sorted {
		record := []string{
			entry.VotePubkey.String(),
			fmt.Sprintf("%d", entry.RewardingDelegations),
			fmt.Sprintf("%d", entry.RewardingStakeLamports),
			entry.TotalPoints.String(),
			fmt.Sprintf("%d", entry.TotalRawRewards),
			fmt.Sprintf("%d", entry.TotalVoterRewards),
			fmt.Sprintf("%d", entry.TotalStakerRewards),
			fmt.Sprintf("%d", entry.SplitDust),
		}
		if err := writer.Write(record); err != nil {
			mlog.Log.Warnf("Rewards Phase 2: unable to write csv row %s: %v", path, err)
			return
		}
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		mlog.Log.Warnf("Rewards Phase 2: unable to flush csv %s: %v", path, err)
		return
	}

	mlog.Log.FileOnlyf("Rewards Phase 2 vote summary dumped to: %s (%d entries)", path, len(sorted))
}

func SlotInYearForInflation(epochSchedule *sealevel.SysvarEpochSchedule, slotsPerYear float64, epoch uint64, f *features.Features) float64 {
	numSlots := GetInflationNumSlots(epochSchedule, epoch, f)
	return float64(numSlots) / slotsPerYear
}

func InflationStartSlot(epochSchedule *sealevel.SysvarEpochSchedule, f *features.Features) uint64 {
	inflationActivationSlot := GetInflationStartSlot(f)
	return epochSchedule.FirstSlotInEpoch(safemath.SaturatingSubU64(epochSchedule.GetEpoch(inflationActivationSlot), 1))
}

func SlotInYearForInflationSlot(epochSchedule *sealevel.SysvarEpochSchedule, slotsPerYear float64, slot uint64, f *features.Features) float64 {
	inflationStartSlot := InflationStartSlot(epochSchedule, f)
	numSlots := safemath.SaturatingSubU64(slot, inflationStartSlot)
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
	slotForRate := epochSchedule.FirstSlotInEpoch(epoch)
	return CalculatePreviousEpochInflationRewardsAtSlot(epochSchedule, inflation, prevEpochCapitalization, prevEpoch, slotForRate, slotsPerYear, f).TotalRewards
}

func CalculatePreviousEpochInflationRewardsAtSlot(epochSchedule *sealevel.SysvarEpochSchedule, inflation *Inflation, prevEpochCapitalization, prevEpoch uint64, slotForRate uint64, slotsPerYear float64, f *features.Features) InflationRewardVariant {
	slotInYear := SlotInYearForInflationSlot(epochSchedule, slotsPerYear, slotForRate, f)
	validatorRate := inflation.Validator(slotInYear)
	prevEpochDurationInYears := float64(epochSchedule.SlotsInEpoch(prevEpoch)) / slotsPerYear

	validatorRewards := validatorRate * float64(prevEpochCapitalization) * prevEpochDurationInYears
	return InflationRewardVariant{
		SlotForRate:              slotForRate,
		SlotInYear:               slotInYear,
		ValidatorRate:            validatorRate,
		PrevEpochDurationInYears: prevEpochDurationInYears,
		TotalRewards:             uint64(validatorRewards),
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

// DeterminePartitionedStakingRewardsInfo calculates the total staking rewards for the epoch.
func DeterminePartitionedStakingRewardsInfo(epochSchedule *sealevel.SysvarEpochSchedule, inflation *Inflation, prevEpochCapitalization uint64, epoch uint64, prevEpoch uint64, slot uint64, slotsPerYear float64, f *features.Features) *PartitionedRewardDistributionInfo {
	firstSlotInEpoch := epochSchedule.FirstSlotInEpoch(epoch)
	firstStakingRewardSlot := safemath.SaturatingAddU64(firstSlotInEpoch, 1)
	currentVariant := CalculatePreviousEpochInflationRewardsAtSlot(epochSchedule, inflation, prevEpochCapitalization, prevEpoch, slot, slotsPerYear, f)
	totalStakingRewards := currentVariant.TotalRewards

	var minusVariant InflationRewardVariant
	if slot > 0 {
		minusVariant = CalculatePreviousEpochInflationRewardsAtSlot(epochSchedule, inflation, prevEpochCapitalization, prevEpoch, slot-1, slotsPerYear, f)
	} else {
		minusVariant = currentVariant
	}
	plusVariant := CalculatePreviousEpochInflationRewardsAtSlot(epochSchedule, inflation, prevEpochCapitalization, prevEpoch, slot+1, slotsPerYear, f)

	mlog.Log.FileOnlyf("Rewards pool debug: epoch=%d prev_epoch=%d boundary_slot=%d first_slot_in_epoch=%d first_staking_reward_slot=%d capitalization=%d slots_per_year=%.6f",
		epoch, prevEpoch, slot, firstSlotInEpoch, firstStakingRewardSlot, prevEpochCapitalization, slotsPerYear)
	mlog.Log.FileOnlyf("  inflation_start_slot=%d prev_epoch_slots=%d prev_epoch_duration_years=%.12f",
		InflationStartSlot(epochSchedule, f), epochSchedule.SlotsInEpoch(prevEpoch), currentVariant.PrevEpochDurationInYears)
	mlog.Log.FileOnlyf("  slot-1: slot_for_rate=%d slot_in_year=%.12f validator_rate=%.12f total_rewards=%d delta_vs_slot=%d",
		minusVariant.SlotForRate, minusVariant.SlotInYear, minusVariant.ValidatorRate, minusVariant.TotalRewards, int64(minusVariant.TotalRewards)-int64(currentVariant.TotalRewards))
	mlog.Log.FileOnlyf("  slot:   slot_for_rate=%d slot_in_year=%.12f validator_rate=%.12f total_rewards=%d delta_vs_slot=%d",
		currentVariant.SlotForRate, currentVariant.SlotInYear, currentVariant.ValidatorRate, currentVariant.TotalRewards, int64(currentVariant.TotalRewards)-int64(currentVariant.TotalRewards))
	mlog.Log.FileOnlyf("  slot+1: slot_for_rate=%d slot_in_year=%.12f validator_rate=%.12f total_rewards=%d delta_vs_slot=%d",
		plusVariant.SlotForRate, plusVariant.SlotInYear, plusVariant.ValidatorRate, plusVariant.TotalRewards, int64(plusVariant.TotalRewards)-int64(currentVariant.TotalRewards))
	return &PartitionedRewardDistributionInfo{TotalStakingRewards: totalStakingRewards, FirstStakingRewardSlot: firstStakingRewardSlot}
}

type idxAndRewardNew struct {
	idx     int
	reward  uint64
	voterPk solana.PublicKey
}

func DistributeVotingRewards(acctsDb *accountsdb.AccountsDb, validatorRewards map[solana.PublicKey]*atomic.Uint64, rewardVoteAccts map[solana.PublicKey]*epochstakes.VoteAccount, slot uint64) ([]*accounts.Account, []*accounts.Account, uint64) {
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
			cachedVoteAcct := rewardVoteAccts[voterPk]
			if cachedVoteAcct == nil {
				return
			}
			voteAcct = cachedVoteAcct.ToAccount(voterPk, slot)
			if voteAcct == nil {
				return
			}
		}
		parentUpdatedAccts[idx] = voteAcct.Clone()

		voteAcct.Lamports, err = safemath.CheckedAddU64(voteAcct.Lamports, uint64(reward))
		if err != nil {
			panic(fmt.Sprintf("overflow in voting rewards distribution in slot %d to acct %s: %s", slot, voterPk, err))
		}
		if cachedVoteAcct := rewardVoteAccts[voterPk]; cachedVoteAcct != nil {
			cachedVoteAcct.Lamports = voteAcct.Lamports
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

func rewardVoteState(votePubkey solana.PublicKey, rewardVoteAccts map[solana.PublicKey]*epochstakes.VoteAccount, liveVoteCache map[solana.PublicKey]*sealevel.VoteStateVersions) *sealevel.VoteStateVersions {
	if voteAcct := rewardVoteAccts[votePubkey]; voteAcct != nil {
		if voteState, err := voteAcct.VoteState(); err == nil && voteState != nil {
			return voteState
		}
	}
	return liveVoteCache[votePubkey]
}

func DistributeStakingRewardsFromSpool(acctsDb *accountsdb.AccountsDb, spoolDir string, spoolSlot uint64, partitionIdx uint64, currentSlot uint64) ([]*accounts.Account, []*accounts.Account, uint64, uint64) {
	reader, err := NewPartitionReader(spoolDir, spoolSlot, uint32(partitionIdx))
	if err != nil {
		panic(fmt.Sprintf("unable to open partition %d spool for distribution: %s", partitionIdx, err))
	}
	if reader == nil {
		return nil, nil, 0, 0
	}
	defer reader.Close()

	var records []*SpoolRecord
	for {
		rec, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			panic(fmt.Sprintf("unable to read partition %d spool record: %s", partitionIdx, err))
		}
		records = append(records, rec)
	}

	if len(records) == 0 {
		return nil, nil, 0, 0
	}

	accts := make([]*accounts.Account, len(records))
	parentAccts := make([]*accounts.Account, len(records))
	var distributedLamports atomic.Uint64
	var burnedLamports atomic.Uint64

	var wg sync.WaitGroup
	workerPool, _ := ants.NewPoolWithFunc(runtime.GOMAXPROCS(0)*8, func(i interface{}) {
		defer wg.Done()
		idx := i.(int)
		rec := records[idx]

		// Per-record failures burn rewards instead of panicking, matching Agave
		// distribution.rs:282-294 and Firedancer fd_rewards.c:958-968.
		// Burned rewards advance the EpochRewards sysvar but do NOT increase
		// capitalization.

		stakeAcct, err := acctsDb.GetAccount(currentSlot, rec.StakePubkey)
		if err != nil {
			if errors.Is(err, accountsdb.ErrNoAccount) {
				// AccountNotFound — matches Agave DistributionError::AccountNotFound
				mlog.Log.Warnf("spool distribution: account %s not found in slot %d, %d lamports burned", rec.StakePubkey, currentSlot, rec.RewardLamports)
				burnedLamports.Add(rec.RewardLamports)
				return
			}
			// Storage/IO error — hard fail, do not burn
			panic(fmt.Sprintf("spool distribution: GetAccount failed for %s in slot %d: %v", rec.StakePubkey, currentSlot, err))
		}
		parentAccts[idx] = stakeAcct.Clone()

		stakeState, err := sealevel.UnmarshalStakeState(stakeAcct.Data)
		if err != nil {
			// Stake state decode failure — matches FD fd_stake_get_state != 0
			mlog.Log.Warnf("spool distribution: stake state decode failed for %s, %d lamports burned: %v", rec.StakePubkey, rec.RewardLamports, err)
			burnedLamports.Add(rec.RewardLamports)
			return
		}

		if stakeState.Status != sealevel.StakeStateV2StatusStake {
			// Non-stake state — matches FD !fd_stake_state_v2_is_stake
			mlog.Log.Warnf("spool distribution: account %s not in Stake state (status=%d), %d lamports burned", rec.StakePubkey, stakeState.Status, rec.RewardLamports)
			burnedLamports.Add(rec.RewardLamports)
			return
		}

		stakeState.Stake.Stake.CreditsObserved = rec.CreditsObserved
		stakeState.Stake.Stake.Delegation.StakeLamports = safemath.SaturatingAddU64(stakeState.Stake.Stake.Delegation.StakeLamports, rec.RewardLamports)

		err = sealevel.MarshalStakeStakeInto(stakeState, stakeAcct.Data)
		if err != nil {
			// Re-encode failure — matches Agave DistributionError::UnableToSetState
			mlog.Log.Warnf("spool distribution: stake state encode failed for %s, %d lamports burned: %v", rec.StakePubkey, rec.RewardLamports, err)
			burnedLamports.Add(rec.RewardLamports)
			return
		}

		stakeAcct.Lamports, err = safemath.CheckedAddU64(stakeAcct.Lamports, rec.RewardLamports)
		if err != nil {
			// Arithmetic overflow — matches Agave DistributionError::ArithmeticOverflow
			mlog.Log.Warnf("spool distribution: lamports overflow for %s, %d lamports burned: %v", rec.StakePubkey, rec.RewardLamports, err)
			burnedLamports.Add(rec.RewardLamports)
			return
		}

		accts[idx] = stakeAcct
		distributedLamports.Add(rec.RewardLamports)
	})

	for idx := range records {
		wg.Add(1)
		workerPool.Invoke(idx)
	}
	wg.Wait()
	workerPool.Release()

	// Filter out nil entries (burned records) before storing
	var filteredAccts []*accounts.Account
	for _, a := range accts {
		if a != nil {
			filteredAccts = append(filteredAccts, a)
		}
	}

	if len(filteredAccts) > 0 {
		err = acctsDb.StoreAccounts(filteredAccts, currentSlot, nil)
		if err != nil {
			panic(fmt.Sprintf("error updating accounts for spool distribution in slot %d: %s", currentSlot, err))
		}
	}

	return accts, parentAccts, distributedLamports.Load(), burnedLamports.Load()
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
	case sealevel.VoteStateVersionV4:
		commission = byte(voteState.V4.InflationRewardsCommissionBps / 100)
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
	case sealevel.VoteStateVersionV4:
		epochCredits = voteState.V4.EpochCredits
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

// CalculateRewardsStreaming performs a streaming calculation of stake rewards.
// Phase 1: Stream stakes to calculate total points + write points spool (single AccountsDB scan)
// Phase 2: Replay points spool to compute rewards + write temp spool (sequential file I/O only)
// Phase 3: Read temp spool, assign partitions, write per-partition spools
func CalculateRewardsStreaming(
	acctsDb *accountsdb.AccountsDb,
	slot uint64,
	stakeHistory *sealevel.SysvarStakeHistory,
	newWarmupCooldownRateEpoch *uint64,
	rewardVoteAccts map[solana.PublicKey]*epochstakes.VoteAccount,
	liveVoteCache map[solana.PublicKey]*sealevel.VoteStateVersions,
	pointValue PointValue,
	rewardedEpoch uint64,
	blockhash [32]byte,
	slotCtx *sealevel.SlotCtx,
	f *features.Features,
) (*StreamingRewardsResult, error) {
	minimum := minimumStakeDelegation(slotCtx)
	spoolDir := filepath.Join(acctsDb.AcctsDir, "..")

	fullRewardVoteAccts := 0
	for _, voteAcct := range rewardVoteAccts {
		if voteAcct != nil && len(voteAcct.Data) > 0 {
			fullRewardVoteAccts++
		}
	}
	mlog.Log.Infof("Rewards: epochVoteAccts=%d fullData=%d liveVoteCache=%d minimum stake delegation=%d",
		len(rewardVoteAccts), fullRewardVoteAccts, len(liveVoteCache), minimum)

	// ==================== PHASE 1: Calculate total points + write points spool ====================
	pointsWriter, err := NewPointsSpoolWriter(spoolDir, slot)
	if err != nil {
		return nil, fmt.Errorf("creating points spool: %w", err)
	}
	pointsPath := pointsWriter.Path()

	var totalPoints wide.Uint128
	var totalPointsMu sync.Mutex
	var phase1StakeCount atomic.Int64
	var phase1TotalStakeLamports atomic.Uint64
	var phase1BelowMinimumStats phase1ReasonStats
	var phase1NoVoteStateStats phase1ReasonStats
	var phase1ZeroPointsStats phase1ReasonStats
	var phase1ForceCreditsStats phase1ReasonStats
	var phase1CreditsRollbackStats phase1ReasonStats
	var phase1ActivationEpochSkipStats phase1ReasonStats
	var phase1ZeroRewardsStats phase1ReasonStats
	var phase1OtherForceCreditsStats phase1ReasonStats

	var phase1NoVoteByVoteMu sync.Mutex
	phase1NoVoteByVote := make(map[solana.PublicKey]*phase1VoteSkipAggregate)
	var phase1CreditsRollbackByVoteMu sync.Mutex
	phase1CreditsRollbackByVote := make(map[solana.PublicKey]*phase1VoteSkipAggregate)

	// Collect ALL vote pubkeys from delegations (matching in-memory path's pre-population)
	var allVotePubkeys sync.Map

	// Channel + single-writer goroutine for points spool writes
	type pointsWriteRequest struct {
		record *PointsSpoolRecord
	}
	pointsWriteChan := make(chan pointsWriteRequest, 10000)
	var pointsWriteErr atomic.Pointer[error]
	var pointsWriterWg sync.WaitGroup
	pointsWriterWg.Add(1)

	go func() {
		defer pointsWriterWg.Done()
		for req := range pointsWriteChan {
			if pointsWriteErr.Load() != nil {
				continue
			}
			if err := pointsWriter.WriteRecord(req.record); err != nil {
				pointsWriteErr.Store(&err)
			}
		}
	}()

	_, err = global.StreamStakeAccounts(acctsDb, slot,
		func(pk solana.PublicKey, delegation *sealevel.Delegation, creditsObs uint64) {
			// Always record the vote pubkey, even for below-minimum delegations
			allVotePubkeys.Store(delegation.VoterPubkey, struct{}{})

			if delegation.StakeLamports < minimum {
				phase1BelowMinimumStats.Add(delegation.StakeLamports, wide.Uint128{})
				return
			}

			voterPk := delegation.VoterPubkey
			voteState := rewardVoteState(voterPk, rewardVoteAccts, liveVoteCache)
			if voteState == nil {
				phase1NoVoteStateStats.Add(delegation.StakeLamports, wide.Uint128{})
				phase1NoVoteByVoteMu.Lock()
				addPhase1VoteSkipAggregate(phase1NoVoteByVote, voterPk, delegation.StakeLamports)
				phase1NoVoteByVoteMu.Unlock()
				return
			}

			delegWithCredits := *delegation
			delegWithCredits.CreditsObserved = creditsObs
			pcs := calculateStakePointsAndCredits(pk, stakeHistory, &delegWithCredits, voteState, newWarmupCooldownRateEpoch)

			zero128 := wide.Uint128FromUint64(0)
			if pcs.Points.Eq(zero128) {
				phase1ZeroPointsStats.Add(delegation.StakeLamports, wide.Uint128{})
			}

			totalPointsMu.Lock()
			totalPoints = totalPoints.Add(pcs.Points)
			totalPointsMu.Unlock()
			phase1StakeCount.Add(1)
			phase1TotalStakeLamports.Add(delegation.StakeLamports)

			// Precompute the full forceCreditsUpdate flag using the same three
			// triggers as CalculateStakeRewardsForAcct:
			isCreditsRollback := pcs.ForceCreditsUpdateWithSkippedReward && pcs.NewCreditsObserved < creditsObs
			isZeroRewards := pointValue.Rewards == 0
			isActivationEpochSkip := delegation.ActivationEpoch == rewardedEpoch
			forceCredits := pcs.ForceCreditsUpdateWithSkippedReward || isZeroRewards || isActivationEpochSkip
			if forceCredits {
				phase1ForceCreditsStats.Add(delegation.StakeLamports, pcs.Points)
				switch {
				case isCreditsRollback:
					phase1CreditsRollbackStats.Add(delegation.StakeLamports, pcs.Points)
					phase1CreditsRollbackByVoteMu.Lock()
					addPhase1VoteSkipAggregate(phase1CreditsRollbackByVote, voterPk, delegation.StakeLamports)
					phase1CreditsRollbackByVoteMu.Unlock()
				case isActivationEpochSkip:
					phase1ActivationEpochSkipStats.Add(delegation.StakeLamports, pcs.Points)
				case isZeroRewards:
					phase1ZeroRewardsStats.Add(delegation.StakeLamports, pcs.Points)
				default:
					phase1OtherForceCreditsStats.Add(delegation.StakeLamports, pcs.Points)
				}
			}

			// Only write points records that Phase 2 will actually use:
			// - forceCredits records produce spool records with 0 rewards
			// - Non-zero points records produce actual rewards
			// - Zero points without forceCredits → nil reward → skip
			if pcs.Points.Eq(zero128) && !forceCredits {
				return
			}

			pointsWriteChan <- pointsWriteRequest{record: &PointsSpoolRecord{
				StakePubkey:                         pk,
				VotePubkey:                          delegation.VoterPubkey,
				Points:                              pcs.Points,
				NewCreditsObserved:                  pcs.NewCreditsObserved,
				StakeLamports:                       delegation.StakeLamports,
				ForceCreditsUpdateWithSkippedReward: forceCredits,
			}}
		})

	close(pointsWriteChan)
	pointsWriterWg.Wait()

	if err != nil {
		pointsWriter.Close()
		CleanupPointsSpoolFile(pointsPath)
		return nil, fmt.Errorf("phase 1 streaming stakes for points: %w", err)
	}

	if werr := pointsWriteErr.Load(); werr != nil {
		pointsWriter.Close()
		CleanupPointsSpoolFile(pointsPath)
		return nil, fmt.Errorf("points spool write failed: %w", *werr)
	}

	if err := pointsWriter.Close(); err != nil {
		CleanupPointsSpoolFile(pointsPath)
		return nil, fmt.Errorf("points spool close failed: %w", err)
	}

	belowMinCount, belowMinStake, _ := phase1BelowMinimumStats.Snapshot()
	noVoteCount, noVoteStake, _ := phase1NoVoteStateStats.Snapshot()
	zeroPointsCount, zeroPointsStake, _ := phase1ZeroPointsStats.Snapshot()
	forceCreditsCount, forceCreditsStake, forceCreditsPoints := phase1ForceCreditsStats.Snapshot()
	creditsRollbackCount, creditsRollbackStake, creditsRollbackPoints := phase1CreditsRollbackStats.Snapshot()
	activationEpochSkipCount, activationEpochSkipStake, activationEpochSkipPoints := phase1ActivationEpochSkipStats.Snapshot()
	zeroRewardsCount, zeroRewardsStake, zeroRewardsPoints := phase1ZeroRewardsStats.Snapshot()
	otherForceCreditsCount, otherForceCreditsStake, otherForceCreditsPoints := phase1OtherForceCreditsStats.Snapshot()

	mlog.Log.Infof("Rewards Phase 1: %d stakes (belowMin=%d, noVote=%d, zeroPoints=%d), totalStakeLamports=%d, totalPoints=%s, totalRewards=%d, pointsSpoolRecords=%d",
		phase1StakeCount.Load(), belowMinCount, noVoteCount, zeroPointsCount,
		phase1TotalStakeLamports.Load(), totalPoints.String(), pointValue.Rewards, pointsWriter.Count())

	if belowMinCount > 0 || noVoteCount > 0 || zeroPointsCount > 0 || forceCreditsCount > 0 {
		mlog.Log.FileOnlyf("Rewards Phase 1 details:")
		mlog.Log.FileOnlyf("  below_minimum: count=%d stake=%d", belowMinCount, belowMinStake)
		mlog.Log.FileOnlyf("  no_vote_state: count=%d stake=%d unique_vote_accts=%d", noVoteCount, noVoteStake, len(phase1NoVoteByVote))
		if len(phase1NoVoteByVote) > 0 {
			topNoVote := sortedPhase1VoteSkipAggregates(phase1NoVoteByVote)
			limit := min(10, len(topNoVote))
			mlog.Log.FileOnlyf("  no_vote_state top_by_stake (showing %d):", limit)
			for i := 0; i < limit; i++ {
				entry := topNoVote[i]
				mlog.Log.FileOnlyf("    %d. vote=%s delegations=%d stake=%d", i+1, entry.VotePubkey, entry.DelegationCnt, entry.StakeLamports)
			}
		}
		mlog.Log.FileOnlyf("  zero_points_with_vote_state: count=%d stake=%d", zeroPointsCount, zeroPointsStake)
		mlog.Log.FileOnlyf("  force_credits_skip: count=%d stake=%d points_included_in_total=%s", forceCreditsCount, forceCreditsStake, forceCreditsPoints.String())
		mlog.Log.FileOnlyf("    credits_rollback: count=%d stake=%d points_included=%s unique_vote_accts=%d",
			creditsRollbackCount, creditsRollbackStake, creditsRollbackPoints.String(), len(phase1CreditsRollbackByVote))
		if len(phase1CreditsRollbackByVote) > 0 {
			topCreditsRollback := sortedPhase1VoteSkipAggregates(phase1CreditsRollbackByVote)
			limit := min(10, len(topCreditsRollback))
			mlog.Log.FileOnlyf("    credits_rollback top_by_stake (showing %d):", limit)
			for i := 0; i < limit; i++ {
				entry := topCreditsRollback[i]
				mlog.Log.FileOnlyf("      %d. vote=%s delegations=%d stake=%d", i+1, entry.VotePubkey, entry.DelegationCnt, entry.StakeLamports)
			}
		}
		mlog.Log.FileOnlyf("    activation_epoch: count=%d stake=%d points_included=%s",
			activationEpochSkipCount, activationEpochSkipStake, activationEpochSkipPoints.String())
		mlog.Log.FileOnlyf("    zero_rewards: count=%d stake=%d points_included=%s",
			zeroRewardsCount, zeroRewardsStake, zeroRewardsPoints.String())
		mlog.Log.FileOnlyf("    other_force_credits: count=%d stake=%d points_included=%s",
			otherForceCreditsCount, otherForceCreditsStake, otherForceCreditsPoints.String())
	}

	// ==================== PHASE 2: Replay points spool → compute rewards → write temp spool ====================
	pv := PointValue{Rewards: pointValue.Rewards, Points: totalPoints}

	tempWriter, err := NewTempSpoolWriter(spoolDir, slot)
	if err != nil {
		CleanupPointsSpoolFile(pointsPath)
		return nil, fmt.Errorf("creating temp spool: %w", err)
	}
	tempPath := tempWriter.Path()

	// Pre-populate validatorRewards with ALL vote pubkeys from delegations
	validatorRewards := make(map[solana.PublicKey]*atomic.Uint64)
	allVotePubkeys.Range(func(key, _ interface{}) bool {
		voterPk := key.(solana.PublicKey)
		validatorRewards[voterPk] = &atomic.Uint64{}
		return true
	})

	pointsReader, err := NewPointsSpoolReader(pointsPath)
	if err != nil {
		CleanupPointsSpoolFile(pointsPath)
		tempWriter.Close()
		CleanupTempSpoolFile(tempPath)
		return nil, fmt.Errorf("opening points spool reader: %w", err)
	}

	var phase2SkippedNilReward int64
	var phase2TotalStakerRewards uint64
	phase2VoteRewardsByVote := make(map[solana.PublicKey]*phase2VoteRewardAggregate)
	zero128 := wide.Uint128FromUint64(0)

	for {
		rec, err := pointsReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			pointsReader.Close()
			CleanupPointsSpoolFile(pointsPath)
			tempWriter.Close()
			CleanupTempSpoolFile(tempPath)
			return nil, fmt.Errorf("reading points spool: %w", err)
		}

		// ForceCreditsUpdateWithSkippedReward was fully precomputed in Phase 1
		// (covers pcs.ForceCredits, pointValue.Rewards==0, activationEpoch==rewardedEpoch)
		if rec.ForceCreditsUpdateWithSkippedReward {
			// Credits update only — write spool record with 0 rewards
			if err := tempWriter.WriteRecord(&SpoolRecord{
				StakePubkey:     rec.StakePubkey,
				VotePubkey:      rec.VotePubkey,
				StakeLamports:   rec.StakeLamports,
				CreditsObserved: rec.NewCreditsObserved,
				RewardLamports:  0,
			}); err != nil {
				pointsReader.Close()
				CleanupPointsSpoolFile(pointsPath)
				tempWriter.Close()
				CleanupTempSpoolFile(tempPath)
				return nil, fmt.Errorf("temp spool write failed: %w", err)
			}
			continue
		}

		// 2. Zero points or zero totalPoints → nil reward
		if rec.Points.Eq(zero128) || pv.Points.Eq(zero128) {
			phase2SkippedNilReward++
			continue
		}

		// 3. Compute reward: (points * totalRewards) / totalPoints
		rewards128 := rec.Points.Mul(wide.Uint128FromUint64(pv.Rewards)).Div(pv.Points)
		if !rewards128.IsUint64() {
			phase2SkippedNilReward++
			continue
		}
		rewards := rewards128.Uint64()
		if rewards == 0 {
			phase2SkippedNilReward++
			continue
		}

		// 4. Commission split
		voteState := rewardVoteState(rec.VotePubkey, rewardVoteAccts, liveVoteCache)
		if voteState == nil {
			phase2SkippedNilReward++
			continue
		}
		splitResult := voteCommissionSplit(voteState, rewards)
		if splitResult.IsSplit && (splitResult.VoterPortion == 0 || splitResult.StakerPortion == 0) {
			phase2SkippedNilReward++
			continue
		}

		phase2TotalStakerRewards += splitResult.StakerPortion
		addPhase2VoteRewardAggregate(
			phase2VoteRewardsByVote,
			rec.VotePubkey,
			rec.StakeLamports,
			rec.Points,
			rewards,
			splitResult.VoterPortion,
			splitResult.StakerPortion,
		)

		if err := tempWriter.WriteRecord(&SpoolRecord{
			StakePubkey:     rec.StakePubkey,
			VotePubkey:      rec.VotePubkey,
			StakeLamports:   rec.StakeLamports,
			CreditsObserved: rec.NewCreditsObserved,
			RewardLamports:  splitResult.StakerPortion,
		}); err != nil {
			pointsReader.Close()
			CleanupPointsSpoolFile(pointsPath)
			tempWriter.Close()
			CleanupTempSpoolFile(tempPath)
			return nil, fmt.Errorf("temp spool write failed: %w", err)
		}

		if splitResult.VoterPortion > 0 {
			validatorRewards[rec.VotePubkey].Add(splitResult.VoterPortion)
		}
	}

	pointsReader.Close()
	CleanupPointsSpoolFile(pointsPath)

	if err := tempWriter.Close(); err != nil {
		CleanupTempSpoolFile(tempPath)
		return nil, fmt.Errorf("temp spool close failed: %w", err)
	}

	// ==================== Calculate numPartitions from ACTUAL count ====================
	actualRewardCount := uint64(tempWriter.Count())
	numPartitions := CalculateNumRewardPartitions(actualRewardCount)

	var totalVotingRewards uint64
	for _, v := range validatorRewards {
		totalVotingRewards += v.Load()
	}
	mlog.Log.Infof("Rewards Phase 2: %d records (skippedNilReward=%d), totalStakerRewards=%d, totalVotingRewards=%d, validatorCount=%d, numPartitions=%d",
		actualRewardCount, phase2SkippedNilReward,
		phase2TotalStakerRewards, totalVotingRewards, len(validatorRewards), numPartitions)
	dumpPhase2VoteRewardSummaryCSV(slot, phase2VoteRewardsByVote)

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
