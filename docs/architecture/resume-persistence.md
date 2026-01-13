# Resume and Persistence

Mithril can stop and resume replay without reprocessing from snapshot.

## State File

**Location**: `mithril_state.json` in data directory

**Structure** (`pkg/state/state.go:24-100`):

```go
type MithrilState struct {
    // Position
    LastSlot     uint64
    LastEpoch    uint64
    LastBankhash string

    // LtHash and fees
    LastAcctsLtHash          string
    LastLamportsPerSignature uint64
    LastNumSignatures        uint64

    // Blockhash context
    LastRecentBlockhashes []BlockhashEntry
    LastBlockhash         string
    LastSlotHashes        []SlotHashEntry

    // Inflation
    LastCapitalization    uint64
    LastSlotsPerYear      float64
    LastInflation*        float64  // Multiple fields

    // EpochStakes (critical for leader schedule)
    ComputedEpochStakes map[uint64]string  // epoch -> JSON
}
```

## What's Persisted vs Rebuilt

| Component | Persisted | Rebuilt From |
|-----------|-----------|--------------|
| Position (slot, bankhash) | Yes (State file) | - |
| LtHash | Yes (State file) | - |
| EpochStakes | Yes (State file) | - |
| VoteCache | No | AccountsDB |
| StakeCache | Partial | `stake_pubkeys.idx` + AccountsDB |
| Leader Schedule | No | EpochStakes |

## Stake Cache Recovery

### Graceful Shutdown

1. `stake_cache.json` saved with full cache
2. `stake_pubkeys.idx` compacted (deduplicated)

### Crash Recovery

1. `stake_cache.json` may be stale (last graceful shutdown)
2. Load `stake_pubkeys.idx` (append-only, up-to-date)
3. Point-lookup each pubkey in AccountsDB (~1M lookups)
4. Much faster than full scan (~200M accounts)

**Index format**: Binary file of 32-byte pubkeys

```go
// pkg/global/global_ctx.go:597-740
func SaveStakePubkeyIndex(path string) error
func LoadStakePubkeyIndex(path string) ([]solana.PublicKey, error)
func AppendStakePubkeyToIndex(path string, pubkey solana.PublicKey) error
```

## EpochStakes Persistence

**Critical for multi-epoch resume** (PR #183)

At each epoch boundary, stakes are cached as `EpochStakes(leaderScheduleEpoch)`. These must be persisted to generate correct leader schedules on resume.

### Save (`pkg/replay/block.go:180-198`)

```go
func serializeAllEpochStakes() map[uint64][]byte {
    epochs := global.GetAllCachedEpochs()
    for _, epoch := range epochs {
        data, _ := global.SerializeEpochStakes(epoch)
        result[epoch] = data
    }
    return result
}
```

### Load (`pkg/replay/block.go:1128-1166`)

```go
for epoch, data := range resumeState.ComputedEpochStakes {
    global.DeserializeAndLoadEpochStakes(data)
}
// Validate current epoch stakes exist
if !global.HasEpochStakes(currentEpoch) {
    // FATAL - cannot resume without leader schedule data
}
```

## Resume Constraints

### Cannot Resume During Rewards Distribution

```go
// pkg/replay/block.go:1345-1362
if rewards.IsWithinRewardsPeriod(block.Epoch, currentSlot, epochSchedule) {
    os.Exit(1)  // Not supported yet
}
```

### Must Have EpochStakes for Current Epoch

If resuming in epoch N, must have `EpochStakes(N)` persisted. Otherwise, cannot generate leader schedule.

### Crash After Boundary Without Persist

If crash happens after epoch boundary but before state file is written:
- `EpochStakes(N+1)` computed in memory
- Never persisted
- On resume: missing required stakes
- **Solution**: Need fresh snapshot

## Mid-Block Crash Safety

**Flag**: `blockReplayInProgress` atomic

```go
// pkg/replay/block.go
blockReplayInProgress.Store(true)   // Block start
// ... process block ...
blockReplayInProgress.Store(false)  // After commit

// On shutdown, skip cache save if mid-block
if blockReplayInProgress.Load() {
    // Don't save stake cache (may have uncommitted updates)
}
```

## Code References

| Component | File | Function |
|-----------|------|----------|
| State struct | `pkg/state/state.go:24` | `MithrilState` |
| Save state | `pkg/state/state.go:250` | `Save()` |
| Load state | `pkg/state/state.go:200` | `Load()` |
| EpochStakes serialize | `pkg/epochstakes/epoch_stakes.go:72` | `SerializeEpoch()` |
| Stake index | `pkg/global/global_ctx.go:597` | `SaveStakePubkeyIndex()` |
| Resume logic | `pkg/replay/block.go:900` | `configureInitialBlockFromResume()` |
