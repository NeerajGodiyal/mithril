package blockprod

import (
	"sync"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/lthash"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
)

// ParentContext carries the parent bank metadata needed to commit a leader slot.
type ParentContext struct {
	ParentSlot          uint64
	ParentBankhash      solana.Hash
	ParentLastEntryHash solana.Hash
	EpochRewardsActive  bool
	PrevNumSigs         uint64
	PrevFeeGovernor     *sealevel.FeeRateGovernor
	AcctsLtHash         *lthash.LtHash
	Features            *features.Features
	UnrootedRead        sealevel.AccountReader
}

// NewLeaderSlotCtx builds a forge-ready slot context at the chain tip.
func NewLeaderSlotCtx(slot, parentSlot uint64, acctsDb *accountsdb.AccountsDb, parent ParentContext, epochSchedule *sealevel.SysvarEpochSchedule) (*sealevel.SlotCtx, error) {
	feats := leaderFeatures(parent.Features)

	var epoch uint64
	if epochSchedule != nil {
		epoch = epochSchedule.GetEpoch(slot)
	}

	lastBlockhash := global.LatestBlockHash()
	prevFee := parent.PrevFeeGovernor
	if prevFee == nil {
		prevFee = &sealevel.FeeRateGovernor{PrevLamportsPerSignature: 5000, LamportsPerSignature: 5000}
	}
	feeGovernor := sealevel.NewFeeRateGovernorDerived(prevFee, parent.PrevNumSigs)
	if feeGovernor.PrevLamportsPerSignature == 0 {
		feeGovernor.PrevLamportsPerSignature = 5000
	}

	slotCtx := &sealevel.SlotCtx{
		Slot:            slot,
		ParentSlot:      parentSlot,
		Epoch:           epoch,
		Accounts:        accounts.NewMemAccounts(),
		ParentAccts:     accounts.NewMemAccounts(),
		AccountsDb:      acctsDb,
		UnrootedRead:    parent.UnrootedRead,
		Features:        feats,
		FeeRateGovernor: feeGovernor,
		LastBlockhash:   lastBlockhash,
		AcctMapsMu:      &sync.Mutex{},
		ModifiedAccts:   make(map[solana.PublicKey]bool),
		WritableAccts:   make(map[solana.PublicKey]bool),
		NumSignatures:   parent.PrevNumSigs,
	}
	if parent.AcctsLtHash != nil {
		slotCtx.AcctsLtHash = parent.AcctsLtHash.Clone()
	}

	if sealevel.SysvarCache.Rent.Sysvar == nil {
		rent := sealevel.NewDefaultRentSysvar()
		sealevel.SysvarCache.Rent.Sysvar = &rent
	}
	return slotCtx, nil
}

func leaderFeatures(parent *features.Features) *features.Features {
	feats := features.NewFeaturesDefault()
	feats.EnableFeature(features.FormalizeLoadedTransactionDataSize, 0)
	if parent == nil {
		return feats
	}
	for gate, info := range *parent {
		if info.Enabled {
			feats.EnableFeature(gate, info.ActivationSlot)
		}
	}
	return feats
}
