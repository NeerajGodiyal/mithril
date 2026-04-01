package replay

import (
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
)

func applyCapitalizationDelta(total uint64, delta int64) (uint64, error) {
	if delta >= 0 {
		next := total + uint64(delta)
		if next < total {
			return 0, fmt.Errorf("capitalization overflow while adding delta %d", delta)
		}
		return next, nil
	}

	absDelta := uint64(-delta)
	if absDelta > total {
		return 0, fmt.Errorf("capitalization underflow while subtracting delta %d from %d", delta, total)
	}
	return total - absDelta, nil
}

func calculateCapitalizationDelta(slotCtx *sealevel.SlotCtx, modifiedAccts []*accounts.Account) (int64, error) {
	seen := make(map[solana.PublicKey]struct{}, len(modifiedAccts))
	var delta int64

	for _, acct := range modifiedAccts {
		if acct == nil {
			continue
		}
		if _, exists := seen[acct.Key]; exists {
			continue
		}
		seen[acct.Key] = struct{}{}

		var parentLamports uint64
		parentAcct, err := slotCtx.GetParentAccount(acct.Key)
		if err == nil && parentAcct != nil {
			parentLamports = parentAcct.Lamports
		}

		delta += int64(acct.Lamports) - int64(parentLamports)
	}

	return delta, nil
}

func logCapitalizationAudit(boundarySlot uint64, replayCapitalization uint64, deltaTrackedCapitalization uint64) {
	delta := int64(deltaTrackedCapitalization) - int64(replayCapitalization)
	mlog.Log.FileOnlyf("Capitalization audit: boundary_slot=%d replay_ctx=%d delta_tracked=%d delta=%d",
		boundarySlot,
		replayCapitalization,
		deltaTrackedCapitalization,
		delta,
	)
}
