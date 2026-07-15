package replay

import (
	"sync"

	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/lthash"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
)

// ChainTipSnapshot carries replay chain metadata for local block production.
type ChainTipSnapshot struct {
	Slot               uint64
	Bankhash           solana.Hash
	AcctsLtHash        *lthash.LtHash
	Features           *features.Features
	PrevNumSigs        uint64
	PrevFeeGovernor    *sealevel.FeeRateGovernor
	LastEntryHash      solana.Hash
	EpochRewardsActive bool
	UnrootedRead       sealevel.AccountReader
}

var (
	chainTipMu                 sync.RWMutex
	chainTipSlot               uint64
	chainTipBankhash           solana.Hash
	chainTipAcctsLtHash        *lthash.LtHash
	chainTipFeatures           *features.Features
	chainTipPrevNumSigs        uint64
	chainTipLastEntryHash      solana.Hash
	chainTipEpochRewardsActive bool
	chainTipPrevFeeGovernor    *sealevel.FeeRateGovernor
	chainTipUnrootedRead       sealevel.AccountReader
)

// InitChainTip seeds blockprod parent context before the first ProcessBlock.
func InitChainTip(acctsLtHash *lthash.LtHash, f *features.Features, prevNumSigs uint64, lastEntryHash solana.Hash) {
	chainTipMu.Lock()
	defer chainTipMu.Unlock()
	chainTipSlot = 0
	chainTipBankhash = solana.Hash{}
	chainTipAcctsLtHash = nil
	chainTipFeatures = nil
	chainTipPrevNumSigs = 0
	chainTipLastEntryHash = solana.Hash{}
	chainTipEpochRewardsActive = false
	chainTipPrevFeeGovernor = nil
	chainTipUnrootedRead = nil
	if acctsLtHash != nil {
		chainTipAcctsLtHash = acctsLtHash.Clone()
	}
	if f != nil {
		chainTipFeatures = f.Clone()
	}
	chainTipPrevNumSigs = prevNumSigs
	if lastEntryHash != (solana.Hash{}) {
		chainTipLastEntryHash = lastEntryHash
	}
}

// ResetChainTip invalidates producer parent state while replay is rewinding or
// restarting. The next successfully replayed block installs a fresh snapshot.
func ResetChainTip() {
	InitChainTip(nil, nil, 0, solana.Hash{})
}

// UpdateChainTipFromSlotCtx refreshes blockprod parent context from replay progress.
func UpdateChainTipFromSlotCtx(slotCtx *sealevel.SlotCtx, f *features.Features, readers ...sealevel.AccountReader) {
	if slotCtx == nil {
		return
	}
	chainTipMu.Lock()
	defer chainTipMu.Unlock()
	chainTipSlot = slotCtx.Slot
	if len(slotCtx.FinalBankhash) == 32 {
		chainTipBankhash = solana.HashFromBytes(slotCtx.FinalBankhash)
	}
	if len(readers) > 0 {
		chainTipUnrootedRead = readers[0]
	} else if slotCtx.UnrootedRead != nil {
		chainTipUnrootedRead = slotCtx.UnrootedRead
	}
	if slotCtx.AcctsLtHash != nil {
		chainTipAcctsLtHash = slotCtx.AcctsLtHash.Clone()
	}
	if f != nil {
		chainTipFeatures = f.Clone()
	}
	chainTipPrevNumSigs = slotCtx.NumSignatures
	if slotCtx.Blockhash != ([32]byte{}) {
		chainTipLastEntryHash = solana.Hash(slotCtx.Blockhash)
	}
	if rewards := sealevel.SysvarCache.EpochRewards.Sysvar; rewards != nil {
		chainTipEpochRewardsActive = rewards.Active
	} else {
		chainTipEpochRewardsActive = false
	}
	// Carry the parent slot's fully-populated (derived) fee rate governor so the
	// next leader block derives the correct lamports_per_signature for the head
	// RecentBlockhashes entry. A partial governor with zeroed Target* fields would
	// derive lamports_per_signature=0 and diverge from Agave's bank hash.
	if slotCtx.FeeRateGovernor != nil {
		gov := *slotCtx.FeeRateGovernor
		chainTipPrevFeeGovernor = &gov
	}
}

// ChainTipParentContext returns a snapshot of replay chain metadata for leader forging.
func ChainTipParentContext() ChainTipSnapshot {
	chainTipMu.RLock()
	defer chainTipMu.RUnlock()
	ctx := ChainTipSnapshot{
		Slot:               chainTipSlot,
		Bankhash:           chainTipBankhash,
		PrevNumSigs:        chainTipPrevNumSigs,
		LastEntryHash:      chainTipLastEntryHash,
		EpochRewardsActive: chainTipEpochRewardsActive,
		UnrootedRead:       chainTipUnrootedRead,
	}
	if chainTipAcctsLtHash != nil {
		ctx.AcctsLtHash = chainTipAcctsLtHash.Clone()
	}
	if chainTipFeatures != nil {
		ctx.Features = chainTipFeatures.Clone()
	}
	if chainTipPrevFeeGovernor != nil {
		gov := *chainTipPrevFeeGovernor
		ctx.PrevFeeGovernor = &gov
	}
	return ctx
}
