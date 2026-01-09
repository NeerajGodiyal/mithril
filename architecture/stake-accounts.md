# Stake Accounts

Stake accounts are the fundamental unit of Solana's Proof of Stake consensus. They delegate SOL to validators and earn rewards proportional to stake weight and validator performance.

## Account Structure

A stake account has 200 bytes of data:

```go
type StakeStateV2 struct {
    Type  uint32         // 0=Uninitialized, 1=Initialized, 2=Stake, 3=RewardsPool
    Meta  Meta           // Rent, authorized keys
    Stake Stake          // Delegation info + credits observed
}

type Meta struct {
    RentExemptReserve uint64
    Authorized        Authorized  // Staker and Withdrawer pubkeys
    Lockup            Lockup      // Optional time/epoch lock
}

type Stake struct {
    Delegation      Delegation
    CreditsObserved uint64      // Vote credits at last reward claim
}

type Delegation struct {
    VoterPubkey        solana.PublicKey
    StakeLamports      uint64
    ActivationEpoch    uint64
    DeactivationEpoch  uint64    // MaxUint64 if not deactivating
    WarmupCooldownRate float64   // 0.25 or 0.09
}
```

## Stake Lifecycle

```
                    ┌─────────────────┐
                    │  Uninitialized  │
                    └────────┬────────┘
                             │ Initialize
                             ▼
                    ┌─────────────────┐
                    │   Initialized   │  Has Meta, no Delegation
                    └────────┬────────┘
                             │ Delegate
                             ▼
                    ┌─────────────────┐
     Activating ──▶ │     Stake       │ ◀── Deactivating
     (warmup)       │   (Delegated)   │     (cooldown)
                    └────────┬────────┘
                             │ Deactivate (after cooldown complete)
                             ▼
                    ┌─────────────────┐
                    │   Initialized   │  Can re-delegate or withdraw
                    └─────────────────┘
```

## Warmup and Cooldown

Stake doesn't become fully effective immediately. This prevents rapid stake shifts that could destabilize consensus.

### Effective Stake Calculation

```go
func (delegation *Delegation) StakeActivatingAndDeactivating(
    targetEpoch uint64,
    stakeHistory *SysvarStakeHistory,
    newRateActivationEpoch *uint64,
) StakeAndActivating {
    // Returns:
    // - Effective: fully active stake (earns full rewards)
    // - Activating: stake still warming up
    // - Deactivating: stake cooling down
}
```

### Warmup/Cooldown Rate

The rate determines how quickly stake becomes effective:

| Rate | Per-Epoch Increase | Epochs to 90% |
|------|-------------------|---------------|
| 0.25 (old) | 25% of remaining | ~9 epochs |
| 0.09 (new) | 9% of remaining | ~25 epochs |

The new rate (0.09) activates with feature `ReduceStakeWarmupCooldown`.

### Example: Activation Timeline

100 SOL delegated at epoch 100 with rate 0.25:

| Epoch | Activating | Effective | Calculation |
|-------|------------|-----------|-------------|
| 100   | 100        | 0         | Just delegated |
| 101   | 75         | 25        | 100 * 0.25 = 25 |
| 102   | 56.25      | 43.75     | 75 * 0.25 = 18.75 more |
| 103   | 42.19      | 57.81     | ... |
| ...   | ...        | ...       | Asymptotically approaches 100 |

**Note:** Effective stake is rounded, and the calculation considers total network stake warming up (stake history sysvar).

## The Stake Cache

Validators maintain an in-memory cache of stake delegations for fast lookup:

```go
var stakeCache map[solana.PublicKey]*Delegation
```

### Cache Population

1. **From snapshot:** Parse stake accounts in snapshot manifest
2. **From AccountsDB scan:** Iterate all accounts owned by Stake Program
3. **Incremental updates:** Track stake account modifications during replay

### Incremental Updates

After each transaction, check modified accounts:

```go
func recordStakeDelegation(acct *accounts.Account) {
    // Skip empty or uninitialized accounts
    if acct.Lamports == 0 {
        global.DeleteStakeCacheItem(acct.Key)
        return
    }
    if isUninitialized(acct.Data) {
        global.DeleteStakeCacheItem(acct.Key)
        return
    }

    stakeState := UnmarshalStakeState(acct.Data)
    if stakeState.Type == StakeStateV2StatusStake {
        global.PutStakeCacheItem(acct.Key, &stakeState.Stake.Delegation)
    }
}
```

### Cache Persistence

For fast resume, persist the cache to disk:

```json
// stake_cache.json
[
  {
    "pubkey": "Stake111...",
    "voter_pubkey": "Vote111...",
    "stake_lamports": 1000000000,
    "activation_epoch": 500,
    "deactivation_epoch": 18446744073709551615,
    "warmup_cooldown_rate": 0.09,
    "credits_observed": 12345678
  },
  ...
]
```

On resume:
1. Try to load `stake_cache.json`
2. If missing/corrupt, scan AccountsDB (expensive but recoverable)
3. Continue incremental updates from resume point

## Minimum Stake for Rewards Eligibility

For rewards eligibility, the minimum is 1 lamport (after StakeMinimumDelegationForRewards feature):

```go
func minimumStakeDelegation(features *Features) uint64 {
    if !features.IsActive(StakeMinimumDelegationForRewards) {
        return 0  // No minimum before feature
    }
    return 1  // 1 lamport
}
```

Accounts with 0 stake exist but don't earn rewards.

## Credits Observed

`CreditsObserved` tracks the vote credits at the time of last reward claim:

```
points = sum over epochs of (earnedCredits * effectiveStake)

where earnedCredits = newCredits - creditsObserved
```

After rewards are distributed, `CreditsObserved` is updated to the current vote credit total.

### Edge Case: Stale Credits

If `creditsObserved >= currentVoteCredits`, the account earns no rewards that epoch (nothing new to claim).

## Stake History Sysvar

Tracks network-wide stake activation/deactivation for warmup calculations:

```go
type SysvarStakeHistory []StakeHistoryEntry

type StakeHistoryEntry struct {
    Epoch        uint64
    Effective    uint64  // Network total effective stake
    Activating   uint64  // Network total activating
    Deactivating uint64  // Network total deactivating
}
```

Warmup rate is modulated by network-wide activation pressure to prevent rapid shifts.

## Common Edge Cases

### 1. Zero-Stake Accounts
Accounts with `StakeLamports == 0` exist (rent-exempt reserve only). Skip in reward calculations.

### 2. Deactivated but Not Withdrawn
`DeactivationEpoch != MaxUint64` but account still has stake. Cooldown may still be in progress.

### 3. Re-delegation
After full deactivation, an account can delegate to a different validator. `ActivationEpoch` resets, `DeactivationEpoch` becomes `MaxUint64`.

### 4. Missing Vote Account
If the delegated vote account doesn't exist or isn't in cache, stake account is ineligible for rewards.

### 5. Split During Warmup
Splitting a stake account during warmup creates two accounts that continue warming up independently.

## Stake Program Instructions

| Instruction | Effect |
|-------------|--------|
| Initialize | Create Meta, set authorized keys |
| Delegate | Set VoterPubkey, ActivationEpoch = current |
| Deactivate | Set DeactivationEpoch = current |
| Withdraw | Remove lamports (after cooldown if staked) |
| Split | Divide stake into two accounts |
| Merge | Combine two stake accounts |
| SetLockup | Update lockup parameters |
| AuthorizeWithSeed | Change authorized keys |

### Blocked During Rewards Period

When `EpochRewards.Active == true`, these instructions fail:
- Delegate
- Deactivate
- Split
- Merge
- Withdraw (staked accounts)

This ensures stake state is stable during reward distribution.

## References

- [Stake Program](https://docs.solanalabs.com/runtime/programs#stake-program)
- Agave: `programs/stake/src/`
- Agave: `sdk/program/src/stake/state.rs`
