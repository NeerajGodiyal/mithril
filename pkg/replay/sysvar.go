package replay

import (
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/duration"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/safemath"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/wide"
	"github.com/gagliardetto/solana-go"
	"github.com/tidwall/btree"
)

const nsPerSlot = 400000000
const maxAllowableDriftFast = 25
const maxAllowableDriftSlow = 150

func updateClockSysvar(clock *sealevel.SysvarClock, block *block.Block, epochSchedule *sealevel.SysvarEpochSchedule) error {
	return updateClockSysvarForMode(clock, block, epochSchedule, false)
}

func updateClockSysvarForMode(clock *sealevel.SysvarClock, block *block.Block, epochSchedule *sealevel.SysvarEpochSchedule, alpenglowClock bool) error {
	epochOld := clock.Epoch
	epochNew := block.Epoch

	if epochOld != epochNew && epochOld+1 != epochNew {
		return fmt.Errorf("unexpected epoch transition in Clock sysvar: clock epoch %d, block epoch %d at slot %d", epochOld, epochNew, block.Slot)
	}

	if alpenglowClock {
		// Alpenglow banks populate timestamp fields from the block footer after
		// execution. At bank start, transactions only see updated slot/epoch
		// fields while timestamp fields are preserved from the parent bank.
	} else if global.CalcUnixTimeForClockSysvar() {
		firstSlotInEpoch := epochSchedule.FirstSlotInEpoch(clock.Epoch)
		epochStartTimestamp := clock.EpochStartTimestamp
		timestampEstimate := getTimestampEstimate(block.Slot, firstSlotInEpoch, epochStartTimestamp, epochSchedule)
		if timestampEstimate > clock.UnixTimestamp {
			clock.UnixTimestamp = timestampEstimate
		}
	} else {
		clock.UnixTimestamp = block.UnixTimestamp
	}

	clock.Slot = block.Slot
	clock.Epoch = epochNew
	clock.LeaderScheduleEpoch = epochSchedule.LeaderScheduleEpoch(clock.Slot)

	if epochOld != epochNew {
		clock.EpochStartTimestamp = clock.UnixTimestamp
	}

	return nil
}

func updateClockSysvarFromAlpenglowFooter(clock *sealevel.SysvarClock, block *block.Block, epochSchedule *sealevel.SysvarEpochSchedule) error {
	if block.UnixTimestamp == 0 {
		return nil
	}

	epochNew := block.Epoch
	parentEpoch := epochSchedule.GetEpoch(block.ParentSlot)
	epochStartTimestamp := clock.EpochStartTimestamp
	if block.Slot == 0 || parentEpoch != epochNew {
		epochStartTimestamp = block.UnixTimestamp
	}

	clock.Slot = block.Slot
	clock.Epoch = epochNew
	clock.LeaderScheduleEpoch = epochSchedule.LeaderScheduleEpoch(clock.Slot)
	clock.EpochStartTimestamp = epochStartTimestamp
	clock.UnixTimestamp = block.UnixTimestamp
	return nil
}

type tsEntry struct {
	pubkey    solana.PublicKey
	slot      uint64
	timestamp int64
}

func getTimestampEstimate(slot uint64, epochStartTimestampSlot uint64, epochStartTimestamp int64, epochSchedule *sealevel.SysvarEpochSchedule) int64 {
	slotsPerEpoch := epochSchedule.SlotsPerEpoch
	voteAccts := global.VoteCache()

	recentTimestamps := make([]*tsEntry, 0, len(voteAccts))
	for addr, voteAcct := range voteAccts {
		lastTs := voteAcct.LastTimestamp()
		slotDelta := safemath.SaturatingSubU64(slot, lastTs.Slot)
		if slotDelta <= slotsPerEpoch {
			recentTimestamps = append(recentTimestamps, &tsEntry{pubkey: addr, slot: lastTs.Slot, timestamp: lastTs.Timestamp})
		}
	}

	slotDuration := duration.NewDurationFromNanos(nsPerSlot)
	epoch := epochSchedule.GetEpoch(slot)
	stakes := global.EpochStakes(epoch)
	timestampEstimate, err := calculateStakeWeightedTimestamp(recentTimestamps, stakes, slot, slotDuration, epochStartTimestampSlot, epochStartTimestamp)
	if err != nil {
		panic(err)
	}

	return timestampEstimate
}

func calculateStakeWeightedTimestamp(
	recentTimestamps []*tsEntry,
	stakes map[solana.PublicKey]uint64,
	slot uint64,
	slotDuration duration.Duration,
	epochStartSlot uint64,
	epochStartTimestamp int64) (int64, error) {

	var stakePerTimestamp btree.Map[int64, wide.Uint128]
	var totalStake wide.Uint128

	for _, r := range recentTimestamps {
		offset := slotDuration.SaturatingMul(uint32(safemath.SaturatingSubU64(slot, r.slot)))
		estimate := safemath.SaturatingAddI64(r.timestamp, int64(offset.Secs))

		stake := stakes[r.pubkey]
		add := wide.Uint128FromUint64(stake)

		if cur, ok := stakePerTimestamp.Get(estimate); ok {
			stakePerTimestamp.Set(estimate, cur.Add(add))
		} else {
			stakePerTimestamp.Set(estimate, add)
		}
		totalStake = totalStake.Add(add)
	}

	if totalStake.Eq(wide.Uint128{}) {
		return 0, fmt.Errorf("total stake == 0")
	}

	halfTotalStake := totalStake.Div(wide.Uint128FromUint64(2))
	var acc wide.Uint128
	var estimate int64

	it := stakePerTimestamp.Iter()
	if !it.First() {
		return 0, fmt.Errorf("no stakes")
	}
	for {
		ts := it.Key()
		stake := it.Value()
		acc = acc.Add(stake)
		if acc.Gt(halfTotalStake) {
			estimate = ts
			break
		}
		if !it.Next() {
			estimate = ts
			break
		}
	}

	pohEstimateOffset := slotDuration.SaturatingMul(uint32(safemath.SaturatingSubU64(slot, epochStartSlot)))

	estimateOffset := duration.NewDurationFromSecs(
		safemath.SaturatingSubU64(uint64(estimate), uint64(epochStartTimestamp)),
	)

	maxDriftFast := pohEstimateOffset.SaturatingMul(maxAllowableDriftFast).Div(100)
	maxDriftSlow := pohEstimateOffset.SaturatingMul(maxAllowableDriftSlow).Div(100)

	if estimateOffset.Gt(pohEstimateOffset) &&
		estimateOffset.SaturatingSub(pohEstimateOffset).Gt(maxDriftSlow) {
		estimate = safemath.SaturatingAddI64(
			safemath.SaturatingAddI64(epochStartTimestamp, int64(pohEstimateOffset.Secs)),
			int64(maxDriftSlow.Secs),
		)
	} else if estimateOffset.Lt(pohEstimateOffset) &&
		pohEstimateOffset.SaturatingSub(estimateOffset).Gt(maxDriftFast) {
		estimate = safemath.SaturatingSubI64(
			safemath.SaturatingAddI64(epochStartTimestamp, int64(pohEstimateOffset.Secs)),
			int64(maxDriftFast.Secs),
		)
	}

	return estimate, nil
}

func collectAndUpdateSysvarAcctsForAdh(slotCtx *sealevel.SlotCtx) []*accounts.Account {
	sysvarPubkeys := []solana.PublicKey{sealevel.SysvarClockAddr, sealevel.SysvarRecentBlockHashesAddr, sealevel.SysvarSlotHashesAddr, sealevel.SysvarSlotHistoryAddr}
	var sysvarAccts []*accounts.Account

	for _, pk := range sysvarPubkeys {
		acct, err := slotCtx.GetAccount(pk)
		if err != nil {
			panic(fmt.Sprintf("unable to get sysvar account for ADH: %s", pk))
		}

		if acct.Key == sealevel.SysvarSlotHistoryAddr {
			slotHistory := sealevel.SysvarCache.SlotHistory.Sysvar
			slotHistory.Add(slotCtx.Slot)
			slotHistory.SetNextSlot(slotCtx.Slot + 1)
			newSlotHistoryBytes := slotHistory.MustMarshal()
			copy(acct.Data, newSlotHistoryBytes)
		}

		if acct.Key == sealevel.SysvarRecentBlockHashesAddr {
			recentBlockhashes := sealevel.SysvarCache.RecentBlockHashes.Sysvar
			slotCtx.LatestEvictedBlockhash = recentBlockhashes.PushLatest(slotCtx.Blockhash, slotCtx.FeeRateGovernor.LamportsPerSignature)
			newRecentBlockhashesBytes := recentBlockhashes.MustMarshal()
			copy(acct.Data, newRecentBlockhashesBytes)
		}

		sysvarAccts = append(sysvarAccts, acct)
	}
	return sysvarAccts
}
