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
	AcctsLtHash     *lthash.LtHash
	Features        *features.Features
	PrevNumSigs     uint64
	PrevFeeGovernor *sealevel.FeeRateGovernor
	LastEntryHash   solana.Hash
}

var (
	chainTipMu              sync.RWMutex
	chainTipAcctsLtHash     *lthash.LtHash
	chainTipFeatures        *features.Features
	chainTipPrevNumSigs     uint64
	chainTipLastEntryHash   solana.Hash
	chainTipPrevFeeGovernor *sealevel.FeeRateGovernor
)

// InitChainTip seeds blockprod parent context before the first ProcessBlock.
func InitChainTip(acctsLtHash *lthash.LtHash, f *features.Features, prevNumSigs uint64, lastEntryHash solana.Hash) {
	chainTipMu.Lock()
	defer chainTipMu.Unlock()
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

// UpdateChainTipFromSlotCtx refreshes blockprod parent context from replay progress.
func UpdateChainTipFromSlotCtx(slotCtx *sealevel.SlotCtx, f *features.Features) {
	if slotCtx == nil {
		return
	}
	chainTipMu.Lock()
	defer chainTipMu.Unlock()
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
		PrevNumSigs:   chainTipPrevNumSigs,
		LastEntryHash: chainTipLastEntryHash,
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
