package sealevel

import (
	"github.com/Overclock-Validator/mithril/pkg/accounts"
)

type sysvarCache struct {
	RecentBlockHashes recentBlockhashesCache
	Rent              rentCache
	Clock             clockCache
	Fees              feesCache
	SlotHashes        slotHashesCache
	SlotHistory       slotHistoryCache
	EpochSchedule     epochScheduleCache
	EpochRewards      epochRewardsCache
	StakeHistory      stakeHistoryCache
	LastRestartSlot   lastRestartSlotCache
}

type recentBlockhashesCache struct {
	Acct   *accounts.Account
	Sysvar *SysvarRecentBlockhashes
}

type rentCache struct {
	Acct   *accounts.Account
	Sysvar *SysvarRent
}
type clockCache struct {
	Acct   *accounts.Account
	Sysvar *SysvarClock
}
type feesCache struct {
	Acct   *accounts.Account
	Sysvar *SysvarFees
}
type slotHashesCache struct {
	Acct   *accounts.Account
	Sysvar *SysvarSlotHashes
}

type slotHistoryCache struct {
	Acct   *accounts.Account
	Sysvar *SysvarSlotHistory
}

type epochScheduleCache struct {
	Acct   *accounts.Account
	Sysvar *SysvarEpochSchedule
}

type epochRewardsCache struct {
	Acct   *accounts.Account
	Sysvar *SysvarEpochRewards
}

type stakeHistoryCache struct {
	Acct   *accounts.Account
	Sysvar *SysvarStakeHistory
}

type lastRestartSlotCache struct {
	Acct   *accounts.Account
	Sysvar *SysvarLastRestartSlot
}

var SysvarCache = sysvarCache{}

// sysvarCacheForSlot returns the sysvar cache a slot's execution should read from:
// its branch-scoped cache when set, otherwise the shared process-global cache. This
// is the read seam that lets sysvars become per-branch without touching the ~69 call
// sites — they already hold a *SlotCtx (directly or via *ExecutionCtx).
func sysvarCacheForSlot(slotCtx *SlotCtx) *sysvarCache {
	if slotCtx != nil && slotCtx.sysvars != nil {
		return slotCtx.sysvars
	}
	return &SysvarCache
}

func sysvarCacheFor(execCtx *ExecutionCtx) *sysvarCache {
	if execCtx != nil {
		return sysvarCacheForSlot(execCtx.SlotCtx)
	}
	return &SysvarCache
}

// CarriedSysvarsSnapshot deep-copies the global cache's carried-forward families
// (Clock, SlotHashes, RecentBlockHashes) so serial branch re-execution can wind
// shared sysvar state back to a branch's parent exactly. Deep copies are required:
// the per-slot loader value-copies slice HEADERS, so in-place element mutation
// during execution (SlotHashes update, RecentBlockhashes push, account-data
// writeback) reaches the shared backing arrays a pointer snapshot would alias.
type CarriedSysvarsSnapshot struct {
	clock   clockCache
	slotH   slotHashesCache
	recentB recentBlockhashesCache
}

func cloneAcct(a *accounts.Account) *accounts.Account {
	if a == nil {
		return nil
	}
	return a.Clone()
}

func SnapshotCarriedSysvars() CarriedSysvarsSnapshot {
	var s CarriedSysvarsSnapshot
	if c := SysvarCache.Clock.Sysvar; c != nil {
		cc := *c
		s.clock.Sysvar = &cc
	}
	s.clock.Acct = cloneAcct(SysvarCache.Clock.Acct)
	if sh := SysvarCache.SlotHashes.Sysvar; sh != nil {
		shc := make(SysvarSlotHashes, len(*sh))
		copy(shc, *sh)
		s.slotH.Sysvar = &shc
	}
	s.slotH.Acct = cloneAcct(SysvarCache.SlotHashes.Acct)
	if rb := SysvarCache.RecentBlockHashes.Sysvar; rb != nil {
		rbc := make(SysvarRecentBlockhashes, len(*rb))
		copy(rbc, *rb)
		s.recentB.Sysvar = &rbc
	}
	s.recentB.Acct = cloneAcct(SysvarCache.RecentBlockHashes.Acct)
	return s
}

func (s CarriedSysvarsSnapshot) Restore() {
	SysvarCache.Clock = s.clock
	SysvarCache.SlotHashes = s.slotH
	SysvarCache.RecentBlockHashes = s.recentB
}

// SeedSysvarsFromGlobal gives this slot a branch-scoped sysvar cache seeded as a
// shallow copy of the process-global cache: the same underlying sysvar structs, so
// in-place mutations remain shared and execution is byte-identical to reading the
// global directly. This routes live replay through the branch cache path while the
// global stays the cross-slot carrier (single-branch mirror phase); true per-branch
// instances replace the shallow copy when multi-branch execution lands.
func (s *SlotCtx) SeedSysvarsFromGlobal() {
	sc := SysvarCache
	s.sysvars = &sc
}

// Sysvars returns the sysvar cache this slot's execution resolves: the branch-scoped
// container when set, else the shared process-global cache. Replay-side sysvar
// mutations go through here so they stay branch-correct once containers diverge.
func (s *SlotCtx) Sysvars() *sysvarCache {
	return sysvarCacheForSlot(s)
}

// SetBranchClock updates the Clock entry in this slot's branch cache (no-op when the
// slot has no branch cache). Used when a post-seeding rewrite replaces the Clock
// pointer, e.g. the Alpenglow footer timestamp.
func (s *SlotCtx) SetBranchClock(clock *SysvarClock, acct *accounts.Account) {
	if s.sysvars == nil {
		return
	}
	s.sysvars.Clock.Sysvar = clock
	s.sysvars.Clock.Acct = acct
}

// InitBranchSysvars allocates this slot's branch-scoped sysvar cache, seeded with the
// carried-forward families (Clock, SlotHashes, RecentBlockHashes) from the parent
// branch's end-of-slot state. Once set, sysvar reads resolve here instead of the
// process-global cache, so speculative execution on one branch never reads a sibling
// branch's sysvar state. Families left nil resolve through the account-lookup
// fallback inside each Read*Sysvar (SlotHistory's reader panics if the account is
// absent); per-slot loader population of the container is not wired yet.
func (s *SlotCtx) InitBranchSysvars(clock *SysvarClock, slotHashes *SysvarSlotHashes, recentBlockhashes *SysvarRecentBlockhashes) {
	sc := &sysvarCache{}
	if clock != nil {
		sc.Clock.Sysvar = clock
	}
	if slotHashes != nil {
		sc.SlotHashes.Sysvar = slotHashes
	}
	if recentBlockhashes != nil {
		sc.RecentBlockHashes.Sysvar = recentBlockhashes
	}
	s.sysvars = sc
}
