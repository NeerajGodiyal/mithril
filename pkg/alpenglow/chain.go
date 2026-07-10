package alpenglow

import (
	"fmt"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"
)

type ChainConfig struct {
	// RequireVerifiedCertificates keeps unverified Votor certificates out of
	// the decision path. Observer tooling can disable this while the verifier is
	// still being brought up, but validator-grade replay should keep it enabled.
	RequireVerifiedCertificates bool
	// RequireStakeVerifiedCertificates requires certificates to have been
	// checked against the epoch validator stake set before they can drive chain
	// decisions. This is useful while aggregate BLS verification is being wired
	// in, because it still keeps malformed or under-staked certs out of the
	// resolver.
	RequireStakeVerifiedCertificates bool
}

func DefaultChainConfig() ChainConfig {
	return ChainConfig{
		RequireVerifiedCertificates: true,
	}
}

type ChainDecisionKind string

const (
	ChainDecisionKindBlock    ChainDecisionKind = "block"
	ChainDecisionKindSkip     ChainDecisionKind = "skip"
	ChainDecisionKindConflict ChainDecisionKind = "conflict"
)

type ChainDecision struct {
	Slot            uint64                `json:"slot"`
	Kind            ChainDecisionKind     `json:"kind"`
	Block           BlockID               `json:"block,omitempty"`
	Observed        bool                  `json:"observed,omitempty"`
	ParentSlot      uint64                `json:"parent_slot,omitempty"`
	ParentHash      solana.Hash           `json:"parent_hash,omitempty"`
	CertificateType CertificateType       `json:"certificate_type,omitempty"`
	Indirect        bool                  `json:"indirect,omitempty"`
	ViaFinalized    BlockID               `json:"via_finalized,omitempty"`
	Reason          string                `json:"reason,omitempty"`
	Candidates      []ChainBlockCandidate `json:"candidates,omitempty"`
}

type ChainBlockCandidate struct {
	Block           BlockID         `json:"block"`
	Observed        bool            `json:"observed,omitempty"`
	ParentSlot      uint64          `json:"parent_slot,omitempty"`
	ParentHash      solana.Hash     `json:"parent_hash,omitempty"`
	CertificateType CertificateType `json:"certificate_type,omitempty"`
}

type ChainPath struct {
	AnchorSlot uint64          `json:"anchor_slot"`
	Decisions  []ChainDecision `json:"decisions"`
	BlockedAt  uint64          `json:"blocked_at,omitempty"`
	Conflict   bool            `json:"conflict,omitempty"`
}

type ChainSnapshot struct {
	CertificatesObserved         uint64              `json:"certificates_observed"`
	CertificatesAccepted         uint64              `json:"certificates_accepted"`
	CertificatesIgnoredUntrusted uint64              `json:"certificates_ignored_untrusted"`
	ReplayBlocksObserved         uint64              `json:"replay_blocks_observed"`
	CertifiedBlocks              uint64              `json:"certified_blocks"`
	CertifiedSkips               uint64              `json:"certified_skips"`
	IndirectSkips                uint64              `json:"indirect_skips"`
	DirectFinalizedBlocks        uint64              `json:"direct_finalized_blocks"`
	FinalizedAncestorBlocks      uint64              `json:"finalized_ancestor_blocks"`
	ConflictingSlots             uint64              `json:"conflicting_slots"`
	LatestCertificateSlot        uint64              `json:"latest_certificate_slot,omitempty"`
	LatestObservedBlock          BlockID             `json:"latest_observed_block,omitempty"`
	LatestDirectFinalizedBlock   BlockID             `json:"latest_direct_finalized_block,omitempty"`
	AcceptedByType               ChainCertTypeCounts `json:"accepted_by_type"`
	UntrustedByType              ChainCertTypeCounts `json:"untrusted_by_type"`
	LatestSkipCertSlot           uint64              `json:"latest_skip_cert_slot,omitempty"`
}

// ChainCertTypeCounts breaks certificate counters down by certificate type.
type ChainCertTypeCounts struct {
	Notarize         uint64 `json:"notarize,omitempty"`
	NotarizeFallback uint64 `json:"notarize_fallback,omitempty"`
	FinalizeFast     uint64 `json:"finalize_fast,omitempty"`
	Finalize         uint64 `json:"finalize,omitempty"`
	Skip             uint64 `json:"skip,omitempty"`
	Genesis          uint64 `json:"genesis,omitempty"`
}

func (c *ChainCertTypeCounts) add(certType CertificateType) {
	switch certType {
	case CertificateNotarize:
		c.Notarize++
	case CertificateNotarizeFallback:
		c.NotarizeFallback++
	case CertificateFinalizeFast:
		c.FinalizeFast++
	case CertificateFinalize:
		c.Finalize++
	case CertificateSkip:
		c.Skip++
	case CertificateGenesis:
		c.Genesis++
	}
}

func (c ChainCertTypeCounts) String() string {
	return fmt.Sprintf("notar=%d notar_fb=%d final_fast=%d final=%d skip=%d genesis=%d",
		c.Notarize, c.NotarizeFallback, c.FinalizeFast, c.Finalize, c.Skip, c.Genesis)
}

type ChainCertificateUpdate struct {
	New      bool          `json:"new"`
	Trusted  bool          `json:"trusted"`
	Snapshot ChainSnapshot `json:"snapshot"`
}

type ChainReplayBlockUpdate struct {
	New      bool          `json:"new"`
	Snapshot ChainSnapshot `json:"snapshot"`
}

type ChainTracker struct {
	mu sync.RWMutex

	cfg ChainConfig

	certificates    map[CertificateKey]Certificate
	blocks          map[BlockID]*chainBlockState
	blockSlots      map[uint64]map[solana.Hash]BlockID
	skipCerts       map[uint64]Certificate
	finalizeCerts   map[uint64]Certificate
	directFinalized map[BlockID]CertificateType
	// finalizedAncestors marks blocks proven canonical because a direct-
	// finalized descendant chains back to them through observed parent links.
	// Keyed by slot: the finalized path has at most one block per slot.
	finalizedAncestors map[uint64]chainFinalizedAncestor
	indirectSkips      map[uint64]chainIndirectSkip
	conflicts          map[uint64]chainConflict

	certificatesObserved         uint64
	certificatesAccepted         uint64
	certificatesIgnoredUntrusted uint64
	replayBlocksObserved         uint64
	latestCertificateSlot        uint64
	latestObservedBlock          BlockID
	latestDirectFinalizedBlock   BlockID
	acceptedByType               ChainCertTypeCounts
	untrustedByType              ChainCertTypeCounts
	latestSkipCertSlot           uint64
}

type chainBlockState struct {
	block        BlockID
	observed     bool
	parentSlot   uint64
	parentHash   solana.Hash
	certificates map[CertificateType]Certificate
}

type chainIndirectSkip struct {
	slot            uint64
	viaFinalized    BlockID
	certificateType CertificateType
}

type chainFinalizedAncestor struct {
	block           BlockID
	certificateType CertificateType
}

type chainConflict struct {
	slot       uint64
	reason     string
	candidates []ChainBlockCandidate
}

func NewChainTracker() *ChainTracker {
	return NewChainTrackerWithConfig(DefaultChainConfig())
}

func NewChainTrackerWithConfig(cfg ChainConfig) *ChainTracker {
	return &ChainTracker{
		cfg:                cfg,
		certificates:       make(map[CertificateKey]Certificate),
		blocks:             make(map[BlockID]*chainBlockState),
		blockSlots:         make(map[uint64]map[solana.Hash]BlockID),
		skipCerts:          make(map[uint64]Certificate),
		finalizeCerts:      make(map[uint64]Certificate),
		directFinalized:    make(map[BlockID]CertificateType),
		finalizedAncestors: make(map[uint64]chainFinalizedAncestor),
		indirectSkips:      make(map[uint64]chainIndirectSkip),
		conflicts:          make(map[uint64]chainConflict),
	}
}

func (t *ChainTracker) ObserveCertificate(cert Certificate) (ChainCertificateUpdate, error) {
	if err := validateChainCertificate(cert); err != nil {
		return ChainCertificateUpdate{Snapshot: t.Snapshot()}, err
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	key := cert.Key()
	if _, exists := t.certificates[key]; exists {
		return ChainCertificateUpdate{New: false, Trusted: t.certificateTrustedLocked(cert), Snapshot: t.snapshotLocked()}, nil
	}

	t.certificates[key] = cert
	t.certificatesObserved++
	if cert.Slot > t.latestCertificateSlot {
		t.latestCertificateSlot = cert.Slot
	}

	trusted := t.certificateTrustedLocked(cert)
	if !trusted {
		t.certificatesIgnoredUntrusted++
		t.untrustedByType.add(cert.Type)
		return ChainCertificateUpdate{New: true, Trusted: false, Snapshot: t.snapshotLocked()}, nil
	}

	t.certificatesAccepted++
	t.acceptedByType.add(cert.Type)
	if cert.Type == CertificateSkip && cert.Slot > t.latestSkipCertSlot {
		t.latestSkipCertSlot = cert.Slot
	}
	t.applyTrustedCertificateLocked(cert)
	return ChainCertificateUpdate{New: true, Trusted: true, Snapshot: t.snapshotLocked()}, nil
}

func (t *ChainTracker) ObserveReplayBlock(obs ReplayBlockObservation) ChainReplayBlockUpdate {
	t.mu.Lock()
	defer t.mu.Unlock()

	if obs.At.IsZero() {
		obs.At = time.Now()
	}
	t.replayBlocksObserved++
	if obs.Block.Slot > t.latestObservedBlock.Slot {
		t.latestObservedBlock = obs.Block
	}
	if obs.Block.IsZero() || !obs.Block.HasHash() {
		return ChainReplayBlockUpdate{New: false, Snapshot: t.snapshotLocked()}
	}

	state := t.ensureBlockStateLocked(obs.Block)
	wasObserved := state.observed
	state.observed = true
	if obs.ParentSlot != 0 {
		state.parentSlot = obs.ParentSlot
	}
	if obs.ParentHash != (solana.Hash{}) {
		state.parentHash = obs.ParentHash
	}
	if obs.ParentSlot != 0 && obs.ParentHash != (solana.Hash{}) {
		t.confirmBlockIdentityFromChildLocked(obs.Block, obs.ParentSlot, obs.ParentHash)
	}

	retryFrom := obs.Block.Slot
	if obs.ParentSlot != 0 && obs.ParentSlot < retryFrom {
		retryFrom = obs.ParentSlot
	}

	if certType, finalized := t.directFinalized[obs.Block]; finalized {
		t.walkFinalizedAncestryLocked(obs.Block, certType)
	} else if ancestor, ok := t.finalizedAncestors[obs.Block.Slot]; ok && ancestor.block == obs.Block {
		t.walkFinalizedAncestryLocked(obs.Block, ancestor.certificateType)
	}
	t.retryObservedFinalizedAncestryWalksFromSlotLocked(retryFrom)

	return ChainReplayBlockUpdate{New: !wasObserved, Snapshot: t.snapshotLocked()}
}

// KnownBlockAtSlot returns the best-known Alpenglow block identity at slot.
// Preference order: sole certified ID, any direct-finalized ID, any observed ID.
func (t *ChainTracker) KnownBlockAtSlot(slot uint64) (BlockID, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if slotBlocks := t.blockSlots[slot]; len(slotBlocks) == 1 {
		for _, block := range slotBlocks {
			return block, true
		}
	}
	for block := range t.directFinalized {
		if block.Slot == slot {
			return block, true
		}
	}
	var fallback BlockID
	var haveFallback bool
	for block, state := range t.blocks {
		if block.Slot != slot {
			continue
		}
		if state.observed {
			return block, true
		}
		if !haveFallback {
			fallback = block
			haveFallback = true
		}
	}
	if haveFallback {
		return fallback, true
	}
	return BlockID{}, false
}

func (t *ChainTracker) NextDecision(anchorSlot uint64) (ChainDecision, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.nextDecisionLocked(anchorSlot)
}

func (t *ChainTracker) ResolvePath(anchorSlot uint64, maxDecisions int) ChainPath {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if maxDecisions <= 0 {
		maxDecisions = 1
	}
	path := ChainPath{AnchorSlot: anchorSlot}
	current := anchorSlot
	for len(path.Decisions) < maxDecisions {
		decision, ok := t.nextDecisionLocked(current)
		if !ok {
			path.BlockedAt = current + 1
			break
		}
		path.Decisions = append(path.Decisions, decision)
		if decision.Kind == ChainDecisionKindConflict {
			path.Conflict = true
			break
		}
		current = decision.Slot
	}
	return path
}

func (t *ChainTracker) Snapshot() ChainSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.snapshotLocked()
}

func validateChainCertificate(cert Certificate) error {
	if err := cert.ValidateBasic(); err != nil {
		return err
	}
	if cert.Type.HasBlock() && cert.BlockHash == (solana.Hash{}) {
		return fmt.Errorf("%s certificate for slot %d has empty block hash", cert.Type, cert.Slot)
	}
	return nil
}

func (t *ChainTracker) certificateTrustedLocked(cert Certificate) bool {
	if t.cfg.RequireVerifiedCertificates && !cert.SignatureVerified {
		return false
	}
	if t.cfg.RequireStakeVerifiedCertificates && !cert.StakeVerified {
		return false
	}
	return true
}

func (t *ChainTracker) applyTrustedCertificateLocked(cert Certificate) {
	switch cert.Type {
	case CertificateNotarize, CertificateNotarizeFallback, CertificateFinalizeFast, CertificateGenesis:
		t.applyTrustedBlockCertificateLocked(cert)
	case CertificateFinalize:
		t.finalizeCerts[cert.Slot] = cert
		t.tryMarkSlowFinalizedLocked(cert.Slot)
	case CertificateSkip:
		t.skipCerts[cert.Slot] = cert
		t.refreshConflictLocked(cert.Slot)
	}
}

func (t *ChainTracker) applyTrustedBlockCertificateLocked(cert Certificate) {
	block, ok := cert.Block()
	if !ok || !block.HasHash() {
		return
	}

	state := t.ensureBlockStateLocked(block)
	state.certificates[cert.Type] = cert
	if t.blockSlots[block.Slot] == nil {
		t.blockSlots[block.Slot] = make(map[solana.Hash]BlockID)
	}
	t.blockSlots[block.Slot][block.Hash] = block

	switch cert.Type {
	case CertificateFinalizeFast, CertificateGenesis:
		t.markDirectFinalizedLocked(block, cert.Type)
	case CertificateNotarize:
		t.tryMarkSlowFinalizedLocked(block.Slot)
	}
	t.refreshConflictLocked(block.Slot)
}

func (t *ChainTracker) ensureBlockStateLocked(block BlockID) *chainBlockState {
	state := t.blocks[block]
	if state == nil {
		state = &chainBlockState{
			block:        block,
			certificates: make(map[CertificateType]Certificate),
		}
		t.blocks[block] = state
	}
	return state
}

func (t *ChainTracker) tryMarkSlowFinalizedLocked(slot uint64) {
	if _, ok := t.finalizeCerts[slot]; !ok {
		return
	}
	for _, block := range t.blockSlots[slot] {
		state := t.blocks[block]
		if state == nil {
			continue
		}
		if _, notarized := state.certificates[CertificateNotarize]; notarized {
			t.markDirectFinalizedLocked(block, CertificateFinalize)
		}
	}
}

func (t *ChainTracker) markDirectFinalizedLocked(block BlockID, certType CertificateType) {
	if block.IsZero() || !block.HasHash() {
		return
	}
	if existing, ok := t.directFinalized[block]; ok && existing == certType {
		return
	}
	t.directFinalized[block] = certType
	if block.Slot >= t.latestDirectFinalizedBlock.Slot {
		t.latestDirectFinalizedBlock = block
	}
	t.walkFinalizedAncestryLocked(block, certType)
	if state := t.blocks[block]; state == nil || !state.observed {
		// Finalized tip blocks often arrive before turbine assembles them.
		// Re-run walks from any already-observed finalized descendants so
		// indirect skips can still propagate toward the replay frontier.
		t.retryObservedFinalizedAncestryWalksFromSlotLocked(0)
	}
}

// confirmBlockIdentityFromChildLocked marks a parent block as observed when a
// child block's Alpenglow parent marker names it. Parent slot/hash on the
// parent block itself still come from direct observation of that parent.
func (t *ChainTracker) confirmBlockIdentityFromChildLocked(child BlockID, parentSlot uint64, parentHash solana.Hash) {
	if parentSlot == 0 || parentHash == (solana.Hash{}) || parentSlot >= child.Slot {
		return
	}
	parent := BlockID{Slot: parentSlot, Hash: parentHash}
	state := t.ensureBlockStateLocked(parent)
	state.observed = true
}

// retryObservedFinalizedAncestryWalksFromSlotLocked re-runs finalized ancestry
// walks from every direct-finalized block that is already observed at or above
// fromSlot. New parent observations can extend walks that previously stopped
// early because parent linkage was missing.
func (t *ChainTracker) retryObservedFinalizedAncestryWalksFromSlotLocked(fromSlot uint64) {
	for block, certType := range t.directFinalized {
		if block.Slot < fromSlot {
			continue
		}
		state := t.blocks[block]
		if state == nil || !state.observed {
			continue
		}
		t.walkFinalizedAncestryLocked(block, certType)
	}
}

// maxFinalizedAncestryWalk bounds a single ancestry walk. Each block is only
// ever marked once, so repeated walks do not repeat work.
const maxFinalizedAncestryWalk = 4096

// walkFinalizedAncestryLocked propagates finality down the observed ancestor
// chain of a finalized block. Finalizing a block finalizes its entire ancestor
// path, so each observed parent link both proves the parent block is canonical
// and proves every slot gap along the link was skipped. This is what lets
// replay hop over leader windows that were skipped before the Votor listener
// connected: those slots' skip certificates are never rebroadcast, but a later
// finalized descendant chaining back over the gap carries the same proof.
// The walk stops at the first unobserved ancestor and resumes from
// ObserveReplayBlock once that ancestor is observed with parent linkage.
func (t *ChainTracker) walkFinalizedAncestryLocked(start BlockID, certType CertificateType) {
	cur := start
	for i := 0; i < maxFinalizedAncestryWalk; i++ {
		state := t.blocks[cur]
		if state == nil || !state.observed {
			return
		}
		t.deriveIndirectSkipsLocked(cur, certType)
		if state.parentSlot == 0 || state.parentSlot >= cur.Slot || state.parentHash == (solana.Hash{}) {
			return
		}
		parent := BlockID{Slot: state.parentSlot, Hash: state.parentHash}
		if _, final := t.directFinalized[parent]; final {
			return
		}
		if existing, ok := t.finalizedAncestors[parent.Slot]; ok && existing.block == parent {
			// Parent was marked on an earlier walk that stopped before parent
			// linkage arrived. Keep walking so deriveIndirectSkips can run once
			// the parent block carries parent slot/hash.
			cur = parent
			continue
		}
		t.finalizedAncestors[parent.Slot] = chainFinalizedAncestor{block: parent, certificateType: certType}
		cur = parent
	}
}

// RefreshParentLinkagesFromSlot fills missing parent hashes on every observed
// block that already names parentSlot but arrived before the parent block id
// was known locally.
func (t *ChainTracker) RefreshParentLinkagesFromSlot(parentSlot uint64, parentHash solana.Hash) {
	if parentSlot == 0 || parentHash == (solana.Hash{}) {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	retryFrom := parentSlot
	for _, state := range t.blocks {
		if state == nil || !state.observed {
			continue
		}
		if state.parentSlot != parentSlot || state.parentHash != (solana.Hash{}) {
			continue
		}
		state.parentHash = parentHash
		if state.block.Slot < retryFrom {
			retryFrom = state.block.Slot
		}
	}
	t.retryObservedFinalizedAncestryWalksFromSlotLocked(retryFrom)
}

func (t *ChainTracker) deriveIndirectSkipsLocked(block BlockID, certType CertificateType) {
	state := t.blocks[block]
	if state == nil || !state.observed || state.parentSlot == 0 || state.parentSlot >= block.Slot {
		return
	}
	for slot := state.parentSlot + 1; slot < block.Slot; slot++ {
		t.indirectSkips[slot] = chainIndirectSkip{
			slot:            slot,
			viaFinalized:    block,
			certificateType: certType,
		}
		t.refreshConflictLocked(slot)
	}
}

func (t *ChainTracker) nextDecisionLocked(anchorSlot uint64) (ChainDecision, bool) {
	slot := anchorSlot + 1
	if conflict, ok := t.conflicts[slot]; ok {
		return ChainDecision{
			Slot:       slot,
			Kind:       ChainDecisionKindConflict,
			Reason:     conflict.reason,
			Candidates: conflict.candidates,
		}, true
	}
	if cert, ok := t.skipCerts[slot]; ok {
		return ChainDecision{
			Slot:            slot,
			Kind:            ChainDecisionKindSkip,
			CertificateType: cert.Type,
			Reason:          "skip certificate",
		}, true
	}
	if indirect, ok := t.indirectSkips[slot]; ok {
		return ChainDecision{
			Slot:            slot,
			Kind:            ChainDecisionKindSkip,
			CertificateType: indirect.certificateType,
			Indirect:        true,
			ViaFinalized:    indirect.viaFinalized,
			Reason:          "omitted by finalized ancestor chain",
		}, true
	}
	candidates := t.blockCandidatesLocked(slot)
	switch len(candidates) {
	case 0:
		return ChainDecision{}, false
	case 1:
		candidate := candidates[0]
		return ChainDecision{
			Slot:            slot,
			Kind:            ChainDecisionKindBlock,
			Block:           candidate.Block,
			Observed:        candidate.Observed,
			ParentSlot:      candidate.ParentSlot,
			ParentHash:      candidate.ParentHash,
			CertificateType: candidate.CertificateType,
			Reason:          "block certificate",
		}, true
	default:
		return ChainDecision{
			Slot:       slot,
			Kind:       ChainDecisionKindConflict,
			Reason:     "multiple certified block IDs for slot",
			Candidates: candidates,
		}, true
	}
}

func (t *ChainTracker) blockCandidatesLocked(slot uint64) []ChainBlockCandidate {
	var candidates []ChainBlockCandidate
	if slotBlocks := t.blockSlots[slot]; len(slotBlocks) > 0 {
		candidates = make([]ChainBlockCandidate, 0, len(slotBlocks))
		for _, block := range slotBlocks {
			state := t.blocks[block]
			if state == nil {
				continue
			}
			candidates = append(candidates, ChainBlockCandidate{
				Block:           block,
				Observed:        state.observed,
				ParentSlot:      state.parentSlot,
				ParentHash:      state.parentHash,
				CertificateType: t.blockCertificateTypeLocked(block, strongestBlockCertificateType(state.certificates)),
			})
		}
	}
	if len(candidates) == 0 {
		if ancestor, ok := t.finalizedAncestors[slot]; ok {
			state := t.blocks[ancestor.block]
			if state != nil && state.observed {
				candidates = append(candidates, ChainBlockCandidate{
					Block:           ancestor.block,
					Observed:        true,
					ParentSlot:      state.parentSlot,
					ParentHash:      state.parentHash,
					CertificateType: ancestor.certificateType,
				})
			}
		}
	}
	return candidates
}

func (t *ChainTracker) blockCertificateTypeLocked(block BlockID, certType CertificateType) CertificateType {
	if finalizedType, ok := t.directFinalized[block]; ok && finalizedType.IsFinalization() {
		return finalizedType
	}
	if certType.IsFinalization() {
		return certType
	}
	if ancestor, ok := t.finalizedAncestors[block.Slot]; ok && ancestor.block == block && ancestor.certificateType.IsFinalization() {
		return ancestor.certificateType
	}
	return certType
}

func strongestBlockCertificateType(certs map[CertificateType]Certificate) CertificateType {
	if _, ok := certs[CertificateFinalizeFast]; ok {
		return CertificateFinalizeFast
	}
	if _, ok := certs[CertificateNotarize]; ok {
		return CertificateNotarize
	}
	if _, ok := certs[CertificateNotarizeFallback]; ok {
		return CertificateNotarizeFallback
	}
	if _, ok := certs[CertificateGenesis]; ok {
		return CertificateGenesis
	}
	return ""
}

func (t *ChainTracker) refreshConflictLocked(slot uint64) {
	candidates := t.blockCandidatesLocked(slot)
	hasBlock := len(candidates) > 0
	_, hasExplicitSkip := t.skipCerts[slot]
	_, hasIndirectSkip := t.indirectSkips[slot]

	switch {
	case len(candidates) > 1:
		t.conflicts[slot] = chainConflict{
			slot:       slot,
			reason:     "multiple certified block IDs for slot",
			candidates: candidates,
		}
	case hasBlock && hasExplicitSkip:
		t.conflicts[slot] = chainConflict{
			slot:       slot,
			reason:     "slot has both block and skip certificates",
			candidates: candidates,
		}
	case hasBlock && hasIndirectSkip:
		t.conflicts[slot] = chainConflict{
			slot:       slot,
			reason:     "slot has a certified block but is omitted by a finalized chain",
			candidates: candidates,
		}
	default:
		delete(t.conflicts, slot)
	}
}

func (t *ChainTracker) snapshotLocked() ChainSnapshot {
	return ChainSnapshot{
		CertificatesObserved:         t.certificatesObserved,
		CertificatesAccepted:         t.certificatesAccepted,
		CertificatesIgnoredUntrusted: t.certificatesIgnoredUntrusted,
		ReplayBlocksObserved:         t.replayBlocksObserved,
		CertifiedBlocks:              uint64(len(t.blocks)),
		CertifiedSkips:               uint64(len(t.skipCerts)),
		IndirectSkips:                uint64(len(t.indirectSkips)),
		DirectFinalizedBlocks:        uint64(len(t.directFinalized)),
		ConflictingSlots:             uint64(len(t.conflicts)),
		FinalizedAncestorBlocks:      uint64(len(t.finalizedAncestors)),
		LatestCertificateSlot:        t.latestCertificateSlot,
		LatestObservedBlock:          t.latestObservedBlock,
		LatestDirectFinalizedBlock:   t.latestDirectFinalizedBlock,
		AcceptedByType:               t.acceptedByType,
		UntrustedByType:              t.untrustedByType,
		LatestSkipCertSlot:           t.latestSkipCertSlot,
	}
}
