package fees

import (
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	a "github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/safemath"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
)

// ParentAccountLoader resolves an account from parent-slot state when it is not
// already present in the in-slot working set. Replay passes a speculative-store
// aware loader so fee deposits see unfinalized parent balances.
type ParentAccountLoader func(pk solana.PublicKey) (*accounts.Account, error)

// TxFeeDistribution is the result of routing collected tx fees at end-of-slot.
type TxFeeDistribution struct {
	LamportsBurnt uint64
	FeeCollector  solana.PublicKey // set when deposit succeeded; zero when burned instead
}

// DistributeTxFeesToSlotLeader routes execution/priority fees at end of slot.
// When SIMD-0232 (CustomCommissionCollector) is active, deposit goes to the leader vote
// account's block_revenue_collector; otherwise the leader identity receives fees.
// loadParent may be nil; when set, it is used instead of AccountsDB for accounts
// missing from the in-slot working set (needed under speculative replay).
func DistributeTxFeesToSlotLeader(
	acctsDb *accountsdb.AccountsDb,
	slotCtx *sealevel.SlotCtx,
	leaderNode solana.PublicKey,
	txFeeAccumulator *TxFeeInfoAccumulator,
	loadParent ParentAccountLoader,
) TxFeeDistribution {
	var feesToBurn uint64
	var feesToDeposit uint64

	if slotCtx.Features.IsActive(features.RewardFullPriorityFee) {
		halfFee := txFeeAccumulator.ExecutionFees / 2
		feesToDeposit = safemath.SaturatingAddU64(txFeeAccumulator.PriorityFees, txFeeAccumulator.ExecutionFees-halfFee)
		feesToBurn = halfFee
	} else {
		feesToBurn = txFeeAccumulator.TotalFees / 2
		feesToDeposit = txFeeAccumulator.TotalFees - feesToBurn
	}

	if feesToDeposit == 0 {
		return TxFeeDistribution{LamportsBurnt: feesToBurn}
	}

	collector, leaderVote := resolveTxFeeCollector(slotCtx, acctsDb, leaderNode, loadParent)
	deposited, err := depositTxFees(acctsDb, slotCtx, collector, leaderVote, feesToDeposit, loadParent)
	if err != nil {
		feesToBurn = safemath.SaturatingAddU64(feesToBurn, feesToDeposit)
		return TxFeeDistribution{LamportsBurnt: feesToBurn}
	}

	return TxFeeDistribution{
		LamportsBurnt: feesToBurn,
		FeeCollector:  deposited,
	}
}

func resolveTxFeeCollector(
	slotCtx *sealevel.SlotCtx,
	acctsDb *accountsdb.AccountsDb,
	leaderNode solana.PublicKey,
	loadParent ParentAccountLoader,
) (collector solana.PublicKey, leaderVote solana.PublicKey) {
	collector = leaderNode
	if slotCtx.Features == nil || !slotCtx.Features.IsActive(features.CustomCommissionCollector) {
		return collector, solana.PublicKey{}
	}

	votePubkey, ok := leaderVotePubkeyForEpoch(slotCtx.Epoch, leaderNode)
	if !ok {
		return collector, solana.PublicKey{}
	}
	leaderVote = votePubkey

	if revenueCollector, ok := blockRevenueCollectorForVote(slotCtx, acctsDb, votePubkey, loadParent); ok && !revenueCollector.IsZero() {
		collector = revenueCollector
	}
	return collector, leaderVote
}

func leaderVotePubkeyForEpoch(epoch uint64, leaderNode solana.PublicKey) (solana.PublicKey, bool) {
	for pk, va := range global.EpochStakesVoteAccts(epoch) {
		if va.NodePubkey == leaderNode {
			return pk, true
		}
	}
	return solana.PublicKey{}, false
}

func blockRevenueCollectorForVote(
	slotCtx *sealevel.SlotCtx,
	acctsDb *accountsdb.AccountsDb,
	votePubkey solana.PublicKey,
	loadParent ParentAccountLoader,
) (solana.PublicKey, bool) {
	acct, _, err := loadFeeAccount(slotCtx, acctsDb, votePubkey, loadParent)
	if err != nil {
		return solana.PublicKey{}, false
	}

	versioned, err := sealevel.UnmarshalVersionedVoteState(acct.Data)
	if err != nil || versioned.Type != sealevel.VoteStateVersionV4 {
		return solana.PublicKey{}, false
	}
	return versioned.V4.BlockRevenueCollector, true
}

func loadFeeAccount(
	slotCtx *sealevel.SlotCtx,
	acctsDb *accountsdb.AccountsDb,
	pk solana.PublicKey,
	loadParent ParentAccountLoader,
) (*accounts.Account, bool, error) {
	if acct, err := slotCtx.GetAccount(pk); err == nil {
		return acct, true, nil
	}
	if loadParent != nil {
		acct, err := loadParent(pk)
		if err != nil {
			return nil, false, err
		}
		return acct, false, nil
	}
	if acctsDb != nil {
		acct, err := acctsDb.GetAccount(slotCtx.Slot, pk)
		if err == nil {
			return acct, false, nil
		}
	}
	return &accounts.Account{Key: pk, Owner: a.SystemProgramAddr}, false, nil
}

func depositTxFees(
	acctsDb *accountsdb.AccountsDb,
	slotCtx *sealevel.SlotCtx,
	collector solana.PublicKey,
	leaderVote solana.PublicKey,
	amount uint64,
	loadParent ParentAccountLoader,
) (solana.PublicKey, error) {
	acct, fromWorkingSet, err := loadFeeAccount(slotCtx, acctsDb, collector, loadParent)
	if err != nil {
		acct = &accounts.Account{Key: collector, Owner: a.SystemProgramAddr}
		fromWorkingSet = false
	}
	if !fromWorkingSet {
		if slotCtx.ParentAccts != nil {
			_ = slotCtx.ParentAccts.SetAccountWithoutLock(collector, acct.Clone())
		}
		acct = acct.Clone()
	}

	if err := validateFeeCollector(collector, leaderVote, acct); err != nil {
		return solana.PublicKey{}, err
	}

	newLamports, err := safemath.CheckedAddU64(acct.Lamports, amount)
	if err != nil {
		return solana.PublicKey{}, fmt.Errorf("lamport overflow: %w", err)
	}
	acct.Lamports = newLamports

	if err := slotCtx.SetAccount(collector, acct); err != nil {
		return solana.PublicKey{}, err
	}
	return collector, nil
}

func validateFeeCollector(collector, leaderVote solana.PublicKey, acct *accounts.Account) error {
	if collector == leaderVote {
		return nil
	}
	if acct.Owner != a.SystemProgramAddr {
		return fmt.Errorf("invalid fee collector owner %s", acct.Owner)
	}
	return nil
}
