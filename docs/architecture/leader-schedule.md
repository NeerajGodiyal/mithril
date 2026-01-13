# Leader Schedule

The leader schedule determines which validator produces each block. Mithril must generate identical schedules to mainnet.

## The N-2 Rule

Leader schedule for epoch N uses stakes frozen at end of epoch **N-2**:

| Schedule For | Stakes From | When Frozen |
|--------------|-------------|-------------|
| Epoch 906 | End of epoch 904 | At 904->905 boundary |
| Epoch 907 | End of epoch 905 | At 905->906 boundary |
| Epoch 908 | End of epoch 906 | At 906->907 boundary |

**Why N-2?** Allows validators time to:
1. Compute the schedule
2. Distribute it via gossip
3. Prepare for their leader slots

## EpochStakes Storage

Stakes are stored under the epoch they're **effective for**, not when they're frozen:

```go
// At 905->906 boundary:
leaderScheduleEpoch := epochSchedule.LeaderScheduleEpoch(block.Slot)  // Returns 907
// Stakes are cached as EpochStakes(907)
```

## Schedule Generation

### From Snapshot (`pkg/replay/leader_schedule_local.go:1461`)

```go
func PrepareLeaderScheduleLocal(epoch uint64, ...) {
    voteAcctStakes := global.EpochStakes(epoch)      // Stakes
    voteAcctMap := global.EpochStakesVoteAccts(epoch) // NodePubkeys
    schedule := buildLocalLeaderSchedule(epoch, epochSchedule, voteAcctStakes, voteAcctMap)
}
```

### At Epoch Boundary (`pkg/replay/leader_schedule_local.go:1549`)

```go
func PrepareLeaderScheduleLocalFromVoteCache(epoch uint64, ...) {
    voteAcctStakes := global.EpochStakes(epoch)
    // BUG: Uses global.VoteCache() instead of EpochStakesVoteAccts
    schedule := buildLocalLeaderScheduleFromVoteCache(epoch, epochSchedule, voteAcctStakes)
}
```

**Issue #3**: This uses the wrong VoteCache for NodePubkey lookups.

## Schedule Algorithm

1. **Filter stakes**: Remove zero-stake and missing-NodePubkey entries
2. **Weight by stake**: Higher stake = more leader slots
3. **Deterministic RNG**: Seeded with epoch number
4. **Assign slots**: 4 consecutive slots per leader assignment

```go
ls := leaderschedule.New(
    epochVoteAccts,      // NodePubkey mapping
    filteredStakes,      // Stake weights
    epochSchedule,
    epoch,
    slotsInEpoch,
    NumConsecutiveLeaderSlots,  // 4
)
```

## Validation

Mithril can validate against RPC schedule:

```go
// pkg/replay/leader_schedule_local.go:1219
func validateLeaderSchedule(blockEpoch uint64, ..., rpcSchedule *leaderschedule.LeaderSchedule, ...) {
    localSchedule := buildLocalLeaderSchedule(...)
    // Compare slot by slot
}
```

## Common Divergence Causes

1. **Using wrong epoch stakes** - N instead of N-2
2. **VoteCache timing** - Using current NodePubkeys instead of frozen ones
3. **Stake cache staleness** - EpochStakes computed from stale data
4. **Missing vote accounts** - NodePubkey lookup fails

## Code References

| Component | File | Function |
|-----------|------|----------|
| Schedule struct | `pkg/leaderschedule/leader_schedule.go` | `LeaderSchedule` |
| Generation | `pkg/replay/leader_schedule_local.go:1461` | `PrepareLeaderScheduleLocal()` |
| Boundary generation | `pkg/replay/leader_schedule_local.go:1549` | `PrepareLeaderScheduleLocalFromVoteCache()` |
| Lookup | `pkg/global/global_ctx.go` | `LeaderForSlot()` |
