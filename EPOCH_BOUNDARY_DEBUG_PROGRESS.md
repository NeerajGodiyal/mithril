# Epoch Boundary Debug Progress

**Branch:** `feature/local-rewards-partitions`
**Issue:** Bank hash mismatch at epoch 907→908 boundary, partition 11

---

## Verified Matching (Mithril = Mainnet/FD/Agave)

| Component | Status | Notes |
|-----------|--------|-------|
| Vote rewards | MATCH | Vote reward distribution matches RPC |
| Leader schedule | MATCH | Matches RPC leader schedule |
| Number of partitions | MATCH | Partition count matches mainnet |
| Total staking rewards | MATCH | Total lamports matches |
| Partition blockhash | MATCH | Using parent blockhash correctly (prevSlotCtx.Blockhash) |
| Partition assignment hash | MATCH | SipHash13 with correct blockhash |
| ForceCreditsUpdate logic | MATCH | Reset to 0 when creditsInVote < creditsInStake (per FD/Agave) |

---

## Test Results

### Commit `5f13cebc` (old working version)

| Test | Mode | Result | Notes |
|------|------|--------|-------|
| Epoch 905→906 | Snapshot straight through boundary | PASS | Ran for a few thousand slots after crossing |
| Epoch 905→906 | Stop/resume mid-distribution | CRASH | Failed on resume |
| Epoch 906→907 | Snapshot straight through boundary | CRASH | Failed ~147 slots in |

### Commit `ca9e8a9` (before maxEpoch test)

| Test | Mode | Result | Notes |
|------|------|--------|-------|
| Epoch 907→908 | Snapshot straight through boundary | FAIL | Bank hash mismatch at partition 11 |

### Commit `3d4e8b7` (maxEpoch filtering disabled)

| Test | Mode | Result | Notes |
|------|------|--------|-------|
| Epoch 907→908 | Snapshot straight through boundary | FAIL | maxEpoch=nil didn't help. Diagnosed: 95 extra ForceCreditsUpdate accounts in local P11 |

### Current Test (ForceCreditsUpdate exclusion)

| Test | Mode | Result | Notes |
|------|------|--------|-------|
| Epoch 907→908 | Snapshot straight through boundary | PENDING | Excluding ForceCreditsUpdate accounts from partitions |

**Changes made:**
- `pkg/rewards/rewards.go:2982-2996` - Partition assignment now requires `hasPoints` only (not `|| needsCreditsUpdate`)
- `pkg/rewards/rewards.go:3118-3130` - Same change in sequential fallback path
- Clear comments added indicating this is EXPERIMENTAL and may need to be reverted

---

## Hypotheses

### Tested

| Hypothesis | Result | Notes |
|------------|--------|-------|
| maxEpoch filtering causes issue | RULED OUT | Disabling had no effect; 95 extra accounts still appeared |

### Pending

| Hypothesis | Priority | Rationale |
|------------|----------|-----------|
| **ForceCreditsUpdate accounts shouldn't be in partitions** | HIGH | RPC shows 95 fewer accounts in P11, all are ForceCreditsUpdate with 0 lamports |
| LtHash computation difference | MEDIUM | Could cause bank hash mismatch even with correct rewards |
| Something specific to epoch 907→908 data | MEDIUM | 5f13cebc also failed at 906→907, maybe epoch-specific |

---

## Key Observations

1. **5f13cebc is NOT fully reliable** - It crossed 905→906 but crashed at 906→907 (~147 slots in). So there may be epoch-specific issues, not just commit regressions.

2. **ForceCreditsUpdate reset-to-0 is CORRECT** per Firedancer/Agave - when vote has 0 epoch_credits, stake's credits_observed should be reset to 0

3. **VoteCache at boundary is clean** - In normal replay (no crash/resume), VoteCache has correct end-of-epoch state with no future epoch credits

4. **maxEpoch filtering is a no-op** - CONFIRMED: At normal boundary, there are no epoch 908 credits to filter, so filtering doesn't change anything

5. **Stop/resume is broken** - 5f13cebc crashed on stop/resume for 905→906

6. **P11 account count mismatch diagnosed** - `local=4072 rpc=3977` (95 extra accounts, all ForceCreditsUpdate with lamports=0)

7. **Reward totals match perfectly** - `local_total=152105939008 rpc_total=152105939008 diff=0`

---

## Commits Between Working and Current

85 commits between `5f13cebc` and current HEAD. Key suspects:

| Commit | Description | Potential Impact |
|--------|-------------|------------------|
| `e3a10ad` | Remove RPC dependency for vote rewards | Changed order: staking rewards calculated BEFORE vote rewards distributed |
| `89a8197` | Fix combined refresh: stale credits and partition assignment | Added ForceCreditsUpdateWithSkippedReward accounts to partitions |
| `350b38a` | Read from AccountsDB at distribution time | Changed from cached snapshots to current state |

---

## Files of Interest

| File | Purpose |
|------|---------|
| `pkg/replay/rewards.go:76` | Where maxEpoch is passed to CombinedRefreshPointsAndPartitions |
| `pkg/rewards/rewards.go` | calculateStakePointsAndCredits with maxEpoch filtering |
| `pkg/replay/epoch_diagnostics.go` | Diagnostic output for partition comparisons |

---

## Diagnostic Output Files

Check these in the diagnostics directory when running:
- `partition_comparison_slot_392256011_p11.json` - Local vs RPC comparison for partition 11
- `partition_diff_slot_392256011_p11.json` - State differences for partition 11

---

## Next Steps

1. **Run current test** - Excluding ForceCreditsUpdate accounts from partitions
   - If PASS: Confirms mainnet doesn't process these accounts
   - If FAIL: Mainnet DOES process them, need different fix

2. If test fails, investigate:
   - Are the 95 accounts being MODIFIED differently by mainnet vs Mithril?
   - LtHash computation differences
   - Check if Agave's partition assignment differs from our understanding
