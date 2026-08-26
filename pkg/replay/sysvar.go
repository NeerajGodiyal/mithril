package replay

import (
	"fmt"
	"math"
	"sort"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/duration"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/safemath"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/wide"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"github.com/tidwall/btree"
)

const legacyNsPerSlot = 400000000
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
		timestampEstimate, ok := getTimestampEstimate(block.Slot, firstSlotInEpoch, epochStartTimestamp, epochSchedule, block.Features)
		if ok && timestampEstimate > clock.UnixTimestamp {
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
	footerUnixTimestamp, ok, err := alpenglowFooterUnixTimestamp(block)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	epochNew := block.Epoch
	parentEpoch := epochSchedule.GetEpoch(block.ParentSlot)
	epochStartTimestamp := clock.EpochStartTimestamp
	if block.Slot == 0 || parentEpoch != epochNew {
		epochStartTimestamp = footerUnixTimestamp
	}

	clock.Slot = block.Slot
	clock.Epoch = epochNew
	clock.LeaderScheduleEpoch = epochSchedule.LeaderScheduleEpoch(clock.Slot)
	clock.EpochStartTimestamp = epochStartTimestamp
	clock.UnixTimestamp = footerUnixTimestamp
	return nil
}

// alpenglowFooterUnixTimestamp returns the footer clock timestamp in seconds.
// Agave stores block_producer_time_nanos on the wire and divides by 1e9 for Clock::unix_timestamp.
func alpenglowFooterUnixTimestamp(block *block.Block) (int64, bool, error) {
	if block == nil {
		return 0, false, nil
	}
	if block.UnixTimestamp != 0 {
		return block.UnixTimestamp, true, nil
	}
	if block.FooterProducerTimeNanos == 0 {
		return 0, false, nil
	}
	if block.FooterProducerTimeNanos > uint64(^uint64(0)>>1) {
		return 0, false, fmt.Errorf("footer producer time nanos %d overflows i64", block.FooterProducerTimeNanos)
	}
	return int64(block.FooterProducerTimeNanos / 1_000_000_000), true, nil
}

type tsEntry struct {
	pubkey    solana.PublicKey
	slot      uint64
	timestamp int64
}

func getTimestampEstimate(slot uint64, epochStartTimestampSlot uint64, epochStartTimestamp int64, epochSchedule *sealevel.SysvarEpochSchedule, f *features.Features) (int64, bool) {
	slotsPerEpoch := epochSchedule.SlotsPerEpoch
	voteAccts := global.VoteCache()

	recentTimestamps := make([]*tsEntry, 0, len(voteAccts))
	for addr, voteAcct := range voteAccts {
		lastTs := voteAcct.LastTimestamp()
		if lastTs.Slot > slot {
			continue
		}
		slotDelta := slot - lastTs.Slot
		if slotDelta <= slotsPerEpoch {
			recentTimestamps = append(recentTimestamps, &tsEntry{pubkey: addr, slot: lastTs.Slot, timestamp: lastTs.Timestamp})
		}
	}

	elapsedSlotDuration := func(fromSlot, toSlot uint64) duration.Duration {
		if fromSlot >= toSlot {
			return duration.Duration{}
		}
		return classicSlotRangeDuration(
			f,
			epochSchedule,
			safemath.SaturatingAddU64(fromSlot, 1),
			toSlot,
		)
	}
	epoch := epochSchedule.GetEpoch(slot)
	stakes := global.EpochStakes(epoch)
	return calculateStakeWeightedTimestamp(recentTimestamps, stakes, slot, elapsedSlotDuration, epochStartTimestampSlot, epochStartTimestamp)
}

type slotTimeTransition struct {
	slot         uint64
	nanosPerSlot uint64
}

// classicSlotRangeDuration mirrors Agave's archived slot-time regimes. A
// reduction activates at the first slot of the epoch after its feature account
// activates, and a later activation can never increase slot duration again.
func classicSlotRangeDuration(f *features.Features, epochSchedule *sealevel.SysvarEpochSchedule, startSlot, endSlot uint64) duration.Duration {
	if startSlot > endSlot {
		return duration.Duration{}
	}

	gates := []struct {
		gate         features.FeatureGate
		nanosPerSlot uint64
	}{
		{features.ReduceSlotTimeTo350ms, 350_000_000},
		{features.ReduceSlotTimeTo300ms, 300_000_000},
		{features.ReduceSlotTimeTo250ms, 250_000_000},
		{features.ReduceSlotTimeTo200ms, 200_000_000},
	}

	transitions := make([]slotTimeTransition, 0, len(gates))
	earliestSameOrShorterSlot := uint64(math.MaxUint64)
	for i := len(gates) - 1; i >= 0; i-- {
		if f == nil {
			break
		}
		activationSlot, ok := f.ActivationSlot(gates[i].gate)
		if !ok {
			continue
		}
		activationEpoch := epochSchedule.GetEpoch(activationSlot)
		effectiveSlot := epochSchedule.FirstSlotInEpoch(safemath.SaturatingAddU64(activationEpoch, 1))
		if effectiveSlot < earliestSameOrShorterSlot {
			transitions = append(transitions, slotTimeTransition{slot: effectiveSlot, nanosPerSlot: gates[i].nanosPerSlot})
			earliestSameOrShorterSlot = effectiveSlot
		}
	}
	sort.Slice(transitions, func(i, j int) bool { return transitions[i].slot < transitions[j].slot })

	cursor := startSlot
	nanosPerSlot := uint64(legacyNsPerSlot)
	var durationNanos uint64
	for _, transition := range transitions {
		if transition.slot <= startSlot {
			nanosPerSlot = transition.nanosPerSlot
			continue
		}
		if transition.slot > endSlot {
			break
		}
		slots := transition.slot - cursor
		durationNanos = safemath.SaturatingAddU64(
			durationNanos,
			safemath.SaturatingMulU64(slots, nanosPerSlot),
		)
		cursor = transition.slot
		nanosPerSlot = transition.nanosPerSlot
	}

	remaining := classicSlotRangeNanos(cursor, endSlot, nanosPerSlot)
	return duration.NewDurationFromNanos(safemath.SaturatingAddU64(durationNanos, remaining))
}

func classicSlotRangeNanos(startSlot, endSlot, nanosPerSlot uint64) uint64 {
	slots := safemath.SaturatingAddU64(safemath.SaturatingSubU64(endSlot, startSlot), 1)
	return safemath.SaturatingMulU64(slots, nanosPerSlot)
}

func calculateStakeWeightedTimestamp(
	recentTimestamps []*tsEntry,
	stakes map[solana.PublicKey]uint64,
	slot uint64,
	elapsedSlotDuration func(uint64, uint64) duration.Duration,
	epochStartSlot uint64,
	epochStartTimestamp int64) (int64, bool) {

	var stakePerTimestamp btree.Map[int64, wide.Uint128]
	var totalStake wide.Uint128

	for _, r := range recentTimestamps {
		offset := elapsedSlotDuration(r.slot, slot)
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
		return 0, false
	}

	halfTotalStake := totalStake.Div(wide.Uint128FromUint64(2))
	var acc wide.Uint128
	var estimate int64

	it := stakePerTimestamp.Iter()
	if !it.First() {
		return 0, false
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

	pohEstimateOffset := elapsedSlotDuration(epochStartSlot, slot)

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

	return estimate, true
}

// finalizeBankSysvars applies the bank-end updates that are shared by replay
// and local production.  The account overlay and the immutable bank sysvar
// snapshot are replaced together before either bank hashing or chain-tip
// publication, so no consumer can observe the pre-finalization bytes.
func finalizeBankSysvars(slotCtx *sealevel.SlotCtx) error {
	if slotCtx == nil {
		return fmt.Errorf("missing slot context while finalizing bank sysvars")
	}

	recentAcct, err := slotCtx.GetAccount(sealevel.SysvarRecentBlockHashesAddr)
	if err != nil {
		return fmt.Errorf("get RecentBlockhashes sysvar: %w", err)
	}
	var recent sealevel.SysvarRecentBlockhashes
	if bankSysvars := slotCtx.BankSysvars(); bankSysvars != nil {
		if cached, ok := bankSysvars.RecentBlockhashes(); ok {
			recent = append(sealevel.SysvarRecentBlockhashes(nil), cached...)
		}
	}
	if recent == nil {
		recent.MustUnmarshalWithDecoder(bin.NewBinDecoder(recentAcct.Data))
	}
	slotCtx.LatestEvictedBlockhash = recent.PushLatest(slotCtx.Blockhash, slotCtx.FeeRateGovernor.LamportsPerSignature)
	recentAcct.Data = recent.MustMarshal()

	historyAcct, err := slotCtx.GetAccount(sealevel.SysvarSlotHistoryAddr)
	if err != nil {
		return fmt.Errorf("get SlotHistory sysvar: %w", err)
	}
	var history sealevel.SysvarSlotHistory
	hasCachedHistory := false
	if bankSysvars := slotCtx.BankSysvars(); bankSysvars != nil {
		if cached, ok := bankSysvars.SlotHistory(); ok {
			history = cached
			history.Bits.Bits.Blocks = append([]uint64(nil), cached.Bits.Bits.Blocks...)
			hasCachedHistory = true
		}
	}
	if !hasCachedHistory {
		history.MustUnmarshalWithDecoder(bin.NewBinDecoder(historyAcct.Data))
	}
	history.Add(slotCtx.Slot)
	history.SetNextSlot(slotCtx.Slot + 1)
	historyAcct.Data = history.MustMarshal()

	if err := slotCtx.SetAccount(recentAcct.Key, recentAcct); err != nil {
		return fmt.Errorf("write RecentBlockhashes sysvar: %w", err)
	}
	if err := slotCtx.SetAccount(historyAcct.Key, historyAcct); err != nil {
		return fmt.Errorf("write SlotHistory sysvar: %w", err)
	}

	bankSysvars := slotCtx.BankSysvars()
	if bankSysvars == nil {
		bankSysvars, err = sealevel.NewBankSysvars(slotCtx.Slot, recentAcct, historyAcct)
	} else {
		bankSysvars, err = bankSysvars.WithAccounts(recentAcct, historyAcct)
	}
	if err != nil {
		return fmt.Errorf("update finalized bank sysvar snapshot: %w", err)
	}
	if err := slotCtx.PublishBankSysvars(bankSysvars); err != nil {
		return err
	}

	// The legacy singleton remains an ordered-replay bootstrap/checkpoint aid.
	// Never publish speculative producer state into it.
	if slotCtx.Replay {
		recentForLegacy := append(sealevel.SysvarRecentBlockhashes(nil), recent...)
		historyForLegacy := history
		historyForLegacy.Bits.Bits.Blocks = append([]uint64(nil), history.Bits.Bits.Blocks...)
		sealevel.SysvarCache.RecentBlockHashes.Sysvar = &recentForLegacy
		sealevel.SysvarCache.RecentBlockHashes.Acct = recentAcct.Clone()
		sealevel.SysvarCache.SlotHistory.Sysvar = &historyForLegacy
		sealevel.SysvarCache.SlotHistory.Acct = historyAcct.Clone()
	}
	return nil
}

func collectSysvarAcctsForAdh(slotCtx *sealevel.SlotCtx) []*accounts.Account {
	sysvarPubkeys := []solana.PublicKey{sealevel.SysvarClockAddr, sealevel.SysvarRecentBlockHashesAddr, sealevel.SysvarSlotHashesAddr, sealevel.SysvarSlotHistoryAddr}
	var sysvarAccts []*accounts.Account

	for _, pk := range sysvarPubkeys {
		acct, err := slotCtx.GetAccount(pk)
		if err != nil {
			panic(fmt.Sprintf("unable to get sysvar account for ADH: %s", pk))
		}

		sysvarAccts = append(sysvarAccts, acct)
	}
	return sysvarAccts
}
