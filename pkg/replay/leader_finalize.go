package replay

import (
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/bankhash"
	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/fees"
	"github.com/Overclock-Validator/mithril/pkg/rent"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
)

// CommitLeaderInput contains the state required to freeze a locally forged bank.
// Despite the historical name, CommitLeaderSlot is deliberately in-memory only:
// the resulting block must still pass through ProcessBlock and the fork-aware
// WorkingSet before any state can become durable.
type CommitLeaderInput struct {
	AcctsDb                 *accountsdb.AccountsDb
	SlotCtx                 *sealevel.SlotCtx
	Block                   *b.Block
	EpochSchedule           *sealevel.SysvarEpochSchedule
	TxFeeAccumulator        fees.TxFeeInfoAccumulator
	AlpenglowClock          bool
	AlpenglowShredVersion   uint16
	FooterTimestamp         int64
	FooterProducerTimeNanos uint64
}

// PrepareLeaderSlotSysvars creates slot-local sysvar copies before TPU
// transactions execute. It never mutates the process-global sysvar cache.
func PrepareLeaderSlotSysvars(slotCtx *sealevel.SlotCtx, block *b.Block, epochSchedule *sealevel.SysvarEpochSchedule, alpenglowClock bool) error {
	if slotCtx == nil || block == nil || epochSchedule == nil {
		return fmt.Errorf("missing leader slot preparation input")
	}
	if slotCtx.ParentAccts == nil {
		slotCtx.ParentAccts = accounts.NewMemAccounts()
	}

	clockAcct, err := leaderParentAccount(slotCtx, sealevel.SysvarClockAddr)
	if err != nil {
		return fmt.Errorf("load clock sysvar: %w", err)
	}
	var clock sealevel.SysvarClock
	if err := clock.UnmarshalWithDecoder(bin.NewBinDecoder(clockAcct.Data)); err != nil {
		return fmt.Errorf("decode clock sysvar: %w", err)
	}
	if err := updateClockSysvarForMode(&clock, block, epochSchedule, alpenglowClock); err != nil {
		return err
	}
	if err := installLeaderSysvar(slotCtx, clockAcct, clock.MustMarshal()); err != nil {
		return err
	}

	slotHashesAcct, err := leaderParentAccount(slotCtx, sealevel.SysvarSlotHashesAddr)
	if err != nil {
		return fmt.Errorf("load slot hashes sysvar: %w", err)
	}
	var slotHashes sealevel.SysvarSlotHashes
	if sealevel.SysvarCache.SlotHashes.Sysvar != nil {
		slotHashes = append(slotHashes, (*sealevel.SysvarCache.SlotHashes.Sysvar)...)
		copy(slotHashesAcct.Data, slotHashes.MustMarshal())
		if err := replaceLeaderParent(slotCtx, slotHashesAcct); err != nil {
			return err
		}
	} else if err := slotHashes.UnmarshalWithDecoder(bin.NewBinDecoder(slotHashesAcct.Data)); err != nil {
		return fmt.Errorf("decode slot hashes sysvar: %w", err)
	}
	slotHashes.Update(block.Slot, block.ParentSlot, block.ParentBankhash)
	if err := setLeaderCurrent(slotCtx, slotHashesAcct, slotHashes.MustMarshal()); err != nil {
		return err
	}

	recentAcct, err := leaderParentAccount(slotCtx, sealevel.SysvarRecentBlockHashesAddr)
	if err != nil {
		return fmt.Errorf("load recent blockhashes sysvar: %w", err)
	}
	if sealevel.SysvarCache.RecentBlockHashes.Sysvar != nil {
		recent := append(sealevel.SysvarRecentBlockhashes(nil), (*sealevel.SysvarCache.RecentBlockHashes.Sysvar)...)
		copy(recentAcct.Data, recent.MustMarshal())
		if err := replaceLeaderParent(slotCtx, recentAcct); err != nil {
			return err
		}
	}
	if err := setLeaderCurrent(slotCtx, recentAcct, recentAcct.Data); err != nil {
		return err
	}

	slotHistoryAcct, err := leaderParentAccount(slotCtx, sealevel.SysvarSlotHistoryAddr)
	if err != nil {
		return fmt.Errorf("load slot history sysvar: %w", err)
	}
	if err := setLeaderCurrent(slotCtx, slotHistoryAcct, slotHistoryAcct.Data); err != nil {
		return err
	}
	return nil
}

// CommitLeaderSlot freezes a forged bank and computes the footer bank hash. It
// does not write AccountsDB, update global replay progress, or bypass forkchoice.
func CommitLeaderSlot(in CommitLeaderInput) (*sealevel.SlotCtx, error) {
	if in.AcctsDb == nil || in.SlotCtx == nil || in.Block == nil || in.EpochSchedule == nil {
		return nil, fmt.Errorf("missing leader finalization input")
	}
	slotCtx, block := in.SlotCtx, in.Block
	block.FooterProducerTimeNanos = in.FooterProducerTimeNanos
	block.UnixTimestamp = in.FooterTimestamp
	slotCtx.Blockhash = block.Blockhash
	slotCtx.Epoch = block.Epoch
	slotCtx.FeeRateGovernor = block.FeeRateGovernor

	if _, err := slotCtx.GetAccount(sealevel.SysvarClockAddr); err != nil {
		if err := PrepareLeaderSlotSysvars(slotCtx, block, in.EpochSchedule, in.AlpenglowClock); err != nil {
			return nil, err
		}
	}
	if in.AlpenglowClock {
		// This is a speculative producer bank until the forged block passes
		// through ordered replay. Keep its Clock slot-local: publishing it to the
		// global replay cache here would replace the true parent Clock before
		// ProcessBlock loads the block and would produce a different LtHash.
		if err := applyAlpenglowFooterClockLocal(slotCtx, block, in.EpochSchedule); err != nil {
			return nil, err
		}
		if err := updateAlpenglowNanosecondClockAccount(slotCtx, block); err != nil {
			return nil, err
		}
		if err := ApplyAlpenglowVoteRewards(slotCtx, block, in.EpochSchedule, block.SkipRewardCert, block.NotarRewardCert, block.BlockFinalCert, in.AlpenglowShredVersion); err != nil {
			return nil, err
		}
	}

	if len(block.Transactions) > 0 {
		slotCtx.LamportsBurnt = fees.DistributeTxFeesToSlotLeader(in.AcctsDb, slotCtx, block.Leader, &in.TxFeeAccumulator)
		slotCtx.RecordModifiedAcct(block.Leader)
	}
	rentAccts := rent.CollectRentEagerly(slotCtx, sealevel.SysvarCache.Rent.Sysvar, in.EpochSchedule)
	runIncinerator(slotCtx)
	if err := finishLeaderSysvars(slotCtx, block); err != nil {
		return nil, err
	}
	if err := ensureParentAccountsForModified(slotCtx); err != nil {
		return nil, err
	}
	writable, modified := compileLeaderAccounts(slotCtx, block, rentAccts)
	slotCtx.FinalBankhash = bankhash.CalculateBankHash(slotCtx, writable, modified, block.ParentBankhash, block.NumSignatures, block.Blockhash)
	copy(block.ExpectedBankhash[:], slotCtx.FinalBankhash)
	block.HasExpectedBankhash = true
	return slotCtx, nil
}

func finishLeaderSysvars(slotCtx *sealevel.SlotCtx, block *b.Block) error {
	recentAcct, err := slotCtx.GetAccount(sealevel.SysvarRecentBlockHashesAddr)
	if err != nil {
		return err
	}
	var recent sealevel.SysvarRecentBlockhashes
	recent.MustUnmarshalWithDecoder(bin.NewBinDecoder(recentAcct.Data))
	slotCtx.LatestEvictedBlockhash = recent.PushLatest(block.Blockhash, slotCtx.FeeRateGovernor.LamportsPerSignature)
	if err := slotCtx.SetAccount(sealevel.SysvarRecentBlockHashesAddr, withData(recentAcct, recent.MustMarshal())); err != nil {
		return err
	}
	slotCtx.RecordModifiedAcct(sealevel.SysvarRecentBlockHashesAddr)

	historyAcct, err := slotCtx.GetAccount(sealevel.SysvarSlotHistoryAddr)
	if err != nil {
		return err
	}
	var history sealevel.SysvarSlotHistory
	history.MustUnmarshalWithDecoder(bin.NewBinDecoder(historyAcct.Data))
	history.Add(block.Slot)
	history.SetNextSlot(block.Slot + 1)
	if err := slotCtx.SetAccount(sealevel.SysvarSlotHistoryAddr, withData(historyAcct, history.MustMarshal())); err != nil {
		return err
	}
	slotCtx.RecordModifiedAcct(sealevel.SysvarSlotHistoryAddr)
	slotCtx.RecordModifiedAcct(sealevel.SysvarClockAddr)
	slotCtx.RecordModifiedAcct(sealevel.SysvarSlotHashesAddr)
	return nil
}

func leaderParentAccount(slotCtx *sealevel.SlotCtx, key solana.PublicKey) (*accounts.Account, error) {
	acct, err := slotCtx.GetAccountFromAccountsDb(key)
	if err != nil {
		return nil, err
	}
	acct = acct.Clone()
	if err := replaceLeaderParent(slotCtx, acct); err != nil {
		return nil, err
	}
	return acct.Clone(), nil
}

func replaceLeaderParent(slotCtx *sealevel.SlotCtx, acct *accounts.Account) error {
	return slotCtx.ParentAccts.SetAccountWithoutLock(acct.Key, acct.Clone())
}

func installLeaderSysvar(slotCtx *sealevel.SlotCtx, acct *accounts.Account, data []byte) error {
	if err := replaceLeaderParent(slotCtx, acct); err != nil {
		return err
	}
	return setLeaderCurrent(slotCtx, acct, data)
}

func setLeaderCurrent(slotCtx *sealevel.SlotCtx, acct *accounts.Account, data []byte) error {
	return slotCtx.SetAccount(acct.Key, withData(acct, data))
}

func withData(acct *accounts.Account, data []byte) *accounts.Account {
	out := acct.Clone()
	out.Data = append(out.Data[:0], data...)
	return out
}

func ensureParentAccountsForModified(slotCtx *sealevel.SlotCtx) error {
	if slotCtx.Features == nil || !slotCtx.Features.IsActive(features.AccountsLtHash) {
		return nil
	}
	for key := range slotCtx.ModifiedAccts {
		if _, err := slotCtx.GetParentAccount(key); err == nil {
			continue
		}
		acct, err := slotCtx.GetAccountFromAccountsDb(key)
		if err != nil {
			acct = &accounts.Account{Key: key}
		}
		if err := slotCtx.ParentAccts.SetAccountWithoutLock(key, acct.Clone()); err != nil {
			return err
		}
	}
	return nil
}

func compileLeaderAccounts(slotCtx *sealevel.SlotCtx, block *b.Block, rentAccts []*accounts.Account) ([]*accounts.Account, []*accounts.Account) {
	writable := make([]*accounts.Account, 0, len(slotCtx.WritableAccts)+len(rentAccts)+8)
	modified := make([]*accounts.Account, 0, len(slotCtx.ModifiedAccts)+len(rentAccts)+8)
	seenWritable := make(map[solana.PublicKey]struct{})
	seenModified := make(map[solana.PublicKey]struct{})
	addWritable := func(acct *accounts.Account) {
		if acct == nil {
			return
		}
		if _, ok := seenWritable[acct.Key]; ok {
			return
		}
		seenWritable[acct.Key] = struct{}{}
		writable = append(writable, acct)
	}
	addModified := func(acct *accounts.Account) {
		if acct == nil {
			return
		}
		if _, ok := seenModified[acct.Key]; ok {
			return
		}
		seenModified[acct.Key] = struct{}{}
		modified = append(modified, acct)
	}
	for key := range slotCtx.WritableAccts {
		acct, _ := slotCtx.GetAccount(key)
		addWritable(acct)
	}
	for key := range slotCtx.ModifiedAccts {
		acct, _ := slotCtx.GetAccount(key)
		addModified(acct)
		addWritable(acct)
	}
	for _, acct := range block.EpochUpdatedAccts {
		if acct == nil {
			continue
		}
		current, err := slotCtx.GetAccount(acct.Key)
		if err != nil {
			current = acct
		}
		addWritable(current)
		addModified(current)
	}
	for _, acct := range rentAccts {
		addWritable(acct)
		addModified(acct)
	}
	return writable, modified
}
