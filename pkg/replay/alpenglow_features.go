package replay

import (
	"github.com/Overclock-Validator/mithril/pkg/consensus"
	"github.com/Overclock-Validator/mithril/pkg/features"
)

func isAlpenglowReplayMode(consensusOpts *ConsensusOpts) bool {
	if consensusOpts != nil && consensusOpts.Mode != "" {
		mode, err := consensus.NormalizeMode(consensusOpts.Mode)
		if err == nil {
			switch mode {
			case consensus.ModeAlpenglowObserver, consensus.ModeAlpenglow:
				return true
			}
		}
	}
	return false
}

func alpenglowClockFeatureActive(f *features.Features) bool {
	if f == nil {
		return false
	}
	return f.IsActive(features.Alpenglow) || f.IsActive(features.AlpenglowDevContext)
}

func useAlpenglowClockSemantics(alpenglowReplayMode bool, f *features.Features) bool {
	return alpenglowReplayMode || alpenglowClockFeatureActive(f)
}

func applyAlpenglowRuntimeFeatureOverrides(f *features.Features, slot uint64) {
	if f == nil {
		return
	}
	f.EnableFeature(features.VoteStateV4, slot)
	f.EnableFeature(features.TimelyVoteCredits, slot)
	f.EnableFeature(features.DeprecateUnusedLegacyVotePlumbing, slot)
}
