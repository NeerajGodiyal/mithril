package replay

import (
	"sync"

	"github.com/Overclock-Validator/mithril/pkg/sealevel"
)

// LocalLeaderCommit records a slot finalized by local block production so replay
// can advance without re-executing the same transactions from RPC/turbine.
type LocalLeaderCommit struct {
	SlotCtx *sealevel.SlotCtx
}

var (
	localLeaderMu      sync.RWMutex
	localLeaderCommits = map[uint64]LocalLeaderCommit{}
)

func RegisterLocalLeaderCommit(slotCtx *sealevel.SlotCtx) {
	if slotCtx == nil {
		return
	}
	localLeaderMu.Lock()
	localLeaderCommits[slotCtx.Slot] = LocalLeaderCommit{SlotCtx: slotCtx}
	localLeaderMu.Unlock()
	UpdateChainTipFromSlotCtx(slotCtx, slotCtx.Features)
}

func TakeLocalLeaderCommit(slot uint64) (LocalLeaderCommit, bool) {
	localLeaderMu.Lock()
	defer localLeaderMu.Unlock()
	commit, ok := localLeaderCommits[slot]
	if ok {
		delete(localLeaderCommits, slot)
	}
	return commit, ok
}

func HasLocalLeaderCommit(slot uint64) bool {
	localLeaderMu.RLock()
	defer localLeaderMu.RUnlock()
	_, ok := localLeaderCommits[slot]
	return ok
}
