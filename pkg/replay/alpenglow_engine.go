package replay

import (
	"fmt"
	"sort"

	"github.com/Overclock-Validator/mithril/pkg/alpenglow"
	b "github.com/Overclock-Validator/mithril/pkg/block"
	consensusengine "github.com/Overclock-Validator/mithril/pkg/consensus"
	"github.com/Overclock-Validator/mithril/pkg/epochstakes"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/turbine"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
)

// isAlpenglowReplayMode reports whether the consensus engine runs Alpenglow
// (observer or full), which switches replay to Alpenglow clock + finality semantics.
func isAlpenglowReplayMode(consensusOpts *ConsensusOpts) bool {
	if consensusOpts == nil || consensusOpts.Mode == "" {
		return false
	}
	mode, err := consensusengine.NormalizeMode(consensusOpts.Mode)
	if err != nil {
		return false
	}
	return mode == consensusengine.ModeAlpenglowObserver || mode == consensusengine.ModeAlpenglow
}

// alpenglowRootedSlot returns the highest slot the engine has seen a finalization
// certificate for — the Alpenglow finality watermark that drives promotion, the
// certificate-based counterpart to TowerBFT's HighestRootedSlot.
func alpenglowRootedSlot(consensusEngine consensusengine.Engine) (uint64, bool) {
	if consensusEngine == nil {
		return 0, false
	}
	snap := consensusEngine.Snapshot()
	if snap.AlpenglowChain == nil {
		return 0, false
	}
	slot := snap.AlpenglowChain.LatestDirectFinalizedBlock.Slot
	return slot, slot > 0
}

// installAlpenglowValidatorSet builds and installs the BLS validator set for one
// epoch into the engine (no-op if the engine isn't a validator-set sink).
func installAlpenglowValidatorSet(consensusEngine consensusengine.Engine, epoch uint64) {
	sink, ok := consensusEngine.(consensusengine.AlpenglowValidatorSetSink)
	if !ok {
		return
	}

	stakes := global.EpochStakes(epoch)
	voteAccts := alpenglowVoteAccountsForEpoch(epoch, stakes)
	set, err := alpenglow.BuildValidatorSet(epoch, stakes, voteAccts, global.EpochTotalStake(epoch))
	if err != nil {
		mlog.Log.FileOnlyf("ALPENGLOW observer: validator set unavailable for epoch %d: %v", epoch, err)
		return
	}
	if err := sink.SetAlpenglowValidatorSet(set); err != nil {
		mlog.Log.FileOnlyf("ALPENGLOW observer: failed to install validator set for epoch %d: %v", epoch, err)
	}
}

// installCachedAlpenglowValidatorSets installs validator sets for every cached
// epoch (so certs spanning an epoch boundary verify), plus the current epoch.
func installCachedAlpenglowValidatorSets(consensusEngine consensusengine.Engine, currentEpoch uint64) {
	sink, ok := consensusEngine.(consensusengine.AlpenglowValidatorSetSink)
	if !ok {
		return
	}

	epochs := global.GetAllCachedEpochs()
	if len(epochs) == 0 {
		installAlpenglowValidatorSet(consensusEngine, currentEpoch)
		return
	}
	sort.Slice(epochs, func(i, j int) bool { return epochs[i] < epochs[j] })
	installedCurrent := false
	for _, epoch := range epochs {
		if epoch == currentEpoch {
			installedCurrent = true
		}
		stakes := global.EpochStakes(epoch)
		voteAccts := alpenglowVoteAccountsForEpoch(epoch, stakes)
		set, err := alpenglow.BuildValidatorSet(epoch, stakes, voteAccts, global.EpochTotalStake(epoch))
		if err != nil {
			mlog.Log.FileOnlyf("ALPENGLOW observer: validator set unavailable for cached epoch %d: %v", epoch, err)
			continue
		}
		if err := sink.SetAlpenglowValidatorSet(set); err != nil {
			mlog.Log.FileOnlyf("ALPENGLOW observer: failed to install cached validator set for epoch %d: %v", epoch, err)
		}
	}
	if !installedCurrent {
		installAlpenglowValidatorSet(consensusEngine, currentEpoch)
	}
}

func alpenglowVoteAccountsForEpoch(epoch uint64, stakes map[solana.PublicKey]uint64) map[solana.PublicKey]*epochstakes.VoteAccount {
	voteAccts := make(map[solana.PublicKey]*epochstakes.VoteAccount, len(stakes))
	for voteAcct, meta := range global.EpochStakesVoteAccts(epoch) {
		if meta == nil {
			continue
		}
		copied := *meta
		copied.BlsPubkeyCompressed = cloneBLSCompressed(meta.BlsPubkeyCompressed)
		voteAccts[voteAcct] = &copied
	}
	return voteAccts
}

func cloneBLSCompressed(src *[48]byte) *[48]byte {
	if src == nil {
		return nil
	}
	copied := *src
	return &copied
}

// applyAlpenglowFooterClock rewrites the Clock sysvar from the block footer's
// timestamp (Alpenglow uses the footer time, not the estimated PoH time).
func applyAlpenglowFooterClock(slotCtx *sealevel.SlotCtx, block *b.Block, epochSchedule *sealevel.SysvarEpochSchedule) error {
	if block.UnixTimestamp == 0 {
		return nil
	}

	clockAcct, err := slotCtx.GetAccount(sealevel.SysvarClockAddr)
	if err != nil {
		return fmt.Errorf("unable to get clock sysvar for Alpenglow footer update: %w", err)
	}

	decoder := bin.NewBinDecoder(clockAcct.Data)
	var clock sealevel.SysvarClock
	if err := clock.UnmarshalWithDecoder(decoder); err != nil {
		return fmt.Errorf("unable to unmarshal clock sysvar for Alpenglow footer update: %w", err)
	}
	if err := updateClockSysvarFromAlpenglowFooter(&clock, block, epochSchedule); err != nil {
		return err
	}

	newClockBytes := clock.MustMarshal()
	copy(clockAcct.Data, newClockBytes)
	// slotCtx.GetAccount returns a clone, so write the footer-updated clock back
	// into slot state — otherwise the timestamp is dropped from the bankhash.
	if err := slotCtx.SetAccount(sealevel.SysvarClockAddr, clockAcct); err != nil {
		return fmt.Errorf("unable to write Alpenglow footer clock back to slot state: %w", err)
	}
	sealevel.SysvarCache.Clock.Sysvar = &clock
	sealevel.SysvarCache.Clock.Acct = clockAcct
	return nil
}

// ingestAlpenglowFooterCertificate decodes a block-footer final_cert and feeds the
// verified certificate(s) to the observer engine's chain tracker (the unstaked
// finality path). No-op unless the engine accepts footer certificates.
func ingestAlpenglowFooterCertificate(consensusEngine consensusengine.Engine, raw []byte) {
	sink, ok := consensusEngine.(consensusengine.AlpenglowFooterCertificateSink)
	if !ok {
		return
	}
	fc, err := turbine.UnmarshalFinalCertificate(raw)
	if err != nil {
		mlog.Log.FileOnlyf("ALPENGLOW footer cert decode: %v", err)
		return
	}
	var notarSig, notarBitmap []byte
	if fc.NotarAggregate != nil {
		notarSig = fc.NotarAggregate.Signature[:]
		notarBitmap = fc.NotarAggregate.Bitmap
	}
	certs, err := alpenglow.FinalCertToCertificates(fc.Slot, fc.BlockID,
		fc.FinalAggregate.Signature[:], fc.FinalAggregate.Bitmap, notarSig, notarBitmap)
	if err != nil {
		mlog.Log.FileOnlyf("ALPENGLOW footer cert convert (slot %d): %v", fc.Slot, err)
		return
	}
	sink.ObserveFooterCertificates(certs)
}
