package replay

import (
	"fmt"

	"github.com/mr-tron/base58"
)

// ConfirmedDivergence reports that replay computed a different bankhash than the
// one a >2/3 stake supermajority confirmed for the slot. In fork-aware mode this
// triggers dump-then-repair: drop the unrooted RAM tail, re-replay the confirmed
// chain from the rooted checkpoint. An IDENTICAL repeat means a deterministic
// replay bug, not a fork — fail closed.
type ConfirmedDivergence struct {
	Slot      uint64
	Ours      [32]byte
	Confirmed [32]byte
}

func (e *ConfirmedDivergence) Error() string {
	return fmt.Sprintf("consensus divergence: slot %d bankhash mismatch (our=%s confirmed=%s)",
		e.Slot, base58.Encode(e.Ours[:]), base58.Encode(e.Confirmed[:]))
}

// Same reports whether two divergences are identical (same slot and hash pair) —
// a repeat implies deterministic replay divergence, so retrying cannot help.
func (e *ConfirmedDivergence) Same(o *ConfirmedDivergence) bool {
	return o != nil && e.Slot == o.Slot && e.Ours == o.Ours && e.Confirmed == o.Confirmed
}
