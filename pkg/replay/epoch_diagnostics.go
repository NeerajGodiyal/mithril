package replay

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Overclock-Validator/mithril/pkg/base58"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
)

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

	// Sysvars
	Clock        *ClockDiag        `json:"clock,omitempty"`
	EpochRewards *EpochRewardsDiag `json:"epoch_rewards,omitempty"`
	SlotHashes   *SlotHashesDiag   `json:"slot_hashes,omitempty"`
	StakeHistory *StakeHistoryDiag `json:"stake_history,omitempty"`
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

// DumpLocalSysvarState dumps the local sysvar state to a JSON file
func DumpLocalSysvarState(slot uint64, epoch uint64, bankHash []byte, parentHash [32]byte, numSigs uint64, blockHash [32]byte, ltHash []byte) {
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
		diag.LtHash = base58.Encode(ltHash)
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
