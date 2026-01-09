# Epoch Transition Architecture

This document describes epoch transition behavior in two parts:
1. **Protocol Specification** - Verified behavior that matches Agave/Firedancer
2. **Mithril Implementation** - How Mithril implements these semantics, including persistence and resume

---

## Part 1: Protocol Specification (Verified Parity)

This section documents behavior that has been verified to match Agave and Firedancer implementations.

### 1.1 Epoch Boundary Timeline

```
Epoch N-1                                         Epoch N
──────────────────────────────────────────────────┬────────────────────────────
                                                  │
                         Last slot of epoch N-1 ──┤── First slot of epoch N
                           (boundary slot)        │   (first reward slot)
                                                  │
              StakeHistory updated here ──────────┤── Vote rewards distributed
              (for epoch N-1 entry)               │   EpochRewards sysvar created
                                                  │   Stake rewards distribution starts
```

**Key timing rules:**
- Epoch transition code runs when processing the FIRST slot of epoch N
- StakeHistory sysvar updated with epoch N-1 entry, stored at boundary slot
- Vote rewards distributed at first slot of epoch N
- Stake rewards distributed across subsequent slots (partitions)

**References:**
- Agave: `runtime/src/bank.rs:new_from_parent()`
- Firedancer: `fd_runtime.c:fd_runtime_block_execute()`

### 1.2 Leader Schedule N-2 Rule

**Verified: MATCH**

Leader schedule for epoch N uses stakes from epoch N-2 (not current stakes).

```
Schedule For    Stakes From    When Computed
────────────    ───────────    ─────────────
Epoch 908       End of 906     At boundary 906→907
Epoch 909       End of 907     At boundary 907→908
Epoch 910       End of 908     At boundary 908→909
```

At each epoch boundary N-1 → N:
1. Cache current stakes as `EpochStakes(N+1)` (for epoch N+1 schedule)
2. Use existing `EpochStakes(N)` for epoch N schedule (from N-2 stakes)

**Why this matters:** Using current-epoch stakes instead of N-2 stakes causes ~99% leader mismatch.

**References:**
- Agave: `runtime/src/bank/leader_schedule.rs:leader_schedule_epoch()`
- Firedancer: `fd_leader_schedule.c:fd_leader_schedule_new()`

### 1.3 Partitioned Epoch Rewards

**Verified: MATCH (partition count, partition assignment)**

#### 1.3.1 Eligible Account Count

An account is eligible for rewards if ALL conditions are true:
- `stake >= minimum_stake_delegation` (1 lamport after feature)
- Has valid vote account in cache
- `points > 0` (earned new credits with non-zero effective stake)

```go
numPartitions = ceil(eligibleAccounts / 4096)
// Clamped to [1, slotsPerEpoch/10]
```

#### 1.3.2 Partition Assignment (SipHash13)

Each stake account is assigned to exactly one partition using parent blockhash:

```
partition = SipHash13(stakePubkey, parentBlockhash) % numPartitions
```

**Critical:** `parentBlockhash` is the blockhash of the LAST slot of the previous epoch (stored in `EpochRewards.ParentBlockhash`).

**References:**
- Agave: `epoch_rewards_hasher.rs` (SipHash13)
- Firedancer: `fd_epoch_rewards.c:220`

#### 1.3.3 Distribution Uses Block Height, Not Slot

```go
partitionIdx = currentBlockHeight - distributionStartingBlockHeight
```

Partitions are selected by consecutive block heights, not slots (which can be skipped).

#### 1.3.4 EpochRewards Sysvar

```go
type SysvarEpochRewards struct {
    DistributionStartingBlockHeight uint64      // Height where partition 0 distributed
    NumPartitions                   uint64      // Total partitions
    ParentBlockhash                 [32]byte    // For partition hashing
    TotalRewards                    uint64      // Total stake rewards pool
    DistributedRewards              uint64      // Cumulative distributed so far
    TotalPoints                     Uint128     // Total points across all stakes
    Active                          bool        // True during distribution
}
```

#### 1.3.5 Stake Program Blocked During Distribution

When `EpochRewards.Active == true`:
- `Delegate`, `Deactivate`, `Split`, `Merge`, `Withdraw` → `StakeErrEpochRewardsActive`

**References:**
- Agave: `programs/stake/src/stake_instruction.rs`
- Firedancer: `fd_stake_program.c`

### 1.4 Distribution: Live AccountsDB Read

**Verified: MATCH with Firedancer**

At each partition distribution slot, the validator reads CURRENT account state from AccountsDB:

```c
// Firedancer: fd_rewards.c:860-875
fd_txn_account_init_from_funk_mutable(stake_acc_rec, stake_pubkey, accdb, ...)
fd_hashes_account_lthash(stake_pubkey, fd_txn_account_get_meta(stake_acc_rec), ...)
```

This is critical for LtHash correctness:
- "Before" hash must reflect account state at START of the distribution slot
- NOT the epoch boundary state (accounts could have received SOL transfers)

### 1.5 Two Computation Paths

| Condition | `EpochRewards.Active` | Behavior |
|-----------|----------------------|----------|
| Fresh boundary | `false` | Compute totals from AccountsDB at boundary slot |
| Resume mid-distribution | `true` | Use sysvar totals, DON'T recompute |

**Resume path constraints:**
- Use `EpochRewards.TotalPoints` and `TotalRewards` directly
- Use `maxEpoch` filter to freeze vote credits at rewarded epoch
- Scale per-account points proportionally if needed

**References:**
- Agave: `partitioned_epoch_rewards/calculation.rs`
- Firedancer: `fd_ssload.c:201-223`

### 1.6 Bank Hash Computation

**Verified: MATCH (formula)**

```
intermediateHash = SHA256(parentBankHash || numSignatures || blockHash)
bankHash = SHA256(intermediateHash || ltHash)
```

Where:
- `parentBankHash`: Bank hash from previous slot
- `numSignatures`: Total signatures in this slot
- `blockHash`: PoH hash for this slot
- `ltHash`: Lattice hash of all account modifications

**LtHash update for each modified account:**
```
ltHash ^= hash(accountPubkey, oldAccountData)
ltHash ^= hash(accountPubkey, newAccountData)
```

**References:**
- Agave: `runtime/src/bank/bank_hash.rs`
- Firedancer: `fd_bank_hash.c`

### 1.7 Vote Credits and Points

**Verified: MATCH**

Points formula:
```
points = Σ (earnedCredits × effectiveStake) over all credit-earning epochs

where:
  earnedCredits = min(epochCredits, currentVoteCredits) - creditsObserved
  effectiveStake = stake after warmup/cooldown for that epoch
```

**Force credits update:** If vote credits went backward (validator issue), stake account's `CreditsObserved` is still updated to current vote credits (no rewards earned, but state synchronized).

---

## Part 2: Mithril Implementation Details

This section documents Mithril-specific implementation choices for the protocol behaviors above.

### 2.1 State Files and Persistence

| File | Purpose | Updated |
|------|---------|---------|
| `mithril_state.json` | Last committed slot, epoch, blockhash | After each block commit |
| `stake_cache.json` | Full stake delegation cache snapshot | Graceful shutdown only |
| `stake_pubkeys.idx` | Append-only binary index of stake pubkeys | After each block commit |
| `boundary_vote_cache.json` | Vote cache at epoch boundary (for resume) | When rewards distribution starts |

#### 2.1.1 Stake Cache (`stake_cache.json`)

Full snapshot of stake delegations:

```json
{
  "slot": 392256000,
  "epoch": 907,
  "entries": [
    {
      "pubkey": "Stake111...",
      "voter_pubkey": "Vote111...",
      "stake_lamports": 1000000000,
      "activation_epoch": 500,
      "deactivation_epoch": 18446744073709551615,
      "warmup_cooldown_rate": 0.09,
      "credits_observed": 12345678
    }
  ]
}
```

**Saved only on graceful shutdown** (Ctrl+C, SIGTERM). Not saved on crash to avoid inconsistent state.

#### 2.1.2 Stake Pubkey Index (`stake_pubkeys.idx`)

Binary file of 32-byte pubkeys, append-only during replay:

```
[32 bytes: pubkey 1]
[32 bytes: pubkey 2]
...
```

**Purpose:** Fast cache rebuild after crash. Instead of scanning 200M accounts, do 1M point lookups.

**Lifecycle:**
1. New stake pubkeys appended after each block commit
2. On resume: load index, point-lookup each pubkey from AccountsDB
3. On graceful shutdown: index compacted (deduplicated)

#### 2.1.3 Boundary Vote Cache (`boundary_vote_cache.json`)

Snapshot of vote cache at epoch boundary, saved when rewards distribution starts:

```json
{
  "slot": 392256000,
  "epoch": 908,
  "entry_count": 1500,
  "entries": [
    {
      "pubkey": "Vote111...",
      "commission": 10,
      "epoch_credits": [
        {"epoch": 906, "credits": 12345, "prev_credits": 12000},
        {"epoch": 907, "credits": 12890, "prev_credits": 12345}
      ]
    }
  ]
}
```

**Purpose:** On resume mid-distribution, vote cache may have post-boundary commission values. This persisted cache ensures we use boundary-slot commission rates.

**Lifecycle:**
1. Saved when `EpochRewards` sysvar created (start of distribution)
2. Loaded on resume if `EpochRewards.Active == true`
3. Deleted when distribution completes

### 2.2 Resume Logic

#### 2.2.1 Normal Resume (Not Mid-Rewards)

```
1. Load mithril_state.json → get lastCommittedSlot
2. Try stake_cache.json:
   - If slot matches lastCommittedSlot → use cache
   - If stale/missing → try stake_pubkeys.idx
3. If no index → full AccountsDB scan (30+ minutes)
4. Resume replay from lastCommittedSlot + 1
```

#### 2.2.2 Resume Mid-Rewards Distribution

When resuming inside the rewards distribution period (`EpochRewards.Active == true`):

```
1. Load mithril_state.json → get lastCommittedSlot
2. Load stake_cache.json or rebuild from index
3. Read EpochRewards sysvar from AccountsDB:
   - Use TotalPoints, TotalRewards, NumPartitions from sysvar
   - DON'T recompute (would give different values)
4. Load boundary_vote_cache.json:
   - MERGE into existing vote cache (preserve LastTimestamp, NodePubkey)
   - Update Commission and EpochCredits from boundary cache
5. Refresh stake cache credits_observed from AccountsDB at boundary slot
6. Rebuild partition assignments using:
   - ParentBlockhash from EpochRewards sysvar
   - maxEpoch = rewardedEpoch (freeze vote credits)
7. Continue distribution from current partition
```

**Key invariants:**
- DON'T recompute totals when `Active == true`
- Use `maxEpoch` filter to freeze vote credits at rewarded epoch
- Boundary vote cache ensures correct commission rates

### 2.3 Stake Cache Update Flow

The stake cache is updated from multiple sources:

#### 2.3.1 Initial Load (Snapshot)

```go
// From build_db.go - loading from snapshot manifest
for _, epochStakes := range manifest.VersionedEpochStakes {
    if epochStakes.Epoch == manifest.Bank.Epoch {
        for stakePubkey, stakePair := range epochStakes.Stakes {
            global.PutStakeCacheItem(stakePubkey, &delegation)
        }
    }
}
```

**Note:** Manifest `CreditsObserved` is stale (snapshot time). Must refresh from AccountsDB before rewards.

#### 2.3.2 Incremental Updates (During Replay)

After each transaction, modified accounts are checked:

```go
// From recording.go - after transaction execution
for _, acct := range modifiedAccounts {
    if acct.Owner == StakeProgramId {
        recordStakeDelegation(acct)
    }
}

func recordStakeDelegation(acct *accounts.Account) {
    if acct.Lamports == 0 || isUninitialized(acct.Data) {
        global.DeleteStakeCacheItem(acct.Key)
        return
    }
    stakeState := UnmarshalStakeState(acct.Data)
    if stakeState.Type == StakeStateV2StatusStake {
        global.PutStakeCacheItem(acct.Key, &stakeState.Delegation)
    }
}
```

#### 2.3.3 Credits Refresh (At Epoch Boundary)

Before rewards calculation, refresh `CreditsObserved` from AccountsDB:

```go
// From rewards.go - RefreshStakeCacheCreditsObserved
for pubkey, delegation := range stakeCache {
    acct := acctsDb.GetAccount(boundarySlot, pubkey)
    stakeState := UnmarshalStakeState(acct.Data)
    delegation.CreditsObserved = stakeState.Stake.CreditsObserved
}
```

This is critical because:
- Manifest credits are stale (snapshot time)
- Cache may have been rebuilt from index (no credits info)
- AccountsDB has authoritative credits after prior rewards

### 2.4 Vote Cache Update Flow

The vote cache is updated less frequently:

#### 2.4.1 Rebuild at Epoch Boundary

```go
// From leader_schedule_local.go - RebuildVoteCacheFromAccountsDB
for _, voteAccount := range voteAccounts {
    acct := acctsDb.GetAccount(boundarySlot, voteAccount.Pubkey)
    voteState := UnmarshalVoteState(acct.Data)
    global.PutVoteCacheItem(voteAccount.Pubkey, voteState)
}
```

This captures:
- `NodePubkey` (for leader schedule)
- `Commission` (for rewards split)
- `EpochCredits` (for points calculation)
- `LastTimestamp` (for clock sysvar)

#### 2.4.2 Incremental Updates (During Replay)

Vote accounts are updated after vote transactions:

```go
// From recording.go
if acct.Owner == VoteProgramId {
    voteState := UnmarshalVoteState(acct.Data)
    global.PutVoteCacheItem(acct.Key, voteState)
}
```

#### 2.4.3 Boundary Cache Merge (On Resume)

When resuming mid-distribution:

```go
// From global_ctx.go - LoadBoundaryVoteCache
for _, entry := range boundaryCache.Entries {
    existing := voteCache[entry.Pubkey]
    if existing != nil {
        // MERGE: Update Commission and EpochCredits
        // PRESERVE: LastTimestamp, NodePubkey
        existing.Commission = entry.Commission
        existing.EpochCredits = entry.EpochCredits
    } else {
        // Create minimal entry (no LastTimestamp)
        voteCache[entry.Pubkey] = newVoteState(entry)
    }
}
```

### 2.5 Mid-Block Crash Safety

Mithril tracks whether a block is being processed:

```go
var blockReplayInProgress atomic.Bool

func ProcessBlock(block) {
    blockReplayInProgress.Store(true)
    defer blockReplayInProgress.Store(false)

    // ... process block ...

    // Only after successful commit:
    FlushPendingStakePubkeys()  // Append new pubkeys to index
    SaveMithrilState()           // Update last committed slot
}
```

On shutdown/panic:
- If `blockReplayInProgress == true`: Don't save stake cache (has uncommitted changes)
- Clear pending pubkeys (not yet flushed)
- Resume from last committed slot (before current block)

### 2.6 Epoch Transition Code Flow

```
ProcessBlock (first slot of new epoch)
  │
  ├─► Detect epoch boundary (block.Epoch != currentEpoch)
  │
  ├─► prepareEpochStakes()
  │     ├─ Read StakeHistory sysvar
  │     ├─ Compute activation/deactivation for all delegations
  │     ├─ Update StakeHistory with new epoch entry
  │     └─ Store at BOUNDARY slot
  │
  ├─► Rebuild vote cache from AccountsDB at boundary slot
  │     └─ Extracts NodePubkey, Commission, EpochCredits
  │
  ├─► computeLeaderSchedule()
  │     ├─ Check if snapshot has schedule → REUSE
  │     └─ Otherwise build from EpochStakes(N) (N-2 stakes)
  │
  ├─► handleEpochRewards()
  │     ├─ If Active: recalculatePartitionedRewardsForResume()
  │     └─ If !Active: beginPartitionedEpochRewardsDistribution()
  │           ├─ Refresh credits_observed from AccountsDB
  │           ├─ Calculate points and rewards
  │           ├─ Distribute vote rewards
  │           ├─ Build partitions
  │           ├─ Create EpochRewards sysvar
  │           └─ Save boundary vote cache
  │
  └─► Continue with normal block processing
```

### 2.7 Key Code Locations

| Component | Location |
|-----------|----------|
| Epoch boundary detection | `pkg/replay/block.go:1494-1500` |
| Stake history update | `pkg/replay/epoch.go:180-237` |
| Vote cache rebuild | `pkg/replay/leader_schedule_local.go:190-350` |
| Leader schedule | `pkg/replay/block.go:1600-1720` |
| Fresh rewards | `pkg/replay/rewards.go:29-380` |
| Resume rewards | `pkg/replay/rewards.go:390-520` |
| Distribution | `pkg/replay/rewards.go:520-700` |
| Stake cache persistence | `pkg/global/global_ctx.go:400-740` |
| Boundary vote cache | `pkg/global/global_ctx.go:832-1044` |

---

## Appendix A: Common Divergence Causes

| Symptom | Likely Cause | Verification |
|---------|--------------|--------------|
| 99% leader mismatch | Using current stakes instead of N-2 | Check which EpochStakes being used |
| Partition count mismatch | Stale credits_observed | Verify refresh from AccountsDB |
| Bank hash mismatch (cumulative matches) | LtHash before/after states wrong | Compare account state at distribution time vs boundary |
| VoteErrSlotHashMismatch | Bank hash diverged at earlier slot | Trace back to find divergence point |

## Appendix B: Verification Checklist

**Leader Schedule:**
- [ ] Snapshot schedule reused if available
- [ ] EpochStakes cached as (N+1) at boundary N-1→N
- [ ] Schedule validated against RPC

**Partitioned Rewards:**
- [ ] credits_observed refreshed from AccountsDB at boundary
- [ ] Partition count = ceil(eligible / 4096)
- [ ] Partition hash uses parent blockhash (not grandparent)
- [ ] Distribution reads from current AccountsDB (not boundary snapshots)
- [ ] Burned lamports tracked and added to DistributedRewards

**Resume:**
- [ ] Active=true: use sysvar totals, don't recompute
- [ ] maxEpoch filter applied to freeze vote credits
- [ ] Boundary vote cache loaded and merged (not replaced)
- [ ] Stakes refreshed from AccountsDB at boundary slot

**Persistence:**
- [ ] stake_pubkeys.idx flushed before state file updated
- [ ] stake_cache.json only saved on graceful shutdown
- [ ] boundary_vote_cache.json saved at distribution start
- [ ] Mid-block crash doesn't save inconsistent state
