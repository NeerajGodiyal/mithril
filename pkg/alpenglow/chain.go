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
	CertificatesObserved         uint64  `json:"certificates_observed"`
	CertificatesAccepted         uint64  `json:"certificates_accepted"`
	CertificatesIgnoredUntrusted uint64  `json:"certificates_ignored_untrusted"`
	ReplayBlocksObserved         uint64  `json:"replay_blocks_observed"`
	CertifiedBlocks              uint64  `json:"certified_blocks"`
	CertifiedSkips               uint64  `json:"certified_skips"`
	IndirectSkips                uint64  `json:"indirect_skips"`
	DirectFinalizedBlocks        uint64  `json:"direct_finalized_blocks"`
	ConflictingSlots             uint64  `json:"conflicting_slots"`
	LatestCertificateSlot        uint64  `json:"latest_certificate_slot,omitempty"`
	LatestObservedBlock          BlockID `json:"latest_observed_block,omitempty"`
	LatestDirectFinalizedBlock   BlockID `json:"latest_direct_finalized_block,omitempty"`
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
	appliedCerts    map[CertificateKey]struct{} // certs already folded into chain state
	blocks          map[BlockID]*chainBlockState
	blockSlots      map[uint64]map[solana.Hash]BlockID
	skipCerts       map[uint64]Certificate
	finalizeCerts   map[uint64]Certificate
	directFinalized map[BlockID]CertificateType
	indirectSkips   map[uint64]chainIndirectSkip
	conflicts       map[uint64]chainConflict

	certificatesObserved         uint64
	certificatesAccepted         uint64
	certificatesIgnoredUntrusted uint64
	replayBlocksObserved         uint64
	latestCertificateSlot        uint64
	latestObservedBlock          BlockID
	latestDirectFinalizedBlock   BlockID
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
		cfg:             cfg,
		certificates:    make(map[CertificateKey]Certificate),
		appliedCerts:    make(map[CertificateKey]struct{}),
		blocks:          make(map[BlockID]*chainBlockState),
		blockSlots:      make(map[uint64]map[solana.Hash]BlockID),
		skipCerts:       make(map[uint64]Certificate),
		finalizeCerts:   make(map[uint64]Certificate),
		directFinalized: make(map[BlockID]CertificateType),
		indirectSkips:   make(map[uint64]chainIndirectSkip),
		conflicts:       make(map[uint64]chainConflict),
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
		// Already seen. If it was previously untrusted (e.g. validator set not yet
		// installed) and is now trustable, fold it in now — otherwise a cert that
		// arrived before its epoch's stakes would be lost, stalling finality.
		trusted := t.certificateTrustedLocked(cert)
		if _, applied := t.appliedCerts[key]; trusted && !applied {
			t.certificates[key] = cert
			t.appliedCerts[key] = struct{}{}
			t.certificatesAccepted++
			t.applyTrustedCertificateLocked(cert)
			return ChainCertificateUpdate{New: false, Trusted: true, Snapshot: t.snapshotLocked()}, nil
		}
		return ChainCertificateUpdate{New: false, Trusted: trusted, Snapshot: t.snapshotLocked()}, nil
	}

	t.certificates[key] = cert
	t.certificatesObserved++
	if cert.Slot > t.latestCertificateSlot {
		t.latestCertificateSlot = cert.Slot
	}

	trusted := t.certificateTrustedLocked(cert)
	if !trusted {
		t.certificatesIgnoredUntrusted++
		return ChainCertificateUpdate{New: true, Trusted: false, Snapshot: t.snapshotLocked()}, nil
	}

	t.appliedCerts[key] = struct{}{}
	t.certificatesAccepted++
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
	state.parentSlot = obs.ParentSlot
	state.parentHash = obs.ParentHash

	if certType, finalized := t.directFinalized[obs.Block]; finalized {
		t.deriveIndirectSkipsLocked(obs.Block, certType)
	}

	return ChainReplayBlockUpdate{New: !wasObserved, Snapshot: t.snapshotLocked()}
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
		// Bound memory on long runs: finalized slots well behind the watermark are
		// settled and no longer needed for decisions or indirect-skip derivation.
		if block.Slot > chainTrackerRetentionSlots {
			t.pruneBeforeSlotLocked(block.Slot - chainTrackerRetentionSlots)
		}
	}
	t.deriveIndirectSkipsLocked(block, certType)
}

// chainTrackerRetentionSlots is how many slots of state the tracker keeps behind the
// finalized watermark. Live decisions are only made near the tip (the block source
// gates on isNearTip, ~32-64 slots), so a window this far behind finality can never
// remove state a live decision or indirect-skip derivation still needs.
const chainTrackerRetentionSlots = 512

// PruneBeforeSlot drops all tracker state for slots strictly below slot, bounding
// memory on a long-running node. Pruning runs automatically behind finality; this
// exported form lets a caller prune explicitly (e.g. behind the rooted watermark).
func (t *ChainTracker) PruneBeforeSlot(slot uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneBeforeSlotLocked(slot)
}

func (t *ChainTracker) pruneBeforeSlotLocked(slot uint64) {
	if slot == 0 {
		return
	}
	for k := range t.certificates {
		if k.Slot < slot {
			delete(t.certificates, k)
			delete(t.appliedCerts, k)
		}
	}
	for k := range t.appliedCerts {
		if k.Slot < slot {
			delete(t.appliedCerts, k)
		}
	}
	for id := range t.blocks {
		if id.Slot < slot {
			delete(t.blocks, id)
		}
	}
	for id := range t.directFinalized {
		if id.Slot < slot {
			delete(t.directFinalized, id)
		}
	}
	for s := range t.blockSlots {
		if s < slot {
			delete(t.blockSlots, s)
		}
	}
	for s := range t.skipCerts {
		if s < slot {
			delete(t.skipCerts, s)
		}
	}
	for s := range t.finalizeCerts {
		if s < slot {
			delete(t.finalizeCerts, s)
		}
	}
	for s := range t.indirectSkips {
		if s < slot {
			delete(t.indirectSkips, s)
		}
	}
	for s := range t.conflicts {
		if s < slot {
			delete(t.conflicts, s)
		}
	}
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
	slotBlocks := t.blockSlots[slot]
	if len(slotBlocks) == 0 {
		return nil
	}
	candidates := make([]ChainBlockCandidate, 0, len(slotBlocks))
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
			CertificateType: strongestBlockCertificateType(state.certificates),
		})
	}
	return candidates
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
		LatestCertificateSlot:        t.latestCertificateSlot,
		LatestObservedBlock:          t.latestObservedBlock,
		LatestDirectFinalizedBlock:   t.latestDirectFinalizedBlock,
	}
}
