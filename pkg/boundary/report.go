package boundary

import "sync/atomic"

// Level represents the verbosity level for boundary reports
type Level int

const (
	LevelOff     Level = iota // Disabled
	LevelSummary              // One-line summary only
	LevelCompare              // + comparison data
	LevelDebug                // + full partition details
)

// configuredLevel holds the globally configured boundary logging level
var configuredLevel atomic.Int32

func init() {
	// Default to compare level
	configuredLevel.Store(int32(LevelCompare))
}

// SetLevel sets the global boundary logging level
func SetLevel(level Level) {
	configuredLevel.Store(int32(level))
}

// GetLevel returns the currently configured boundary logging level
func GetLevel() Level {
	return Level(configuredLevel.Load())
}

// ParseLevel converts a string level to Level
func ParseLevel(s string) Level {
	switch s {
	case "off", "":
		return LevelOff
	case "summary":
		return LevelSummary
	case "compare":
		return LevelCompare
	case "debug":
		return LevelDebug
	default:
		return LevelCompare // default to compare
	}
}

// IsEnabled returns true if boundary logging is enabled
func IsEnabled() bool {
	return GetLevel() != LevelOff
}

// String returns the level name for filenames
func (l Level) String() string {
	switch l {
	case LevelSummary:
		return "summary"
	case LevelCompare:
		return "compare"
	case LevelDebug:
		return "debug"
	default:
		return "unknown"
	}
}

// BoundaryReport is the main structure written as JSON at epoch boundaries
type BoundaryReport struct {
	Header     HeaderSection      `json:"header"`
	Inputs     InputsSection      `json:"inputs"`
	Schedule   ScheduleSection    `json:"leader_schedule"`
	Rewards    RewardsSection     `json:"rewards"`
	Sysvars    SysvarsSection     `json:"sysvars"`
	Partitions *PartitionsSection `json:"partitions,omitempty"` // debug level only
}

// HeaderSection contains metadata about the report
type HeaderSection struct {
	Source          string `json:"source"`            // "LOCAL" or "RPC"
	Epoch           uint64 `json:"epoch"`             // The epoch being entered
	PrevEpoch       uint64 `json:"prev_epoch"`        // The epoch being left
	BoundarySlot    uint64 `json:"boundary_slot"`     // Last slot of prev epoch
	FirstRewardSlot uint64 `json:"first_reward_slot"` // First slot where stake rewards are distributed
	Timestamp       string `json:"timestamp"`         // When this report was generated
	GitCommit       string `json:"git_commit"`        // Mithril version
	RunID           string `json:"run_id"`            // Unique run identifier
}

// InputsSection contains the inputs used for rewards calculation
type InputsSection struct {
	Capitalization   uint64  `json:"capitalization"`      // Total lamports in circulation
	InflationRate    float64 `json:"inflation_rate"`      // Inflation rate for this epoch
	ValidatorRewards float64 `json:"validator_rewards"`   // Validator portion of inflation
	TotalStaked      uint64  `json:"total_staked"`        // Total lamports staked
	NumStakeAccounts int     `json:"num_stake_accounts"`  // Total stake accounts in cache
	NumVoteAccounts  int     `json:"num_vote_accounts"`   // Total vote accounts receiving rewards
	ParentBlockhash  string  `json:"parent_blockhash"`    // Used for partition hashing
}

// ScheduleSection contains leader schedule information
type ScheduleSection struct {
	Source              string `json:"source"`               // "snapshot", "local", or "rpc"
	LeaderScheduleEpoch uint64 `json:"leader_schedule_epoch"` // Epoch the schedule is for (N+1)
	StakeEpoch          uint64 `json:"stake_epoch"`          // Epoch stakes are from (N-1 = N+1-2)
	NumValidators       int    `json:"num_validators"`       // Number of validators in schedule
	TotalStake          uint64 `json:"total_stake"`          // Total stake weight
}

// RewardsSection contains rewards computation results
type RewardsSection struct {
	TotalPoints            string `json:"total_points"`              // wide.Uint128 as string (too large for JSON number)
	TotalStakingRewards    uint64 `json:"total_staking_rewards"`     // Total lamports for stake rewards
	TotalVoteRewards       uint64 `json:"total_vote_rewards"`        // Total lamports distributed to vote accounts
	NumPartitions          int    `json:"num_partitions"`            // Number of stake reward distribution blocks
	NumEligibleStakeAccts  int    `json:"num_eligible_stake_accounts"` // Accounts with points > 0
	NumForceCreditsUpdate  int    `json:"num_force_credits_update"`  // Accounts with creditsInVote < creditsInStake
	NumZeroPoints          int    `json:"num_zero_points"`           // Accounts with zero points (excluded from rewards)
	NumZeroReward          int    `json:"num_zero_reward"`           // Accounts where calculated reward was 0
}

// SysvarsSection contains EpochRewards sysvar state
type SysvarsSection struct {
	EpochRewardsActive      bool   `json:"epoch_rewards_active"`       // Is rewards distribution in progress
	DistributionStartHeight uint64 `json:"distribution_start_height"`  // Block height where stake distribution starts
	NumPartitions           uint64 `json:"num_partitions"`             // Number of partitions (from sysvar)
	TotalRewards            uint64 `json:"total_rewards"`              // Total rewards (from sysvar)
	DistributedRewards      uint64 `json:"distributed_rewards"`        // Rewards distributed so far
	ParentBlockhash         string `json:"parent_blockhash"`           // Parent blockhash (from sysvar)
}

// PartitionsSection contains per-partition details (debug level only)
type PartitionsSection struct {
	PartitionCounts []int `json:"partition_counts"` // Number of accounts per partition
}

// ComparisonReport documents differences between LOCAL and RPC calculations
type ComparisonReport struct {
	Header      HeaderSection `json:"header"`
	Matches     bool          `json:"matches"`
	Differences []Difference  `json:"differences,omitempty"`
}

// Difference represents a single field that differs between LOCAL and RPC
type Difference struct {
	Field    string `json:"field"`
	LocalVal string `json:"local"`
	RPCVal   string `json:"rpc"`
}
