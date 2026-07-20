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
	Generation                    uint64
	Slot                          uint64
	Bankhash                      solana.Hash
	AlpenglowBlockID              solana.Hash
	HasAlpenglowBlockID           bool
	AlpenglowChainedMerkleRoot    solana.Hash
	HasAlpenglowChainedMerkleRoot bool
	AcctsLtHash                   *lthash.LtHash
	Features                      *features.Features
	TransactionStatuses           *TransactionStatusView
	PrevNumSigs                   uint64
	PrevFeeGovernor               *sealevel.FeeRateGovernor
	LastEntryHash                 solana.Hash
	EpochRewardsActive            bool
	UnrootedRead                  sealevel.AccountReader
}

// ChainTipIdentity is the Alpenglow identity committed by the same replay
// transition as the parent bank, accounts view, and transaction-status view.
// A producer must never recover these fields from process-wide maps after it
// has selected a ChainTipSnapshot: those maps may already describe another
// fork generation.
type ChainTipIdentity struct {
	AlpenglowBlockID              solana.Hash
	HasAlpenglowBlockID           bool
	AlpenglowChainedMerkleRoot    solana.Hash
	HasAlpenglowChainedMerkleRoot bool
}

var (
	chainTipMu                            sync.RWMutex
	chainTipGeneration                    uint64
	chainTipSlot                          uint64
	chainTipBankhash                      solana.Hash
	chainTipAlpenglowBlockID              solana.Hash
	chainTipHasAlpenglowBlockID           bool
	chainTipAlpenglowChainedMerkleRoot    solana.Hash
	chainTipHasAlpenglowChainedMerkleRoot bool
	chainTipAcctsLtHash                   *lthash.LtHash
	chainTipFeatures                      *features.Features
	chainTipTransactionStatuses           *TransactionStatusView
	chainTipPrevNumSigs                   uint64
	chainTipLastEntryHash                 solana.Hash
	chainTipEpochRewardsActive            bool
	chainTipPrevFeeGovernor               *sealevel.FeeRateGovernor
	chainTipUnrootedRead                  sealevel.AccountReader
)

// InitChainTip seeds blockprod parent context before the first ProcessBlock.
func InitChainTip(acctsLtHash *lthash.LtHash, f *features.Features, prevNumSigs uint64, lastEntryHash solana.Hash, statusViews ...*TransactionStatusView) {
	chainTipMu.Lock()
	defer chainTipMu.Unlock()
	chainTipGeneration++
	chainTipSlot = 0
	chainTipBankhash = solana.Hash{}
	chainTipAlpenglowBlockID = solana.Hash{}
	chainTipHasAlpenglowBlockID = false
	chainTipAlpenglowChainedMerkleRoot = solana.Hash{}
	chainTipHasAlpenglowChainedMerkleRoot = false
	chainTipAcctsLtHash = nil
	chainTipFeatures = nil
	chainTipTransactionStatuses = nil
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
	if len(statusViews) > 0 {
		chainTipTransactionStatuses = statusViews[0]
	}
}

// ResetChainTip invalidates producer parent state while replay is rewinding or
// restarting. The next successfully replayed block installs a fresh snapshot.
func ResetChainTip() {
	InitChainTip(nil, nil, 0, solana.Hash{})
}

// UpdateChainTipFromSlotCtx refreshes blockprod parent context from replay progress.
func UpdateChainTipFromSlotCtx(slotCtx *sealevel.SlotCtx, f *features.Features, statuses *TransactionStatusView, identity ChainTipIdentity, readers ...sealevel.AccountReader) {
	if slotCtx == nil {
		return
	}
	chainTipMu.Lock()
	defer chainTipMu.Unlock()
	chainTipGeneration++
	chainTipSlot = slotCtx.Slot
	chainTipBankhash = solana.Hash{}
	if len(slotCtx.FinalBankhash) == 32 {
		chainTipBankhash = solana.HashFromBytes(slotCtx.FinalBankhash)
	}
	chainTipAlpenglowBlockID = identity.AlpenglowBlockID
	chainTipHasAlpenglowBlockID = identity.HasAlpenglowBlockID
	chainTipAlpenglowChainedMerkleRoot = identity.AlpenglowChainedMerkleRoot
	chainTipHasAlpenglowChainedMerkleRoot = identity.HasAlpenglowChainedMerkleRoot
	chainTipUnrootedRead = nil
	chainTipAcctsLtHash = nil
	chainTipFeatures = nil
	chainTipLastEntryHash = solana.Hash{}
	chainTipPrevFeeGovernor = nil
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
	// TransactionStatusView is immutable after publication. Retain this exact
	// parent-lineage view so a concurrently opened producer bank is insulated
	// from subsequent replay commits or fork unwinds.
	chainTipTransactionStatuses = statuses
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
		Generation:                    chainTipGeneration,
		Slot:                          chainTipSlot,
		Bankhash:                      chainTipBankhash,
		AlpenglowBlockID:              chainTipAlpenglowBlockID,
		HasAlpenglowBlockID:           chainTipHasAlpenglowBlockID,
		AlpenglowChainedMerkleRoot:    chainTipAlpenglowChainedMerkleRoot,
		HasAlpenglowChainedMerkleRoot: chainTipHasAlpenglowChainedMerkleRoot,
		PrevNumSigs:                   chainTipPrevNumSigs,
		LastEntryHash:                 chainTipLastEntryHash,
		EpochRewardsActive:            chainTipEpochRewardsActive,
		UnrootedRead:                  chainTipUnrootedRead,
		TransactionStatuses:           chainTipTransactionStatuses,
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
