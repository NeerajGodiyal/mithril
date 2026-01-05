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
	EahStartOffsetSlot     uint64
	EahStopOffsetSlot      uint64
	NumRewardPartitions    uint64
	Credits                map[solana.PublicKey]CalculatedStakePoints
	RewardPartitions       Partitions
	StakingRewards         map[solana.PublicKey]*CalculatedStakeRewards
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

type idxAndReward struct {
	idx    int
	reward rpc.BlockReward
}

func DistributeVotingRewards(acctsDb *accountsdb.AccountsDb, rewards []rpc.BlockReward, slot uint64) ([]*accounts.Account, []*accounts.Account, uint64) {
	var totalVotingRewards atomic.Uint64

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
				panic(fmt.Sprintf("unable to get acct %s from acctsdb for voting rewards distribution in slot %d", reward.Pubkey, slot))
			}
			parentUpdatedAccts[idx] = stakeAcct.Clone()

			stakeAcct.Lamports, err = safemath.CheckedAddU64(stakeAcct.Lamports, uint64(reward.Lamports))
			if err != nil {
				panic(fmt.Sprintf("overflow in voting rewards distribution in slot %d to acct %s: %s", slot, reward.Pubkey, err))
			}

			if stakeAcct.Lamports != reward.PostBalance {
				panic(fmt.Sprintf("post-balance for acct %s in distributing voting rewards in slot %d did not match expected %d (actual %d)", reward.Pubkey, slot, reward.PostBalance, stakeAcct.Lamports))
			}

			accts[idx] = stakeAcct

			new := totalVotingRewards.Add(uint64(reward.Lamports))
			if new < uint64(reward.Lamports) {
				panic(fmt.Sprintf("overflow in accumulating voting rewards in slot %d", slot))
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

	err := acctsDb.StoreAccounts(accts, slot)
	if err != nil {
		panic(fmt.Sprintf("error updating accounts for voting rewards in slot %d: %s", slot, err))
	}

	return accts, parentUpdatedAccts, totalVotingRewards.Load()
}

type idxAndPubkey struct {
	idx    int
	pubkey solana.PublicKey
}

func DistributeStakingRewardsForPartition(acctsDb *accountsdb.AccountsDb, partition *Partition, stakingRewards map[solana.PublicKey]*CalculatedStakeRewards, slot uint64) ([]*accounts.Account, []*accounts.Account, uint64) {
	var distributedLamports atomic.Uint64
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
			panic(fmt.Sprintf("unable to get acct %s from acctsdb for partitioned epoch rewards distribution in slot %d", stakePk, slot))
		}
		parentAccts[idx] = stakeAcct.Clone()

		// update the delegation in the stake account state
		stakeState, err := sealevel.UnmarshalStakeState(stakeAcct.Data)
		if err != nil {
			panic(fmt.Sprintf("unable to deserialize stake account in distributing partitioned rewards: %s", err))
		}

		stakeState.Stake.Stake.CreditsObserved = reward.NewCreditsObserved
		stakeState.Stake.Stake.Delegation.StakeLamports = safemath.SaturatingAddU64(stakeState.Stake.Stake.Delegation.StakeLamports, uint64(reward.StakerRewards))

		newStakeStateBytes, err := sealevel.MarshalStakeStake(stakeState)
		if err != nil {
			panic(fmt.Sprintf("unable to serialize new stake account state in distributing partitioned rewards: %s", err))
		}
		copy(stakeAcct.Data, newStakeStateBytes)

		// update lamports in stake account
		stakeAcct.Lamports, err = safemath.CheckedAddU64(stakeAcct.Lamports, uint64(reward.StakerRewards))
		if err != nil {
			panic(fmt.Sprintf("overflow in partitioned epoch rewards distribution in slot %d to acct %s: %s", slot, stakePk, err))
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

	err := acctsDb.StoreAccounts(accts, slot)
	if err != nil {
		panic(fmt.Sprintf("error updating accounts for partitioned epoch rewards in slot %d: %s", slot, err))
	}

	return accts, parentAccts, distributedLamports.Load()
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

	var mu sync.Mutex
	var wg sync.WaitGroup

	workerPool, _ := ants.NewPoolWithFunc(runtime.GOMAXPROCS(0)*8, func(i interface{}) {
		defer wg.Done()

		delegation := i.(*delegationAndPubkey)

		if delegation.delegation.StakeLamports < minimumStakeDelegation {
			return
		}

		voterPk := delegation.delegation.VoterPubkey
		voteStateVersioned := global.VoteCacheItem(voterPk)
		if voteStateVersioned == nil {
			return
		}

		pointsForStakeAcct := pointsPerStakeAcct[delegation.pubkey]
		calculatedStakeRewards := CalculateStakeRewardsForAcct(delegation.pubkey, pointsForStakeAcct, delegation.delegation, voteStateVersioned, rewardedEpoch, pointValue, newRateActivationEpoch)
		if calculatedStakeRewards != nil {
			mu.Lock()
			stakeInfoResults[delegation.pubkey] = calculatedStakeRewards
			mu.Unlock()
		}
	})

	for pk, delegation := range global.StakeCache() {
		d := &delegationAndPubkey{delegation: delegation, pubkey: pk}
		wg.Add(1)
		workerPool.Invoke(d)
	}
	wg.Wait()
	workerPool.Release()
	ants.Release()

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
			return
		}

		voterPk := d.VoterPubkey
		voteState := global.VoteCacheItem(voterPk)
		if voteState == nil {
			return
		}

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
