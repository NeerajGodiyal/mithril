package blockprod

import (
	"sync"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
)

// NewLeaderSlotCtx builds a forge-ready slot context at the chain tip.
func NewLeaderSlotCtx(slot, parentSlot uint64, acctsDb *accountsdb.AccountsDb) (*sealevel.SlotCtx, error) {
	feats := features.NewFeaturesDefault()
	feats.EnableFeature(features.FormalizeLoadedTransactionDataSize, 0)

	lastBlockhash := global.LatestBlockHash()
	slotCtx := &sealevel.SlotCtx{
		Slot:            slot,
		ParentSlot:      parentSlot,
		Accounts:        accounts.NewMemAccounts(),
		AccountsDb:      acctsDb,
		Features:        feats,
		FeeRateGovernor: &sealevel.FeeRateGovernor{PrevLamportsPerSignature: 5000},
		LastBlockhash:   lastBlockhash,
		AcctMapsMu:      &sync.Mutex{},
		ModifiedAccts:   make(map[solana.PublicKey]bool),
		WritableAccts:   make(map[solana.PublicKey]bool),
	}

	if sealevel.SysvarCache.Rent.Sysvar == nil {
		rent := sealevel.NewDefaultRentSysvar()
		sealevel.SysvarCache.Rent.Sysvar = &rent
	}
	if sealevel.SysvarCache.RecentBlockHashes.Sysvar == nil {
		rbh := sealevel.SysvarRecentBlockhashes{{
			Blockhash:     lastBlockhash,
			FeeCalculator: sealevel.FeeCalculator{LamportsPerSignature: 5000},
		}}
		sealevel.SysvarCache.RecentBlockHashes.Sysvar = &rbh
	}
	return slotCtx, nil
}

// DefaultBankHash returns the working bank hash used in Alpenglow footers.
func DefaultBankHash(bank *WorkingBank) solana.Hash {
	if bank == nil || bank.SlotCtx() == nil {
		return solana.Hash{}
	}
	return bank.SlotCtx().LastBlockhash
}
