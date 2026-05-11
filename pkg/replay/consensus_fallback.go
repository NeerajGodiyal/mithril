package replay

import (
	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/blockstream"
)

func shouldDiscardLightbringerObservationAfterFallback(isLive, useLightbringer bool, block *b.Block, stats blockstream.FetchStatsSnapshot) bool {
	return isLive &&
		useLightbringer &&
		block != nil &&
		block.FromLightbringer &&
		(!stats.IsNearTip || stats.CurrentSource != "lightbringer")
}
