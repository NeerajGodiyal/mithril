# Partitioned Epoch Rewards

At each epoch boundary, staking rewards are distributed across multiple blocks to avoid overloading a single block.

## Overview

1. **Vote rewards**: Distributed immediately at boundary (single block)
2. **Stake rewards**: Distributed across N partitions over N blocks

## Partition Formula

```go
numPartitions = ceil(eligibleStakeAccounts / 4096)
```

On mainnet with ~1M stake accounts: ~250 partitions = ~250 blocks

## Distribution Timeline

```
Epoch N-1          | Epoch N
-------------------+--------------------------------------------
                   | Slot 0: Vote rewards + partition 0
                   | Slot 1: Partition 1
                   | Slot 2: Partition 2
                   | ...
                   | Slot N: Partition N (last, Active=false)
```

## Key Sysvar: EpochRewards

**Location**: `pkg/sealevel/sysvar_epoch_rewards.go`

```go
type SysvarEpochRewards struct {
    DistributionStartingBlockHeight uint64    // First stake rewards block
    NumPartitions                   uint64    // Remaining partitions
    ParentBlockhash                 [32]byte  // For partition assignment hash
    TotalRewards                    uint64    // Total to distribute
    DistributedRewards              uint64    // Already distributed
    TotalPoints                     uint64    // Total stake points
    Active                          bool      // true during distribution
}
```

### Active Flag

- `Active = true`: Distribution in progress, stake operations blocked
- `Active = false`: Distribution complete

## Partition Assignment

Accounts are assigned to partitions using SipHash13:

```go
// Uses ParentBlockhash as hash key
partitionIndex = SipHash13(stakePubkey, parentBlockhash) % numPartitions
```

**Critical**: Must use the correct `ParentBlockhash` (parent of first epoch slot, NOT grandparent).

## Reward Calculation

### Points Calculation (`pkg/rewards/rewards.go:505-577`)

```go
creditsInStake := delegation.CreditsObserved  // From stake account
creditsInVote := voteState.EpochCredits       // From vote account
newCredits := creditsInVote - creditsInStake  // Credits earned this epoch
points := newCredits * effectiveStake         // Stake-weighted points
```

### Reward Distribution

```go
reward = (points / totalPoints) * totalRewards
stakeAccount.Lamports += stakerReward
voteAccount.Lamports += commissionReward
```

## Code Flow

### At Boundary (`pkg/replay/rewards.go:164-208`)

```go
func beginPartitionedEpochRewardsDistribution(...) {
    // 1. Calculate totals
    partitionedRewardsInfo := rewards.DeterminePartitionedStakingRewardsInfo(...)

    // 2. Calculate stake points
    credits := rewards.CalculateStakePointsForAllDelegations(...)

    // 3. Calculate rewards per account
    stakeRewards := rewards.CalculateStakeRewardsFromPoints(...)

    // 4. Distribute vote rewards (immediate)
    rewards.DistributeVotingRewards(...)

    // 5. Set up EpochRewards sysvar
    epochRewards.Active = true
}
```

### Subsequent Blocks (`pkg/replay/rewards.go:210-247`)

```go
func distributePartitionedEpochRewardsForSlot(...) {
    partitionIdx := blockHeight - startingBlockHeight
    partition := rewardPartitions[partitionIdx]

    rewards.DistributeStakingRewardsForPartition(acctsDb, partition, ...)

    epochRewards.NumPartitions--
    if epochRewards.NumPartitions == 0 {
        epochRewards.Active = false
    }
}
```

## Resume During Distribution

**NOT SUPPORTED** - Mithril exits if `EpochRewards.Active == true` on resume.

```go
// pkg/replay/block.go:1345-1362
if rewards.IsWithinRewardsPeriod(block.Epoch, currentSlot, epochSchedule) {
    mlog.Log.Errorf("RESUME DURING REWARDS PERIOD NOT YET SUPPORTED")
    os.Exit(1)
}
```

## Known Issues

### Stake Cache Not Updated (Issue #2)

`DistributeStakingRewardsForPartition()` modifies stake accounts but doesn't update stake cache:

```go
// Stores to AccountsDB
err := acctsDb.StoreAccounts(accts, slot)
// Missing: global.PutStakeCacheItem() for each modified stake
```

## Code References

| Component | File | Function |
|-----------|------|----------|
| Begin distribution | `pkg/replay/rewards.go:164` | `beginPartitionedEpochRewardsDistribution()` |
| Per-slot distribution | `pkg/replay/rewards.go:210` | `distributePartitionedEpochRewardsForSlot()` |
| Points calculation | `pkg/rewards/rewards.go:505` | `calculateStakePointsAndCredits()` |
| Partition distribution | `pkg/rewards/rewards.go:180` | `DistributeStakingRewardsForPartition()` |
| EpochRewards sysvar | `pkg/sealevel/sysvar_epoch_rewards.go` | `SysvarEpochRewards` |
