package consensus

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/alpenglow"
	"github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/gagliardetto/solana-go"
)

const (
	maxRecentAlpenglowBlockIDs   = 8192
	maxVerifiedVotorCertificates = 8192
	// Network events continuously re-anchor this short extrapolation. Capping
	// it to one leader window prevents a quiet or disconnected receiver from
	// recreating the unbounded startup-wall-clock drift that can skip our own
	// leader slots.
	alpenglowLiveClockSlotDuration = 200 * time.Millisecond
)

type certificateAttemptKey struct {
	key       alpenglow.CertificateKey
	signature [sha256.Size]byte
	bitmap    [sha256.Size]byte
}

type certificateVerification struct {
	done     chan struct{}
	verified alpenglow.Certificate
	result   alpenglow.CertificateVerifyResult
	err      error
}

type verificationDisposition uint8

const (
	verificationFresh verificationDisposition = iota
	verificationCached
	verificationInFlight
)

type BlockObservation struct {
	Block  *block.Block
	Source string
	At     time.Time
}

type SlotReplayResult struct {
	Slot     uint64
	Bankhash [32]byte
	Source   string
	At       time.Time
}

type AlpenglowBlockIDSink func(slot uint64, blockID solana.Hash)

type AlpenglowBlockIDPublisher interface {
	SetAlpenglowBlockIDSink(sink AlpenglowBlockIDSink)
}

type AlpenglowDecisionSource interface {
	NextAlpenglowDecision(anchorSlot uint64) (alpenglow.ChainDecision, bool)
}

// AlpenglowFinalityIndex answers per-slot finality queries for the promotion gate.
type AlpenglowFinalityIndex interface {
	FinalizedBlockAt(slot uint64) (alpenglow.BlockID, bool)
	FinalityConflictAt(slot uint64) bool
}

type AlpenglowCandidateBlockObserver interface {
	ObserveAlpenglowCandidateBlock(obs alpenglow.ReplayBlockObservation)
}

type AlpenglowFirstShredObserver interface {
	ObserveAlpenglowFirstShred(slot uint64)
}

// AlpenglowLiveSlotSource exposes the short, verified-network-anchored clock
// used by block production and the voting live-window gate.
type AlpenglowLiveSlotSource interface {
	AlpenglowLiveSlot() (uint64, bool)
}

// AlpenglowFooterCertificateSink ingests finalization certs decoded from block
// footers (the unstaked finality path). Implemented only by the observer engine.
type AlpenglowFooterCertificateSink interface {
	// ObserveFooterCertificates returns the blocks finalized by the verified certs,
	// so the caller can capture expected finality at ingest time (the tracker's own
	// state may be pruned by the time promotion consults it).
	ObserveFooterCertificates(certs []alpenglow.Certificate) []alpenglow.BlockID
}

type AlpenglowValidatorSetSink interface {
	SetAlpenglowValidatorSet(set alpenglow.ValidatorSet) error
}

type AlpenglowEpochLookupSink interface {
	SetAlpenglowEpochLookup(fn func(slot uint64) uint64)
}

// AlpenglowPruneSink lets replay prune consensus bookkeeping as slots fold.
type AlpenglowPruneSink interface {
	PruneAlpenglowBefore(slot uint64)
}

// AlpenglowSafetyStatus exposes a latched consensus fault to replay. Once set,
// replay may not advance the durable watermark or continue producing blocks.
type AlpenglowSafetyStatus interface {
	AlpenglowSafetyError() error
}

type AlpenglowBlockProductionParentSource interface {
	AlpenglowBlockProductionParent(slot uint64) alpenglow.BlockProductionParent
}

// AlpenglowChainQuery answers the switch sweep's decisive-certificate
// questions (unique-strength or finalized block per slot; certified skips).
type AlpenglowChainQuery interface {
	CertifiedBlockAt(slot uint64) (alpenglow.BlockID, alpenglow.CertificateType, bool)
	SkipCertifiedAt(slot uint64) bool
	// ChainDecisionVersion gates the sweep: it advances on ANY decision-relevant
	// change (cert acceptance, replay-derived parent links, finalized ancestry,
	// indirect skips, conflicts), not only on new certificates — so a
	// contradiction that arises without a new cert is never missed.
	ChainDecisionVersion() uint64
}

// AlpenglowWantedBlocksSource surfaces certified-but-unobserved blocks so the
// block source can steer turbine/repair toward data the cluster has already
// voted real (cert-driven repair).
type AlpenglowWantedBlocksSource interface {
	AlpenglowWantedBlocks(afterSlot uint64, max int) []alpenglow.WantedBlock
	SkipCertifiedAt(slot uint64) bool
}

type Snapshot struct {
	Mode           string                           `json:"mode"`
	ObservedBlocks uint64                           `json:"observed_blocks"`
	ReplayedSlots  uint64                           `json:"replayed_slots"`
	Alpenglow      *alpenglow.Snapshot              `json:"alpenglow,omitempty"`
	AlpenglowPool  *alpenglow.ConsensusPoolSnapshot `json:"alpenglow_pool,omitempty"`
	AlpenglowChain *alpenglow.ChainSnapshot         `json:"alpenglow_chain,omitempty"`
	Receiver       *alpenglow.ReceiverStats         `json:"receiver,omitempty"`
	Voting         *VotingStats                     `json:"voting,omitempty"`
}

type Engine interface {
	Name() string
	Start(ctx context.Context) error
	ObserveBlock(ctx context.Context, obs BlockObservation) error
	OnReplayResult(ctx context.Context, result SlotReplayResult) error
	Snapshot() Snapshot
	Close() error
}

type Config struct {
	AlpenglowObserverBindAddr string
	AlpenglowMaxMessageBytes  int64
	AlpenglowBLSDST           string             // BLS hash-to-curve DST; empty keeps the default (must match cluster's solana-bls version)
	AlpenglowShredVersion     uint16             // included in each Alpenglow BLS vote-signing payload
	AlpenglowIdentity         ed25519.PrivateKey // validator identity for staked Votor QUIC; empty is passive observer mode
}

// NewEngine constructs the Alpenglow observer engine — the only consensus
// engine in this Alpenglow-only build.
func NewEngine(cfg Config) (*AlpenglowObserverEngine, error) {
	alpenglow.SetHashToPointDST(strings.TrimSpace(cfg.AlpenglowBLSDST))
	verifier := alpenglow.NewCertificateVerifier()
	verifier.SetShredVersion(cfg.AlpenglowShredVersion)
	e := &AlpenglowObserverEngine{
		observer:                alpenglow.NewObserver(),
		chain:                   newAlpenglowObserverChainTracker(),
		pool:                    alpenglow.NewConsensusPool(alpenglow.DefaultConsensusPoolConfig()),
		verifier:                verifier,
		receiverBindAddr:        strings.TrimSpace(cfg.AlpenglowObserverBindAddr),
		receiverMaxMessageBytes: cfg.AlpenglowMaxMessageBytes,
		shredVersion:            cfg.AlpenglowShredVersion,
		identity:                append(ed25519.PrivateKey(nil), cfg.AlpenglowIdentity...),
		recentBlockIDs:          make(map[uint64]solana.Hash),
		observedReplayBlocks:    make(map[uint64]alpenglow.ReplayBlockObservation),
		validatorSets:           make(map[uint64]alpenglow.ValidatorSet),
	}
	// CertPool remains the bounded, lazy BLS batch-verification front end. Its
	// verified votes feed ConsensusPool, which is the sole certificate/finality
	// authority; CertPool's parallel certificate output is deliberately disabled.
	e.certPool = alpenglow.NewCertPool(alpenglow.DefaultCertPoolConfig(), e.verifier, nil)
	e.certPool.SetVerifiedVoteSink(e.acceptVerifiedVote)
	return e, nil
}

type AlpenglowObserverEngine struct {
	observedBlocks          atomic.Uint64
	replayedSlots           atomic.Uint64
	observer                *alpenglow.Observer
	certPool                *alpenglow.CertPool
	pool                    *alpenglow.ConsensusPool
	chain                   *alpenglow.ChainTracker
	verifier                *alpenglow.CertificateVerifier
	receiverBindAddr        string
	receiverMaxMessageBytes int64
	shredVersion            uint16
	identity                ed25519.PrivateKey
	receiver                *alpenglow.Receiver
	voterMu                 sync.RWMutex
	voter                   *alpenglowVoter
	observedReplayMu        sync.Mutex
	observedReplayBlocks    map[uint64]alpenglow.ReplayBlockObservation
	validatorSetsMu         sync.RWMutex
	validatorSets           map[uint64]alpenglow.ValidatorSet
	blockIDSinkMu           sync.RWMutex
	blockIDSink             AlpenglowBlockIDSink
	recentBlockIDs          map[uint64]solana.Hash
	recentBlockIDOrder      []uint64
	epochLookupMu           sync.RWMutex
	epochForSlot            func(slot uint64) uint64
	pendingCertsMu          sync.Mutex
	pendingCerts            map[uint64][]pendingCertificate // deferred until their epoch's stakes install
	certVerifyLogMu         sync.Mutex
	certVerifyDropCount     uint64
	certVerifyDetailCount   uint64
	lastCertVerifyLog       time.Time
	// Votor messages are redundantly broadcast by many QUIC peers. Collapse
	// identical in-flight certificate work and retain only successful logical
	// verifications. Vote verification is owned by CertPool's bounded batch path.
	verificationMu           sync.Mutex
	verifiedCertificates     map[alpenglow.CertificateKey]alpenglow.Certificate
	verifiedCertificateOrder []alpenglow.CertificateKey
	certificateInFlight      map[certificateAttemptKey]*certificateVerification
	certificateCacheHits     atomic.Uint64
	certificateInflightDrops atomic.Uint64
	networkProofsMu          sync.Mutex
	networkProofs            map[certificateAttemptKey]struct{}
	networkProofOrder        []certificateAttemptKey
	votorMessageHookMu       sync.RWMutex
	votorMessageHook         func(alpenglow.Message)
	liveClockMu              sync.Mutex
	liveClockSlot            uint64
	liveClockAt              time.Time
	// Pool transitions and downstream chain delivery are one ordered stream.
	poolOutputMu sync.Mutex
	safetyMu     sync.RWMutex
	safetyErr    error
}

// SetVotorMessageHook registers an additional consumer for inbound Votor
// messages. Block production uses it to build footer reward certificates while
// the observer retains authority over consensus certificate verification.
func (e *AlpenglowObserverEngine) SetVotorMessageHook(fn func(alpenglow.Message)) {
	e.votorMessageHookMu.Lock()
	e.votorMessageHook = fn
	e.votorMessageHookMu.Unlock()
}

func (e *AlpenglowObserverEngine) Name() string { return "alpenglow-observer" }

func (e *AlpenglowObserverEngine) Start(ctx context.Context) error {
	observer := e.ensureObserver()
	e.ensureChain()
	e.ensureVerifier()
	mlog.Log.FileOnlyf("Consensus engine started: %s (voting activates only after validator key/rank/history checks)", e.Name())
	mlog.Log.FileOnlyf("ALPENGLOW observer: certified path resolver requires stake and aggregate BLS signature verified certificates")
	if e.receiverBindAddr == "" {
		mlog.Log.FileOnlyf("ALPENGLOW observer: Votor receiver disabled; set consensus.alpenglow_observer_bind_addr to listen for Votor QUIC messages")
		return nil
	}

	receiver, err := alpenglow.NewReceiver(alpenglow.ReceiverConfig{
		BindAddr:        e.receiverBindAddr,
		MaxMessageBytes: e.receiverMaxMessageBytes,
		ShredVersion:    e.shredVersion,
		Identity:        e.identity,
		AdmitMessage:    e.admitVotorMessage,
	}, observer)
	if err != nil {
		return err
	}
	e.receiver = receiver
	go func() {
		if err := receiver.Run(ctx); err != nil {
			mlog.Log.Warnf("ALPENGLOW Votor receiver stopped: %v", err)
		}
	}()
	return nil
}

// EnableVoting turns the verified observer into an active Votor participant.
// It fails closed on missing/corrupt history and does not emit anything until
// the on-chain epoch rank proves the identity, vote account, and derived BLS
// key are one validator.
func (e *AlpenglowObserverEngine) EnableVoting(cfg VotingConfig) error {
	if err := e.safetyError(); err != nil {
		return err
	}
	e.voterMu.Lock()
	if e.voter != nil {
		e.voterMu.Unlock()
		return fmt.Errorf("Alpenglow voting is already enabled")
	}
	root := alpenglow.BlockID{Slot: e.ensurePool().Snapshot().RootSlot}
	if block, ok := e.ensureChain().FinalizedBlockAt(root.Slot); ok {
		root = block
	}
	voter, err := newAlpenglowVoter(e, cfg, root)
	if err != nil {
		e.voterMu.Unlock()
		return err
	}
	// Resume the furthest persisted ParentReady boundary before accepting new
	// events, matching Agave's initial_parent_ready selection.
	if slot, parent, ok := voter.history.HighestParentReady(); ok && slot > root.Slot {
		e.ensurePool().RestoreParentReady(slot, parent)
	}
	e.voter = voter
	e.voterMu.Unlock()

	e.validatorSetsMu.RLock()
	sets := make([]alpenglow.ValidatorSet, 0, len(e.validatorSets))
	for _, set := range e.validatorSets {
		sets = append(sets, cloneValidatorSet(set))
	}
	e.validatorSetsMu.RUnlock()
	for _, set := range sets {
		if err := voter.enqueue(voterEvent{kind: voterEventValidatorSet, set: set}); err != nil {
			e.removeAndCloseVoter(voter)
			return err
		}
	}
	if slot, parent, ok := e.ensurePool().HighestParentReady(); ok {
		if err := voter.enqueue(voterEvent{kind: voterEventConsensus, consensus: alpenglow.ConsensusEvent{
			Kind:  alpenglow.ConsensusEventParentReady,
			Slot:  slot,
			Block: parent,
		}}); err != nil {
			e.removeAndCloseVoter(voter)
			return err
		}
	}
	mlog.Log.Infof("ALPENGLOW voting engine enabled for identity %s vote account %s (history=%s)", voter.node, voter.voteAccount, cfg.HistoryDir)
	return nil
}

func (e *AlpenglowObserverEngine) removeAndCloseVoter(voter *alpenglowVoter) {
	e.voterMu.Lock()
	if e.voter == voter {
		e.voter = nil
	}
	e.voterMu.Unlock()
	_ = voter.close()
}

func (e *AlpenglowObserverEngine) enqueueVoter(event voterEvent) error {
	e.voterMu.RLock()
	voter := e.voter
	e.voterMu.RUnlock()
	if voter == nil {
		return nil
	}
	return voter.enqueue(event)
}

func (e *AlpenglowObserverEngine) ObserveAlpenglowFirstShred(slot uint64) {
	if slot == 0 || e.safetyError() != nil {
		return
	}
	e.noteAlpenglowLiveSlot(slot)
	if err := e.enqueueVoter(voterEvent{kind: voterEventFirstShred, slot: slot}); err != nil {
		e.latchSafetyError(err)
	}
}

// noteAlpenglowLiveSlot re-anchors the production clock on authenticated live
// traffic. Comparing with the anchor (rather than its extrapolated value) lets
// each later network event correct accumulated 200ms slot-duration drift.
func (e *AlpenglowObserverEngine) noteAlpenglowLiveSlot(slot uint64) {
	if slot == 0 {
		return
	}
	e.liveClockMu.Lock()
	if slot > e.liveClockSlot {
		e.liveClockSlot = slot
		e.liveClockAt = time.Now()
	}
	e.liveClockMu.Unlock()
}

// AlpenglowLiveSlot returns a bounded extrapolation from the latest verified
// first shred or Votor proof. It deliberately becomes only four slots fresher
// than its anchor: enough to cover our own leader window after the preceding
// leader's traffic, but not enough to run hundreds of slots ahead in silence.
func (e *AlpenglowObserverEngine) AlpenglowLiveSlot() (uint64, bool) {
	e.liveClockMu.Lock()
	slot, at := e.liveClockSlot, e.liveClockAt
	e.liveClockMu.Unlock()
	if slot == 0 || at.IsZero() {
		return 0, false
	}
	elapsed := time.Since(at)
	if elapsed < 0 {
		elapsed = 0
	}
	elapsedSlots := uint64(elapsed / alpenglowLiveClockSlotDuration)
	if elapsedSlots > alpenglow.LeaderWindowSlots {
		elapsedSlots = alpenglow.LeaderWindowSlots
	}
	if slot > ^uint64(0)-elapsedSlots {
		return ^uint64(0), true
	}
	return slot + elapsedSlots, true
}

func (e *AlpenglowObserverEngine) ObserveBlock(_ context.Context, obs BlockObservation) error {
	if err := e.safetyError(); err != nil {
		return err
	}
	if obs.Block != nil {
		if e.observedBlocks.Add(1) == 1 {
			mlog.Log.Infof("ALPENGLOW observer: first replay block observed at slot %d (source=%s, alpenglow_block_id=%t)", obs.Block.Slot, obs.Source, obs.Block.HasAlpenglowBlockID)
		}
		blockID := alpenglow.BlockID{Slot: obs.Block.Slot}
		if obs.Block.HasAlpenglowBlockID {
			blockID.Hash = solana.Hash(obs.Block.AlpenglowBlockID)
		}
		replayObs := alpenglow.ReplayBlockObservation{
			Block:      blockID,
			ParentSlot: alpenglowParentSlot(obs.Block),
			// ParentHash must stay in the alpenglow block-id domain (the tracker keys
			// blocks by cert block-id); PoH hashes never match, so zero = unknown.
			ParentHash: parentBlockIDOrZero(obs.Block),
			Source:     obs.Source,
			At:         obs.At,
		}
		e.ensureObserver().ObserveReplayBlock(replayObs)
		e.observedReplayMu.Lock()
		e.observedReplayBlocks[blockID.Slot] = replayObs
		// Replay is sequential and only needs the current speculative horizon.
		for slot := range e.observedReplayBlocks {
			if slot+maxRecentAlpenglowBlockIDs < blockID.Slot {
				delete(e.observedReplayBlocks, slot)
			}
		}
		e.observedReplayMu.Unlock()
		if blockID.HasHash() {
			e.observeChainReplayBlock(replayObs)
		}
	}
	return e.safetyError()
}

func (e *AlpenglowObserverEngine) OnReplayResult(_ context.Context, result SlotReplayResult) error {
	if err := e.safetyError(); err != nil {
		return err
	}
	if result.Slot != 0 {
		if e.replayedSlots.Add(1) == 1 {
			mlog.Log.Infof("ALPENGLOW observer: first replay result at slot %d", result.Slot)
		}
		e.ensureObserver().ObserveReplayResult(alpenglow.ReplayResultObservation{
			Slot:     result.Slot,
			Bankhash: solana.Hash(result.Bankhash),
			Source:   result.Source,
			At:       result.At,
		})
		// Anchor the cert pool's vote window to execution-proven progress — a
		// trusted signal an attacker cannot advance (unlike raw vote slots).
		if e.certPool != nil {
			e.certPool.NoteLiveSlot(result.Slot)
		}
		e.ensurePool().NoteLiveSlot(result.Slot)
		e.observedReplayMu.Lock()
		obs, observed := e.observedReplayBlocks[result.Slot]
		delete(e.observedReplayBlocks, result.Slot)
		e.observedReplayMu.Unlock()
		if observed {
			if err := e.enqueueVoter(voterEvent{kind: voterEventBlock, block: obs}); err != nil {
				e.latchSafetyError(err)
				return err
			}
		}
	}
	return nil
}

func (e *AlpenglowObserverEngine) Snapshot() Snapshot {
	snapshot := Snapshot{
		Mode:           "alpenglow-observer",
		ObservedBlocks: e.observedBlocks.Load(),
		ReplayedSlots:  e.replayedSlots.Load(),
	}
	if e.observer != nil {
		agSnapshot := e.observer.Snapshot()
		snapshot.Alpenglow = &agSnapshot
	}
	if e.receiver != nil {
		receiverStats := e.receiver.Stats()
		snapshot.Receiver = &receiverStats
	}
	if e.chain != nil {
		chainSnapshot := e.chain.Snapshot()
		snapshot.AlpenglowChain = &chainSnapshot
	}
	if e.pool != nil {
		poolSnapshot := e.pool.Snapshot()
		snapshot.AlpenglowPool = &poolSnapshot
	}
	e.voterMu.RLock()
	voter := e.voter
	e.voterMu.RUnlock()
	if voter != nil {
		votingSnapshot := voter.snapshot()
		snapshot.Voting = &votingSnapshot
	}
	return snapshot
}

func (e *AlpenglowObserverEngine) SetAlpenglowBlockIDSink(sink AlpenglowBlockIDSink) {
	e.blockIDSinkMu.Lock()
	e.blockIDSink = sink
	recent := make([]struct {
		slot    uint64
		blockID solana.Hash
	}, 0, len(e.recentBlockIDs))
	if sink != nil {
		for slot, blockID := range e.recentBlockIDs {
			recent = append(recent, struct {
				slot    uint64
				blockID solana.Hash
			}{slot: slot, blockID: blockID})
		}
	}
	e.blockIDSinkMu.Unlock()

	for _, entry := range recent {
		sink(entry.slot, entry.blockID)
	}
}

func (e *AlpenglowObserverEngine) observeVotorMessage(msg alpenglow.Message) {
	_, _ = e.admitVotorMessage(msg)
}

// admitVotorMessage is the trust boundary between wire decoding and observer
// state. The Votor overlay sends the same certificate/vote over many peer
// connections; expensive BLS work is single-flighted and successful results
// are cached by their logical key. The attempt key also hashes the untrusted
// signature/bitmap, so an invalid packet cannot suppress different, valid
// material for the same certificate.
func (e *AlpenglowObserverEngine) admitVotorMessage(msg alpenglow.Message) (alpenglow.Message, bool) {
	if msg.Vote != nil {
		// Raw votes stop here. CertPool verifies them lazily in bounded BLS
		// batches, then invokes acceptVerifiedVote. Neither the observer nor block
		// production sees unauthenticated or merely sampled input.
		if e.certPool != nil {
			e.certPool.AddVote(*msg.Vote)
		}
		return alpenglow.Message{}, false
	}
	if msg.Certificate != nil {
		verified, result, disposition, err := e.verifyCertificateCollapsed(*msg.Certificate, false)
		if disposition == verificationInFlight {
			return alpenglow.Message{}, false
		}
		if err != nil {
			e.logCertificateVerifyDrop(*msg.Certificate, result, err)
			e.deferCertIfStakesMissing(*msg.Certificate, err, true)
			return alpenglow.Message{}, false
		}
		// This exact wire proof arrived over Votor QUIC and passed aggregate
		// BLS/stake verification, so it is a safe live-clock anchor even when
		// the logical certificate was already cached from another source.
		e.noteAlpenglowLiveSlot(verified.Slot)
		// A logical certificate can be cached before this particular wire proof
		// arrives (for example from a block footer). Only attribute network
		// confirmation when the exact signature+bitmap received over Votor QUIC
		// is the proof that was cryptographically verified. Substituting a cached
		// aggregate with a different signer set could falsely claim our rank.
		if disposition == verificationFresh || sameCertificateProof(*msg.Certificate, verified) {
			e.noteNetworkCertificate(verified)
		}
		if disposition != verificationFresh {
			return alpenglow.Message{}, false
		}
		e.acceptVerifiedCertificate(verified)
	}
	// Admission is fully consumed above. Accepted verified messages are
	// delivered exactly once by the ConsensusPool output path.
	return alpenglow.Message{}, false
}

func sameCertificateProof(wire, verified alpenglow.Certificate) bool {
	return wire.Key() == verified.Key() &&
		bytes.Equal(wire.Signature, verified.Signature) &&
		bytes.Equal(wire.Bitmap, verified.Bitmap)
}

// noteNetworkCertificate is called only for a certificate proof received over
// Votor QUIC whose exact signature and bitmap have passed BLS verification.
// Footer certificates and locally assembled certificates never enter here.
func (e *AlpenglowObserverEngine) noteNetworkCertificate(cert alpenglow.Certificate) {
	e.voterMu.RLock()
	voter := e.voter
	e.voterMu.RUnlock()
	if voter == nil {
		return
	}

	proof := certificateAttemptKey{
		key:       cert.Key(),
		signature: sha256.Sum256(cert.Signature),
		bitmap:    sha256.Sum256(cert.Bitmap),
	}
	e.networkProofsMu.Lock()
	if _, seen := e.networkProofs[proof]; seen {
		e.networkProofsMu.Unlock()
		return
	}
	if e.networkProofs == nil {
		e.networkProofs = make(map[certificateAttemptKey]struct{})
	}
	e.networkProofs[proof] = struct{}{}
	e.networkProofOrder = append(e.networkProofOrder, proof)
	for len(e.networkProofs) > maxVerifiedVotorCertificates {
		old := e.networkProofOrder[0]
		e.networkProofOrder = e.networkProofOrder[1:]
		delete(e.networkProofs, old)
	}
	e.networkProofsMu.Unlock()

	if err := voter.enqueue(voterEvent{kind: voterEventNetworkCertificate, certificate: cert}); err != nil {
		// Landing telemetry must never trip consensus safety. Permit a later
		// duplicate proof to retry after transient queue pressure.
		e.networkProofsMu.Lock()
		delete(e.networkProofs, proof)
		for i := len(e.networkProofOrder) - 1; i >= 0; i-- {
			if e.networkProofOrder[i] != proof {
				continue
			}
			copy(e.networkProofOrder[i:], e.networkProofOrder[i+1:])
			e.networkProofOrder = e.networkProofOrder[:len(e.networkProofOrder)-1]
			break
		}
		e.networkProofsMu.Unlock()
		mlog.Log.FileOnlyf("ALPENGLOW voting: dropped network certificate landing observation at slot %d: %v", cert.Slot, err)
	}
}

func (e *AlpenglowObserverEngine) acceptVerifiedVote(vote alpenglow.VerifiedVote) {
	if e.safetyError() != nil {
		return
	}
	e.poolOutputMu.Lock()
	defer e.poolOutputMu.Unlock()
	if e.safetyError() != nil {
		return
	}
	update, err := e.ensurePool().AddVerifiedVote(vote)
	if err != nil {
		fault := fmt.Errorf("consensus pool rejected verified %s vote at slot %d rank %d: %w", vote.Message.Vote.Type, vote.Message.Vote.Slot, vote.Message.Rank, err)
		e.latchSafetyError(fault)
		mlog.Log.Errorf("ALPENGLOW SAFETY: %v", fault)
		return
	}
	if err := e.handleConsensusUpdate(update); err != nil {
		e.latchSafetyError(err)
		mlog.Log.Errorf("ALPENGLOW SAFETY: consensus-pool vote output rejected downstream: %v", err)
		return
	}
	if len(update.Certificates) > 0 {
		e.voterMu.RLock()
		voter := e.voter
		e.voterMu.RUnlock()
		if voter != nil {
			if err := voter.broadcastCertificates(update.Certificates); err != nil {
				e.latchSafetyError(fmt.Errorf("broadcast locally assembled certificates: %w", err))
				return
			}
		}
	}
	if !update.VoteAccepted {
		return
	}
	if !vote.Local {
		e.noteAlpenglowLiveSlot(vote.Message.Vote.Slot)
	}
	if _, err := e.ensureObserver().ObserveVote(vote.Message); err != nil {
		fault := fmt.Errorf("observer rejected pool-accepted %s vote at slot %d: %w", vote.Message.Vote.Type, vote.Message.Vote.Slot, err)
		e.latchSafetyError(fault)
		mlog.Log.Errorf("ALPENGLOW SAFETY: %v", fault)
		return
	}
	e.deliverVerifiedVotorMessage(alpenglow.Message{Vote: &vote.Message})
}

func (e *AlpenglowObserverEngine) injectLocalVote(message alpenglow.VoteMessage, result alpenglow.VoteVerifyResult) error {
	if err := e.safetyError(); err != nil {
		return err
	}
	if epoch, ok := e.alpenglowEpochForSlot(message.Vote.Slot); ok && epoch != result.Epoch {
		return fmt.Errorf("local vote epoch mismatch at slot %d: slot lookup=%d signer=%d", message.Vote.Slot, epoch, result.Epoch)
	}
	// Verify locally even though this process just signed it. This protects the
	// pool from rank/DST/encoding integration mistakes and makes local and
	// network admission share the same cryptographic boundary. Validator sets
	// for the next epoch are normally preloaded, so verification must use the
	// vote slot's selected epoch rather than the verifier's latest installed set.
	verifiedResult, err := e.ensureVerifier().VerifyVoteMessageForEpoch(result.Epoch, message)
	if err != nil {
		return fmt.Errorf("self-verify signed vote: %w", err)
	}
	if verifiedResult != result {
		return fmt.Errorf("self-verified vote metadata mismatch: got %+v want %+v", verifiedResult, result)
	}
	e.acceptVerifiedVote(alpenglow.VerifiedVote{Message: message, Result: verifiedResult, Local: true})
	if err := e.safetyError(); err != nil {
		return err
	}
	if !e.ensurePool().HasVerifiedVote(message) {
		return fmt.Errorf("verified consensus pool did not accept local %s vote at slot %d rank %d", message.Vote.Type, message.Vote.Slot, message.Rank)
	}
	return nil
}

func (e *AlpenglowObserverEngine) acceptVerifiedCertificate(verified alpenglow.Certificate) (alpenglow.ConsensusUpdate, bool) {
	if e.safetyError() != nil {
		return alpenglow.ConsensusUpdate{}, false
	}
	e.poolOutputMu.Lock()
	defer e.poolOutputMu.Unlock()
	if e.safetyError() != nil {
		return alpenglow.ConsensusUpdate{}, false
	}
	update, err := e.ensurePool().AddVerifiedCertificate(verified)
	if err != nil {
		fault := fmt.Errorf("consensus pool rejected verified %s certificate at slot %d: %w", verified.Type, verified.Slot, err)
		e.latchSafetyError(fault)
		mlog.Log.Errorf("ALPENGLOW SAFETY: %v", fault)
		return alpenglow.ConsensusUpdate{}, false
	}
	if len(update.Certificates) == 0 {
		return update, false
	}
	if err := e.handleConsensusUpdate(update); err != nil {
		e.latchSafetyError(err)
		mlog.Log.Errorf("ALPENGLOW SAFETY: consensus-pool certificate rejected downstream: %v", err)
		return alpenglow.ConsensusUpdate{}, false
	}
	e.deliverVerifiedVotorMessage(alpenglow.NewCertificateMessage(verified))
	return update, true
}

func (e *AlpenglowObserverEngine) handleConsensusUpdate(update alpenglow.ConsensusUpdate) error {
	for _, cert := range update.Certificates {
		if _, err := e.ensureObserver().ObserveCertificate(cert); err != nil {
			return fmt.Errorf("observer rejected accepted %s certificate at slot %d: %w", cert.Type, cert.Slot, err)
		}
		chain := e.ensureChain()
		chainUpdate, err := chain.ObserveCertificate(cert)
		if err != nil {
			return fmt.Errorf("chain tracker rejected accepted %s certificate at slot %d: %w", cert.Type, cert.Slot, err)
		}
		if chainUpdate.Trusted {
			if reason, conflicted := chain.FinalityConflictReasonAt(cert.Slot); conflicted {
				return fmt.Errorf("chain tracker detected accepted-certificate conflict at slot %d: %s", cert.Slot, reason)
			}
		}
		if cert.Type == alpenglow.CertificateGenesis {
			block, ok := cert.Block()
			if !ok {
				return fmt.Errorf("accepted genesis certificate at slot %d has no block identity", cert.Slot)
			}
			if err := chain.ObserveFinalized(block, cert.Type); err != nil {
				return fmt.Errorf("chain tracker rejected genesis finalization at slot %d: %w", cert.Slot, err)
			}
			e.ensurePool().PruneBehindFinality(block.Slot)
		}
		e.observeVotorBlockID(alpenglow.NewCertificateMessage(cert))
	}
	for _, event := range update.Events {
		switch event.Kind {
		case alpenglow.ConsensusEventFinalized:
			if err := e.ensureChain().ObserveFinalized(event.Block, event.CertificateType); err != nil {
				return fmt.Errorf("chain tracker rejected pool finalization at slot %d: %w", event.Slot, err)
			}
			e.ensurePool().PruneBehindFinality(event.Slot)
		case alpenglow.ConsensusEventConflict:
			fault := fmt.Errorf("consensus pool conflict at slot %d: %s", event.Slot, event.Reason)
			e.latchSafetyError(fault)
			mlog.Log.Errorf("ALPENGLOW SAFETY: %v", fault)
		}
		if err := e.enqueueVoter(voterEvent{kind: voterEventConsensus, consensus: event}); err != nil {
			return fmt.Errorf("deliver %s event to voting engine: %w", event.Kind, err)
		}
	}
	return e.safetyError()
}

func (e *AlpenglowObserverEngine) deliverVerifiedVotorMessage(msg alpenglow.Message) {
	e.votorMessageHookMu.RLock()
	hook := e.votorMessageHook
	e.votorMessageHookMu.RUnlock()
	if hook != nil {
		hook(msg)
	}
}

// ObserveFooterCertificates verifies and ingests certificates decoded from a block
// footer (the unstaked finality path — no Votor QUIC needed). Each is verified and
// fed to the chain tracker exactly like a QUIC cert. Returns the blocks these certs
// finalize (fast: the FinalizeFast block; slow: the notarized block paired with the
// Finalize cert), derived from the verified certs themselves so the result cannot
// be raced away by tracker pruning.
func (e *AlpenglowObserverEngine) ObserveFooterCertificates(certs []alpenglow.Certificate) []alpenglow.BlockID {
	finalizeSlots := make(map[uint64]struct{})
	for _, cert := range certs {
		// A footer may race the same certificate arriving over Votor QUIC.
		// Wait for that exact in-flight verification instead of duplicating the
		// pairing work; cached certificates still participate in this footer's
		// finalized-block result below.
		verified, result, _, err := e.verifyCertificateCollapsed(cert, true)
		if err != nil {
			e.logCertificateVerifyDrop(cert, result, err)
			e.deferCertIfStakesMissing(cert, err, false)
			continue
		}
		switch verified.Type {
		case alpenglow.CertificateFinalizeFast, alpenglow.CertificateFinalize, alpenglow.CertificateGenesis:
			finalizeSlots[verified.Slot] = struct{}{}
		}
		e.acceptVerifiedCertificate(verified)
	}
	var finalized []alpenglow.BlockID
	for slot := range finalizeSlots {
		if block, ok := e.ensureChain().FinalizedBlockAt(slot); ok {
			finalized = append(finalized, block)
		}
	}
	return finalized
}

// alpenglowPendingCertCap bounds deferred certs per epoch; alpenglowPendingEpochCap
// bounds the number of distinct epoch buckets. Certs carry a network-controlled slot
// and are buffered before authentication, so both caps are needed to keep a peer
// feeding far-future/garbage slots from growing the buffer without bound.
const (
	alpenglowPendingCertCap  = 512
	alpenglowPendingEpochCap = 4
)

type pendingCertificate struct {
	certificate alpenglow.Certificate
	fromNetwork bool
}

// deferCertIfStakesMissing buffers a certificate that failed verification only
// because its epoch's validator set isn't installed yet, so it can be replayed once
// the stakes arrive (otherwise a QUIC cert that races ahead of its epoch is lost and
// that slot's decision stalls). Certs that fail for any other reason are not buffered.
func (e *AlpenglowObserverEngine) deferCertIfStakesMissing(cert alpenglow.Certificate, err error, fromNetwork bool) {
	if err == nil || !strings.Contains(err.Error(), "no validator set") {
		return
	}
	epoch, ok := e.alpenglowEpochForSlot(cert.Slot)
	if !ok {
		return
	}
	// Only defer certs for epochs plausibly near the installed sets — a cert with a
	// far-off epoch can never verify soon and would just occupy buckets.
	if latest := e.ensureVerifier().LatestEpoch(); latest > 0 {
		if epoch+1 < latest || epoch > latest+2 {
			return
		}
	}
	e.pendingCertsMu.Lock()
	defer e.pendingCertsMu.Unlock()
	if e.pendingCerts == nil {
		e.pendingCerts = make(map[uint64][]pendingCertificate)
	}
	// Bound distinct epoch buckets: evict the HIGHEST epoch when a new one would
	// exceed the cap — garbage slots are typically far-future, genuine pending
	// epochs sit near the current one.
	if _, exists := e.pendingCerts[epoch]; !exists && len(e.pendingCerts) >= alpenglowPendingEpochCap {
		highest, first := uint64(0), true
		for ep := range e.pendingCerts {
			if first || ep > highest {
				highest, first = ep, false
			}
		}
		delete(e.pendingCerts, highest)
	}
	q := e.pendingCerts[epoch]
	if len(q) >= alpenglowPendingCertCap {
		q = q[1:] // drop oldest
	}
	e.pendingCerts[epoch] = append(q, pendingCertificate{certificate: cert, fromNetwork: fromNetwork})
}

// replayPendingCertsForEpoch re-verifies and ingests certs deferred until this
// epoch's validator set installed.
func (e *AlpenglowObserverEngine) replayPendingCertsForEpoch(epoch uint64) {
	e.pendingCertsMu.Lock()
	certs := e.pendingCerts[epoch]
	delete(e.pendingCerts, epoch)
	e.pendingCertsMu.Unlock()
	for _, pending := range certs {
		verified, _, err := e.verifyCertificate(pending.certificate)
		if err != nil {
			continue
		}
		if pending.fromNetwork {
			e.noteNetworkCertificate(verified)
		}
		e.acceptVerifiedCertificate(verified)
	}
}

func (e *AlpenglowObserverEngine) observeVotorBlockID(msg alpenglow.Message) {
	if msg.Certificate == nil {
		return
	}
	blockID, ok := msg.Certificate.Block()
	if !ok || !blockID.HasHash() {
		return
	}
	decisive, _, ok := e.ensureChain().CertifiedBlockAt(blockID.Slot)
	if !ok || decisive != blockID {
		// Fallback-certified siblings are repair leads, not a safe hard filter for
		// the shred assembler. Publish only a decisive block identity.
		return
	}

	e.blockIDSinkMu.Lock()
	if e.recentBlockIDs == nil {
		e.recentBlockIDs = make(map[uint64]solana.Hash)
	}
	if existing, exists := e.recentBlockIDs[blockID.Slot]; !exists {
		e.recentBlockIDOrder = append(e.recentBlockIDOrder, blockID.Slot)
	} else if existing == blockID.Hash {
		sink := e.blockIDSink
		e.blockIDSinkMu.Unlock()
		if sink != nil {
			sink(blockID.Slot, blockID.Hash)
		}
		return
	}
	e.recentBlockIDs[blockID.Slot] = blockID.Hash
	for len(e.recentBlockIDOrder) > maxRecentAlpenglowBlockIDs {
		old := e.recentBlockIDOrder[0]
		e.recentBlockIDOrder = e.recentBlockIDOrder[1:]
		delete(e.recentBlockIDs, old)
	}
	sink := e.blockIDSink
	e.blockIDSinkMu.Unlock()
	if sink != nil {
		sink(blockID.Slot, blockID.Hash)
	}
}

func (e *AlpenglowObserverEngine) NextAlpenglowDecision(anchorSlot uint64) (alpenglow.ChainDecision, bool) {
	if err := e.safetyError(); err != nil {
		return alpenglow.ChainDecision{
			Slot:   anchorSlot + 1,
			Kind:   alpenglow.ChainDecisionKindConflict,
			Reason: err.Error(),
		}, true
	}
	return e.ensureChain().NextDecision(anchorSlot)
}

func (e *AlpenglowObserverEngine) FinalizedBlockAt(slot uint64) (alpenglow.BlockID, bool) {
	return e.ensureChain().FinalizedBlockAt(slot)
}

func (e *AlpenglowObserverEngine) FinalityConflictAt(slot uint64) bool {
	return e.ensureChain().FinalityConflictAt(slot)
}

func (e *AlpenglowObserverEngine) ObserveAlpenglowCandidateBlock(obs alpenglow.ReplayBlockObservation) {
	if !obs.Block.HasHash() {
		return
	}
	e.observeChainReplayBlock(obs)
}

func (e *AlpenglowObserverEngine) observeChainReplayBlock(obs alpenglow.ReplayBlockObservation) {
	update := e.ensureChain().ObserveReplayBlock(obs)
	if !update.Conflict {
		return
	}
	fault := fmt.Errorf("chain tracker detected replay-link conflict at slot %d: %s", update.ConflictSlot, update.ConflictReason)
	e.latchSafetyError(fault)
	mlog.Log.Errorf("ALPENGLOW SAFETY: %v", fault)
}

func (e *AlpenglowObserverEngine) SetAlpenglowValidatorSet(set alpenglow.ValidatorSet) error {
	if err := e.ensureVerifier().SetValidatorSet(set); err != nil {
		return err
	}
	e.validatorSetsMu.Lock()
	e.validatorSets[set.Epoch] = cloneValidatorSet(set)
	e.validatorSetsMu.Unlock()
	mlog.Log.FileOnlyf("ALPENGLOW observer: installed validator set for epoch %d (validators=%d total_stake=%d)", set.Epoch, len(set.Validators), set.TotalStake)
	if e.certPool != nil {
		e.certPool.OnValidatorSetInstalled(set.Epoch)
	}
	// Install the set in the voter before replaying deferred network material.
	// That keeps both consensus events and landing-proof events ordered behind
	// the rank map they need.
	if err := e.enqueueVoter(voterEvent{kind: voterEventValidatorSet, set: cloneValidatorSet(set)}); err != nil {
		e.latchSafetyError(err)
		return err
	}
	e.replayPendingCertsForEpoch(set.Epoch)
	return nil
}

func (e *AlpenglowObserverEngine) SetAlpenglowRoot(block alpenglow.BlockID) {
	e.poolOutputMu.Lock()
	defer e.poolOutputMu.Unlock()

	pool := e.ensurePool()
	if snapshot := pool.Snapshot(); snapshot.RootSlot == 0 && block.Slot > 0 && block.HasHash() {
		pool.RestoreParentReady(block.Slot+1, block)
	} else {
		pool.SetRoot(block)
	}
	// A restart root is also the lower bound for every downstream structure.
	// Otherwise old certificates received immediately after startup could regrow
	// historical chain state or consume the raw-vote pool before replay advances.
	e.ensureChain().PruneBeforeSlot(block.Slot)
	if e.certPool != nil {
		e.certPool.ObserveFloor(block.Slot)
	}
	if err := e.enqueueVoter(voterEvent{kind: voterEventRoot, root: block}); err != nil {
		e.latchSafetyError(err)
	}
}

func (e *AlpenglowObserverEngine) AlpenglowBlockProductionParent(slot uint64) alpenglow.BlockProductionParent {
	if e.safetyError() != nil {
		return alpenglow.BlockProductionParent{Kind: alpenglow.BlockProductionParentNotReady}
	}
	return e.ensurePool().BlockProductionParent(slot)
}

// FlushAlpenglowRewardVotes makes the lazy verifier publish all valid plain
// skip/notarize votes needed by the slot+8 reward footer.
func (e *AlpenglowObserverEngine) FlushAlpenglowRewardVotes(slot uint64) {
	if e.safetyError() != nil || e.certPool == nil {
		return
	}
	e.certPool.FlushRewardVotes(slot)
}

// CertifiedBlockAt exposes the tracker's decisive block for the switch sweep.
func (e *AlpenglowObserverEngine) CertifiedBlockAt(slot uint64) (alpenglow.BlockID, alpenglow.CertificateType, bool) {
	return e.ensureChain().CertifiedBlockAt(slot)
}

// SkipCertifiedAt exposes certified skips for the switch sweep.
func (e *AlpenglowObserverEngine) SkipCertifiedAt(slot uint64) bool {
	return e.ensureChain().SkipCertifiedAt(slot)
}

// ChainDecisionVersion gates the sweep: it re-walks executed slots whenever the
// tracker became more decisive, including replay-derived changes that land
// without a new certificate.
func (e *AlpenglowObserverEngine) ChainDecisionVersion() uint64 {
	return e.ensureChain().DecisionVersion()
}

// AlpenglowWantedBlocks lists certified-but-unobserved blocks for cert-driven
// repair (ascending, capped).
func (e *AlpenglowObserverEngine) AlpenglowWantedBlocks(afterSlot uint64, max int) []alpenglow.WantedBlock {
	return e.ensureChain().WantedBlocks(afterSlot, max)
}

func (e *AlpenglowObserverEngine) SetAlpenglowEpochLookup(fn func(slot uint64) uint64) {
	e.epochLookupMu.Lock()
	e.epochForSlot = fn
	e.epochLookupMu.Unlock()
	if e.certPool != nil {
		e.certPool.SetEpochLookup(fn)
	}
}

// PruneAlpenglowBefore advances the durable consensus root. ChainTracker keeps
// the exact root and drops older ancestry; the pool/tally front ends no longer
// need votes through the rooted slot.
// Equivocation and conflict evidence survive pruning by design.
func (e *AlpenglowObserverEngine) PruneAlpenglowBefore(slot uint64) {
	if slot == 0 {
		return
	}
	e.poolOutputMu.Lock()
	defer e.poolOutputMu.Unlock()

	chain := e.ensureChain()
	root := alpenglow.BlockID{Slot: slot}
	if finalized, ok := chain.FinalizedBlockAt(slot); ok {
		root = finalized
	}
	e.ensurePool().SetRoot(root)
	chain.PruneBeforeSlot(slot)
	if e.certPool != nil {
		e.certPool.ObserveFloor(slot)
	}
	if err := e.enqueueVoter(voterEvent{kind: voterEventRoot, root: root}); err != nil {
		e.latchSafetyError(err)
	}
}

func (e *AlpenglowObserverEngine) latchSafetyError(err error) {
	if err == nil {
		return
	}
	e.safetyMu.Lock()
	if e.safetyErr == nil {
		e.safetyErr = fmt.Errorf("alpenglow safety fault: %w", err)
	}
	e.safetyMu.Unlock()
}

func (e *AlpenglowObserverEngine) safetyError() error {
	e.safetyMu.RLock()
	err := e.safetyErr
	e.safetyMu.RUnlock()
	return err
}

func (e *AlpenglowObserverEngine) AlpenglowSafetyError() error {
	return e.safetyError()
}

func (e *AlpenglowObserverEngine) Close() error {
	e.voterMu.Lock()
	voter := e.voter
	e.voter = nil
	e.voterMu.Unlock()
	if voter != nil {
		if err := voter.close(); err != nil {
			return err
		}
	}
	if e.receiver != nil {
		return e.receiver.Close()
	}
	return nil
}

func (e *AlpenglowObserverEngine) ensureObserver() *alpenglow.Observer {
	if e.observer == nil {
		e.observer = alpenglow.NewObserver()
	}
	return e.observer
}

func (e *AlpenglowObserverEngine) ensureChain() *alpenglow.ChainTracker {
	if e.chain == nil {
		e.chain = newAlpenglowObserverChainTracker()
	}
	return e.chain
}

func (e *AlpenglowObserverEngine) ensurePool() *alpenglow.ConsensusPool {
	if e.pool == nil {
		e.pool = alpenglow.NewConsensusPool(alpenglow.DefaultConsensusPoolConfig())
	}
	return e.pool
}

func (e *AlpenglowObserverEngine) ensureVerifier() *alpenglow.CertificateVerifier {
	if e.verifier == nil {
		e.verifier = alpenglow.NewCertificateVerifier()
	}
	return e.verifier
}

func (e *AlpenglowObserverEngine) verifyCertificate(cert alpenglow.Certificate) (alpenglow.Certificate, alpenglow.CertificateVerifyResult, error) {
	if epoch, ok := e.alpenglowEpochForSlot(cert.Slot); ok {
		return e.ensureVerifier().VerifyCertificateForEpoch(epoch, cert)
	}
	return e.ensureVerifier().VerifyCertificate(cert)
}

func (e *AlpenglowObserverEngine) verifyCertificateCollapsed(cert alpenglow.Certificate, wait bool) (alpenglow.Certificate, alpenglow.CertificateVerifyResult, verificationDisposition, error) {
	logical := cert.Key()
	attempt := certificateAttemptKey{
		key:       logical,
		signature: sha256.Sum256(cert.Signature),
		bitmap:    sha256.Sum256(cert.Bitmap),
	}

	e.verificationMu.Lock()
	if verified, ok := e.verifiedCertificates[logical]; ok {
		e.certificateCacheHits.Add(1)
		e.verificationMu.Unlock()
		return verified, certificateResultFromVerified(verified), verificationCached, nil
	}
	if pending := e.certificateInFlight[attempt]; pending != nil {
		e.certificateInflightDrops.Add(1)
		e.verificationMu.Unlock()
		if !wait {
			return alpenglow.Certificate{}, alpenglow.CertificateVerifyResult{}, verificationInFlight, nil
		}
		<-pending.done
		return pending.verified, pending.result, verificationCached, pending.err
	}
	if e.certificateInFlight == nil {
		e.certificateInFlight = make(map[certificateAttemptKey]*certificateVerification)
	}
	pending := &certificateVerification{done: make(chan struct{})}
	e.certificateInFlight[attempt] = pending
	e.verificationMu.Unlock()

	verified, result, err := e.verifyCertificate(cert)

	e.verificationMu.Lock()
	pending.verified, pending.result, pending.err = verified, result, err
	delete(e.certificateInFlight, attempt)
	if err == nil {
		if e.verifiedCertificates == nil {
			e.verifiedCertificates = make(map[alpenglow.CertificateKey]alpenglow.Certificate)
		}
		if _, exists := e.verifiedCertificates[logical]; !exists {
			e.verifiedCertificateOrder = append(e.verifiedCertificateOrder, logical)
		}
		e.verifiedCertificates[logical] = verified
		for len(e.verifiedCertificates) > maxVerifiedVotorCertificates {
			old := e.verifiedCertificateOrder[0]
			e.verifiedCertificateOrder = e.verifiedCertificateOrder[1:]
			delete(e.verifiedCertificates, old)
		}
	}
	close(pending.done)
	e.verificationMu.Unlock()
	return verified, result, verificationFresh, err
}

func certificateResultFromVerified(cert alpenglow.Certificate) alpenglow.CertificateVerifyResult {
	return alpenglow.CertificateVerifyResult{
		IncludedStake:     cert.IncludedStake,
		TotalStake:        cert.TotalStake,
		StakeVerified:     cert.StakeVerified,
		SignatureVerified: cert.SignatureVerified,
	}
}

func (e *AlpenglowObserverEngine) alpenglowEpochForSlot(slot uint64) (uint64, bool) {
	e.epochLookupMu.RLock()
	fn := e.epochForSlot
	e.epochLookupMu.RUnlock()
	if fn == nil {
		return 0, false
	}
	return fn(slot), true
}

func (e *AlpenglowObserverEngine) logCertificateVerifyDrop(cert alpenglow.Certificate, result alpenglow.CertificateVerifyResult, err error) {
	e.certVerifyLogMu.Lock()
	e.certVerifyDropCount++
	now := time.Now()
	shouldLog := e.lastCertVerifyLog.IsZero() || now.Sub(e.lastCertVerifyLog) >= 10*time.Second
	if shouldLog {
		e.lastCertVerifyLog = now
		e.certVerifyDetailCount++
	}
	drops := e.certVerifyDropCount
	details := e.certVerifyDetailCount
	e.certVerifyLogMu.Unlock()

	if !shouldLog {
		return
	}

	var diag alpenglow.CertificateDiagnostics
	if epoch, ok := e.alpenglowEpochForSlot(cert.Slot); ok {
		diag = e.ensureVerifier().DiagnoseCertificateForEpoch(epoch, cert, 8)
	} else {
		diag = e.ensureVerifier().DiagnoseCertificate(cert, 8)
	}
	mlog.Log.FileOnlyf("ALPENGLOW observer: ignored certificate before certificate verification (drops=%d latest=%v)", drops, err)
	mlog.Log.FileOnlyf("ALPENGLOW observer: certificate verify debug #%d: cert=%s slot=%d block=%s epoch=%d validators=%d bitmap=%s bits=%d bytes=%d signers=%d base=%d fallback=%d stake=%d/%d result={epoch:%d signers:%d stake:%d/%d stake_ok:%t sig_ok:%t} payload_lens=%d/%d base_ranks=%v fallback_ranks=%v signer_samples=%s bitmap_error=%q",
		details,
		cert.Type,
		cert.Slot,
		cert.BlockHash,
		diag.Epoch,
		diag.ValidatorCount,
		diag.BitmapEncoding,
		diag.BitmapLength,
		diag.BitmapBytes,
		diag.SignerCount,
		diag.BaseSignerCount,
		diag.FallbackSignerCount,
		diag.IncludedStake,
		diag.TotalStake,
		result.Epoch,
		result.SignerCount,
		result.IncludedStake,
		result.TotalStake,
		result.StakeVerified,
		result.SignatureVerified,
		diag.PrimaryPayloadLen,
		diag.FallbackPayloadLen,
		diag.BaseRanks,
		diag.FallbackRanks,
		formatSignerSamples(diag.SignerSamples),
		diag.BitmapError,
	)
}

func formatSignerSamples(samples []alpenglow.SignerSample) string {
	if len(samples) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(samples))
	for _, sample := range samples {
		bls := sample.BLSPubkeyHex
		if bls == "" {
			bls = sample.BLSPubkeyPrefix
		}
		parts = append(parts, fmt.Sprintf("{rank:%d stake:%d vote:%s node:%s bls:%s}",
			sample.Rank,
			sample.Stake,
			sample.VoteAccount,
			sample.NodePubkey,
			bls,
		))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func newAlpenglowObserverChainTracker() *alpenglow.ChainTracker {
	return alpenglow.NewChainTrackerWithConfig(alpenglow.ChainConfig{
		RequireVerifiedCertificates:      true,
		RequireStakeVerifiedCertificates: true,
	})
}

func alpenglowParentSlot(block *block.Block) uint64 {
	if block == nil {
		return 0
	}
	if block.SourceParentSlot != 0 {
		return block.SourceParentSlot
	}
	return block.ParentSlot
}

func parentBlockIDOrZero(block *block.Block) solana.Hash {
	if block == nil || !block.HasAlpenglowParentBlockID {
		return solana.Hash{}
	}
	return solana.Hash(block.AlpenglowParentBlockID)
}
