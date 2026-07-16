package replay

import (
	"fmt"
	"sort"

	"github.com/Overclock-Validator/mithril/pkg/alpenglow"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/safemath"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
)

const (
	legacyVATBurnPerEpoch = uint64(1_600_000_000)
	vatBurn350ms          = uint64(1_400_000_000)
	vatBurn300ms          = uint64(1_200_000_000)
	vatBurn250ms          = uint64(1_000_000_000)
	vatBurn200ms          = uint64(800_000_000)
)

type vatStakeCandidate struct {
	voteAccount solana.PublicKey
	stake       uint64
}

// alpenglowVATBurnPerEpoch mirrors Agave's version-specific SlotParams table.
// Slot-time gates take effect at the next epoch boundary, not at activation.
func alpenglowVATBurnPerEpoch(f *features.Features, epochSchedule *sealevel.SysvarEpochSchedule, slot uint64) uint64 {
	if !alpenglowClockFeatureActive(f) {
		return 0
	}
	burn := legacyVATBurnPerEpoch
	for _, transition := range []struct {
		gate features.FeatureGate
		burn uint64
	}{
		{features.ReduceSlotTimeTo350ms, vatBurn350ms},
		{features.ReduceSlotTimeTo300ms, vatBurn300ms},
		{features.ReduceSlotTimeTo250ms, vatBurn250ms},
		{features.ReduceSlotTimeTo200ms, vatBurn200ms},
	} {
		activationSlot, ok := f.ActivationSlot(transition.gate)
		if !ok {
			continue
		}
		activationEpoch := epochSchedule.GetEpoch(activationSlot)
		effectiveSlot := epochSchedule.FirstSlotInEpoch(safemath.SaturatingAddU64(activationEpoch, 1))
		if effectiveSlot <= slot {
			burn = transition.burn
		}
	}
	return burn
}

func minimumVoteAccountBalanceForVAT(f *features.Features, epochSchedule *sealevel.SysvarEpochSchedule, slot uint64) (uint64, error) {
	if epochSchedule == nil {
		return 0, fmt.Errorf("epoch schedule unavailable while building VAT epoch stakes")
	}
	rent := sealevel.SysvarCache.Rent.Sysvar
	if rent == nil {
		return 0, fmt.Errorf("rent sysvar unavailable while building VAT epoch stakes")
	}
	rentMinimum := rent.MinimumBalance(sealevel.VoteStateV4Size)
	return safemath.SaturatingAddU64(rentMinimum, alpenglowVATBurnPerEpoch(f, epochSchedule, slot)), nil
}

// filterEpochStakesForVAT applies SIMD-0357 before the epoch stakes become a
// leader schedule and BLS rank map. Underfunded accounts must be removed here:
// admitting one assigns different ranks than Agave and invalidates cert bitmaps.
func filterEpochStakesForVAT(
	stakes map[solana.PublicKey]uint64,
	voteCache map[solana.PublicKey]*sealevel.VoteStateVersions,
	metadata map[solana.PublicKey]rebuiltVoteAccountMeta,
	minimumBalance uint64,
) (map[solana.PublicKey]uint64, uint64) {
	candidates := make([]vatStakeCandidate, 0, len(stakes))
	for voteAccount, stake := range stakes {
		voteState := voteCache[voteAccount]
		meta, ok := metadata[voteAccount]
		if stake == 0 || voteState == nil || voteState.BlsPubkeyCompressed() == nil || !ok || meta.Lamports < minimumBalance {
			continue
		}
		candidates = append(candidates, vatStakeCandidate{voteAccount: voteAccount, stake: stake})
	}

	if len(candidates) > alpenglow.MaximumVATValidators {
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].stake > candidates[j].stake })
		cutoffStake := candidates[alpenglow.MaximumVATValidators].stake
		kept := candidates[:0]
		for _, candidate := range candidates {
			// Removing the entire cutoff tie prevents a pubkey-ordering grind for
			// the final VAT position, matching Agave's clone_and_filter_for_vat.
			if candidate.stake > cutoffStake {
				kept = append(kept, candidate)
			}
		}
		candidates = kept
	}

	filtered := make(map[solana.PublicKey]uint64, len(candidates))
	var total uint64
	for _, candidate := range candidates {
		filtered[candidate.voteAccount] = candidate.stake
		total = safemath.SaturatingAddU64(total, candidate.stake)
	}
	return filtered, total
}
