# Epoch Boundary Logging Consolidation Plan

**Purpose:** Consolidate scattered epoch boundary diagnostic logging into a structured BoundaryReport with explicit LOCAL/RPC/COMPARE labeling.

**Key Principle:** Every LOCAL field has a corresponding RPC field (even if `null` / `not_fetched`). This makes it obvious what data we have vs don't have, and clearly indicates when RPC data is "unanchored" (fetched at a different slot than local computation).

---

## 1. Canonical JSON Schema

Output file: `epoch_diagnostics/boundary_report_{slot}.json`

```json
{
  "schema_version": "epoch_boundary_v1",
  "header": {
    "LOCAL": {
      "slot": 392256001,
      "boundary_slot": 392256000,
      "first_new_epoch_slot": 392256001,
      "epoch_prev": 907,
      "epoch_new": 908,
      "block_height": 12345678
    },
    "RPC": {
      "snapshot_time_utc": "2026-01-09T21:08:42Z",
      "clock_slot": 392299291,
      "slot_hashes_newest_slot": 392299290,
      "source": "getBlock|getAccountInfo|sysvar",
      "unanchored": true
    }
  },
  "inputs": {
    "LOCAL": {
      "stake_cache": {
        "total_delegations": 1040000,
        "total_stake_lamports": 500000000000000,
        "total_credits": 0,
        "removed": {
          "tombstone": 0,
          "not_stake": 0,
          "unmarshal_err": 0,
          "not_found": 0
        }
      },
      "vote_cache": {
        "vote_accounts": 1423,
        "total_stake_lamports": 500000000000000,
        "total_credits": 0,
        "missing_in_cache": 0,
        "missing_in_accountsdb": 0,
        "credits_mismatch": 0
      },
      "features": {
        "partitioned_rewards_enabled": true,
        "stake_min_delegation_active": true,
        "stake_raise_min_1sol_active": true
      }
    },
    "RPC": {
      "vote_accounts": {
        "current": 0,
        "delinquent": 0,
        "total": 0,
        "total_stake_lamports": 0,
        "unanchored": true,
        "not_fetched": true
      }
    }
  },
  "leader_schedule": {
    "LOCAL": {
      "validators": 1423,
      "total_stake_lamports": 500000000000000,
      "hash": "abc123..."
    },
    "RPC": {
      "hash": "abc123...",
      "unanchored": true,
      "not_fetched": false
    },
    "COMPARE": {
      "match": true,
      "details_file": "leader_schedule_mismatch.json"
    }
  },
  "rewards": {
    "LOCAL": {
      "num_partitions": 254,
      "first_reward_slot": 392256001,
      "last_reward_slot": 392256254,
      "eligible_accounts": 1040000,
      "total_points": "0x123456789abcdef",
      "total_rewards": 137738298524815,
      "voter_rewards_total": 35388011402096,
      "staker_rewards_total": 102350287122719,
      "rounding_loss": 0
    },
    "RPC": {
      "num_partitions": 254,
      "vote_rewards_total": 35388011402096,
      "unanchored": false,
      "source": "getBlock"
    },
    "COMPARE": {
      "num_partitions_match": true,
      "vote_rewards_diff": 0
    }
  },
  "epoch_rewards_sysvar": {
    "LOCAL": {
      "active": true,
      "distribution_starting_block_height": 12345678,
      "num_partitions": 254,
      "total_rewards": 137738298524815,
      "total_points": "0x123456789abcdef",
      "distributed_rewards": 0,
      "parent_blockhash": "abc123..."
    },
    "RPC": {
      "active": null,
      "distribution_starting_block_height": null,
      "num_partitions": null,
      "total_rewards": null,
      "total_points": null,
      "distributed_rewards": null,
      "parent_blockhash": null,
      "unanchored": true,
      "not_fetched": true
    },
    "COMPARE": {
      "match": null,
      "not_comparable": true
    }
  },
  "sysvars": {
    "clock": {
      "LOCAL": { "slot": 392256001, "epoch": 908, "leader_schedule_epoch": 909, "unix_timestamp": 1736456922 },
      "RPC":   { "slot": null, "epoch": null, "leader_schedule_epoch": null, "unix_timestamp": null, "unanchored": true, "not_fetched": true }
    },
    "slot_hashes": {
      "LOCAL": { "newest_slot": 392256000, "newest_hash": "abc123..." },
      "RPC":   { "newest_slot": null, "newest_hash": null, "unanchored": true, "not_fetched": true }
    },
    "stake_history": {
      "LOCAL": { "latest_epoch": 907, "effective": 500000000000000, "activating": 0, "deactivating": 0 },
      "RPC":   { "latest_epoch": null, "effective": null, "activating": null, "deactivating": null, "unanchored": true, "not_fetched": true }
    }
  },
  "partitions": {
    "summary": [
      {
        "partition_idx": 1,
        "slot": 392256001,
        "local_count": 4096,
        "local_lamports": 123456789,
        "burned_lamports": 0,
        "local_only_count": 96,
        "rpc_count": 4000,
        "rpc_lamports": 123456789,
        "COMPARE": { "diff_lamports": 0, "diff_count": 96 }
      }
    ],
    "debug_files": {
      "staking_rewards": ["staking_rewards_slot_392256001_p1.json"],
      "partition_details": ["partition_details_slot_392256001_p1.json"],
      "modified_accounts": ["slot_modified_accounts_392256001_p1.json"]
    }
  },
  "files": {
    "local_sysvar_json": "local_slot_392256001.json",
    "rpc_sysvar_json": "rpc_slot_392256001.json",
    "comparison_txt": "comparison_slot_392256001.txt"
  }
}
```

---

## 2. Logging Levels

Config option: `epoch_boundary_log_level = "summary" | "compare" | "debug"`

| Level | Console Output | JSON Files Written |
|-------|----------------|-------------------|
| **summary** | One-line per phase + final summary | `boundary_report_{slot}.json` (header + summary only) |
| **compare** | + LOCAL vs RPC totals | + full comparison sections |
| **debug** | + per-partition account lists | + all detail files (partition_details, stake_comparison, etc.) |

---

## 3. Go Struct Definitions

```go
// LogLevel controls verbosity of epoch boundary diagnostics
type LogLevel int

const (
    LogLevelSummary LogLevel = iota // Minimal console output, just boundary_report.json
    LogLevelCompare                  // + LOCAL vs RPC comparison details
    LogLevelDebug                    // + per-partition account lists, all detail files
)

// BoundaryReport is the main container for epoch boundary diagnostics
type BoundaryReport struct {
    SchemaVersion string                 `json:"schema_version"`
    Header        HeaderSection          `json:"header"`
    Inputs        InputsSection          `json:"inputs"`
    LeaderSchedule LeaderScheduleSection `json:"leader_schedule"`
    Rewards       RewardsSection         `json:"rewards"`
    EpochRewardsSysvar EpochRewardsSysvarSection `json:"epoch_rewards_sysvar"`
    Sysvars       SysvarsSection         `json:"sysvars"`
    Partitions    PartitionsSection      `json:"partitions"`
    Files         FilesSection           `json:"files"`

    // Internal (not serialized)
    level         LogLevel
    outputDir     string
    mu            sync.Mutex
}

// HeaderSection contains slot/epoch context for LOCAL and RPC
type HeaderSection struct {
    LOCAL HeaderLocal `json:"LOCAL"`
    RPC   HeaderRpc   `json:"RPC"`
}

type HeaderLocal struct {
    Slot              uint64 `json:"slot"`
    BoundarySlot      uint64 `json:"boundary_slot"`
    FirstNewEpochSlot uint64 `json:"first_new_epoch_slot"`
    EpochPrev         uint64 `json:"epoch_prev"`
    EpochNew          uint64 `json:"epoch_new"`
    BlockHeight       uint64 `json:"block_height"`
}

type HeaderRpc struct {
    SnapshotTimeUtc      string `json:"snapshot_time_utc"`
    ClockSlot            uint64 `json:"clock_slot"`
    SlotHashesNewestSlot uint64 `json:"slot_hashes_newest_slot"`
    Source               string `json:"source"`
    Unanchored           bool   `json:"unanchored"`
}

// InputsSection contains stake/vote cache stats
type InputsSection struct {
    LOCAL InputsLocal `json:"LOCAL"`
    RPC   InputsRpc   `json:"RPC"`
}

type InputsLocal struct {
    StakeCache StakeCacheStats `json:"stake_cache"`
    VoteCache  VoteCacheStats  `json:"vote_cache"`
    Features   FeaturesInfo    `json:"features"`
}

type StakeCacheStats struct {
    TotalDelegations    int    `json:"total_delegations"`
    TotalStakeLamports  uint64 `json:"total_stake_lamports"`
    TotalCredits        uint64 `json:"total_credits"`
    Removed             RemovedStats `json:"removed"`
}

type RemovedStats struct {
    Tombstone    int `json:"tombstone"`
    NotStake     int `json:"not_stake"`
    UnmarshalErr int `json:"unmarshal_err"`
    NotFound     int `json:"not_found"`
}

type VoteCacheStats struct {
    VoteAccounts       int    `json:"vote_accounts"`
    TotalStakeLamports uint64 `json:"total_stake_lamports"`
    TotalCredits       uint64 `json:"total_credits"`
    MissingInCache     int    `json:"missing_in_cache"`
    MissingInAccountsdb int   `json:"missing_in_accountsdb"`
    CreditsMismatch    int    `json:"credits_mismatch"`
}

type FeaturesInfo struct {
    PartitionedRewardsEnabled  bool `json:"partitioned_rewards_enabled"`
    StakeMinDelegationActive   bool `json:"stake_min_delegation_active"`
    StakeRaiseMin1solActive    bool `json:"stake_raise_min_1sol_active"`
}

type InputsRpc struct {
    VoteAccounts VoteAccountsRpc `json:"vote_accounts"`
}

type VoteAccountsRpc struct {
    Current           int    `json:"current"`
    Delinquent        int    `json:"delinquent"`
    Total             int    `json:"total"`
    TotalStakeLamports uint64 `json:"total_stake_lamports"`
    Unanchored        bool   `json:"unanchored"`
    NotFetched        bool   `json:"not_fetched"`
}

// LeaderScheduleSection contains schedule comparison
type LeaderScheduleSection struct {
    LOCAL   LeaderScheduleLocal   `json:"LOCAL"`
    RPC     LeaderScheduleRpc     `json:"RPC"`
    COMPARE LeaderScheduleCompare `json:"COMPARE"`
}

type LeaderScheduleLocal struct {
    Validators         int    `json:"validators"`
    TotalStakeLamports uint64 `json:"total_stake_lamports"`
    Hash               string `json:"hash"`
}

type LeaderScheduleRpc struct {
    Hash       string `json:"hash"`
    Unanchored bool   `json:"unanchored"`
    NotFetched bool   `json:"not_fetched"`
}

type LeaderScheduleCompare struct {
    Match       bool   `json:"match"`
    DetailsFile string `json:"details_file,omitempty"`
}

// RewardsSection contains rewards totals comparison
type RewardsSection struct {
    LOCAL   RewardsLocal   `json:"LOCAL"`
    RPC     RewardsRpc     `json:"RPC"`
    COMPARE RewardsCompare `json:"COMPARE"`
}

type RewardsLocal struct {
    NumPartitions      uint64 `json:"num_partitions"`
    FirstRewardSlot    uint64 `json:"first_reward_slot"`
    LastRewardSlot     uint64 `json:"last_reward_slot"`
    EligibleAccounts   int    `json:"eligible_accounts"`
    TotalPoints        string `json:"total_points"` // hex string for uint128
    TotalRewards       uint64 `json:"total_rewards"`
    VoterRewardsTotal  uint64 `json:"voter_rewards_total"`
    StakerRewardsTotal uint64 `json:"staker_rewards_total"`
    RoundingLoss       uint64 `json:"rounding_loss"`
}

type RewardsRpc struct {
    NumPartitions     uint64 `json:"num_partitions"`
    VoteRewardsTotal  uint64 `json:"vote_rewards_total"`
    Unanchored        bool   `json:"unanchored"`
    Source            string `json:"source"`
}

type RewardsCompare struct {
    NumPartitionsMatch bool  `json:"num_partitions_match"`
    VoteRewardsDiff    int64 `json:"vote_rewards_diff"`
}

// EpochRewardsSysvarSection contains sysvar comparison
type EpochRewardsSysvarSection struct {
    LOCAL   EpochRewardsSysvarLocal   `json:"LOCAL"`
    RPC     EpochRewardsSysvarRpc     `json:"RPC"`
    COMPARE EpochRewardsSysvarCompare `json:"COMPARE"`
}

type EpochRewardsSysvarLocal struct {
    Active                       bool   `json:"active"`
    DistributionStartingBlockHeight uint64 `json:"distribution_starting_block_height"`
    NumPartitions                uint64 `json:"num_partitions"`
    TotalRewards                 uint64 `json:"total_rewards"`
    TotalPoints                  string `json:"total_points"` // hex for uint128
    DistributedRewards           uint64 `json:"distributed_rewards"`
    ParentBlockhash              string `json:"parent_blockhash"`
}

type EpochRewardsSysvarRpc struct {
    Active                       *bool   `json:"active"`
    DistributionStartingBlockHeight *uint64 `json:"distribution_starting_block_height"`
    NumPartitions                *uint64 `json:"num_partitions"`
    TotalRewards                 *uint64 `json:"total_rewards"`
    TotalPoints                  *string `json:"total_points"`
    DistributedRewards           *uint64 `json:"distributed_rewards"`
    ParentBlockhash              *string `json:"parent_blockhash"`
    Unanchored                   bool    `json:"unanchored"`
    NotFetched                   bool    `json:"not_fetched"`
}

type EpochRewardsSysvarCompare struct {
    Match         *bool `json:"match"`
    NotComparable bool  `json:"not_comparable"`
}

// SysvarsSection contains clock, slot_hashes, stake_history
type SysvarsSection struct {
    Clock        SysvarPair `json:"clock"`
    SlotHashes   SysvarPair `json:"slot_hashes"`
    StakeHistory SysvarPair `json:"stake_history"`
}

type SysvarPair struct {
    LOCAL interface{} `json:"LOCAL"`
    RPC   interface{} `json:"RPC"`
}

type ClockLocal struct {
    Slot               uint64 `json:"slot"`
    Epoch              uint64 `json:"epoch"`
    LeaderScheduleEpoch uint64 `json:"leader_schedule_epoch"`
    UnixTimestamp      int64  `json:"unix_timestamp"`
}

type ClockRpc struct {
    Slot               *uint64 `json:"slot"`
    Epoch              *uint64 `json:"epoch"`
    LeaderScheduleEpoch *uint64 `json:"leader_schedule_epoch"`
    UnixTimestamp      *int64  `json:"unix_timestamp"`
    Unanchored         bool    `json:"unanchored"`
    NotFetched         bool    `json:"not_fetched"`
}

type SlotHashesLocal struct {
    NewestSlot uint64 `json:"newest_slot"`
    NewestHash string `json:"newest_hash"`
}

type SlotHashesRpc struct {
    NewestSlot *uint64 `json:"newest_slot"`
    NewestHash *string `json:"newest_hash"`
    Unanchored bool    `json:"unanchored"`
    NotFetched bool    `json:"not_fetched"`
}

type StakeHistoryLocal struct {
    LatestEpoch  uint64 `json:"latest_epoch"`
    Effective    uint64 `json:"effective"`
    Activating   uint64 `json:"activating"`
    Deactivating uint64 `json:"deactivating"`
}

type StakeHistoryRpc struct {
    LatestEpoch  *uint64 `json:"latest_epoch"`
    Effective    *uint64 `json:"effective"`
    Activating   *uint64 `json:"activating"`
    Deactivating *uint64 `json:"deactivating"`
    Unanchored   bool    `json:"unanchored"`
    NotFetched   bool    `json:"not_fetched"`
}

// PartitionsSection contains per-partition summaries
type PartitionsSection struct {
    Summary    []PartitionSummary `json:"summary"`
    DebugFiles DebugFiles         `json:"debug_files"`
}

type PartitionSummary struct {
    PartitionIdx    uint64           `json:"partition_idx"`
    Slot            uint64           `json:"slot"`
    LocalCount      int              `json:"local_count"`
    LocalLamports   uint64           `json:"local_lamports"`
    BurnedLamports  uint64           `json:"burned_lamports"`
    LocalOnlyCount  int              `json:"local_only_count"`
    RpcCount        int              `json:"rpc_count"`
    RpcLamports     uint64           `json:"rpc_lamports"`
    COMPARE         PartitionCompare `json:"COMPARE"`
}

type PartitionCompare struct {
    DiffLamports int64 `json:"diff_lamports"`
    DiffCount    int   `json:"diff_count"`
}

type DebugFiles struct {
    StakingRewards   []string `json:"staking_rewards"`
    PartitionDetails []string `json:"partition_details"`
    ModifiedAccounts []string `json:"modified_accounts"`
}

// FilesSection references generated detail files
type FilesSection struct {
    LocalSysvarJson  string `json:"local_sysvar_json,omitempty"`
    RpcSysvarJson    string `json:"rpc_sysvar_json,omitempty"`
    ComparisonTxt    string `json:"comparison_txt,omitempty"`
}

// ForceCreditsBreakdown tracks accounts with special credits handling
type ForceCreditsBreakdown struct {
    TotalAccounts      int `json:"total_accounts"`
    WithReward         int `json:"with_reward"`
    NoReward           int `json:"no_reward"`           // ForceCreditsUpdate=true
    CreditsBackward    int `json:"credits_backward"`    // CreditsObserved > NewCreditsObserved
    ActivationEpoch    int `json:"activation_epoch"`    // Stake activated this epoch
    TotalRewardsZero   int `json:"total_rewards_zero"`  // Points > 0 but reward rounded to 0
}
```

---

## 4. Report Methods API

```go
// Constructor
func NewBoundaryReport(boundarySlot, firstNewSlot, epochPrev, epochNew, blockHeight uint64, level LogLevel, outputDir string) *BoundaryReport

// Header methods
func (r *BoundaryReport) SetRpcContext(clockSlot, slotHashesNewest uint64, source string, unanchored bool)

// Inputs methods
func (r *BoundaryReport) SetStakeCacheStats(stats StakeCacheStats)
func (r *BoundaryReport) SetVoteCacheStats(stats VoteCacheStats)
func (r *BoundaryReport) SetFeatures(features FeaturesInfo)
func (r *BoundaryReport) SetRpcVoteAccounts(stats VoteAccountsRpc)

// Leader schedule methods
func (r *BoundaryReport) SetLeaderScheduleLocal(validators int, totalStake uint64, hash string)
func (r *BoundaryReport) SetLeaderScheduleRpc(hash string, unanchored, notFetched bool)
func (r *BoundaryReport) SetLeaderScheduleCompare(match bool, detailsFile string)

// Rewards methods
func (r *BoundaryReport) SetRewardsLocal(local RewardsLocal)
func (r *BoundaryReport) SetRewardsRpc(rpc RewardsRpc)
func (r *BoundaryReport) SetRewardsCompare(compare RewardsCompare)

// Partition methods (called once per partition during distribution)
func (r *BoundaryReport) AddPartition(summary PartitionSummary)
func (r *BoundaryReport) AddDebugFile(category string, filename string) // category: "staking_rewards", "partition_details", "modified_accounts"

// EpochRewardsSysvar methods
func (r *BoundaryReport) SetEpochRewardsSysvarLocal(sysvar EpochRewardsSysvarLocal)
func (r *BoundaryReport) SetEpochRewardsSysvarRpc(sysvar EpochRewardsSysvarRpc)
func (r *BoundaryReport) SetEpochRewardsSysvarCompare(compare EpochRewardsSysvarCompare)

// Sysvars methods
func (r *BoundaryReport) SetClockLocal(slot, epoch, leaderScheduleEpoch uint64, unixTimestamp int64)
func (r *BoundaryReport) SetClockRpc(slot, epoch, leaderScheduleEpoch *uint64, unixTimestamp *int64, unanchored, notFetched bool)
func (r *BoundaryReport) SetSlotHashesLocal(newestSlot uint64, newestHash string)
func (r *BoundaryReport) SetStakeHistoryLocal(latestEpoch, effective, activating, deactivating uint64)

// ForceCreditsUpdate tracking (debug level)
func (r *BoundaryReport) AddForceCreditsBreakdown(partitionIdx uint64, breakdown ForceCreditsBreakdown)

// Finalize writes JSON report and emits console summary
func (r *BoundaryReport) Finalize() error

// Console output helpers (called by Finalize based on level)
func (r *BoundaryReport) printSummary()    // Always printed
func (r *BoundaryReport) printCompare()    // level >= LogLevelCompare
func (r *BoundaryReport) printDebug()      // level >= LogLevelDebug
```

---

## 5. Console Output Formats

**Summary Level** (one-line per phase):
```
===============================================================================
                    EPOCH BOUNDARY: 907 -> 908
===============================================================================
LOCAL  boundary=392256000 first_new=392256001 block_height=12345678
RPC    clock=392299291 slot_hashes=392299290 (UNANCHORED - fetched at later slot)
-------------------------------------------------------------------------------
[LEADER]     LOCAL validators=1423 hash=abc1... | RPC hash=abc1... | MATCH
[REWARDS]    LOCAL partitions=254 total=137738298524815 | RPC num=254 | MATCH
[VOTES]      LOCAL total=35388011402096 | RPC total=35388011402096 | MATCH
-------------------------------------------------------------------------------
Report: epoch_diagnostics/boundary_report_392256001.json
```

**Compare Level** (+ LOCAL vs RPC diff per partition):
```
[P1/254]  slot=392256001 LOCAL count=4096 lamports=123456789 | RPC count=4000 | diff=96
[P2/254]  slot=392256002 LOCAL count=4096 lamports=234567890 | RPC count=4096 | MATCH
...
[P11/254] slot=392256011 LOCAL count=4144 lamports=759161486581 | RPC count=4048 | diff=96 !!!
```

**Debug Level** (+ per-partition file references):
```
[P11/254] Writing debug files:
          - staking_rewards_slot_392256011_p11.json (4144 accounts)
          - partition_details_slot_392256011_p11.json (with ForceCredits breakdown)
          - slot_modified_accounts_392256011.json
```

---

## 6. Files to Modify

| File | Changes |
|------|---------|
| `pkg/replay/boundary_report.go` | **NEW** - BoundaryReport struct + methods |
| `pkg/replay/block.go` | Replace scattered mlog calls with `report.Add*()` |
| `pkg/replay/rewards.go` | Replace mlog calls with `report.Add*()` |
| `pkg/replay/epoch_diagnostics.go` | Keep Write* functions, call from report.Finalize() |
| `cmd/mithril/config/config.go` | Add `epoch_boundary_log_level` config |

---

## 7. Current Log Locations to Consolidate

**pkg/replay/block.go** (lines 1645-1850):
- `EPOCH BOUNDARY` banner -> `report.Header`
- `[SLOTS]`, `[EPOCHS]`, `[RPC]` -> `report.Header`
- `[VOTE CACHE]` logs -> `report.AddStakeVoteCacheInputs()`
- `[LEADER SCHEDULE]` logs -> `report.AddLeaderSchedule()`
- `[EPOCH STAKES]` -> `report.Header`

**pkg/replay/rewards.go** (lines 39-766):
- Partition count/init logs -> `report.AddPartitionedRewards()`
- Debug mode banner -> `report.Finalize()` (debug level)
- Per-partition distribution -> `report.AddStakingPartition()`
- Diagnostic comparisons -> `report.AddStakingPartition()` details

**pkg/replay/epoch_diagnostics.go**:
- Keep all `Write*` functions
- Call them from `report.Finalize()` based on log level

---

## 8. Implementation Order

1. **Create `boundary_report.go`** with all struct definitions from section 3
2. **Add config option** in `config.go`
3. **Wire into `block.go`** - create report at boundary start
4. **Modify `rewards.go`** - add report parameter, replace mlog calls
5. **Update `epoch_diagnostics.go`** - integrate with report.Finalize()
6. **Test all three log levels** - verify file output matches matrix

---

## 9. File Output Matrix

| Log Level | Files Generated |
|-----------|-----------------|
| `summary` | `boundary_report_{slot}.json` only |
| `compare` | + `staking_rewards_comparison_{slot}_p{N}.json` for each partition |
| `debug`   | + `partition_details_{slot}_p{N}.json`, `slot_modified_accounts_{slot}.json`, `local_slot_{slot}.json` |

---

## 10. Quick Reference: Where to Find Data

| Data Type | Source | Notes |
|-----------|--------|-------|
| Stake cache stats | `globalCtx.StakeCache.Entries()` | Count, total lamports |
| Vote cache stats | `globalCtx.VoteCache.Entries()` | Validators, credits |
| Leader schedule | `globalCtx.LeaderSchedule.Hash()` | Generated at boundary |
| Partition rewards | `stakingRewards` map | Per-partition from distribution |
| RPC vote accounts | `rpcclient.GetVoteAccounts()` | Unanchored, async fetch |
| RPC block rewards | `rpcclient.GetBlock()` | Vote rewards from block.Rewards |
| EpochRewards sysvar | `slotCtx.EpochRewards` | LOCAL state, RPC needs getAccountInfo |
