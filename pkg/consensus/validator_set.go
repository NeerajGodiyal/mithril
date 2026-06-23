package consensus

import (
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/alpenglow"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
)

// RefreshAlpenglowValidatorSet installs BLS validator ranks for cert verification.
func RefreshAlpenglowValidatorSet(engine Engine, epoch uint64) error {
	sink, ok := engine.(AlpenglowValidatorSetSink)
	if !ok {
		return nil
	}
	stakes := global.EpochStakes(epoch)
	if len(stakes) == 0 {
		return fmt.Errorf("consensus: no epoch stakes cached for epoch %d", epoch)
	}
	set, err := alpenglow.BuildValidatorSet(
		epoch,
		stakes,
		global.EpochStakesVoteAccts(epoch),
		global.EpochTotalStake(epoch),
	)
	if err != nil {
		return fmt.Errorf("consensus: build validator set for epoch %d: %w", epoch, err)
	}
	if err := sink.SetAlpenglowValidatorSet(set); err != nil {
		return fmt.Errorf("consensus: install validator set for epoch %d: %w", epoch, err)
	}
	mlog.Log.Infof("ALPENGLOW observer: validator set ready for epoch %d (validators=%d total_stake=%d)",
		set.Epoch, len(set.Validators), set.TotalStake)
	return nil
}

// RefreshAlpenglowValidatorSetsFromCache reloads every cached epoch stakes entry.
func RefreshAlpenglowValidatorSetsFromCache(engine Engine) {
	if engine == nil {
		return
	}
	for _, epoch := range global.GetAllCachedEpochs() {
		if err := RefreshAlpenglowValidatorSet(engine, epoch); err != nil {
			mlog.Log.Warnf("ALPENGLOW observer: %v", err)
		}
	}
}
