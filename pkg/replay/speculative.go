package replay

import (
	"encoding/base64"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/alpenglow"
	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/base58"
	"github.com/Overclock-Validator/mithril/pkg/blockstream"
	"github.com/Overclock-Validator/mithril/pkg/forkchoice"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/lthash"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/state"
	"github.com/gagliardetto/solana-go"
)

// DeferredBlockCommit holds account and bankhash writes deferred until Alpenglow confirms.
type DeferredBlockCommit struct {
	SlotCtx             *sealevel.SlotCtx
	ModifiedAccts       []*accounts.Account
	BlockSlot           uint64
	BlockHeight         uint64
	Bankhash            []byte
	HasAlpenglowBlockID bool
	AlpenglowBlockID    solana.Hash
}

// ReplayHeadSnapshot captures replay-visible chain head state at a persisted slot.
type ReplayHeadSnapshot struct {
	Slot                   uint64
	ParentSlot             uint64
	BlockHeight            uint64
	FinalBankhash          []byte
	Blockhash              [32]byte
	LatestEvictedBlockhash [32]byte
	NumSignatures          uint64
	AcctsLtHash            *lthash.LtHash
	FeeRateGovernor        *sealevel.FeeRateGovernor
	Capitalization         uint64
	Clock                  *sealevel.SysvarClock
	SlotHashes             *sealevel.SysvarSlotHashes
	RecentBlockhashes      *sealevel.SysvarRecentBlockhashes
}

// SpeculativeReplay defers AccountsDB persistence for turbine blocks until Alpenglow finalizes them.
type SpeculativeReplay struct {
	mu            sync.Mutex
	enabled       bool
	committedSlot uint64
	headSnapshot  *ReplayHeadSnapshot
	pending       map[uint64]*DeferredBlockCommit
	store         *SpeculativeStore
}

func NewSpeculativeReplay() *SpeculativeReplay {
	return &SpeculativeReplay{
		pending: make(map[uint64]*DeferredBlockCommit),
		store:   newSpeculativeStore(),
	}
}

func (sr *SpeculativeReplay) Enable() {
	sr.mu.Lock()
	sr.enabled = true
	sr.mu.Unlock()
}

func (sr *SpeculativeReplay) Enabled() bool {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	return sr.enabled
}

func CaptureHeadSnapshot(slotCtx *sealevel.SlotCtx, replayCtx *ReplayCtx, blockHeight uint64) *ReplayHeadSnapshot {
	if slotCtx == nil || replayCtx == nil {
		return nil
	}
	snapshot := &ReplayHeadSnapshot{
		Slot:                   slotCtx.Slot,
		ParentSlot:             slotCtx.ParentSlot,
		BlockHeight:            blockHeight,
		FinalBankhash:          append([]byte(nil), slotCtx.FinalBankhash...),
		Blockhash:              slotCtx.Blockhash,
		LatestEvictedBlockhash: slotCtx.LatestEvictedBlockhash,
		NumSignatures:          slotCtx.NumSignatures,
		Capitalization:         replayCtx.Capitalization,
	}
	if slotCtx.AcctsLtHash != nil {
		snapshot.AcctsLtHash = slotCtx.AcctsLtHash.Clone()
	}
	if slotCtx.FeeRateGovernor != nil {
		gov := *slotCtx.FeeRateGovernor
		snapshot.FeeRateGovernor = &gov
	}
	if sealevel.SysvarCache.Clock.Sysvar != nil {
		clock := *sealevel.SysvarCache.Clock.Sysvar
		snapshot.Clock = &clock
	}
	if sealevel.SysvarCache.SlotHashes.Sysvar != nil {
		slotHashes := *sealevel.SysvarCache.SlotHashes.Sysvar
		snapshot.SlotHashes = &slotHashes
	}
	if sealevel.SysvarCache.RecentBlockHashes.Sysvar != nil {
		recent := *sealevel.SysvarCache.RecentBlockHashes.Sysvar
		snapshot.RecentBlockhashes = &recent
	}
	return snapshot
}

func (sr *SpeculativeReplay) FinalizedSlot() uint64 {
	return sr.store.FinalizedSlot()
}

func (sr *SpeculativeReplay) LayerCount() int {
	return sr.store.LayerCount()
}

func (sr *SpeculativeReplay) UseStoreForParent(parentSlot uint64) bool {
	if !sr.Enabled() {
		return false
	}
	return sr.store.UseStoreForParent(parentSlot)
}

func (sr *SpeculativeReplay) Resolve(endSlot uint64, pk solana.PublicKey, db *accountsdb.AccountsDb) (*accounts.Account, error) {
	return sr.store.Resolve(endSlot, pk, db)
}

func (sr *SpeculativeReplay) ParentNotPersisted(parentSlot uint64) bool {
	return sr.UseStoreForParent(parentSlot)
}

func (sr *SpeculativeReplay) SeedFromManifest(mithrilState *state.MithrilState, replayCtx *ReplayCtx) {
	if mithrilState == nil || replayCtx == nil {
		return
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()
	if !sr.enabled || mithrilState.ManifestParentSlot == 0 {
		return
	}

	snapshot := &ReplayHeadSnapshot{
		Slot:           mithrilState.ManifestParentSlot,
		BlockHeight:    mithrilState.ManifestBlockHeight,
		NumSignatures:  mithrilState.ManifestSignatureCount,
		Capitalization: replayCtx.Capitalization,
	}
	if mithrilState.ManifestParentBankhash != "" {
		if bankhash, err := base58.DecodeFromString(mithrilState.ManifestParentBankhash); err == nil {
			snapshot.FinalBankhash = append([]byte(nil), bankhash[:]...)
		}
	}
	if mithrilState.ManifestAcctsLtHash != "" {
		if ltHashBytes, err := base64.StdEncoding.DecodeString(mithrilState.ManifestAcctsLtHash); err == nil && len(ltHashBytes) == 32 {
			snapshot.AcctsLtHash = new(lthash.LtHash).InitWithHash(ltHashBytes)
		}
	}
	if mithrilState.ManifestLamportsPerSignature != 0 {
		snapshot.FeeRateGovernor = &sealevel.FeeRateGovernor{
			LamportsPerSignature:     mithrilState.ManifestLamportsPerSignature,
			PrevLamportsPerSignature: mithrilState.ManifestLamportsPerSignature,
		}
	}
	if sealevel.SysvarCache.Clock.Sysvar != nil {
		clock := *sealevel.SysvarCache.Clock.Sysvar
		snapshot.Clock = &clock
	}
	if sealevel.SysvarCache.SlotHashes.Sysvar != nil {
		slotHashes := *sealevel.SysvarCache.SlotHashes.Sysvar
		snapshot.SlotHashes = &slotHashes
	}
	if sealevel.SysvarCache.RecentBlockHashes.Sysvar != nil {
		recent := *sealevel.SysvarCache.RecentBlockHashes.Sysvar
		snapshot.RecentBlockhashes = &recent
	}
	if len(mithrilState.ManifestRecentBlockhashes) > 0 {
		if hash, err := base58.DecodeFromString(mithrilState.ManifestRecentBlockhashes[0].Blockhash); err == nil {
			snapshot.Blockhash = [32]byte(hash)
		}
	}

	sr.committedSlot = mithrilState.ManifestParentSlot
	sr.headSnapshot = snapshot
	sr.store.SetFinalizedSlot(mithrilState.ManifestParentSlot)
	sr.store.Clear()
}

func (sr *SpeculativeReplay) SeedFromResume(resume *ResumeState, replayCtx *ReplayCtx) {
	if resume == nil || replayCtx == nil {
		return
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()
	if !sr.enabled {
		return
	}

	snapshot := &ReplayHeadSnapshot{
		Slot:                   resume.ParentSlot,
		BlockHeight:            resume.ParentBlockHeight,
		FinalBankhash:          append([]byte(nil), resume.ParentBankhash...),
		Blockhash:              resume.LastBlockhash,
		LatestEvictedBlockhash: resume.EvictedBlockhash,
		NumSignatures:          resume.NumSignatures,
		Capitalization:         replayCtx.Capitalization,
	}
	if resume.AcctsLtHash != nil {
		snapshot.AcctsLtHash = resume.AcctsLtHash.Clone()
	}
	if resume.LamportsPerSignature != 0 || resume.PrevLamportsPerSignature != 0 {
		snapshot.FeeRateGovernor = &sealevel.FeeRateGovernor{
			LamportsPerSignature:     resume.LamportsPerSignature,
			PrevLamportsPerSignature: resume.PrevLamportsPerSignature,
		}
	}
	if resume.RecentBlockhashes != nil {
		recent := *resume.RecentBlockhashes
		snapshot.RecentBlockhashes = &recent
	}
	if resume.SlotHashes != nil {
		slotHashes := *resume.SlotHashes
		snapshot.SlotHashes = &slotHashes
	}
	if sealevel.SysvarCache.Clock.Sysvar != nil {
		clock := *sealevel.SysvarCache.Clock.Sysvar
		snapshot.Clock = &clock
	}

	sr.committedSlot = resume.ParentSlot
	sr.headSnapshot = snapshot
	sr.store.SetFinalizedSlot(resume.ParentSlot)
	sr.store.Clear()
}

func (sr *SpeculativeReplay) UpdateCommittedHead(slotCtx *sealevel.SlotCtx, replayCtx *ReplayCtx, blockHeight uint64) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	if !sr.enabled || slotCtx == nil {
		return
	}
	sr.committedSlot = slotCtx.Slot
	sr.headSnapshot = CaptureHeadSnapshot(slotCtx, replayCtx, blockHeight)
	sr.store.SetFinalizedSlot(slotCtx.Slot)
	sr.store.PruneLayersThrough(slotCtx.Slot)
	for slot := range sr.pending {
		if slot <= sr.committedSlot {
			delete(sr.pending, slot)
		}
	}
}

func (sr *SpeculativeReplay) TrackPending(deferred *DeferredBlockCommit) {
	if deferred == nil {
		return
	}
	if err := sr.store.RecordLayer(deferred.BlockSlot, deferred.SlotCtx.ParentSlot, deferred.SlotCtx, deferred.ModifiedAccts); err != nil {
		mlog.Log.Errorf("speculative replay: %v", err)
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sr.pending[deferred.BlockSlot] = deferred
}

func alpenglowCertConfirmsPersist(certType alpenglow.CertificateType) bool {
	return certType.IsFinalization() || certType == alpenglow.CertificateGenesis
}

// TryFlushPending commits consecutive finalized pending slots starting at
// committedSlot+1. It is safe to call before each block is processed, for
// example when Votor certificates arrive while waiting for the next block.
func (sr *SpeculativeReplay) TryFlushPending(
	acctsDb *accountsdb.AccountsDb,
	pt *persistedTracker,
	replayCtx *ReplayCtx,
	decisionSource func(anchorSlot uint64) (alpenglow.ChainDecision, bool),
) int {
	if decisionSource == nil {
		return 0
	}

	sr.mu.Lock()
	defer sr.mu.Unlock()
	if !sr.enabled || len(sr.pending) == 0 {
		return 0
	}
	return sr.flushPendingConfirmedLocked(acctsDb, pt, replayCtx, decisionSource, 0)
}

func (sr *SpeculativeReplay) TryCommitPending(
	acctsDb *accountsdb.AccountsDb,
	pt *persistedTracker,
	block *b.Block,
	blockHeight uint64,
	replayCtx *ReplayCtx,
	decisionSource func(anchorSlot uint64) (alpenglow.ChainDecision, bool),
) bool {
	if block == nil || decisionSource == nil {
		return false
	}

	sr.mu.Lock()
	defer sr.mu.Unlock()
	if !sr.enabled {
		return false
	}
	if _, ok := sr.pending[block.Slot]; !ok {
		return false
	}

	before := sr.committedSlot
	sr.flushPendingConfirmedLocked(acctsDb, pt, replayCtx, decisionSource, block.Slot)
	return sr.committedSlot >= block.Slot && sr.committedSlot > before
}

func (sr *SpeculativeReplay) flushPendingConfirmedLocked(
	acctsDb *accountsdb.AccountsDb,
	pt *persistedTracker,
	replayCtx *ReplayCtx,
	decisionSource func(anchorSlot uint64) (alpenglow.ChainDecision, bool),
	currentSlot uint64,
) int {
	flushed := 0
	anchor := sr.committedSlot
	for {
		decision, ok := decisionSource(anchor)
		if !ok || decision.Kind != alpenglow.ChainDecisionKindBlock {
			break
		}
		pending, ok := sr.pending[decision.Block.Slot]
		if !ok {
			break
		}
		if !alpenglowCertConfirmsPersist(decision.CertificateType) {
			break
		}
		if pending.HasAlpenglowBlockID && decision.Block.Hash != pending.AlpenglowBlockID {
			mlog.Log.Warnf("speculative replay: defer persist at slot %d; Alpenglow block id %s != replayed %s",
				decision.Block.Slot,
				base58.Encode(decision.Block.Hash[:]),
				base58.Encode(pending.AlpenglowBlockID[:]),
			)
			break
		}

		height := pending.BlockHeight
		if height == 0 {
			height = pending.BlockSlot
		}
		if err := sr.commitPendingLocked(acctsDb, pt, pending, replayCtx, height); err != nil {
			mlog.Log.Errorf("speculative replay: failed to persist slot %d after Alpenglow confirmation: %v", decision.Block.Slot, err)
			break
		}
		flushed++
		if currentSlot != 0 && decision.Block.Slot == currentSlot {
			currentSlot = 0
		}
		mlog.Log.Infof("speculative replay: persisted slot %d after Alpenglow %s confirmation", decision.Block.Slot, decision.CertificateType)
		anchor = sr.committedSlot
	}
	return flushed
}

func (sr *SpeculativeReplay) commitPendingLocked(
	acctsDb *accountsdb.AccountsDb,
	pt *persistedTracker,
	pending *DeferredBlockCommit,
	replayCtx *ReplayCtx,
	blockHeight uint64,
) error {
	persistedSlot := pending.SlotCtx.Slot
	persistedBankhash := append([]byte(nil), pending.Bankhash...)
	persistedBlockSlot := pending.BlockSlot
	stakeIndexDir := filepath.Join(acctsDb.AcctsDir, "..")

	afterStoreAccounts := func() {
		if err := acctsDb.StoreBankHashForSlot(persistedSlot, persistedBankhash); err != nil {
			mlog.Log.Infof("unable to store bankhash for slot %d", persistedSlot)
		}
		flushed, err := global.FlushPendingStakePubkeys(stakeIndexDir)
		if err != nil {
			mlog.Log.Errorf("failed to flush stake pubkey index: %v", err)
		} else if flushed > 0 {
			mlog.Log.Debugf("flushed %d new stake pubkeys to index", flushed)
		}
		pt.Set(persistedBlockSlot, persistedBankhash)
	}

	var err error
	if len(pending.ModifiedAccts) > 0 {
		err = acctsDb.StoreAccounts(pending.ModifiedAccts, persistedSlot, afterStoreAccounts)
	} else {
		afterStoreAccounts()
	}
	if err != nil {
		return err
	}

	sr.committedSlot = pending.BlockSlot
	sr.headSnapshot = CaptureHeadSnapshot(pending.SlotCtx, replayCtx, blockHeight)
	sr.store.SetFinalizedSlot(pending.BlockSlot)
	sr.store.PruneLayersThrough(pending.BlockSlot)
	delete(sr.pending, pending.BlockSlot)
	return nil
}

type SpeculativeRollbackParams struct {
	AcctsDb    *accountsdb.AccountsDb
	PT         *persistedTracker
	ReplayCtx  *ReplayCtx
	LastSlotCtx **sealevel.SlotCtx
	BlockStream *blockstream.BlockSource
	ForkChoice  *forkchoice.ForkChoiceService
	RPCServer SlotCtxSetter
}

// HandleParentMismatch rolls back speculative execution when a waiting block's parent
// does not connect to the last emitted slot.
func (sr *SpeculativeReplay) HandleParentMismatch(
	waitingSlot, observedParent, expectedParent uint64,
	params SpeculativeRollbackParams,
) bool {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	if !sr.enabled || observedParent >= expectedParent {
		return false
	}
	if sr.committedSlot > observedParent {
		mlog.Log.Errorf("speculative replay: cannot rollback to slot %d; already persisted through %d",
			observedParent, sr.committedSlot)
		return false
	}

	anchor := observedParent
	if err := sr.rollbackToLocked(anchor, params); err != nil {
		mlog.Log.Errorf("speculative replay: rollback to slot %d failed: %v", anchor, err)
		return false
	}
	params.BlockStream.RollbackEmissionFrontier(anchor, waitingSlot-1)
	global.ClearPendingStakePubkeys()
	mlog.Log.Warnf("speculative replay: parent mismatch at slot %d (observed_parent=%d expected_parent=%d); rolled back to %d",
		waitingSlot, observedParent, expectedParent, anchor)
	return true
}

func (sr *SpeculativeReplay) rollbackToLocked(anchor uint64, params SpeculativeRollbackParams) error {
	for slot := range sr.pending {
		if slot > anchor {
			delete(sr.pending, slot)
		}
	}
	sr.store.PruneLayersAbove(anchor)
	if sr.headSnapshot == nil || sr.headSnapshot.Slot != anchor {
		return fmt.Errorf("missing head snapshot for anchor slot %d", anchor)
	}
	return restoreReplayHeadFromSnapshot(sr.headSnapshot, params)
}

func restoreReplayHeadFromSnapshot(snapshot *ReplayHeadSnapshot, params SpeculativeRollbackParams) error {
	if snapshot == nil {
		return fmt.Errorf("nil snapshot")
	}
	if err := restoreSysvarCacheFromSnapshot(snapshot, params.AcctsDb, snapshot.Slot); err != nil {
		return err
	}

	slotCtx := slotCtxFromSnapshot(snapshot)
	*params.LastSlotCtx = slotCtx
	params.ReplayCtx.Capitalization = snapshot.Capitalization
	global.SetSlot(snapshot.Slot)
	global.SetBlockHeight(snapshot.BlockHeight)
	UpdateChainTipFromSlotCtx(slotCtx, params.ReplayCtx.CurrentFeatures)
	if params.ForkChoice != nil {
		params.ForkChoice.ObserveExecutionAnchor(snapshot.Slot, solana.Hash(snapshot.Blockhash))
	}
	if params.RPCServer != nil {
		params.RPCServer.SetSlotCtx(slotCtx)
	}
	return nil
}

func slotCtxFromSnapshot(snapshot *ReplayHeadSnapshot) *sealevel.SlotCtx {
	slotCtx := &sealevel.SlotCtx{
		Slot:                   snapshot.Slot,
		ParentSlot:             snapshot.ParentSlot,
		FinalBankhash:          append([]byte(nil), snapshot.FinalBankhash...),
		Blockhash:              snapshot.Blockhash,
		LatestEvictedBlockhash: snapshot.LatestEvictedBlockhash,
		NumSignatures:          snapshot.NumSignatures,
	}
	if snapshot.AcctsLtHash != nil {
		slotCtx.AcctsLtHash = snapshot.AcctsLtHash.Clone()
	}
	if snapshot.FeeRateGovernor != nil {
		gov := *snapshot.FeeRateGovernor
		slotCtx.FeeRateGovernor = &gov
	}
	return slotCtx
}

func restoreSysvarCacheFromSnapshot(snapshot *ReplayHeadSnapshot, acctsDb *accountsdb.AccountsDb, slot uint64) error {
	if snapshot.Clock != nil {
		clockAcct, err := acctsDb.GetAccount(slot, sealevel.SysvarClockAddr)
		if err != nil {
			return fmt.Errorf("load clock sysvar at slot %d: %w", slot, err)
		}
		clockAcct = clockAcct.Clone()
		clock := *snapshot.Clock
		copy(clockAcct.Data, clock.MustMarshal())
		sealevel.SysvarCache.Clock.Sysvar = &clock
		sealevel.SysvarCache.Clock.Acct = clockAcct
	}

	if snapshot.SlotHashes != nil {
		slotHashesAcct, err := acctsDb.GetAccount(slot, sealevel.SysvarSlotHashesAddr)
		if err != nil {
			return fmt.Errorf("load slothashes sysvar at slot %d: %w", slot, err)
		}
		slotHashesAcct = slotHashesAcct.Clone()
		slotHashes := *snapshot.SlotHashes
		copy(slotHashesAcct.Data, slotHashes.MustMarshal())
		sealevel.SysvarCache.SlotHashes.Sysvar = &slotHashes
		sealevel.SysvarCache.SlotHashes.Acct = slotHashesAcct
	}

	if snapshot.RecentBlockhashes != nil {
		recentAcct, err := acctsDb.GetAccount(slot, sealevel.SysvarRecentBlockHashesAddr)
		if err != nil {
			return fmt.Errorf("load recent blockhashes sysvar at slot %d: %w", slot, err)
		}
		recentAcct = recentAcct.Clone()
		recent := *snapshot.RecentBlockhashes
		copy(recentAcct.Data, recent.MustMarshal())
		sealevel.SysvarCache.RecentBlockHashes.Sysvar = &recent
		sealevel.SysvarCache.RecentBlockHashes.Acct = recentAcct
	}

	return nil
}
