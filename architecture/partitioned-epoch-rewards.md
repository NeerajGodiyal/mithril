# Partitioned Epoch Rewards

Solana distributes staking rewards at the start of each epoch. Originally, all rewards were distributed in a single block, but as the number of stake accounts grew, this became a bottleneck. **Partitioned epoch rewards** spreads this work across multiple blocks.

## Overview

At each epoch boundary:
1. **Vote rewards** are distributed immediately in the first block of the new epoch
2. **Stake rewards** are distributed across N subsequent blocks (partitions)
3. During distribution, the **EpochRewards sysvar** tracks progress and blocks certain stake operations

## Timeline Within an Epoch

```
Epoch N                                          Epoch N+1
─────────────────────────────────────────────────┬─────────────────────────────────
                                                 │
                                          First slot in epoch
                                                 │
                                                 ▼
                                         ┌──────────────┐
                                         │ Vote Rewards │  Block 0 (epoch boundary)
                                         │ Distributed  │  - EpochRewards sysvar set Active=true
                                         └──────────────┘  - DistributionStartingBlockHeight = block_height + 1
                                                 │
                                                 ▼
                                         ┌──────────────┐
                                         │ Partition 0  │  Block at height = StartHeight + 0
                                         └──────────────┘
                                                 │
                                                 ▼
                                         ┌──────────────┐
                                         │ Partition 1  │  Block at height = StartHeight + 1
                                         └──────────────┘
                                                 │
                                                ...
                                                 │
                                                 ▼
                                         ┌──────────────┐
                                         │ Partition N-1│  Block at height = StartHeight + N-1
                                         │ (final)      │  - EpochRewards sysvar set Active=false
                                         └──────────────┘
```

## Computing Number of Partitions

The number of partitions is computed locally based on the number of **eligible** stake accounts:

```go
const MaxRewardsPerBlock = 4096

func ComputeNumRewardPartitions(epoch, slotsPerEpoch, numStakeAccounts, firstNormalEpoch uint64) uint64 {
    // Warmup epochs use 1 partition (legacy behavior)
    if epoch < firstNormalEpoch {
        return 1
    }

    // Base calculation: ceil(numStakeAccounts / MaxRewardsPerBlock)
    partitions := (numStakeAccounts + MaxRewardsPerBlock - 1) / MaxRewardsPerBlock

    // Clamp to [1, slotsPerEpoch]
    if partitions == 0 {
        partitions = 1
    }
    if partitions > slotsPerEpoch {
        partitions = slotsPerEpoch
    }

    return partitions
}
```

### Critical: What Counts as an "Eligible" Stake Account?

This is the most common source of partition count mismatches. Not all stake accounts in the cache earn rewards:

**An account is eligible if ALL of these are true:**
1. `stake >= minimum_stake_delegation` (1 lamport after StakeMinimumDelegationForRewards feature)
2. Has a corresponding vote account in the vote cache
3. `points > 0` - meaning:
   - Has new credits to earn (`creditsInVote > creditsInStake`)
   - Has non-zero effective stake for at least one credit-earning epoch

```go
// Pseudocode for eligibility check
func isEligible(delegation, voteState, stakeHistory) bool {
    if delegation.StakeLamports < minimumStakeDelegation {
        return false
    }
    if voteState == nil {
        return false
    }

    // Check if account has points > 0
    creditsInStake := delegation.CreditsObserved
    creditsInVote := voteState.EpochCredits[last].Credits

    if creditsInVote <= creditsInStake {
        return false  // No new credits to earn
    }

    // Check effective stake across credit-earning epochs
    for each epochCredit in voteState.EpochCredits {
        earnedCredits := computeEarnedCredits(epochCredit, creditsInStake)
        if earnedCredits > 0 {
            effectiveStake := delegation.StakeActivatingAndDeactivating(epoch, stakeHistory)
            if effectiveStake.Effective > 0 {
                return true  // Has points > 0
            }
        }
    }
    return false  // Zero points (warmup/cooldown ate all effective stake)
}
```

## Partition Assignment

Each stake account is assigned to exactly one partition using a deterministic hash:

```go
func CalculateRewardPartitionForPubkey(pubkey, parentBlockhash, numPartitions) uint64 {
    // Hash: SHA256(pubkey || parentBlockhash)
    hasher := sha256.New()
    hasher.Write(pubkey[:])
    hasher.Write(parentBlockhash[:])
    hash := hasher.Sum(nil)

    // Take first 8 bytes as little-endian u64, mod numPartitions
    hashValue := binary.LittleEndian.Uint64(hash[:8])
    return hashValue % numPartitions
}
```

**Key insight:** The `parentBlockhash` is the blockhash of the **last block of the previous epoch**, stored in the EpochRewards sysvar. This ensures all validators compute identical partition assignments.

## Distribution Mechanics

### Partition Selection: Block Height, Not Slot

Partitions are selected by **block height**, not slot number:

```go
partitionIdx := currentBlockHeight - epochRewards.DistributionStartingBlockHeight
```

This is critical because slots can be skipped (leader didn't produce a block), but block heights are consecutive. Using block height ensures:
- No partition is skipped due to missed slots
- Distribution completes in exactly N blocks regardless of slot gaps

### Stop Condition: Partition-Based

Distribution stops when all partitions have been processed:

```go
if partitionIdx >= numRewardPartitions - 1 {
    epochRewards.Active = false
}
```

**Not slot-based:** Earlier implementations incorrectly used `currentSlot == lastRewardSlot`, which failed when slots were skipped.

## The EpochRewards Sysvar

```go
type SysvarEpochRewards struct {
    DistributionStartingBlockHeight uint64      // Height where partition 0 is distributed
    NumPartitions                   uint64      // Total number of partitions
    ParentBlockhash                 [32]byte    // Used for partition assignment hashing
    TotalRewards                    uint64      // Total stake rewards to distribute
    DistributedRewards              uint64      // Rewards distributed so far
    TotalPoints                     wide.Uint128 // Total points across all stake accounts
    Active                          bool        // True while distribution in progress
}
```

### Stake Program Behavior During Distribution

When `EpochRewards.Active == true`, the stake program **rejects** certain operations:
- `Delegate`
- `Deactivate`
- `Merge`
- `Split`
- `Withdraw` (for staked accounts)

This prevents stake state changes during reward distribution, ensuring consistent point calculations.

## Resume After Crash

If a validator crashes during the rewards period, it must reconstruct `partitionedRewardsInfo` on resume:

1. Load `EpochRewards` sysvar from AccountsDB
2. If `Active == false`, rewards already complete - skip reconstruction
3. Otherwise, rebuild:
   - Use stored `ParentBlockhash` for partition assignment
   - Use stored `TotalPoints` for reward calculations
   - Recalculate partition assignments from stake cache
   - Continue distribution from current block height

**Stake cache persistence:** Since stake accounts can't change during rewards period (stake program rejects operations), the stake cache state is deterministic and can be reconstructed from AccountsDB if needed.

## Vote Credit Source and Boundary Correctness

**Critical invariant:** Boundary correctness depends on the **vote credit source**, not just replay progress.

### Core Idea

Snapshots are trustworthy for the slot they represent, but rewards are computed using **epoch-boundary data** (the last slot of the previous epoch). If the snapshot isn't at that boundary, its vote credits are not the correct source for rewards.

### Why the Boundary Matters

Rewards for epoch N-1 are computed at the first slot of epoch N, using vote credits as of the last slot of N-1. So:
- If snapshot is **mid-epoch**, its vote credits are **stale** for the boundary
- If using **"live" vote credits** (post-boundary), you can **overshoot** (credits keep accumulating)
- Both are wrong unless you're using the boundary slot state or the recalc path

### The Two Computation Paths

#### 1) Resume During Rewards Distribution (`EpochRewards.Active == true`)

You're inside the early-epoch distribution window (rewards still being paid out).

**Correct behavior:**
- Do NOT recompute `total_points` or `total_rewards`
- Use EpochRewards sysvar totals directly
- Use `maxEpoch` filtering on epoch credits to freeze view at rewarded epoch
- Scale individual points proportionally if needed to match sysvar totals

Firedancer also uses a `prev_vote_credits` snapshot for this path, which we approximate via `maxEpoch` filtering in `calculateStakePointsAndCredits`.

#### 2) Fresh Boundary Distribution (`EpochRewards.Active == false`)

You just crossed into the new epoch and distribution hasn't started yet.

**Correct behavior:**
- Compute using AccountsDB state at the boundary slot (prev slot)
- Vote cache must reflect boundary-slot vote credits for ALL vote accounts used
- "Replayed to boundary" is NOT sufficient unless vote cache was explicitly rebuilt from boundary-slot state

**Common mistake:** Using global vote cache that contains snapshot-time or post-boundary credits. This causes `total_points` divergence.

### Stake Cache Loading

The stake cache is loaded from the decoded snapshot manifest (`VersionedEpochStakes`), **keyed by epoch**:

```go
// From build_db.go
for _, epochStakes := range manifest.VersionedEpochStakes {
    if epochStakes.Epoch == manifest.Bank.Epoch {  // Matches bank epoch, NOT fixed index
        for stakePubkey, stakePair := range epochStakes.Val.Stakes.StakeDelegations {
            global.PutStakeCacheItem(stakePubkey, &sealevel.Delegation{
                VoterPubkey:        d.VoterPubkey,
                StakeLamports:      d.Stake,
                CreditsObserved:    stakePair.Stake.CreditsObserved,
                // ...
            })
        }
    }
}
```

**Important:** There is no `epoch_stakes[0]/[1]` indexing. We iterate and match by epoch number.

### Sanity Check

To verify correctness:
1. Read EpochRewards sysvar from AccountsDB at boundary slot
2. Compare computed `total_points` against sysvar value
3. If mismatch → vote credit source is wrong (not boundary-slot data)

Logging the mismatch is useful for debugging; RPC fallback is a temporary aid, not a solution.

### Summary Table

| Condition | `EpochRewards.Active` | Data Source |
|-----------|----------------------|-------------|
| Fresh boundary | `false` | AccountsDB at prev_slot (boundary) |
| Resume mid-distribution | `true` | EpochRewards sysvar totals (don't recompute) |

## Points Calculation

Points determine each stake account's share of rewards:

```
points = sum over epochs of (earnedCredits * effectiveStake)
```

Where:
- `earnedCredits` = new vote credits earned since last reward
- `effectiveStake` = stake after accounting for warmup/cooldown

### Warmup and Cooldown

Stake doesn't become fully effective immediately:

```go
func StakeActivatingAndDeactivating(epoch, stakeHistory, newRateEpoch) StakeAndActivating {
    // Returns: Effective, Activating, Deactivating
    //
    // Effective = stake that earns full rewards
    // Activating = stake warming up (earns partial)
    // Deactivating = stake cooling down (earns partial)
}
```

The warmup/cooldown rate changed with feature `ReduceStakeWarmupCooldown`:
- Old rate: 25% per epoch
- New rate: 9% per epoch (faster warmup/cooldown)

## Common Implementation Bugs

### 1. Counting Total vs Eligible Accounts
```
Wrong: numPartitions = ceil(len(stakeCache) / 4096)
Right: numPartitions = ceil(countEligible(stakeCache) / 4096)
```

### 2. Using Slot Instead of Block Height
```
Wrong: partitionIdx = currentSlot - firstRewardSlot
Right: partitionIdx = currentBlockHeight - distributionStartingBlockHeight
```

### 3. Slot-Based Stop Condition
```
Wrong: if currentSlot == lastRewardSlot { active = false }
Right: if partitionIdx >= numPartitions - 1 { active = false }
```

### 4. Missing Vote Cache Entries
Stake accounts whose voter isn't in the vote cache are ineligible. Ensure vote cache is fully populated before counting.

### 5. Stale Stake Cache on Resume
After crash, stake cache may be empty or stale. Must either:
- Persist stake cache to disk
- Scan AccountsDB to rebuild stake cache

## References

- [SIMD-0118: Partitioned Epoch Rewards](https://github.com/solana-foundation/solana-improvement-documents/blob/main/proposals/0118-partitioned-epoch-rewards.md)
- Agave: `runtime/src/bank/partitioned_epoch_rewards/`
- Agave: `runtime/src/epoch_rewards_hasher.rs`
