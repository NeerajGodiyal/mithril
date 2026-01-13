# Epoch Boundary Logic

This document describes what happens when Mithril crosses a Solana epoch boundary, known issues, and how to debug divergences.

## Overview

An epoch boundary occurs at the transition from the last slot of epoch N-1 to the first slot of epoch N. At this boundary, several critical operations must happen in the correct order.

## Order of Operations

### Current Mithril Order (epoch.go:133-175)

```
1. updateEpochStakesAndRefreshVoteCache()    # Cache stakes, rebuild vote cache
2. PrepareLeaderScheduleLocalFromVoteCache() # Generate leader schedule
3. beginPartitionedEpochRewardsDistribution() # Calculate and begin rewards
4. updateStakeHistorySysvar()                 # Update StakeHistory sysvar
```

### Correct Order (Firedancer - fd_runtime.c:604-707)

```
1. fd_stakes_activate_epoch()      # Update StakeHistory FIRST
2. fd_begin_partitioned_rewards()  # Calculate rewards AFTER
```

**Critical Issue**: Mithril updates StakeHistory AFTER rewards, but rewards calculation uses StakeHistory for effective stake computation. See [Issue #4](#issue-4-stakehistory-order).

---

## Key Data Structures

### Stake Cache
- **Location**: `global.StakeCache()` in `pkg/global/global_ctx.go`
- **Contains**: `map[solana.PublicKey]*sealevel.Delegation`
- **Updated**: During transaction processing when stake accounts are modified
- **NOT Updated**: During rewards distribution (see Issue #2)

### Vote Cache
- **Location**: `global.VoteCache()` in `pkg/global/global_ctx.go`
- **Contains**: `map[solana.PublicKey]*sealevel.VoteStateVersions`
- **Updated**:
  - At epoch boundaries via `RebuildVoteCacheFromAccountsDB()`
  - During transaction processing when vote accounts are modified
- **NOT Updated**: During vote rewards distribution

### EpochStakes
- **Location**: `pkg/epochstakes/epoch_stakes.go`
- **Contains**: Per-epoch frozen stakes and vote account info
- **Used For**: Leader schedule generation (N-2 epoch lag rule)
- **Persisted**: To state file for resume

---

## Known Issues

### Issue #2: Stake Cache Staleness After Rewards

**Status**: Being fixed

**Problem**: When rewards are distributed, stake accounts are modified in AccountsDB but the in-memory stake cache is NOT updated.

**Location**: `pkg/rewards/rewards.go:180-247` - `DistributeStakingRewardsForPartition()`

```go
// Stores to AccountsDB
err := acctsDb.StoreAccounts(accts, slot)
// But NO call to global.PutStakeCacheItem()
```

**Impact**:
- At next epoch boundary, `EpochStakes` is computed from stale stake cache
- Affects leader schedule for epoch N+2
- Affects StakeHistory computation

**Why Second Boundary Fails**:
- First boundary: Stake cache is fresh from snapshot
- Rewards distributed: Stake cache becomes stale
- Second boundary: EpochStakes computed from stale cache

---

### Issue #3: VoteCache Timing Mismatch

**Status**: Being fixed

**Problem**: Leader schedule generation uses the wrong epoch's VoteCache for NodePubkey lookups.

**Location**: `pkg/replay/leader_schedule_local.go:1066-1197`

At the 906->907 boundary:
1. `RebuildVoteCacheFromAccountsDB()` rebuilds VoteCache with end-of-epoch-906 state
2. `PrepareLeaderScheduleLocalFromVoteCache(907)` generates schedule for epoch 907
3. But epoch 907 schedule should use NodePubkeys from end-of-epoch-905 (when EpochStakes(907) was computed)

```
EpochStakes(907): Computed at 905->906 boundary (end-of-905 state)
VoteCache:        Rebuilt at 906->907 boundary (end-of-906 state)
                  MISMATCH if any NodePubkey changed during epoch 906
```

**Fix**: Use `EpochStakesVoteAccts(epoch)` instead of `global.VoteCache()` for leader schedule generation.

---

### Issue #4: StakeHistory Order

**Status**: Being fixed

**Problem**: StakeHistory sysvar is updated AFTER rewards are computed, but rewards calculation depends on StakeHistory.

**Location**: `pkg/replay/epoch.go:165-171`

**Why It Matters**:
- `StakeActivatingAndDeactivating()` uses StakeHistory to compute effective stake during warmup/cooldown
- If the entry for the just-ended epoch is missing, effective stake can be wrong
- This affects reward calculations

**Interaction with Issue #2**:
Once #2 is fixed (stake cache updated during rewards):
- `updateStakeHistorySysvar()` runs AFTER rewards with POST-REWARD stake values
- But StakeHistory entry should reflect PRE-REWARD state
- Makes the entry not just late, but INCORRECT

**Fix Order**: Must fix #4 BEFORE #2 to avoid making StakeHistory entries incorrect.

---

## Epoch Stakes and Leader Schedule (N-2 Rule)

Leader schedule for epoch N uses stakes from end of epoch N-2:

| Schedule For | Stakes From | Where Stored |
|--------------|-------------|--------------|
| Epoch 906 | End of 904 | Snapshot `EpochStakes(906)` |
| Epoch 907 | End of 905 | Computed at 905->906 boundary |
| Epoch 908 | End of 906 | Computed at 906->907 boundary |

**Key Function**: `epochSchedule.LeaderScheduleEpoch(block.Slot)`
- At first slot of epoch N, returns N+1
- Stakes are cached as `EpochStakes(N+1)` at boundary N-1->N

---

## Rewards Distribution

### Partitioned Epoch Rewards

Rewards are distributed across multiple blocks:

1. **Vote rewards**: Distributed immediately at epoch boundary
2. **Stake rewards**: Distributed across N partitions (4096 accounts each)

**Key Sysvar**: `EpochRewards`
- `Active = true` during distribution
- `NumPartitions` decremented each block
- `ParentBlockhash` used for partition assignment (SipHash13)

### Partition Formula

```
numPartitions = ceil(eligibleAccounts / 4096)
partitionIdx = currentBlockHeight - DistributionStartingBlockHeight
```

### Resume During Rewards Period

**NOT SUPPORTED** - Mithril exits with error if attempting to resume while `EpochRewards.Active == true`

Location: `pkg/replay/block.go:1345-1362`

---

## Stop/Resume Behavior

### What's Persisted (state file)

| Field | Purpose |
|-------|---------|
| `LastSlot` | Resume position |
| `LastBankhash` | Verification |
| `ComputedEpochStakes` | Leader schedule on resume |
| `LastCapitalization` | Inflation calculations |
| LtHash, fees, blockhashes | Bank hash computation |

### What's Rebuilt on Resume

| Component | Source |
|-----------|--------|
| VoteCache | Rebuilt from AccountsDB |
| StakeCache | Loaded from `stake_pubkeys.idx` + AccountsDB lookups |
| Leader Schedule | Generated from persisted EpochStakes |

---

## Debugging Checklist

### Bank Hash Mismatch at Epoch Boundary

1. Check if stake cache is stale (Issue #2)
2. Check if VoteCache has wrong NodePubkeys (Issue #3)
3. Check StakeHistory computation order (Issue #4)
4. Verify EpochStakes has correct epoch's data

### Leader Schedule Mismatch

1. Verify using N-2 epoch stakes (not current epoch)
2. Check VoteCache timing - should match EpochStakes epoch
3. Verify snapshot schedule is being reused at first boundary

### Second Boundary Crash

Most likely causes:
1. Stake cache staleness accumulated from first boundary
2. VoteCache timing mismatch more pronounced at second boundary
3. StakeHistory entry incorrect after first rewards distribution

---

## Code References

| Component | File | Key Functions |
|-----------|------|---------------|
| Epoch transition | `pkg/replay/epoch.go` | `handleEpochTransition()` |
| Rewards | `pkg/replay/rewards.go` | `beginPartitionedEpochRewardsDistribution()` |
| Stake cache | `pkg/global/global_ctx.go` | `StakeCache()`, `PutStakeCacheItem()` |
| Vote cache | `pkg/global/global_ctx.go` | `VoteCache()`, `PutVoteCacheItem()` |
| EpochStakes | `pkg/epochstakes/epoch_stakes.go` | `EpochStakesCache` |
| Leader schedule | `pkg/replay/leader_schedule_local.go` | `PrepareLeaderScheduleLocalFromVoteCache()` |
| StakeHistory | `pkg/replay/epoch.go` | `updateStakeHistorySysvar()` |

### Firedancer References

| Component | File |
|-----------|------|
| Epoch boundary | `src/flamenco/runtime/fd_runtime.c:604-707` |
| Stakes activation | `src/flamenco/stakes/fd_stakes.c:87-155` |
| Rewards | `src/flamenco/rewards/fd_rewards.c` |
