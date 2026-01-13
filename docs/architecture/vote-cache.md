# Vote Cache

The vote cache is an in-memory cache of vote account state, primarily used for leader schedule generation (NodePubkey lookups) and timestamp estimation.

## Location

- **Global accessor**: `global.VoteCache()` in `pkg/global/global_ctx.go:180`
- **Type**: `map[solana.PublicKey]*sealevel.VoteStateVersions`

## Data Stored

Each entry contains a `VoteStateVersions` struct with:
- `NodePubkey` - Validator identity (used for leader schedule)
- `Commission` - Validator commission percentage
- `Votes` - Recent vote history
- `RootSlot` - Oldest confirmed slot
- `LastTimestamp` - Most recent timestamp vote
- `EpochCredits` - Credits earned per epoch

## Lifecycle

### Initialization

**From snapshot** (`pkg/replay/block.go:718-724`):
```go
versionedVoteState, err := sealevel.UnmarshalVersionedVoteState(voteAcct.Data)
if err == nil {
    global.PutVoteCacheItem(pk, versionedVoteState)
}
```

### Rebuild at Epoch Boundary

**Always refreshed** (`pkg/replay/epoch.go:224-228`):
```go
// ALWAYS refresh vote cache from AccountsDB
if err := RebuildVoteCacheFromAccountsDB(acctsDb, slot, voteAcctStakes, 0); err != nil {
    mlog.Log.Errorf("failed to rebuild vote cache at epoch boundary: %v", err)
}
```

This reads ALL vote accounts from AccountsDB and replaces the cache.

### Updates During Replay

**Transaction processing** (`pkg/replay/transaction.go:234-237`):
```go
newVersionedVoteState, wasModified := execCtx.ModifiedVoteStates[acct.Key]
if wasModified {
    global.PutVoteCacheItem(acct.Key, newVersionedVoteState)
}
```

### NOT Updated During Vote Rewards

Vote rewards modify vote account lamports in AccountsDB but do NOT update the cache. However, this is a **non-issue** because:
1. Lamports are never read from VoteCache
2. Only NodePubkey and LastTimestamp are used
3. Vote rewards don't change these fields

## Usage

### Leader Schedule Generation

**Issue #3**: At epoch boundary, leader schedule uses freshly rebuilt VoteCache instead of epoch-frozen VoteAccts.

```go
// pkg/replay/leader_schedule_local.go:1076
voteCache := global.VoteCache()  // WRONG - uses current epoch's NodePubkeys
// Should use: global.EpochStakesVoteAccts(epoch)
```

### Timestamp Estimation (`pkg/replay/sysvar.go:54-78`)
```go
voteAccts := global.VoteCache()
for addr, voteAcct := range voteAccts {
    lastTs := voteAcct.LastTimestamp()  // Used for Clock sysvar
}
```

### EpochStakes VoteAccount Storage

When EpochStakes is computed, VoteAccount info (including NodePubkey) is stored:
```go
// pkg/replay/epoch.go:237
voteCache := global.VoteCache()
esb := newEpochStakesBuilder(leaderScheduleEpoch, voteCache)
```

This frozen copy should be used for leader schedule, not the live VoteCache.

## Timing Mismatch (Issue #3)

At 906->907 boundary:
```
1. RebuildVoteCacheFromAccountsDB()     # VoteCache = end-of-906 state
2. Compute EpochStakes(908)             # Stores end-of-906 VoteAccts
3. PrepareLeaderScheduleLocalFromVoteCache(907)  # Uses VoteCache (end-of-906)
   BUT epoch 907 schedule needs end-of-905 NodePubkeys (in EpochStakes(907))
```

**Fix**: Use `EpochStakesVoteAccts(epoch)` for leader schedule generation.

## Persistence

VoteCache is **NOT persisted**. On resume:
- Rebuilt from AccountsDB at startup
- Refreshed again at each epoch boundary

## Code References

| Operation | File | Function |
|-----------|------|----------|
| Get cache | `pkg/global/global_ctx.go:180` | `VoteCache()` |
| Put item | `pkg/global/global_ctx.go:159` | `PutVoteCacheItem()` |
| Rebuild | `pkg/replay/leader_schedule_local.go:202` | `RebuildVoteCacheFromAccountsDB()` |
| Transaction update | `pkg/replay/transaction.go:236` | (inline) |
