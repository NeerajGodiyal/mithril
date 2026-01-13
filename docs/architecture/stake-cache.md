# Stake Cache

The stake cache is an in-memory cache of all stake account delegations, used for EpochStakes computation and leader schedule generation.

## Location

- **Global accessor**: `global.StakeCache()` in `pkg/global/global_ctx.go:141`
- **Type**: `map[solana.PublicKey]*sealevel.Delegation`

## Data Stored

Each entry contains a `Delegation` struct:
- `VoterPubkey` - The vote account this stake is delegated to
- `Stake` - The delegated amount (lamports)
- `ActivationEpoch` - When delegation became active
- `DeactivationEpoch` - When delegation started deactivating
- `CreditsObserved` - Vote credits at last reward

## Lifecycle

### Initialization

**From snapshot** (`pkg/replay/block.go:726-774`):
1. Load stake pubkeys from `stake_pubkeys.idx` (fast path)
2. Or scan all accounts in AccountsDB for stake program owner (slow path)
3. Deserialize each stake account and cache delegation

### Updates During Replay

**Transaction processing** (`pkg/replay/transaction.go:175-195`):
```go
func recordStakeDelegation(acct *accounts.Account) {
    if isEmpty || isUninitialized {
        global.DeleteStakeCacheItem(acct.Key)
    } else {
        delegation := stakeState.Stake.Stake.Delegation
        global.PutStakeCacheItem(acct.Key, &delegation)
    }
}
```

Called for every stake account modified by transactions.

### NOT Updated During Rewards

**BUG (Issue #2)**: `DistributeStakingRewardsForPartition()` modifies stake accounts in AccountsDB but does NOT update the stake cache.

```go
// pkg/rewards/rewards.go:241
err := acctsDb.StoreAccounts(accts, slot)
// Missing: global.PutStakeCacheItem() calls
```

## Usage

### EpochStakes Computation (`pkg/replay/epoch.go:237-257`)
```go
stakes := global.StakeCache()
for _, delegation := range stakes {
    effectiveStake := delegation.Stake(epoch, stakeHistory, ...)
    // ... accumulate into EpochStakes
}
```

### StakeHistory Sysvar (`pkg/replay/epoch.go:103`)
```go
stakes := global.StakeCache()
// Compute activating/deactivating totals
```

## Staleness Impact

If stake cache is stale (after rewards):
1. **EpochStakes(N+2)** computed with wrong effective stakes
2. **StakeHistory** entry has wrong activating/deactivating totals
3. **Leader schedule** diverges from mainnet

## Persistence

### On Shutdown
- `stake_cache.json` - Full cache snapshot (graceful shutdown only)
- `stake_pubkeys.idx` - Append-only pubkey index (after each block)

### On Resume
1. Load `stake_pubkeys.idx`
2. Point-lookup each pubkey in AccountsDB
3. Rebuild cache with fresh data

## Code References

| Operation | File | Function |
|-----------|------|----------|
| Get cache | `pkg/global/global_ctx.go:141` | `StakeCache()` |
| Put item | `pkg/global/global_ctx.go:188` | `PutStakeCacheItem()` |
| Delete item | `pkg/global/global_ctx.go:197` | `DeleteStakeCacheItem()` |
| Transaction update | `pkg/replay/transaction.go:175` | `recordStakeDelegation()` |
| Index save | `pkg/global/global_ctx.go:597` | `SaveStakePubkeyIndex()` |
