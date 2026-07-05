package replay

import (
	"fmt"
	"math"
	"math/big"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/alpenglow"
	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/rewardcerts"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
)

var agMigrationEpochCredit = sealevel.EpochCredits{
	Epoch:       math.MaxUint64,
	Credits:     math.MaxUint64,
	PrevCredits: math.MaxUint64,
}

// ApplyAlpenglowVoteRewards updates vote account state from validated footer reward and final certs.
func ApplyAlpenglowVoteRewards(
	acctsDb *accountsdb.AccountsDb,
	slotCtx *sealevel.SlotCtx,
	block *b.Block,
	epochSchedule *sealevel.SysvarEpochSchedule,
	skipRaw, notarRaw, finalCertRaw []byte,
) error {
	if len(skipRaw) == 0 && len(notarRaw) == 0 && len(finalCertRaw) == 0 {
		return nil
	}
	if slotCtx.Features == nil || !(slotCtx.Features.IsActive(features.Alpenglow) || slotCtx.Features.IsActive(features.AlpenglowDevContext)) {
		if block.FromLightbringer && (len(skipRaw) > 0 || len(notarRaw) > 0 || len(finalCertRaw) > 0) {
			mlog.Log.Infof(
				"cavey debug: vote rewards skipped slot=%d skip_len=%d notar_len=%d final_len=%d reason=alpenglow_feature_inactive",
				block.Slot, len(skipRaw), len(notarRaw), len(finalCertRaw),
			) // cavey TODO: remove once we are done debugging.
		}
		return nil
	}

	var rewardValidators map[solana.PublicKey]struct{}
	var rewardSlot uint64
	var rewardEpoch uint64
	var inflationState EpochInflationState
	var rewardEpochStakes map[solana.PublicKey]uint64
	var totalStake uint64
	var migrationEpoch uint64
	var leaderVote solana.PublicKey
	var leaderVoteOK bool

	if len(skipRaw) > 0 || len(notarRaw) > 0 {
		var ok bool
		rewardSlot, ok = rewardcerts.RewardSlotForLeader(block.Slot)
		if !ok {
			return fmt.Errorf("slot %d vote rewards: invalid reward slot offset", block.Slot)
		}
		rewardEpoch = epochSchedule.GetEpoch(rewardSlot)

		validatorSet, err := buildValidatorSetForEpoch(rewardEpoch)
		if err != nil {
			return fmt.Errorf("slot %d vote rewards: %w", block.Slot, err)
		}

		validated, err := rewardcerts.ValidateRewardCertificates(block.Slot, skipRaw, notarRaw, validatorSet)
		if err != nil {
			return fmt.Errorf("slot %d validate reward certs: %w", block.Slot, err)
		}
		if validated == nil {
			if block.FromLightbringer {
				mlog.Log.Infof(
					"cavey debug: vote rewards skipped slot=%d skip_len=%d notar_len=%d reason=validate_returned_nil",
					block.Slot, len(skipRaw), len(notarRaw),
				) // cavey TODO: remove once we are done debugging.
			}
		} else {
			rewardValidators = validated.Validators
			rewardSlot = validated.RewardSlot
		}

		inflationAcct, err := loadEpochInflationAccountState(acctsDb, block.Slot)
		if err != nil {
			return fmt.Errorf("slot %d vote rewards: %w", block.Slot, err)
		}
		var okInflation bool
		inflationState, okInflation = inflationAcct.epochState(rewardEpoch)
		if !okInflation {
			return fmt.Errorf("slot %d vote rewards: missing epoch inflation for reward epoch %d", block.Slot, rewardEpoch)
		}

		migrationEpoch, err = alpenglowMigrationEpoch(block, epochSchedule)
		if err != nil {
			return fmt.Errorf("slot %d vote rewards: %w", block.Slot, err)
		}

		leaderVote, leaderVoteOK = leaderVotePubkey(rewardEpoch, block.Leader)
		if !leaderVoteOK {
			return fmt.Errorf("slot %d vote rewards: leader vote account not found for %s", block.Slot, block.Leader)
		}

		rewardEpochStakes = global.EpochStakes(rewardEpoch)
		totalStake = global.EpochTotalStake(rewardEpoch)
		if totalStake == 0 {
			for _, stake := range rewardEpochStakes {
				totalStake += stake
			}
		}
	}

	var finalSigners map[solana.PublicKey]struct{}
	var finalSlot uint64
	if len(finalCertRaw) > 0 {
		fc, err := rewardcerts.DecodeFinalCertificate(finalCertRaw)
		if err != nil {
			return fmt.Errorf("slot %d decode final cert: %w", block.Slot, err)
		}
		finalEpoch := epochSchedule.GetEpoch(fc.Slot)
		finalValidatorSet, err := buildValidatorSetForEpoch(finalEpoch)
		if err != nil {
			return fmt.Errorf("slot %d final cert: %w", block.Slot, err)
		}
		validatedFinal, err := rewardcerts.ValidateBlockFinalCertificate(finalCertRaw, finalValidatorSet)
		if err != nil {
			return fmt.Errorf("slot %d validate final cert: %w", block.Slot, err)
		}
		if validatedFinal != nil {
			finalSigners = validatedFinal.Signers
			finalSlot = validatedFinal.FinalSlot
		}
	}

	if len(rewardValidators) == 0 && len(finalSigners) == 0 {
		return nil
	}

	currentEpoch := block.Epoch
	var leaderRewardAccum uint64
	var voteAccountsUpdated int

	for votePubkey := range unionVotePubkeys(rewardValidators, finalSigners) {
		// Read the live in-slot account (reflecting same-slot transaction writes such as a
		// Vote Withdraw) and fall back to the persisted parent state otherwise. Rewards are
		// then a normal in-slot account write: SetAccount + RecordModifiedAcct, and the
		// end-of-slot batch lt-hash computes -h(parent)+h(final) once from the true parent.
		acct, err := slotCtx.GetAccountLiveOrPersisted(votePubkey)
		if err != nil {
			mlog.Log.Infof("slot %d vote rewards: skip missing vote account %s: %v", block.Slot, votePubkey, err)
			continue
		}
		_, inReward := rewardValidators[votePubkey]
		_, inFinal := finalSigners[votePubkey]

		var applied bool
		if inReward {
			applied, err = applyVoteRewardToAccount(
				acct,
				rewardSlot,
				currentEpoch,
				migrationEpoch,
				inflationState,
				totalStake,
				rewardEpochStakes[votePubkey],
				&leaderRewardAccum,
			)
			if err != nil {
				mlog.Log.Infof("slot %d vote rewards: skip %s: %v", block.Slot, votePubkey, err)
				continue
			}
		}
		if inFinal {
			if err := applyFinalCertToAccount(acct, finalSlot); err != nil {
				mlog.Log.Infof("slot %d vote rewards: skip %s: %v", block.Slot, votePubkey, err)
				continue
			}
			applied = true
		}
		if !applied {
			continue
		}
		if err := slotCtx.SetAccount(votePubkey, acct); err != nil {
			return fmt.Errorf("slot %d vote rewards: set account %s: %w", block.Slot, votePubkey, err)
		}
		slotCtx.RecordModifiedAcct(votePubkey)
		voteAccountsUpdated++
	}

	if leaderRewardAccum > 0 {
		if !leaderVoteOK {
			return fmt.Errorf("slot %d vote rewards: leader vote account not found for %s", block.Slot, block.Leader)
		}
		// Read the live account so the leader reward stacks on top of any update the union loop
		// (or a same-slot transaction) already applied to the leader's vote account, mirroring
		// Agave's single-map Entry::Occupied/Vacant handling.
		_, leaderAlreadyUpdated := slotCtx.ModifiedAccts[leaderVote]
		acct, err := slotCtx.GetAccountLiveOrPersisted(leaderVote)
		if err != nil {
			return fmt.Errorf("slot %d vote rewards: load leader vote %s: %w", block.Slot, leaderVote, err)
		}
		applied, err := applyLeaderVoteReward(acct, currentEpoch, migrationEpoch, leaderRewardAccum)
		if err != nil {
			return fmt.Errorf("slot %d vote rewards: leader vote %s: %w", block.Slot, leaderVote, err)
		}
		if !applied {
			return fmt.Errorf("slot %d vote rewards: leader vote %s: apply returned false", block.Slot, leaderVote)
		}
		if err := slotCtx.SetAccount(leaderVote, acct); err != nil {
			return fmt.Errorf("slot %d vote rewards: set leader vote %s: %w", block.Slot, leaderVote, err)
		}
		slotCtx.RecordModifiedAcct(leaderVote)
		if !leaderAlreadyUpdated {
			voteAccountsUpdated++
		}
		if block.FromLightbringer {
			mlog.Log.Infof(
				"cavey debug: leader vote reward slot=%d leader_vote=%s already_updated=%t leader_reward=%d",
				block.Slot, leaderVote, leaderAlreadyUpdated, leaderRewardAccum,
			) // cavey TODO: remove once we are done debugging.
		}
	}

	if block.FromLightbringer {
		mlog.Log.Infof(
			"cavey debug: vote rewards applied slot=%d reward_slot=%d reward_epoch=%d skip_len=%d notar_len=%d final_len=%d validated_validators=%d final_signers=%d vote_accounts_updated=%d leader_reward_accum=%d",
			block.Slot,
			rewardSlot,
			rewardEpoch,
			len(skipRaw),
			len(notarRaw),
			len(finalCertRaw),
			len(rewardValidators),
			len(finalSigners),
			voteAccountsUpdated,
			leaderRewardAccum,
		) // cavey TODO: remove once we are done debugging.
	}

	return nil
}

func unionVotePubkeys(rewardValidators, finalSigners map[solana.PublicKey]struct{}) map[solana.PublicKey]struct{} {
	if len(rewardValidators) == 0 {
		return finalSigners
	}
	if len(finalSigners) == 0 {
		return rewardValidators
	}
	out := make(map[solana.PublicKey]struct{}, len(rewardValidators)+len(finalSigners))
	for pk := range rewardValidators {
		out[pk] = struct{}{}
	}
	for pk := range finalSigners {
		out[pk] = struct{}{}
	}
	return out
}

func buildValidatorSetForEpoch(epoch uint64) (alpenglow.ValidatorSet, error) {
	stakes := global.EpochStakes(epoch)
	if len(stakes) == 0 {
		return alpenglow.ValidatorSet{}, fmt.Errorf("missing epoch stakes for epoch %d", epoch)
	}
	return alpenglow.BuildValidatorSet(epoch, stakes, global.EpochStakesVoteAccts(epoch), global.EpochTotalStake(epoch))
}

func alpenglowMigrationEpoch(block *b.Block, epochSchedule *sealevel.SysvarEpochSchedule) (uint64, error) {
	if block.Features == nil {
		return 0, fmt.Errorf("missing feature set")
	}
	slot, ok := block.Features.ActivationSlot(features.Alpenglow)
	if !ok {
		return 0, fmt.Errorf("alpenglow feature not active")
	}
	return epochSchedule.GetEpoch(slot), nil
}

func leaderVotePubkey(epoch uint64, leaderNode solana.PublicKey) (solana.PublicKey, bool) {
	for pk, va := range global.EpochStakesVoteAccts(epoch) {
		if va.NodePubkey == leaderNode {
			return pk, true
		}
	}
	return solana.PublicKey{}, false
}

func applyVoteRewardToAccount(
	acct *accounts.Account,
	rewardSlot, currentEpoch, migrationEpoch uint64,
	inflation EpochInflationState,
	totalStake, validatorStake uint64,
	leaderRewardAccum *uint64,
) (bool, error) {
	versioned, err := sealevel.UnmarshalVersionedVoteState(acct.Data)
	if err != nil {
		return false, err
	}
	if versioned.Type != sealevel.VoteStateVersionV4 {
		return false, fmt.Errorf("unsupported vote state version %d", versioned.Type)
	}

	maybeUpdateVotesV4(&versioned.V4, rewardSlot)
	validatorReward, leaderReward := calculateAlpenglowReward(inflation, totalStake, validatorStake)
	*leaderRewardAccum += leaderReward
	if validatorReward > 0 {
		incrementAlpenglowCredits(&versioned.V4.EpochCredits, migrationEpoch, currentEpoch, validatorReward)
	}

	if err := sealevel.WriteVersionedVoteStateInPlace(acct.Data, versioned); err != nil {
		return false, err
	}
	return true, nil
}

func applyLeaderVoteReward(
	acct *accounts.Account,
	currentEpoch, migrationEpoch, leaderReward uint64,
) (bool, error) {
	versioned, err := sealevel.UnmarshalVersionedVoteState(acct.Data)
	if err != nil {
		return false, err
	}
	if versioned.Type != sealevel.VoteStateVersionV4 {
		return false, fmt.Errorf("unsupported vote state version %d", versioned.Type)
	}
	incrementAlpenglowCredits(&versioned.V4.EpochCredits, migrationEpoch, currentEpoch, leaderReward)
	if err := sealevel.WriteVersionedVoteStateInPlace(acct.Data, versioned); err != nil {
		return false, err
	}
	return true, nil
}

func maybeUpdateVotesV4(vs *sealevel.VoteState4, slot uint64) {
	latest := slot
	if last, ok := lastVotedSlotV4(vs); ok && last > latest {
		latest = last
	}
	if vs.RootSlot != nil && *vs.RootSlot > latest {
		latest = *vs.RootSlot
	}
	vs.Votes.Clear()
	vs.Votes.SetBaseCap(1)
	vs.Votes.PushBack(sealevel.LandedVote{
		Latency: 0,
		Lockout: sealevel.VoteLockout{Slot: latest, ConfirmationCount: 1},
	})
}

func applyFinalCertToAccount(acct *accounts.Account, finalSlot uint64) error {
	versioned, err := sealevel.UnmarshalVersionedVoteState(acct.Data)
	if err != nil {
		return err
	}
	if versioned.Type != sealevel.VoteStateVersionV4 {
		return fmt.Errorf("unsupported vote state version %d", versioned.Type)
	}
	maybeUpdateRootV4(&versioned.V4, finalSlot)
	maybeUpdateVotesV4(&versioned.V4, finalSlot)
	return sealevel.WriteVersionedVoteStateInPlace(acct.Data, versioned)
}

func maybeUpdateRootV4(vs *sealevel.VoteState4, slot uint64) {
	latestRoot := slot
	if vs.RootSlot != nil && *vs.RootSlot > latestRoot {
		latestRoot = *vs.RootSlot
	}
	vs.RootSlot = &latestRoot
}

func lastVotedSlotV4(vs *sealevel.VoteState4) (uint64, bool) {
	if vs.Votes.Len() == 0 {
		return 0, false
	}
	return vs.Votes.At(vs.Votes.Len() - 1).Lockout.Slot, true
}

func calculateAlpenglowReward(inflation EpochInflationState, totalStake, validatorStake uint64) (validatorReward, leaderReward uint64) {
	if totalStake == 0 || validatorStake == 0 || inflation.SlotsPerEpoch == 0 {
		return 0, 0
	}
	num := new(big.Int).SetUint64(inflation.MaxPossibleValidatorReward)
	num.Mul(num, new(big.Int).SetUint64(validatorStake))
	den := new(big.Int).SetUint64(inflation.SlotsPerEpoch)
	den.Mul(den, new(big.Int).SetUint64(totalStake))
	if den.Sign() == 0 {
		return 0, 0
	}
	rewardLamports := new(big.Int).Div(num, den).Uint64()
	validatorReward = rewardLamports / 2
	leaderReward = rewardLamports - validatorReward
	return validatorReward, leaderReward
}

func incrementAlpenglowCredits(epochCredits *[]sealevel.EpochCredits, migrationEpoch, epoch, newCredits uint64) {
	if newCredits == 0 {
		return
	}
	if epoch == migrationEpoch {
		ensureMigrationMarker(epochCredits)
	}

	if len(*epochCredits) == 0 {
		*epochCredits = append(*epochCredits, sealevel.EpochCredits{Epoch: epoch, Credits: newCredits, PrevCredits: 0})
		return
	}

	last := &(*epochCredits)[len(*epochCredits)-1]
	if isMigrationMarker(*last) {
		var finalTowerCredits uint64
		if len(*epochCredits) >= 2 {
			prev := (*epochCredits)[len(*epochCredits)-2]
			finalTowerCredits = prev.Credits
		}
		*epochCredits = append(*epochCredits, sealevel.EpochCredits{
			Epoch:       epoch,
			Credits:     newCredits + finalTowerCredits,
			PrevCredits: finalTowerCredits,
		})
		trimEpochCreditsHistory(epochCredits)
		return
	}

	if last.Epoch == epoch {
		last.Credits += newCredits
		return
	}

	if last.Credits == last.PrevCredits {
		last.Epoch = epoch
		last.Credits += newCredits
		return
	}

	finalCredits := last.Credits
	*epochCredits = append(*epochCredits, sealevel.EpochCredits{
		Epoch:       epoch,
		Credits:     newCredits + finalCredits,
		PrevCredits: finalCredits,
	})
	trimEpochCreditsHistory(epochCredits)
}

func ensureMigrationMarker(epochCredits *[]sealevel.EpochCredits) {
	for i := len(*epochCredits) - 1; i >= 0; i-- {
		if isMigrationMarker((*epochCredits)[i]) {
			return
		}
	}
	*epochCredits = append(*epochCredits, agMigrationEpochCredit)
}

func isMigrationMarker(entry sealevel.EpochCredits) bool {
	return entry.Epoch == agMigrationEpochCredit.Epoch &&
		entry.Credits == agMigrationEpochCredit.Credits &&
		entry.PrevCredits == agMigrationEpochCredit.PrevCredits
}

func trimEpochCreditsHistory(epochCredits *[]sealevel.EpochCredits) {
	for len(*epochCredits) > sealevel.MaxEpochCreditsHistory {
		*epochCredits = (*epochCredits)[1:]
	}
}
