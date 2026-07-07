package replay

import (
	"bytes"
	"fmt"
	"os"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/lthash"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
)

// shadowBranchCheckEnabled turns on the branch-execution differential: after each
// block replays on the linear path, it is re-executed as a sibling branch reading
// through the fork tree's parent branch view, with shared globals wound back to the
// parent's state and restored afterwards. Both executions must produce the identical
// bankhash — any contamination between serial branch executions (sysvar carry, vote
// cache, counters) surfaces as a loud halt. Requires storage.fork_aware.
func shadowBranchCheckEnabled() bool {
	return os.Getenv("MITHRIL_SHADOW_BRANCH_CHECK") == "1"
}

// execGlobalsSnapshot captures the shared execution state a block's execution
// mutates, so a serial branch re-execution can be wound back exactly: the vote cache
// (shallow copy — cached states are never mutated in place), the cumulative
// transaction count, and the carried global sysvar pointer pairs.
type execGlobalsSnapshot struct {
	voteCache        map[solana.PublicKey]*sealevel.VoteStateVersions
	transactionCount uint64
	carried          sealevel.CarriedSysvarsSnapshot
	feeGov           *sealevel.FeeRateGovernor // pre-block clone: execution mutates the governor in place
	prevFeeGov       *sealevel.FeeRateGovernor
	acctsLtHash      *lthash.LtHash // pre-block clone: the LtHash accumulator is mixed in place
}

// captureBlockFeeState clones the block's fee governors, which execution mutates in
// place; the shadow re-execution must start from the parent's values.
func (s *execGlobalsSnapshot) captureBlockFeeState(block *b.Block) {
	if block.FeeRateGovernor != nil {
		g := *block.FeeRateGovernor
		s.feeGov = &g
	}
	if block.PrevFeeRateGovernor != nil {
		g := *block.PrevFeeRateGovernor
		s.prevFeeGov = &g
	}
	if block.AcctsLtHash != nil {
		s.acctsLtHash = block.AcctsLtHash.Clone()
	}
}

func snapshotExecGlobals() *execGlobalsSnapshot {
	return &execGlobalsSnapshot{
		voteCache:        global.VoteCacheSnapshot(),
		transactionCount: global.TransactionCount(),
		carried:          sealevel.SnapshotCarriedSysvars(),
	}
}

func (s *execGlobalsSnapshot) restore() {
	global.RestoreVoteCache(s.voteCache)
	global.SetTransactionCount(s.transactionCount)
	s.carried.Restore()
}

// runShadowBranchCheck re-executes an already-replayed block as a sibling branch:
// account reads resolve through the PARENT branch of the fork tree (not the just-
// committed tip), shared globals are wound back to the pre-block (parent) state
// first, and the post-linear state is restored afterwards. Returns an error when
// the shadow bankhash differs from the linear one.
func runShadowBranchCheck(
	acctsDb *accountsdb.AccountsDb,
	tail unrootedState,
	block *b.Block,
	linearBankhash []byte,
	preBlock *execGlobalsSnapshot,
	epochSchedule *sealevel.SysvarEpochSchedule,
	txParallelism int,
	dbgOpts *DebugOptions,
	alpenglowClock bool,
) error {
	ft, ok := tail.(*forkTail)
	if !ok {
		return nil // differential requires the fork-aware branch tree
	}
	parentBranch, ok := ft.branchOf(block.ParentSlot)
	if !ok {
		return nil // parent not held in the tree (e.g. first slot above the durable base)
	}

	postLinear := snapshotExecGlobals()
	preBlock.restore() // wind shared state back to the parent's
	defer postLinear.restore()

	// Swap in the pre-block fee governors; restore the linear run's afterwards.
	linGov, linPrevGov := block.FeeRateGovernor, block.PrevFeeRateGovernor
	if preBlock.feeGov != nil {
		g := *preBlock.feeGov
		block.FeeRateGovernor = &g
	}
	if preBlock.prevFeeGov != nil {
		g := *preBlock.prevFeeGov
		block.PrevFeeRateGovernor = &g
	}
	linLtHash := block.AcctsLtHash
	if preBlock.acctsLtHash != nil {
		block.AcctsLtHash = preBlock.acctsLtHash.Clone()
	}
	defer func() {
		block.FeeRateGovernor, block.PrevFeeRateGovernor = linGov, linPrevGov
		block.AcctsLtHash = linLtHash
	}()

	shadow := &captureTail{branchReader: branchReader{fc: ft.fc, branch: parentBranch}}
	// Throwaway tracker: the shadow's persisted-hash bookkeeping must not clobber
	// the linear run's resume checkpointing.
	shadowTracker := &persistedTracker{}
	shadowCtx, err := ProcessBlock(acctsDb, block, epochSchedule, txParallelism, dbgOpts, shadowTracker, shadow, alpenglowClock)
	if err != nil {
		return fmt.Errorf("shadow branch execution failed at slot %d: %w", block.Slot, err)
	}
	if !bytes.Equal(shadowCtx.FinalBankhash, linearBankhash) {
		// Forensics: name the accounts whose end state differs between the shadow's
		// captured delta and the linear branch's committed state.
		if linearBranch, ok := ft.branchOf(block.Slot); ok {
			named := 0
			for _, acct := range shadow.capturedDelta {
				lin, lerr := ft.fc.GetAccount(linearBranch, block.Slot, acct.Key)
				if lerr != nil || lin == nil || lin.Lamports != acct.Lamports || !bytes.Equal(lin.Data, acct.Data) {
					s := "missing"
					if lin != nil {
						s = fmt.Sprintf("%d (delta %d)", lin.Lamports, int64(acct.Lamports)-int64(lin.Lamports))
					}
					mlog.Log.Errorf("shadow diff acct %s: shadow=%d linear=%s dataDiff=%v", acct.Key, acct.Lamports, s, lin != nil && !bytes.Equal(lin.Data, acct.Data))
					named++
					if named >= 10 {
						break
					}
				}
			}
			mlog.Log.Errorf("shadow diff: %d differing accounts named (shadow delta size %d)", named, len(shadow.capturedDelta))
		}
		return fmt.Errorf("shadow branch DIVERGED at slot %d: linear=%x shadow=%x",
			block.Slot, linearBankhash, shadowCtx.FinalBankhash)
	}
	mlog.Log.FileOnlyf("shadow branch check: slot %d identical", block.Slot)
	return nil
}
