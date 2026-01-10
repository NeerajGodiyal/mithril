package boundary

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/mlog"
)

// WriteReport writes a BoundaryReport to the boundary subdirectory as JSON
func WriteReport(report *BoundaryReport, level Level) error {
	// Check if boundary logging is enabled
	configLevel := GetLevel()
	if configLevel == LevelOff {
		return nil // Boundary logging disabled
	}

	// Don't write if requested level is higher than configured
	if level > configLevel {
		return nil
	}

	logDir := mlog.GetLogDir()
	if logDir == "" {
		return nil // No logging configured
	}

	boundaryDir := filepath.Join(logDir, "boundary")
	if err := os.MkdirAll(boundaryDir, 0755); err != nil {
		return fmt.Errorf("failed to create boundary dir: %w", err)
	}

	// Filename: epoch_<epoch>_<source>_<level>.json
	filename := fmt.Sprintf("epoch_%d_%s_%s.json",
		report.Header.Epoch,
		strings.ToLower(report.Header.Source),
		level.String())

	path := filepath.Join(boundaryDir, filename)

	// Marshal with indentation for readability
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal report: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write report: %w", err)
	}

	mlog.Log.FileOnlyf("boundary report written: %s", path)
	return nil
}

// WriteComparison compares LOCAL and RPC reports and writes a comparison file
func WriteComparison(local, rpc *BoundaryReport) error {
	logDir := mlog.GetLogDir()
	if logDir == "" {
		return nil
	}

	comparison := ComparisonReport{
		Header: HeaderSection{
			Source:          "COMPARE",
			Epoch:           local.Header.Epoch,
			PrevEpoch:       local.Header.PrevEpoch,
			BoundarySlot:    local.Header.BoundarySlot,
			FirstRewardSlot: local.Header.FirstRewardSlot,
			Timestamp:       time.Now().UTC().Format(time.RFC3339),
			GitCommit:       local.Header.GitCommit,
			RunID:           local.Header.RunID,
		},
		Matches: true,
	}

	// Compare key fields
	comparison.addDiff("total_points", local.Rewards.TotalPoints, rpc.Rewards.TotalPoints)
	comparison.addDiff("total_staking_rewards", fmt.Sprint(local.Rewards.TotalStakingRewards), fmt.Sprint(rpc.Rewards.TotalStakingRewards))
	comparison.addDiff("total_vote_rewards", fmt.Sprint(local.Rewards.TotalVoteRewards), fmt.Sprint(rpc.Rewards.TotalVoteRewards))
	comparison.addDiff("num_partitions", fmt.Sprint(local.Rewards.NumPartitions), fmt.Sprint(rpc.Rewards.NumPartitions))
	comparison.addDiff("num_eligible_stake_accounts", fmt.Sprint(local.Rewards.NumEligibleStakeAccts), fmt.Sprint(rpc.Rewards.NumEligibleStakeAccts))
	comparison.addDiff("capitalization", fmt.Sprint(local.Inputs.Capitalization), fmt.Sprint(rpc.Inputs.Capitalization))
	comparison.addDiff("parent_blockhash", local.Inputs.ParentBlockhash, rpc.Inputs.ParentBlockhash)

	boundaryDir := filepath.Join(logDir, "boundary")
	if err := os.MkdirAll(boundaryDir, 0755); err != nil {
		return fmt.Errorf("failed to create boundary dir: %w", err)
	}

	path := filepath.Join(boundaryDir, fmt.Sprintf("epoch_%d_comparison.json", local.Header.Epoch))

	data, err := json.MarshalIndent(comparison, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal comparison: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write comparison: %w", err)
	}

	if comparison.Matches {
		mlog.Log.FileOnlyf("boundary comparison: MATCH for epoch %d", local.Header.Epoch)
	} else {
		mlog.Log.Warnf("boundary comparison: MISMATCH for epoch %d (%d differences)", local.Header.Epoch, len(comparison.Differences))
	}

	return nil
}

// addDiff adds a difference if values don't match
func (c *ComparisonReport) addDiff(field, localVal, rpcVal string) {
	if localVal != rpcVal {
		c.Matches = false
		c.Differences = append(c.Differences, Difference{
			Field:    field,
			LocalVal: localVal,
			RPCVal:   rpcVal,
		})
	}
}

// GetBoundaryDir returns the boundary subdirectory path for the current run
func GetBoundaryDir() string {
	logDir := mlog.GetLogDir()
	if logDir == "" {
		return ""
	}
	return filepath.Join(logDir, "boundary")
}
