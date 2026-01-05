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
1. `stake >= minimum_stake_delegation` (typically 1 SOL on mainnet)
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
