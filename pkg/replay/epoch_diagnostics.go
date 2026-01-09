package replay

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/base58"
	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
)

// TxRpcMeta contains RPC metadata for a single transaction (used in divergence dumps)
type TxRpcMeta struct {
	Index       int      `json:"index"`
	Signature   string   `json:"signature"`
	IsVote      bool     `json:"is_vote"`
	RpcSuccess  bool     `json:"rpc_success"`
	RpcError    string   `json:"rpc_error,omitempty"`
	RpcCU       uint64   `json:"rpc_cu"`
	RpcFee      uint64   `json:"rpc_fee"`
	PreBalance  []uint64 `json:"pre_balances,omitempty"`
	PostBalance []uint64 `json:"post_balances,omitempty"`
}

// DivergenceDump contains all transaction metadata for a slot where divergence occurred
type DivergenceDump struct {
	Slot               uint64      `json:"slot"`
	Epoch              uint64      `json:"epoch"`
	DivergentTxSig     string      `json:"divergent_tx_sig"`
	DivergentTxError   string      `json:"divergent_tx_error,omitempty"`
	DivergentOnchainOk bool        `json:"divergent_onchain_ok"`
	TotalTransactions  int         `json:"total_transactions"`
	TotalVotes         int         `json:"total_votes"`
	TotalRpcCU         uint64      `json:"total_rpc_cu"`
	TotalRpcFee        uint64      `json:"total_rpc_fee"`
	Transactions       []TxRpcMeta `json:"transactions"`
}

// EpochBoundaryDiagnostics contains state info for debugging epoch transitions
type EpochBoundaryDiagnostics struct {
	Slot        uint64 `json:"slot"`
	Epoch       uint64 `json:"epoch"`
	Source      string `json:"source"` // "local" or "rpc"
	BankHash    string `json:"bank_hash,omitempty"`
	ParentHash  string `json:"parent_hash,omitempty"`
	NumSigs     uint64 `json:"num_signatures,omitempty"`
	BlockHash   string `json:"block_hash,omitempty"`
	LtHash      string `json:"lt_hash,omitempty"`

	// Additional bank hash inputs
	Capitalization uint64 `json:"capitalization,omitempty"`

	// Sysvars
	Clock        *ClockDiag        `json:"clock,omitempty"`
	EpochRewards *EpochRewardsDiag `json:"epoch_rewards,omitempty"`
	SlotHashes   *SlotHashesDiag   `json:"slot_hashes,omitempty"`
	StakeHistory *StakeHistoryDiag `json:"stake_history,omitempty"`

	// Modified accounts (for ltHash debugging)
	ModifiedAccounts *ModifiedAccountsDiag `json:"modified_accounts,omitempty"`
}

type ClockDiag struct {
	Slot                uint64 `json:"slot"`
	EpochStartTimestamp int64  `json:"epoch_start_timestamp"`
	Epoch               uint64 `json:"epoch"`
	LeaderScheduleEpoch uint64 `json:"leader_schedule_epoch"`
	UnixTimestamp       int64  `json:"unix_timestamp"`
}

type EpochRewardsDiag struct {
	DistributionStartingBlockHeight uint64 `json:"distribution_starting_block_height"`
	NumPartitions                   uint64 `json:"num_partitions"`
	ParentBlockhash                 string `json:"parent_blockhash"`
	TotalPoints                     string `json:"total_points"`
	TotalRewards                    uint64 `json:"total_rewards"`
	DistributedRewards              uint64 `json:"distributed_rewards"`
	Active                          bool   `json:"active"`
}

type SlotHashesDiag struct {
	NumEntries  int      `json:"num_entries"`
	NewestSlot  uint64   `json:"newest_slot"`
	NewestHash  string   `json:"newest_hash"`
	OldestSlot  uint64   `json:"oldest_slot"`
	OldestHash  string   `json:"oldest_hash"`
	// Include a few recent entries for comparison
	RecentEntries []SlotHashEntry `json:"recent_entries,omitempty"`
}

type SlotHashEntry struct {
	Slot uint64 `json:"slot"`
	Hash string `json:"hash"`
}

type StakeHistoryDiag struct {
	NumEntries   int                     `json:"num_entries"`
	LatestEpoch  uint64                  `json:"latest_epoch,omitempty"`
	LatestEntry  *StakeHistoryEntryDiag  `json:"latest_entry,omitempty"`
	RecentEpochs []StakeHistoryEntryDiag `json:"recent_epochs,omitempty"`
}

type StakeHistoryEntryDiag struct {
	Epoch        uint64 `json:"epoch"`
	Effective    uint64 `json:"effective"`
	Activating   uint64 `json:"activating"`
	Deactivating uint64 `json:"deactivating"`
}

type ModifiedAccountsDiag struct {
	TotalCount        int                   `json:"total_count"`
	WritableCount     int                   `json:"writable_count"`
	EpochUpdatedCount int                   `json:"epoch_updated_count"`
	SysvarCount       int                   `json:"sysvar_count"`
	Sysvars           []string              `json:"sysvars,omitempty"`           // List of modified sysvar addresses
	EpochUpdated      []string              `json:"epoch_updated,omitempty"`     // First 50 epoch-updated accounts
	TopModified       []ModifiedAccountInfo `json:"top_modified,omitempty"`      // Top 20 by lamport change
}

type ModifiedAccountInfo struct {
	Address       string `json:"address"`
	LamportsBefore uint64 `json:"lamports_before,omitempty"`
	LamportsAfter  uint64 `json:"lamports_after,omitempty"`
	LamportDelta   int64  `json:"lamport_delta,omitempty"`
	Owner          string `json:"owner,omitempty"`
}

// getDiagnosticsDir returns the diagnostics subdirectory within the run's log directory
func getDiagnosticsDir() string {
	baseDir := mlog.GetLogDir()
	if baseDir == "" {
		baseDir = "/mnt/mithril-logs"
	}
	diagDir := filepath.Join(baseDir, "epoch_diagnostics")
	os.MkdirAll(diagDir, 0755)
	return diagDir
}

// DiagnosticParams holds optional parameters for diagnostic dumps
type DiagnosticParams struct {
	Capitalization    uint64
	WritableAccts     []string // pubkey strings
	EpochUpdatedAccts []string // pubkey strings
	ModifiedSysvars   []string // sysvar addresses that were modified
}

// DumpLocalSysvarState dumps the local sysvar state to a JSON file
func DumpLocalSysvarState(slot uint64, epoch uint64, bankHash []byte, parentHash [32]byte, numSigs uint64, blockHash [32]byte, ltHash []byte, params *DiagnosticParams) {
	diag := &EpochBoundaryDiagnostics{
		Slot:       slot,
		Epoch:      epoch,
		Source:     "local",
		BankHash:   base58.Encode(bankHash),
		ParentHash: base58.Encode(parentHash[:]),
		NumSigs:    numSigs,
		BlockHash:  solana.HashFromBytes(blockHash[:]).String(),
	}

	if ltHash != nil {
		// Use hex encoding for ltHash since it's 2048 bytes (base58 would be too long)
		diag.LtHash = hex.EncodeToString(ltHash)
	}

	// Add optional params
	if params != nil {
		diag.Capitalization = params.Capitalization

		// Build modified accounts summary
		modAccts := &ModifiedAccountsDiag{
			WritableCount:     len(params.WritableAccts),
			EpochUpdatedCount: len(params.EpochUpdatedAccts),
			SysvarCount:       len(params.ModifiedSysvars),
			Sysvars:           params.ModifiedSysvars,
		}

		// Include first 50 epoch-updated accounts
		maxEpochUpdated := 50
		if len(params.EpochUpdatedAccts) < maxEpochUpdated {
			maxEpochUpdated = len(params.EpochUpdatedAccts)
		}
		if maxEpochUpdated > 0 {
			modAccts.EpochUpdated = params.EpochUpdatedAccts[:maxEpochUpdated]
		}

		modAccts.TotalCount = modAccts.WritableCount + modAccts.EpochUpdatedCount
		diag.ModifiedAccounts = modAccts
	}

	// Clock sysvar
	if sealevel.SysvarCache.Clock.Sysvar != nil {
		clock := sealevel.SysvarCache.Clock.Sysvar
		diag.Clock = &ClockDiag{
			Slot:                clock.Slot,
			EpochStartTimestamp: clock.EpochStartTimestamp,
			Epoch:               clock.Epoch,
			LeaderScheduleEpoch: clock.LeaderScheduleEpoch,
			UnixTimestamp:       clock.UnixTimestamp,
		}
	}

	// EpochRewards sysvar
	if sealevel.SysvarCache.EpochRewards.Sysvar != nil {
		er := sealevel.SysvarCache.EpochRewards.Sysvar
		diag.EpochRewards = &EpochRewardsDiag{
			DistributionStartingBlockHeight: er.DistributionStartingBlockHeight,
			NumPartitions:                   er.NumPartitions,
			ParentBlockhash:                 solana.HashFromBytes(er.ParentBlockhash[:]).String(),
			TotalPoints:                     er.TotalPoints.String(),
			TotalRewards:                    er.TotalRewards,
			DistributedRewards:              er.DistributedRewards,
			Active:                          er.Active,
		}
	}

	// SlotHashes sysvar
	if sealevel.SysvarCache.SlotHashes.Sysvar != nil {
		sh := *sealevel.SysvarCache.SlotHashes.Sysvar
		shDiag := &SlotHashesDiag{
			NumEntries: len(sh),
		}
		if len(sh) > 0 {
			shDiag.NewestSlot = sh[0].Slot
			shDiag.NewestHash = base58.Encode(sh[0].Hash[:])
			shDiag.OldestSlot = sh[len(sh)-1].Slot
			shDiag.OldestHash = base58.Encode(sh[len(sh)-1].Hash[:])

			// Include 10 most recent entries
			numRecent := 10
			if len(sh) < numRecent {
				numRecent = len(sh)
			}
			for i := 0; i < numRecent; i++ {
				shDiag.RecentEntries = append(shDiag.RecentEntries, SlotHashEntry{
					Slot: sh[i].Slot,
					Hash: base58.Encode(sh[i].Hash[:]),
				})
			}
		}
		diag.SlotHashes = shDiag
	}

	// StakeHistory sysvar
	if sealevel.SysvarCache.StakeHistory.Sysvar != nil {
		history := *sealevel.SysvarCache.StakeHistory.Sysvar
		shDiag := &StakeHistoryDiag{
			NumEntries: len(history),
		}
		if len(history) > 0 {
			// Entries are stored newest first (it's a slice of StakeHistoryPair)
			latest := history[0]
			shDiag.LatestEpoch = latest.Epoch
			shDiag.LatestEntry = &StakeHistoryEntryDiag{
				Epoch:        latest.Epoch,
				Effective:    latest.Entry.Effective,
				Activating:   latest.Entry.Activating,
				Deactivating: latest.Entry.Deactivating,
			}

			// Include 5 most recent epochs
			numRecent := 5
			if len(history) < numRecent {
				numRecent = len(history)
			}
			for i := 0; i < numRecent; i++ {
				entry := history[i]
				shDiag.RecentEpochs = append(shDiag.RecentEpochs, StakeHistoryEntryDiag{
					Epoch:        entry.Epoch,
					Effective:    entry.Entry.Effective,
					Activating:   entry.Entry.Activating,
					Deactivating: entry.Entry.Deactivating,
				})
			}
		}
		diag.StakeHistory = shDiag
	}

	// Write to file
	diagDir := getDiagnosticsDir()
	filename := filepath.Join(diagDir, fmt.Sprintf("local_slot_%d.json", slot))

	data, err := json.MarshalIndent(diag, "", "  ")
	if err != nil {
		mlog.Log.Warnf("failed to marshal local diagnostics for slot %d: %v", slot, err)
		return
	}

	if err := os.WriteFile(filename, data, 0644); err != nil {
		mlog.Log.Warnf("failed to write local diagnostics for slot %d: %v", slot, err)
		return
	}

	mlog.Log.Infof("wrote local sysvar diagnostics to %s", filename)
}

// FetchAndDumpRPCSysvarState fetches sysvar state from RPC and dumps to JSON
func FetchAndDumpRPCSysvarState(rpcc *rpcclient.RpcClient, slot uint64, epoch uint64) {
	if rpcc == nil {
		return
	}

	diag := &EpochBoundaryDiagnostics{
		Slot:   slot,
		Epoch:  epoch,
		Source: "rpc",
	}

	// Fetch bank hash for this slot (from next slot's previousBlockhash)
	expectedBankhash, err := fetchBankhashForSlot(rpcc, slot)
	if err != nil {
		mlog.Log.Warnf("RPC diagnostics: failed to fetch bankhash for slot %d: %v", slot, err)
	} else {
		diag.BankHash = base58.Encode(expectedBankhash)
	}

	// Fetch EpochRewards sysvar
	epochRewardsData, err := rpcc.GetEpochRewardsSysvar()
	if err != nil {
		mlog.Log.Warnf("RPC diagnostics: failed to fetch EpochRewards: %v", err)
	} else if len(epochRewardsData) >= sealevel.SysvarEpochRewardsStructLen {
		var er sealevel.SysvarEpochRewards
		decoder := bin.NewBinDecoder(epochRewardsData)
		if decErr := er.UnmarshalWithDecoder(decoder); decErr == nil {
			diag.EpochRewards = &EpochRewardsDiag{
				DistributionStartingBlockHeight: er.DistributionStartingBlockHeight,
				NumPartitions:                   er.NumPartitions,
				ParentBlockhash:                 solana.HashFromBytes(er.ParentBlockhash[:]).String(),
				TotalPoints:                     er.TotalPoints.String(),
				TotalRewards:                    er.TotalRewards,
				DistributedRewards:              er.DistributedRewards,
				Active:                          er.Active,
			}
		}
	}

	// Fetch Clock sysvar
	clockAddr := solana.MustPublicKeyFromBase58("SysvarC1ock11111111111111111111111111111111")
	clockData, err := fetchAccountData(rpcc, clockAddr)
	if err != nil {
		mlog.Log.Warnf("RPC diagnostics: failed to fetch Clock: %v", err)
	} else if len(clockData) >= 40 {
		var clock sealevel.SysvarClock
		decoder := bin.NewBinDecoder(clockData)
		if decErr := clock.UnmarshalWithDecoder(decoder); decErr == nil {
			diag.Clock = &ClockDiag{
				Slot:                clock.Slot,
				EpochStartTimestamp: clock.EpochStartTimestamp,
				Epoch:               clock.Epoch,
				LeaderScheduleEpoch: clock.LeaderScheduleEpoch,
				UnixTimestamp:       clock.UnixTimestamp,
			}
		}
	}

	// Fetch SlotHashes sysvar
	slotHashesAddr := solana.MustPublicKeyFromBase58("SysvarS1otHashes111111111111111111111111111")
	slotHashesData, err := fetchAccountData(rpcc, slotHashesAddr)
	if err != nil {
		mlog.Log.Warnf("RPC diagnostics: failed to fetch SlotHashes: %v", err)
	} else if len(slotHashesData) > 8 {
		var sh sealevel.SysvarSlotHashes
		decoder := bin.NewBinDecoder(slotHashesData)
		if decErr := sh.UnmarshalWithDecoder(decoder); decErr == nil {
			shDiag := &SlotHashesDiag{
				NumEntries: len(sh),
			}
			if len(sh) > 0 {
				shDiag.NewestSlot = sh[0].Slot
				shDiag.NewestHash = base58.Encode(sh[0].Hash[:])
				shDiag.OldestSlot = sh[len(sh)-1].Slot
				shDiag.OldestHash = base58.Encode(sh[len(sh)-1].Hash[:])

				// Include 10 most recent entries
				numRecent := 10
				if len(sh) < numRecent {
					numRecent = len(sh)
				}
				for i := 0; i < numRecent; i++ {
					shDiag.RecentEntries = append(shDiag.RecentEntries, SlotHashEntry{
						Slot: sh[i].Slot,
						Hash: base58.Encode(sh[i].Hash[:]),
					})
				}
			}
			diag.SlotHashes = shDiag
		}
	}

	// Write to file
	diagDir := getDiagnosticsDir()
	filename := filepath.Join(diagDir, fmt.Sprintf("rpc_slot_%d.json", slot))

	data, err := json.MarshalIndent(diag, "", "  ")
	if err != nil {
		mlog.Log.Warnf("failed to marshal RPC diagnostics for slot %d: %v", slot, err)
		return
	}

	if err := os.WriteFile(filename, data, 0644); err != nil {
		mlog.Log.Warnf("failed to write RPC diagnostics for slot %d: %v", slot, err)
		return
	}

	mlog.Log.Infof("wrote RPC sysvar diagnostics to %s", filename)
}

// fetchAccountData fetches account data from RPC
func fetchAccountData(rpcc *rpcclient.RpcClient, pubkey solana.PublicKey) ([]byte, error) {
	result, err := rpcc.GetClient().GetAccountInfo(rpcc.GetContext(), pubkey)
	if err != nil {
		return nil, err
	}
	if result == nil || result.Value == nil {
		return nil, fmt.Errorf("account not found")
	}
	return result.Value.Data.GetBinary(), nil
}

// GenerateDiagnosticComparison reads local and RPC dumps and creates a comparison summary
func GenerateDiagnosticComparison(slot uint64) {
	diagDir := getDiagnosticsDir()
	localFile := filepath.Join(diagDir, fmt.Sprintf("local_slot_%d.json", slot))
	rpcFile := filepath.Join(diagDir, fmt.Sprintf("rpc_slot_%d.json", slot))

	// Read local dump
	localData, err := os.ReadFile(localFile)
	if err != nil {
		mlog.Log.Warnf("comparison: failed to read local file for slot %d: %v", slot, err)
		return
	}
	var localDiag EpochBoundaryDiagnostics
	if err := json.Unmarshal(localData, &localDiag); err != nil {
		mlog.Log.Warnf("comparison: failed to parse local file for slot %d: %v", slot, err)
		return
	}

	// Read RPC dump
	rpcData, err := os.ReadFile(rpcFile)
	if err != nil {
		mlog.Log.Warnf("comparison: failed to read RPC file for slot %d: %v", slot, err)
		return
	}
	var rpcDiag EpochBoundaryDiagnostics
	if err := json.Unmarshal(rpcData, &rpcDiag); err != nil {
		mlog.Log.Warnf("comparison: failed to parse RPC file for slot %d: %v", slot, err)
		return
	}

	// Build comparison
	comparison := buildComparison(slot, &localDiag, &rpcDiag)

	// Write comparison file
	compFile := filepath.Join(diagDir, fmt.Sprintf("comparison_slot_%d.txt", slot))
	if err := os.WriteFile(compFile, []byte(comparison), 0644); err != nil {
		mlog.Log.Warnf("comparison: failed to write comparison for slot %d: %v", slot, err)
		return
	}

	mlog.Log.Infof("wrote diagnostic comparison to %s", compFile)
}

func buildComparison(slot uint64, local, rpc *EpochBoundaryDiagnostics) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("=== DIAGNOSTIC COMPARISON FOR SLOT %d ===\n\n", slot))

	// Bank Hash
	sb.WriteString("BANK HASH:\n")
	if local.BankHash == rpc.BankHash {
		sb.WriteString(fmt.Sprintf("  [MATCH] %s\n", local.BankHash))
	} else {
		sb.WriteString(fmt.Sprintf("  [MISMATCH]\n"))
		sb.WriteString(fmt.Sprintf("    local: %s\n", local.BankHash))
		sb.WriteString(fmt.Sprintf("    rpc:   %s\n", rpc.BankHash))
	}

	// NumSigs
	sb.WriteString("\nNUM SIGNATURES:\n")
	if local.NumSigs == rpc.NumSigs {
		sb.WriteString(fmt.Sprintf("  [MATCH] %d\n", local.NumSigs))
	} else {
		sb.WriteString(fmt.Sprintf("  [MISMATCH] local=%d, rpc=%d\n", local.NumSigs, rpc.NumSigs))
	}

	// Clock
	sb.WriteString("\nCLOCK SYSVAR:\n")
	if local.Clock != nil && rpc.Clock != nil {
		if local.Clock.Slot == rpc.Clock.Slot &&
			local.Clock.Epoch == rpc.Clock.Epoch &&
			local.Clock.LeaderScheduleEpoch == rpc.Clock.LeaderScheduleEpoch {
			sb.WriteString(fmt.Sprintf("  [MATCH] slot=%d, epoch=%d, leader_schedule_epoch=%d\n",
				local.Clock.Slot, local.Clock.Epoch, local.Clock.LeaderScheduleEpoch))
		} else {
			sb.WriteString("  [MISMATCH]\n")
			sb.WriteString(fmt.Sprintf("    local: slot=%d, epoch=%d, leader_schedule_epoch=%d\n",
				local.Clock.Slot, local.Clock.Epoch, local.Clock.LeaderScheduleEpoch))
			sb.WriteString(fmt.Sprintf("    rpc:   slot=%d, epoch=%d, leader_schedule_epoch=%d\n",
				rpc.Clock.Slot, rpc.Clock.Epoch, rpc.Clock.LeaderScheduleEpoch))
		}
	} else {
		sb.WriteString("  [INCOMPLETE] local or rpc clock is nil\n")
	}

	// EpochRewards
	sb.WriteString("\nEPOCH REWARDS SYSVAR:\n")
	if local.EpochRewards != nil && rpc.EpochRewards != nil {
		allMatch := local.EpochRewards.NumPartitions == rpc.EpochRewards.NumPartitions &&
			local.EpochRewards.TotalPoints == rpc.EpochRewards.TotalPoints &&
			local.EpochRewards.TotalRewards == rpc.EpochRewards.TotalRewards &&
			local.EpochRewards.Active == rpc.EpochRewards.Active
		if allMatch {
			sb.WriteString(fmt.Sprintf("  [MATCH] partitions=%d, total_points=%s, total_rewards=%d, active=%v\n",
				local.EpochRewards.NumPartitions, local.EpochRewards.TotalPoints, local.EpochRewards.TotalRewards, local.EpochRewards.Active))
		} else {
			sb.WriteString("  [MISMATCH]\n")
			sb.WriteString(fmt.Sprintf("    local: partitions=%d, total_points=%s, total_rewards=%d, active=%v\n",
				local.EpochRewards.NumPartitions, local.EpochRewards.TotalPoints, local.EpochRewards.TotalRewards, local.EpochRewards.Active))
			sb.WriteString(fmt.Sprintf("    rpc:   partitions=%d, total_points=%s, total_rewards=%d, active=%v\n",
				rpc.EpochRewards.NumPartitions, rpc.EpochRewards.TotalPoints, rpc.EpochRewards.TotalRewards, rpc.EpochRewards.Active))
		}
	} else {
		sb.WriteString("  [INCOMPLETE] local or rpc epoch_rewards is nil\n")
	}

	// SlotHashes
	sb.WriteString("\nSLOT HASHES SYSVAR:\n")
	if local.SlotHashes != nil && rpc.SlotHashes != nil {
		sb.WriteString(fmt.Sprintf("  local: %d entries, newest=%d\n", local.SlotHashes.NumEntries, local.SlotHashes.NewestSlot))
		sb.WriteString(fmt.Sprintf("  rpc:   %d entries, newest=%d\n", rpc.SlotHashes.NumEntries, rpc.SlotHashes.NewestSlot))

		// Check if newest entries match
		if len(local.SlotHashes.RecentEntries) > 0 && len(rpc.SlotHashes.RecentEntries) > 0 {
			localNewest := local.SlotHashes.RecentEntries[0]
			rpcNewest := rpc.SlotHashes.RecentEntries[0]
			if localNewest.Slot == rpcNewest.Slot && localNewest.Hash == rpcNewest.Hash {
				sb.WriteString(fmt.Sprintf("  newest entry: [MATCH] slot=%d hash=%s\n", localNewest.Slot, localNewest.Hash))
			} else {
				sb.WriteString(fmt.Sprintf("  newest entry: [MISMATCH]\n"))
				sb.WriteString(fmt.Sprintf("    local: slot=%d hash=%s\n", localNewest.Slot, localNewest.Hash))
				sb.WriteString(fmt.Sprintf("    rpc:   slot=%d hash=%s\n", rpcNewest.Slot, rpcNewest.Hash))
			}
		}
	}

	// Modified accounts summary
	if local.ModifiedAccounts != nil {
		sb.WriteString("\nMODIFIED ACCOUNTS (local):\n")
		sb.WriteString(fmt.Sprintf("  writable_count: %d\n", local.ModifiedAccounts.WritableCount))
		sb.WriteString(fmt.Sprintf("  epoch_updated_count: %d\n", local.ModifiedAccounts.EpochUpdatedCount))
		sb.WriteString(fmt.Sprintf("  sysvar_count: %d\n", local.ModifiedAccounts.SysvarCount))
		if len(local.ModifiedAccounts.Sysvars) > 0 {
			sb.WriteString(fmt.Sprintf("  sysvars: %v\n", local.ModifiedAccounts.Sysvars))
		}
	}

	sb.WriteString("\n=== END COMPARISON ===\n")
	return sb.String()
}

// ============================================================================
// Transaction Comparison Dump for Divergent Slots
// ============================================================================

// TxDumpEntry holds comparison data for a single transaction
type TxDumpEntry struct {
	Index      int    `json:"index"`
	Signature  string `json:"signature"`
	IsVote     bool   `json:"is_vote"`
	NumSigners int    `json:"num_signers"`

	// Local execution results
	LocalSuccess bool   `json:"local_success"`
	LocalError   string `json:"local_error,omitempty"`
	LocalCU      uint64 `json:"local_cu"`
	LocalFee     uint64 `json:"local_fee"`

	// RPC (on-chain) results
	RpcSuccess bool   `json:"rpc_success"`
	RpcError   string `json:"rpc_error,omitempty"`
	RpcCU      uint64 `json:"rpc_cu"`
	RpcFee     uint64 `json:"rpc_fee"`

	// Diff flags
	StatusMismatch bool `json:"status_mismatch,omitempty"`
	CUMismatch     bool `json:"cu_mismatch,omitempty"`
	FeeMismatch    bool `json:"fee_mismatch,omitempty"`
}

// TxDumpSummary holds the full transaction comparison for a slot
type TxDumpSummary struct {
	Slot             uint64        `json:"slot"`
	Epoch            uint64        `json:"epoch"`
	TotalTransactions int          `json:"total_transactions"`
	StatusMismatches  int          `json:"status_mismatches"`
	CUMismatches      int          `json:"cu_mismatches"`
	FeeMismatches     int          `json:"fee_mismatches"`
	TotalLocalCU      uint64       `json:"total_local_cu"`
	TotalRpcCU        uint64       `json:"total_rpc_cu"`
	TotalLocalFee     uint64       `json:"total_local_fee"`
	TotalRpcFee       uint64       `json:"total_rpc_fee"`
	Transactions      []TxDumpEntry `json:"transactions"`
	MismatchedTxs     []TxDumpEntry `json:"mismatched_txs,omitempty"` // Only those with differences
}

// TxLocalResult holds local execution results for a transaction
type TxLocalResult struct {
	Success bool
	Error   error
	CU      uint64
	Fee     uint64
}

// WriteTxComparisonDump creates a comprehensive transaction comparison file
// Call this when divergence is detected to capture detailed tx-level data
func WriteTxComparisonDump(slot uint64, epoch uint64, block *b.Block, localResults []TxLocalResult) {
	if block == nil || len(block.Transactions) == 0 {
		return
	}

	summary := TxDumpSummary{
		Slot:              slot,
		Epoch:             epoch,
		TotalTransactions: len(block.Transactions),
		Transactions:      make([]TxDumpEntry, 0, len(block.Transactions)),
	}

	for i, tx := range block.Transactions {
		if tx == nil {
			continue
		}

		entry := TxDumpEntry{
			Index:      i,
			NumSigners: len(tx.Signatures),
			IsVote:     tx.IsVote(),
		}

		// Signature
		if len(tx.Signatures) > 0 {
			entry.Signature = tx.Signatures[0].String()
		}

		// Local results
		if i < len(localResults) {
			entry.LocalSuccess = localResults[i].Success
			if localResults[i].Error != nil {
				entry.LocalError = localResults[i].Error.Error()
			}
			entry.LocalCU = localResults[i].CU
			entry.LocalFee = localResults[i].Fee
			summary.TotalLocalCU += localResults[i].CU
			summary.TotalLocalFee += localResults[i].Fee
		}

		// RPC results from TxMeta
		if i < len(block.TxMetas) && block.TxMetas[i] != nil {
			meta := block.TxMetas[i]
			entry.RpcSuccess = meta.Err == nil
			if meta.Err != nil {
				entry.RpcError = fmt.Sprintf("%+v", meta.Err)
			}
			if meta.ComputeUnitsConsumed != nil {
				entry.RpcCU = *meta.ComputeUnitsConsumed
				summary.TotalRpcCU += *meta.ComputeUnitsConsumed
			}
			entry.RpcFee = meta.Fee
			summary.TotalRpcFee += meta.Fee
		}

		// Check for mismatches
		if entry.LocalSuccess != entry.RpcSuccess {
			entry.StatusMismatch = true
			summary.StatusMismatches++
		}
		if entry.LocalCU != entry.RpcCU {
			entry.CUMismatch = true
			summary.CUMismatches++
		}
		if entry.LocalFee != entry.RpcFee {
			entry.FeeMismatch = true
			summary.FeeMismatches++
		}

		summary.Transactions = append(summary.Transactions, entry)

		// Also collect mismatched transactions separately for easy viewing
		if entry.StatusMismatch || entry.CUMismatch || entry.FeeMismatch {
			summary.MismatchedTxs = append(summary.MismatchedTxs, entry)
		}
	}

	// Write full dump
	diagDir := getDiagnosticsDir()
	filename := filepath.Join(diagDir, fmt.Sprintf("tx_comparison_slot_%d.json", slot))

	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		mlog.Log.Warnf("failed to marshal tx comparison for slot %d: %v", slot, err)
		return
	}

	if err := os.WriteFile(filename, data, 0644); err != nil {
		mlog.Log.Warnf("failed to write tx comparison for slot %d: %v", slot, err)
		return
	}

	mlog.Log.Infof("wrote tx comparison dump to %s (txs=%d status_mismatch=%d cu_mismatch=%d fee_mismatch=%d)",
		filename, summary.TotalTransactions, summary.StatusMismatches, summary.CUMismatches, summary.FeeMismatches)

	// Also write a human-readable diff summary
	writeTxDiffSummary(slot, &summary)
}

// writeTxDiffSummary writes a human-readable summary of transaction differences
func writeTxDiffSummary(slot uint64, summary *TxDumpSummary) {
	diagDir := getDiagnosticsDir()
	filename := filepath.Join(diagDir, fmt.Sprintf("tx_diff_slot_%d.txt", slot))

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("=== TRANSACTION COMPARISON FOR SLOT %d ===\n\n", slot))
	sb.WriteString(fmt.Sprintf("Total transactions: %d\n", summary.TotalTransactions))
	sb.WriteString(fmt.Sprintf("Status mismatches:  %d\n", summary.StatusMismatches))
	sb.WriteString(fmt.Sprintf("CU mismatches:      %d\n", summary.CUMismatches))
	sb.WriteString(fmt.Sprintf("Fee mismatches:     %d\n", summary.FeeMismatches))
	sb.WriteString(fmt.Sprintf("\nTotal Local CU:  %d\n", summary.TotalLocalCU))
	sb.WriteString(fmt.Sprintf("Total RPC CU:    %d\n", summary.TotalRpcCU))
	sb.WriteString(fmt.Sprintf("CU Difference:   %d\n", int64(summary.TotalLocalCU)-int64(summary.TotalRpcCU)))
	sb.WriteString(fmt.Sprintf("\nTotal Local Fee: %d\n", summary.TotalLocalFee))
	sb.WriteString(fmt.Sprintf("Total RPC Fee:   %d\n", summary.TotalRpcFee))
	sb.WriteString(fmt.Sprintf("Fee Difference:  %d\n", int64(summary.TotalLocalFee)-int64(summary.TotalRpcFee)))

	if len(summary.MismatchedTxs) > 0 {
		sb.WriteString(fmt.Sprintf("\n=== MISMATCHED TRANSACTIONS (%d) ===\n", len(summary.MismatchedTxs)))
		for _, tx := range summary.MismatchedTxs {
			sb.WriteString(fmt.Sprintf("\n[%d] %s\n", tx.Index, tx.Signature))
			if tx.IsVote {
				sb.WriteString("  Type: VOTE\n")
			}
			if tx.StatusMismatch {
				sb.WriteString(fmt.Sprintf("  STATUS MISMATCH: local=%v (err: %s) rpc=%v (err: %s)\n",
					tx.LocalSuccess, tx.LocalError, tx.RpcSuccess, tx.RpcError))
			}
			if tx.CUMismatch {
				diff := int64(tx.LocalCU) - int64(tx.RpcCU)
				sb.WriteString(fmt.Sprintf("  CU MISMATCH: local=%d rpc=%d (diff=%+d)\n", tx.LocalCU, tx.RpcCU, diff))
			}
			if tx.FeeMismatch {
				diff := int64(tx.LocalFee) - int64(tx.RpcFee)
				sb.WriteString(fmt.Sprintf("  FEE MISMATCH: local=%d rpc=%d (diff=%+d)\n", tx.LocalFee, tx.RpcFee, diff))
			}
		}
	} else {
		sb.WriteString("\n=== NO MISMATCHED TRANSACTIONS ===\n")
	}

	sb.WriteString("\n=== END COMPARISON ===\n")

	if err := os.WriteFile(filename, []byte(sb.String()), 0644); err != nil {
		mlog.Log.Warnf("failed to write tx diff summary for slot %d: %v", slot, err)
		return
	}

	mlog.Log.Infof("wrote tx diff summary to %s", filename)
}

// DumpDivergentSlotTransactions dumps all transaction metadata for a slot where divergence occurred.
// This is called automatically when a DivergenceError is detected.
func DumpDivergentSlotTransactions(block *b.Block, divErr *DivergenceError) {
	if block == nil {
		return
	}

	dump := DivergenceDump{
		Slot:               block.Slot,
		Epoch:              block.Epoch,
		DivergentTxSig:     divErr.TxSig,
		DivergentOnchainOk: divErr.OnchainOk,
		TotalTransactions:  len(block.Transactions),
	}
	if divErr.LocalErr != nil {
		dump.DivergentTxError = divErr.LocalErr.Error()
	}

	for i, tx := range block.Transactions {
		if tx == nil {
			continue
		}

		meta := TxRpcMeta{
			Index:  i,
			IsVote: tx.IsVote(),
		}

		if len(tx.Signatures) > 0 {
			meta.Signature = tx.Signatures[0].String()
		}

		if i < len(block.TxMetas) && block.TxMetas[i] != nil {
			rpcMeta := block.TxMetas[i]
			meta.RpcSuccess = rpcMeta.Err == nil
			if rpcMeta.Err != nil {
				meta.RpcError = fmt.Sprintf("%+v", rpcMeta.Err)
			}
			if rpcMeta.ComputeUnitsConsumed != nil {
				meta.RpcCU = *rpcMeta.ComputeUnitsConsumed
				dump.TotalRpcCU += *rpcMeta.ComputeUnitsConsumed
			}
			meta.RpcFee = rpcMeta.Fee
			dump.TotalRpcFee += rpcMeta.Fee

			// Include balance changes for first few accounts (useful for debugging)
			if len(rpcMeta.PreBalances) > 0 && len(rpcMeta.PreBalances) <= 10 {
				meta.PreBalance = rpcMeta.PreBalances
			}
			if len(rpcMeta.PostBalances) > 0 && len(rpcMeta.PostBalances) <= 10 {
				meta.PostBalance = rpcMeta.PostBalances
			}
		}

		if meta.IsVote {
			dump.TotalVotes++
		}

		dump.Transactions = append(dump.Transactions, meta)
	}

	// Write JSON dump
	diagDir := getDiagnosticsDir()
	filename := filepath.Join(diagDir, fmt.Sprintf("divergence_slot_%d.json", block.Slot))

	data, err := json.MarshalIndent(dump, "", "  ")
	if err != nil {
		mlog.Log.Warnf("failed to marshal divergence dump for slot %d: %v", block.Slot, err)
		return
	}

	if err := os.WriteFile(filename, data, 0644); err != nil {
		mlog.Log.Warnf("failed to write divergence dump for slot %d: %v", block.Slot, err)
		return
	}

	mlog.Log.Infof("wrote divergence dump to %s (txs=%d votes=%d total_cu=%d divergent_tx=%s)",
		filename, dump.TotalTransactions, dump.TotalVotes, dump.TotalRpcCU, dump.DivergentTxSig)

	// Write human-readable summary
	writeDivergenceSummary(block.Slot, &dump)
}

// writeDivergenceSummary writes a human-readable summary of the divergent slot
func writeDivergenceSummary(slot uint64, dump *DivergenceDump) {
	diagDir := getDiagnosticsDir()
	filename := filepath.Join(diagDir, fmt.Sprintf("divergence_slot_%d.txt", slot))

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("=== DIVERGENCE IN SLOT %d ===\n\n", slot))
	sb.WriteString(fmt.Sprintf("Epoch: %d\n", dump.Epoch))
	sb.WriteString(fmt.Sprintf("Total transactions: %d\n", dump.TotalTransactions))
	sb.WriteString(fmt.Sprintf("Total votes: %d\n", dump.TotalVotes))
	sb.WriteString(fmt.Sprintf("Total RPC CU: %d\n", dump.TotalRpcCU))
	sb.WriteString(fmt.Sprintf("Total RPC Fee: %d\n", dump.TotalRpcFee))
	sb.WriteString(fmt.Sprintf("\n=== DIVERGENT TRANSACTION ===\n"))
	sb.WriteString(fmt.Sprintf("Signature: %s\n", dump.DivergentTxSig))
	sb.WriteString(fmt.Sprintf("Onchain success: %v\n", dump.DivergentOnchainOk))
	if dump.DivergentTxError != "" {
		sb.WriteString(fmt.Sprintf("Local error: %s\n", dump.DivergentTxError))
	}

	// Find and highlight the divergent transaction
	sb.WriteString(fmt.Sprintf("\n=== ALL TRANSACTIONS ===\n"))
	for _, tx := range dump.Transactions {
		marker := ""
		if tx.Signature == dump.DivergentTxSig {
			marker = " <<< DIVERGENT"
		}
		voteMarker := ""
		if tx.IsVote {
			voteMarker = "[VOTE] "
		}
		statusStr := "SUCCESS"
		if !tx.RpcSuccess {
			statusStr = fmt.Sprintf("FAILED(%s)", tx.RpcError)
		}
		sb.WriteString(fmt.Sprintf("[%d] %s%s cu=%d fee=%d status=%s%s\n",
			tx.Index, voteMarker, tx.Signature[:16]+"...", tx.RpcCU, tx.RpcFee, statusStr, marker))
	}

	sb.WriteString("\n=== END DIVERGENCE DUMP ===\n")

	if err := os.WriteFile(filename, []byte(sb.String()), 0644); err != nil {
		mlog.Log.Warnf("failed to write divergence summary for slot %d: %v", slot, err)
		return
	}

	mlog.Log.Infof("wrote divergence summary to %s", filename)
}

// StakingRewardComparison holds the comparison between local and RPC staking rewards
type StakingRewardComparison struct {
	Slot             uint64                       `json:"slot"`
	Epoch            uint64                       `json:"epoch"`
	PartitionIdx     uint64                       `json:"partition_idx"`
	LocalCount       int                          `json:"local_count"`
	RpcCount         int                          `json:"rpc_count"`
	LocalTotal       uint64                       `json:"local_total_lamports"`
	RpcTotal         uint64                       `json:"rpc_total_lamports"`
	MatchCount       int                          `json:"match_count"`
	MissingFromLocal []StakingRewardEntry         `json:"missing_from_local,omitempty"`
	ExtraInLocal     []StakingRewardEntry         `json:"extra_in_local,omitempty"`
	AmountMismatch   []StakingRewardAmountMismatch `json:"amount_mismatch,omitempty"`
}

type StakingRewardEntry struct {
	Pubkey      string `json:"pubkey"`
	Lamports    uint64 `json:"lamports"`
	PostBalance uint64 `json:"post_balance,omitempty"`
	Commission  uint8  `json:"commission,omitempty"`
}

type StakingRewardAmountMismatch struct {
	Pubkey          string `json:"pubkey"`
	LocalLamports   uint64 `json:"local_lamports"`
	RpcLamports     uint64 `json:"rpc_lamports"`
	Diff            int64  `json:"diff"`
	LocalCommission uint8  `json:"local_commission,omitempty"`
	RpcCommission   uint8  `json:"rpc_commission,omitempty"`
}

// LocalStakingReward holds local reward info including commission for comparison
type LocalStakingReward struct {
	Lamports   uint64
	Commission uint8
	VotePubkey solana.PublicKey
	// Debug info for diagnosing force_credits_update triggers
	ForceCreditsReason string // "credits_backward", "activation_epoch", "total_rewards_zero", ""
	CreditsInVote      uint64 // vote credits at maxEpoch
	CreditsInStake     uint64 // stake's credits_observed
	ActivationEpoch    uint64 // delegation's activation epoch
	RewardedEpoch      uint64 // epoch being rewarded
}

// CompareStakingRewardsToRpc compares local staking rewards distribution to RPC rewards for a slot.
// localRewards maps pubkey -> LocalStakingReward with lamports and commission info.
// block.Rewards contains RPC rewards including Staking type.
func CompareStakingRewardsToRpc(block *b.Block, localRewards map[solana.PublicKey]LocalStakingReward, partitionIdx uint64) *StakingRewardComparison {
	if block == nil {
		return nil
	}

	comp := &StakingRewardComparison{
		Slot:         block.Slot,
		Epoch:        block.Epoch,
		PartitionIdx: partitionIdx,
		LocalCount:   len(localRewards),
	}

	// Build map of RPC staking rewards (pubkey -> reward info)
	rpcRewards := make(map[solana.PublicKey]StakingRewardEntry)
	for _, reward := range block.Rewards {
		if string(reward.RewardType) != "Staking" {
			continue
		}
		pk := reward.Pubkey
		lamports := uint64(reward.Lamports)
		if reward.Lamports < 0 {
			// Negative rewards shouldn't happen for staking, but handle defensively
			lamports = 0
		}
		rpcRewards[pk] = StakingRewardEntry{
			Pubkey:      pk.String(),
			Lamports:    lamports,
			PostBalance: reward.PostBalance,
		}
		if reward.Commission != nil {
			rpcRewards[pk] = StakingRewardEntry{
				Pubkey:      pk.String(),
				Lamports:    lamports,
				PostBalance: reward.PostBalance,
				Commission:  *reward.Commission,
			}
		}
		comp.RpcCount++
		comp.RpcTotal += lamports
	}

	// Calculate local total
	for _, localReward := range localRewards {
		comp.LocalTotal += localReward.Lamports
	}

	// Compare: find matches, mismatches, missing, and extra
	for pk, localReward := range localRewards {
		if rpcEntry, exists := rpcRewards[pk]; exists {
			lamportsMatch := localReward.Lamports == rpcEntry.Lamports
			commissionMatch := localReward.Commission == rpcEntry.Commission

			if lamportsMatch && commissionMatch {
				comp.MatchCount++
			} else {
				comp.AmountMismatch = append(comp.AmountMismatch, StakingRewardAmountMismatch{
					Pubkey:          pk.String(),
					LocalLamports:   localReward.Lamports,
					RpcLamports:     rpcEntry.Lamports,
					Diff:            int64(localReward.Lamports) - int64(rpcEntry.Lamports),
					LocalCommission: localReward.Commission,
					RpcCommission:   rpcEntry.Commission,
				})
			}
			delete(rpcRewards, pk) // Mark as processed
		} else {
			// In local but not in RPC (includes ForceCreditsUpdate accounts with 0 lamports)
			comp.ExtraInLocal = append(comp.ExtraInLocal, StakingRewardEntry{
				Pubkey:     pk.String(),
				Lamports:   localReward.Lamports,
				Commission: localReward.Commission,
			})
		}
	}

	// Remaining RPC rewards are missing from local
	for _, entry := range rpcRewards {
		comp.MissingFromLocal = append(comp.MissingFromLocal, entry)
	}

	return comp
}

// WriteStakingRewardsComparison writes the comparison to a JSON file and logs summary
func WriteStakingRewardsComparison(comp *StakingRewardComparison) {
	if comp == nil {
		return
	}

	diagDir := getDiagnosticsDir()
	filename := filepath.Join(diagDir, fmt.Sprintf("staking_rewards_slot_%d_p%d.json", comp.Slot, comp.PartitionIdx+1))

	data, err := json.MarshalIndent(comp, "", "  ")
	if err != nil {
		mlog.Log.Warnf("failed to marshal staking rewards comparison: %v", err)
		return
	}

	if err := os.WriteFile(filename, data, 0644); err != nil {
		mlog.Log.Warnf("failed to write staking rewards comparison: %v", err)
		return
	}

	// Log summary
	mlog.Log.Infof("STAKING_REWARDS_CMP slot=%d partition=%d: local=%d rpc=%d match=%d missing=%d extra=%d mismatch=%d local_total=%d rpc_total=%d diff=%d",
		comp.Slot, comp.PartitionIdx+1,
		comp.LocalCount, comp.RpcCount, comp.MatchCount,
		len(comp.MissingFromLocal), len(comp.ExtraInLocal), len(comp.AmountMismatch),
		comp.LocalTotal, comp.RpcTotal, int64(comp.LocalTotal)-int64(comp.RpcTotal))

	if len(comp.MissingFromLocal) > 0 {
		mlog.Log.Warnf("STAKING_REWARDS_CMP slot=%d: %d accounts in RPC but not in local distribution",
			comp.Slot, len(comp.MissingFromLocal))
		for i, entry := range comp.MissingFromLocal {
			if i < 10 { // Log first 10
				mlog.Log.Warnf("  MISSING: %s lamports=%d", entry.Pubkey, entry.Lamports)
			}
		}
	}

	if len(comp.ExtraInLocal) > 0 {
		mlog.Log.Warnf("STAKING_REWARDS_CMP slot=%d: %d accounts in local but not in RPC",
			comp.Slot, len(comp.ExtraInLocal))
		for i, entry := range comp.ExtraInLocal {
			if i < 10 { // Log first 10
				mlog.Log.Warnf("  EXTRA: %s lamports=%d", entry.Pubkey, entry.Lamports)
			}
		}
	}

	if len(comp.AmountMismatch) > 0 {
		mlog.Log.Warnf("STAKING_REWARDS_CMP slot=%d: %d accounts have different reward amounts",
			comp.Slot, len(comp.AmountMismatch))
		for i, m := range comp.AmountMismatch {
			if i < 10 { // Log first 10
				mlog.Log.Warnf("  MISMATCH: %s local=%d rpc=%d diff=%+d", m.Pubkey, m.LocalLamports, m.RpcLamports, m.Diff)
			}
		}
	}

	mlog.Log.Infof("wrote staking rewards comparison to %s", filename)
}

// PartitionAccountDetail holds before/after details for an account during distribution
type PartitionAccountDetail struct {
	Pubkey              string `json:"pubkey"`
	Reward              uint64 `json:"reward"`
	BeforeCredits       uint64 `json:"before_credits"`
	AfterCredits        uint64 `json:"after_credits"`
	BeforeStakeLamports uint64 `json:"before_stake_lamports"`
	AfterStakeLamports  uint64 `json:"after_stake_lamports"`
	BeforeAcctLamports  uint64 `json:"before_acct_lamports"`
	IsForceCreditsUpdate bool  `json:"is_force_credits_update"` // true if reward=0 but credits changed
	// RPC comparison (fetched from actual account state on mainnet)
	RpcCreditsObserved  *uint64 `json:"rpc_credits_observed,omitempty"`  // nil if fetch failed
	RpcStakeLamports    *uint64 `json:"rpc_stake_lamports,omitempty"`    // nil if fetch failed
	RpcAcctLamports     *uint64 `json:"rpc_acct_lamports,omitempty"`     // nil if fetch failed
	CreditsMatch        *bool   `json:"credits_match,omitempty"`         // nil if not comparable
	StakeMatch          *bool   `json:"stake_match,omitempty"`           // nil if not comparable
	LamportsMatch       *bool   `json:"lamports_match,omitempty"`        // nil if not comparable
}

// PartitionAccountDetails holds all account details for a partition
type PartitionAccountDetails struct {
	Slot           uint64                   `json:"slot"`
	PartitionIdx   uint64                   `json:"partition_idx"`
	AccountCount   int                      `json:"account_count"`
	TotalRewards   uint64                   `json:"total_rewards"`
	ForceCreditsCount int                   `json:"force_credits_update_count"`
	Accounts       []PartitionAccountDetail `json:"accounts"`
}

// WritePartitionAccountDetails writes partition account before/after details to a JSON file
func WritePartitionAccountDetails(details *PartitionAccountDetails) {
	if details == nil {
		return
	}

	diagDir := getDiagnosticsDir()
	filename := filepath.Join(diagDir, fmt.Sprintf("partition_details_slot_%d_p%d.json", details.Slot, details.PartitionIdx+1))

	data, err := json.MarshalIndent(details, "", "  ")
	if err != nil {
		mlog.Log.Warnf("failed to marshal partition account details: %v", err)
		return
	}

	if err := os.WriteFile(filename, data, 0644); err != nil {
		mlog.Log.Warnf("failed to write partition account details: %v", err)
		return
	}

	mlog.Log.Infof("wrote partition account details to %s (accounts=%d force_credits=%d)",
		filename, details.AccountCount, details.ForceCreditsCount)
}

// StakeAccountComparison holds local vs RPC comparison for a single stake account
type StakeAccountComparison struct {
	Pubkey               string `json:"pubkey"`
	// Local state (after distribution)
	LocalPostBalance     uint64 `json:"local_post_balance"`     // Account lamports after distribution
	LocalCreditsObserved uint64 `json:"local_credits_observed"` // CreditsObserved from stake state
	LocalStakeLamports   uint64 `json:"local_stake_lamports"`   // stake.delegation.stake
	LocalReward          uint64 `json:"local_reward"`           // Reward we calculated
	LocalCommission      uint8  `json:"local_commission"`       // Commission rate used
	// Debug info for 0-reward accounts (ForceCreditsUpdate)
	ForceCreditsReason   string `json:"force_credits_reason,omitempty"` // "credits_backward", "activation_epoch", "total_rewards_zero", ""
	CreditsInVote        uint64 `json:"credits_in_vote,omitempty"`       // vote credits at maxEpoch
	CreditsInStake       uint64 `json:"credits_in_stake,omitempty"`      // stake's credits_observed
	ActivationEpoch      uint64 `json:"activation_epoch,omitempty"`      // delegation's activation epoch
	RewardedEpoch        uint64 `json:"rewarded_epoch,omitempty"`        // epoch being rewarded
	// RPC state (from block.Rewards) - nil means RPC didn't report this account (N/A)
	RpcPostBalance       *uint64 `json:"rpc_post_balance"`       // postBalance from RPC (nil if not in RPC)
	RpcReward            *uint64 `json:"rpc_reward"`             // lamports from RPC reward (nil if not in RPC)
	RpcCommission        *uint8  `json:"rpc_commission"`         // commission from RPC (nil if not in RPC)
	// Comparison - only set when both local and RPC have data
	RewardDiff           *int64  `json:"reward_diff,omitempty"`       // local_reward - rpc_reward (nil if not comparable)
	PostBalanceDiff      *int64  `json:"post_balance_diff,omitempty"` // local_post_balance - rpc_post_balance (nil if not comparable)
	CommissionMatch      *bool   `json:"commission_match,omitempty"`  // nil if not comparable
	InLocalOnly          bool    `json:"in_local_only,omitempty"`
	InRpcOnly            bool    `json:"in_rpc_only,omitempty"`
}

// PartitionStakeComparison holds full local vs RPC comparison for a partition
type PartitionStakeComparison struct {
	Slot                uint64                    `json:"slot"`
	PartitionIdx        uint64                    `json:"partition_idx"`
	LocalCount          int                       `json:"local_count"`
	RpcCount            int                       `json:"rpc_count"`
	MatchCount          int                       `json:"match_count"`
	LamportsMismatch    int                       `json:"lamports_mismatch_count"`
	PostBalMismatch     int                       `json:"post_balance_mismatch_count"`
	CommissionMismatch  int                       `json:"commission_mismatch_count"`
	InLocalOnlyCount    int                       `json:"in_local_only_count"`
	InRpcOnlyCount      int                       `json:"in_rpc_only_count"`
	Accounts            []StakeAccountComparison  `json:"accounts,omitempty"`
	// Only include divergent accounts to keep file size reasonable
	DivergentAccounts   []StakeAccountComparison  `json:"divergent_accounts,omitempty"`
}

// ComparePartitionStakeToRpc compares local stake account state to RPC for P10/P11
// localAccts are the accounts we wrote (post-distribution state)
// localRewards are the calculated rewards with CreditsObserved
// block contains RPC rewards
func ComparePartitionStakeToRpc(
	slot, partitionIdx uint64,
	localAccts []*accounts.Account,
	localRewards map[solana.PublicKey]LocalStakingReward,
	block *b.Block,
) *PartitionStakeComparison {
	if block == nil {
		return nil
	}

	// Only for P10/P11
	if partitionIdx != 9 && partitionIdx != 10 {
		return nil
	}

	comp := &PartitionStakeComparison{
		Slot:         slot,
		PartitionIdx: partitionIdx,
	}

	// Build map of RPC rewards
	rpcRewards := make(map[solana.PublicKey]struct {
		lamports    uint64
		postBalance uint64
		commission  uint8
	})
	for _, reward := range block.Rewards {
		if string(reward.RewardType) != "Staking" {
			continue
		}
		lamports := uint64(0)
		if reward.Lamports > 0 {
			lamports = uint64(reward.Lamports)
		}
		commission := uint8(0)
		if reward.Commission != nil {
			commission = *reward.Commission
		}
		rpcRewards[reward.Pubkey] = struct {
			lamports    uint64
			postBalance uint64
			commission  uint8
		}{lamports, reward.PostBalance, commission}
		comp.RpcCount++
	}

	// Build map of local accounts for lookup
	localAcctMap := make(map[solana.PublicKey]*accounts.Account)
	for _, acct := range localAccts {
		if acct != nil {
			localAcctMap[acct.Key] = acct
		}
	}

	// Compare each local reward to RPC
	for pk, localReward := range localRewards {
		comp.LocalCount++

		var acctComp StakeAccountComparison
		acctComp.Pubkey = pk.String()
		acctComp.LocalReward = localReward.Lamports
		acctComp.LocalCommission = localReward.Commission
		// Debug info for ForceCreditsUpdate accounts
		acctComp.ForceCreditsReason = localReward.ForceCreditsReason
		acctComp.CreditsInVote = localReward.CreditsInVote
		acctComp.CreditsInStake = localReward.CreditsInStake
		acctComp.ActivationEpoch = localReward.ActivationEpoch
		acctComp.RewardedEpoch = localReward.RewardedEpoch

		// Get local account state
		if localAcct, ok := localAcctMap[pk]; ok {
			acctComp.LocalPostBalance = localAcct.Lamports
			// Try to deserialize to get CreditsObserved
			if stakeState, err := sealevel.UnmarshalStakeState(localAcct.Data); err == nil && stakeState.Status == sealevel.StakeStateV2StatusStake {
				acctComp.LocalCreditsObserved = stakeState.Stake.Stake.CreditsObserved
				acctComp.LocalStakeLamports = stakeState.Stake.Stake.Delegation.StakeLamports
			}
		}

		// Get RPC data
		if rpcData, ok := rpcRewards[pk]; ok {
			// RPC has this account - set pointer values
			acctComp.RpcReward = &rpcData.lamports
			acctComp.RpcPostBalance = &rpcData.postBalance
			acctComp.RpcCommission = &rpcData.commission
			delete(rpcRewards, pk) // Mark as processed

			// Compare (only when we have RPC data)
			rewardDiff := int64(acctComp.LocalReward) - int64(rpcData.lamports)
			postBalDiff := int64(acctComp.LocalPostBalance) - int64(rpcData.postBalance)
			commMatch := acctComp.LocalCommission == rpcData.commission
			acctComp.RewardDiff = &rewardDiff
			acctComp.PostBalanceDiff = &postBalDiff
			acctComp.CommissionMatch = &commMatch

			// Count mismatches
			if rewardDiff != 0 {
				comp.LamportsMismatch++
			}
			if postBalDiff != 0 {
				comp.PostBalMismatch++
			}
			if !commMatch {
				comp.CommissionMismatch++
			}

			// Is it a match?
			if rewardDiff == 0 && postBalDiff == 0 && commMatch {
				comp.MatchCount++
			} else {
				comp.DivergentAccounts = append(comp.DivergentAccounts, acctComp)
			}
		} else {
			// In local but not in RPC - RPC fields stay nil (will show as null in JSON)
			acctComp.InLocalOnly = true
			comp.InLocalOnlyCount++
			comp.DivergentAccounts = append(comp.DivergentAccounts, acctComp)
		}
	}

	// Remaining RPC rewards are in RPC but not local
	for pk, rpcData := range rpcRewards {
		comp.InRpcOnlyCount++
		rpcLamports := rpcData.lamports
		rpcPostBal := rpcData.postBalance
		rpcComm := rpcData.commission
		comp.DivergentAccounts = append(comp.DivergentAccounts, StakeAccountComparison{
			Pubkey:         pk.String(),
			RpcReward:      &rpcLamports,
			RpcPostBalance: &rpcPostBal,
			RpcCommission:  &rpcComm,
			InRpcOnly:      true,
			// Local fields stay at zero values (not pointers, so will show 0 not null)
		})
	}

	return comp
}

// WritePartitionStakeComparison writes the local vs RPC comparison to a file
func WritePartitionStakeComparison(comp *PartitionStakeComparison) {
	if comp == nil {
		return
	}

	diagDir := getDiagnosticsDir()
	filename := filepath.Join(diagDir, fmt.Sprintf("stake_comparison_slot_%d_p%d.json", comp.Slot, comp.PartitionIdx+1))

	data, err := json.MarshalIndent(comp, "", "  ")
	if err != nil {
		mlog.Log.Warnf("failed to marshal stake comparison: %v", err)
		return
	}

	if err := os.WriteFile(filename, data, 0644); err != nil {
		mlog.Log.Warnf("failed to write stake comparison: %v", err)
		return
	}

	// Log summary
	mlog.Log.Infof("STAKE_CMP P%d: local=%d rpc=%d match=%d lamports_diff=%d postbal_diff=%d commission_diff=%d local_only=%d rpc_only=%d",
		comp.PartitionIdx+1, comp.LocalCount, comp.RpcCount, comp.MatchCount,
		comp.LamportsMismatch, comp.PostBalMismatch, comp.CommissionMismatch,
		comp.InLocalOnlyCount, comp.InRpcOnlyCount)

	if len(comp.DivergentAccounts) > 0 {
		mlog.Log.Warnf("STAKE_CMP P%d: %d divergent accounts - see %s", comp.PartitionIdx+1, len(comp.DivergentAccounts), filename)
	}
}

// SlotModifiedAccount holds info about an account modified in a distribution slot
type SlotModifiedAccount struct {
	Pubkey       string `json:"pubkey"`
	Type         string `json:"type"` // "stake", "sysvar", "vote", "other"
	Owner        string `json:"owner"`
	Lamports     uint64 `json:"lamports"`
	DataLen      int    `json:"data_len"`
	IsNil        bool   `json:"is_nil,omitempty"`
}

// SlotModifiedAccountsDump holds all modified accounts for a distribution slot
type SlotModifiedAccountsDump struct {
	Slot           uint64                `json:"slot"`
	PartitionIdx   uint64                `json:"partition_idx"`
	TotalCount     int                   `json:"total_count"`
	StakeCount     int                   `json:"stake_count"`
	SysvarCount    int                   `json:"sysvar_count"`
	VoteCount      int                   `json:"vote_count"`
	OtherCount     int                   `json:"other_count"`
	NilCount       int                   `json:"nil_count"`
	Accounts       []SlotModifiedAccount `json:"accounts"`
}

// WriteSlotModifiedAccounts writes all modified accounts for a distribution slot to a JSON file
// This helps identify if non-stake accounts are causing divergence
func WriteSlotModifiedAccounts(slot, partitionIdx uint64, accounts []*accounts.Account) {
	if len(accounts) == 0 {
		return
	}

	// Only write for P10/P11 (partitionIdx 9 and 10)
	if partitionIdx != 9 && partitionIdx != 10 {
		return
	}

	diagDir := getDiagnosticsDir()
	filename := filepath.Join(diagDir, fmt.Sprintf("slot_modified_accounts_%d_p%d.json", slot, partitionIdx+1))

	dump := SlotModifiedAccountsDump{
		Slot:         slot,
		PartitionIdx: partitionIdx,
		TotalCount:   len(accounts),
	}

	for _, acct := range accounts {
		if acct == nil {
			dump.NilCount++
			dump.Accounts = append(dump.Accounts, SlotModifiedAccount{
				IsNil: true,
			})
			continue
		}

		// Categorize by owner
		owner := acct.Owner
		var acctType string
		switch owner {
		case addresses.StakeProgramAddr:
			acctType = "stake"
			dump.StakeCount++
		case addresses.VoteProgramAddr:
			acctType = "vote"
			dump.VoteCount++
		case addresses.SysvarOwnerAddr:
			acctType = "sysvar"
			dump.SysvarCount++
		default:
			acctType = "other"
			dump.OtherCount++
		}

		dump.Accounts = append(dump.Accounts, SlotModifiedAccount{
			Pubkey:   acct.Key.String(),
			Type:     acctType,
			Owner:    solana.PublicKeyFromBytes(owner[:]).String(),
			Lamports: acct.Lamports,
			DataLen:  len(acct.Data),
		})
	}

	data, err := json.MarshalIndent(dump, "", "  ")
	if err != nil {
		mlog.Log.Warnf("failed to marshal slot modified accounts: %v", err)
		return
	}

	if err := os.WriteFile(filename, data, 0644); err != nil {
		mlog.Log.Warnf("failed to write slot modified accounts: %v", err)
		return
	}

	mlog.Log.Infof("wrote slot modified accounts to %s (total=%d stake=%d sysvar=%d vote=%d other=%d nil=%d)",
		filename, dump.TotalCount, dump.StakeCount, dump.SysvarCount, dump.VoteCount, dump.OtherCount, dump.NilCount)
}

// AccountStateDiff shows local vs RPC difference for a single account
type AccountStateDiff struct {
	Pubkey             string  `json:"pubkey"`
	Field              string  `json:"field"`               // "credits_observed", "stake_lamports", "lamports"
	LocalValue         uint64  `json:"local_value"`
	RpcValue           uint64  `json:"rpc_value"`
	Diff               int64   `json:"diff"`
}

// PartitionStateDiff contains only the differences between local and RPC for a partition
type PartitionStateDiff struct {
	Slot              uint64             `json:"slot"`
	PartitionIdx      uint64             `json:"partition_idx"`
	LocalAccountCount int                `json:"local_account_count"`
	RpcFetchedCount   int                `json:"rpc_fetched_count"`
	RpcFailedCount    int                `json:"rpc_failed_count"`
	DiffCount         int                `json:"diff_count"`
	Diffs             []AccountStateDiff `json:"diffs,omitempty"`
}

// ComparePartitionStateToRpc fetches actual account state from RPC and compares to local
// Only outputs differences - if everything matches, diffs array is empty
func ComparePartitionStateToRpc(
	rpcc *rpcclient.RpcClient,
	slot uint64,
	partitionIdx uint64,
	localAccts []*accounts.Account,
) *PartitionStateDiff {
	if rpcc == nil {
		return nil
	}

	// Only for P10/P11
	if partitionIdx != 9 && partitionIdx != 10 {
		return nil
	}

	diff := &PartitionStateDiff{
		Slot:              slot,
		PartitionIdx:      partitionIdx,
		LocalAccountCount: len(localAccts),
	}

	for _, localAcct := range localAccts {
		if localAcct == nil {
			continue
		}

		// Fetch actual account state from RPC
		rpcAcctInfo, err := rpcc.GetClient().GetAccountInfo(rpcc.GetContext(), localAcct.Key)
		if err != nil || rpcAcctInfo == nil || rpcAcctInfo.Value == nil {
			diff.RpcFailedCount++
			continue
		}
		diff.RpcFetchedCount++

		rpcData := rpcAcctInfo.Value.Data.GetBinary()
		rpcLamports := rpcAcctInfo.Value.Lamports

		// Compare lamports
		if localAcct.Lamports != rpcLamports {
			diff.Diffs = append(diff.Diffs, AccountStateDiff{
				Pubkey:     localAcct.Key.String(),
				Field:      "lamports",
				LocalValue: localAcct.Lamports,
				RpcValue:   rpcLamports,
				Diff:       int64(localAcct.Lamports) - int64(rpcLamports),
			})
			diff.DiffCount++
		}

		// Deserialize both and compare stake state
		localStake, localErr := sealevel.UnmarshalStakeState(localAcct.Data)
		rpcStake, rpcErr := sealevel.UnmarshalStakeState(rpcData)

		if localErr == nil && rpcErr == nil &&
			localStake.Status == sealevel.StakeStateV2StatusStake &&
			rpcStake.Status == sealevel.StakeStateV2StatusStake {

			// Compare CreditsObserved
			localCredits := localStake.Stake.Stake.CreditsObserved
			rpcCredits := rpcStake.Stake.Stake.CreditsObserved
			if localCredits != rpcCredits {
				diff.Diffs = append(diff.Diffs, AccountStateDiff{
					Pubkey:     localAcct.Key.String(),
					Field:      "credits_observed",
					LocalValue: localCredits,
					RpcValue:   rpcCredits,
					Diff:       int64(localCredits) - int64(rpcCredits),
				})
				diff.DiffCount++
			}

			// Compare stake lamports
			localStakeLam := localStake.Stake.Stake.Delegation.StakeLamports
			rpcStakeLam := rpcStake.Stake.Stake.Delegation.StakeLamports
			if localStakeLam != rpcStakeLam {
				diff.Diffs = append(diff.Diffs, AccountStateDiff{
					Pubkey:     localAcct.Key.String(),
					Field:      "stake_lamports",
					LocalValue: localStakeLam,
					RpcValue:   rpcStakeLam,
					Diff:       int64(localStakeLam) - int64(rpcStakeLam),
				})
				diff.DiffCount++
			}
		}
	}

	return diff
}

// WritePartitionStateDiff writes the partition state diff to a JSON file
func WritePartitionStateDiff(diff *PartitionStateDiff) {
	if diff == nil {
		return
	}

	diagDir := getDiagnosticsDir()
	filename := filepath.Join(diagDir, fmt.Sprintf("partition_diff_slot_%d_p%d.json", diff.Slot, diff.PartitionIdx+1))

	data, err := json.MarshalIndent(diff, "", "  ")
	if err != nil {
		mlog.Log.Warnf("failed to marshal partition state diff: %v", err)
		return
	}

	if err := os.WriteFile(filename, data, 0644); err != nil {
		mlog.Log.Warnf("failed to write partition state diff: %v", err)
		return
	}

	if diff.DiffCount > 0 {
		mlog.Log.Warnf("PARTITION_DIFF P%d: %d differences found (fetched=%d failed=%d)",
			diff.PartitionIdx+1, diff.DiffCount, diff.RpcFetchedCount, diff.RpcFailedCount)
	} else {
		mlog.Log.Infof("PARTITION_DIFF P%d: no differences (fetched=%d failed=%d)",
			diff.PartitionIdx+1, diff.RpcFetchedCount, diff.RpcFailedCount)
	}
}
