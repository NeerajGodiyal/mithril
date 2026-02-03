package replay

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	a "github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/base58"
	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/config"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

// RecordedTxLoop contains all state needed to replay a TxLoop offline.
type RecordedTxLoop struct {
	// Metadata
	Slot       uint64 `json:"slot"`
	ParentSlot uint64 `json:"parent_slot"`
	Epoch      uint64 `json:"epoch"`

	// Blockhashes (base58 encoded)
	Blockhash              string `json:"blockhash"`
	LastBlockhash          string `json:"last_blockhash"`
	LatestEvictedBlockhash string `json:"latest_evicted_blockhash"`

	// Fee state
	FeeRateGovernor *sealevel.FeeRateGovernor `json:"fee_rate_governor"`

	// Features (map of feature name -> activation slot)
	ActiveFeatures map[string]uint64 `json:"active_features"`

	// Accounts (pubkey base58 -> account)
	Accounts map[string]*accounts.Account `json:"accounts"`

	// Transactions
	Transactions []*solana.Transaction `json:"transactions"`

	// TxMetas for verification (optional, can be null entries)
	// Contains per-tx: Err (success/fail), ComputeUnitsConsumed, Pre/PostBalances
	TxMetas []*rpc.TransactionMeta `json:"tx_metas,omitempty"`

	// Address Lookup Tables - maps ALT pubkey to list of addresses in the table
	// Needed to re-resolve transactions after JSON deserialization
	AddressTables map[solana.PublicKey]solana.PublicKeySlice `json:"address_tables,omitempty"`

	// Expected post-TxLoop account states for modified accounts (full data, not just lamports)
	ExpectedModifiedAccounts map[solana.PublicKey]*accounts.Account `json:"expected_modified_accounts,omitempty"`

	// Block contains the unresolved block for parallel replay scheduling.
	// This is stored separately from Transactions/TxMetas because it preserves
	// the unresolved transaction state needed by the topsort planner.
	Block *b.Block `json:"block,omitempty"`
}

// RecordTxLoopSlots maps slot numbers to output file paths for TxLoop recording.
// Populated from config via InitTxLoopRecording() or set programmatically.
var RecordTxLoopSlots = map[uint64]string{}

// InitTxLoopRecording loads TxLoop recording configuration from [replay.txloop_record].
// Call this during startup after config is loaded.
func InitTxLoopRecording() {
	txloopRecord := config.GetStringMapString("replay.txloop_record")
	if len(txloopRecord) == 0 {
		return
	}

	for slotStr, path := range txloopRecord {
		slot, err := strconv.ParseUint(slotStr, 10, 64)
		if err != nil {
			mlog.Log.Warnf("Invalid slot number in replay.txloop_record: %q", slotStr)
			continue
		}
		RecordTxLoopSlots[slot] = path
		mlog.Log.Infof("TxLoop recording enabled for slot %d -> %s", slot, path)
	}
}

// CapturePreTxLoopState captures the state needed for TxLoop replay.
// Call this right before TxLoop starts in ProcessBlock.
// The block parameter should be the unresolved block (before ALT resolution).
func CapturePreTxLoopState(
	slotCtx *sealevel.SlotCtx,
	block *b.Block,
	addressTables map[solana.PublicKey]solana.PublicKeySlice,
) *RecordedTxLoop {
	acctsMap := accountsToMap(slotCtx.Accounts)

	// Also capture ProgramData accounts for BPFLoaderUpgradeable programs
	captureProgramDataAccounts(slotCtx, acctsMap)

	record := &RecordedTxLoop{
		Slot:                   slotCtx.Slot,
		ParentSlot:             slotCtx.ParentSlot,
		Epoch:                  slotCtx.Epoch,
		Blockhash:              base58.Encode(slotCtx.Blockhash[:]),
		LastBlockhash:          base58.Encode(slotCtx.LastBlockhash[:]),
		LatestEvictedBlockhash: base58.Encode(slotCtx.LatestEvictedBlockhash[:]),
		FeeRateGovernor:        slotCtx.FeeRateGovernor,
		ActiveFeatures:         featuresToJSON(slotCtx.Features),
		Accounts:               acctsMap,
		Transactions:           block.Transactions,
		TxMetas:                block.TxMetas,
		AddressTables:          addressTables,
		Block:                  block,
	}
	return record
}

// captureProgramDataAccounts finds all BPFLoaderUpgradeable program accounts and fetches
// their associated ProgramData accounts from AccountsDb. This is needed because the
// ProgramData accounts contain the actual program bytecode but aren't directly referenced
// in transactions.
func captureProgramDataAccounts(slotCtx *sealevel.SlotCtx, acctsMap map[string]*accounts.Account) {
	if slotCtx.AccountsDb == nil {
		return
	}

	bpfLoaderUpgradeableAddr := solana.PublicKey(a.BpfLoaderUpgradeableAddr)

	for _, acct := range acctsMap {
		// Skip non-executable accounts
		if !acct.Executable {
			continue
		}
		// Only process BPFLoaderUpgradeable programs
		if acct.Owner != bpfLoaderUpgradeableAddr {
			continue
		}

		// Parse the program account to get the ProgramData address
		state, err := sealevel.UnmarshalUpgradeableLoaderState(acct.Data)
		if err != nil {
			mlog.Log.Warnf("Failed to unmarshal upgradeable loader state for %s: %v", base58.Encode(acct.Key[:]), err)
			continue
		}
		// Check if this is a Program state (not Buffer, ProgramData, or Uninitialized)
		if state.Type != sealevel.UpgradeableLoaderStateTypeProgram {
			continue
		}

		programDataAddr := state.Program.ProgramDataAddress
		programDataKey := base58.Encode(programDataAddr[:])

		// Skip if already in map
		if _, exists := acctsMap[programDataKey]; exists {
			continue
		}

		// Fetch from AccountsDb (use parent slot to get state before current slot's TxLoop)
		programDataAcct, err := slotCtx.AccountsDb.GetAccount(slotCtx.ParentSlot, programDataAddr)
		if err != nil {
			mlog.Log.Warnf("Failed to fetch ProgramData account %s: %v", programDataKey, err)
			continue
		}

		acctsMap[programDataKey] = programDataAcct.Clone()
	}
}

// SaveRecordedTxLoop saves the recorded state to a JSON file.
func SaveRecordedTxLoop(path string, record *RecordedTxLoop) error {
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create file %s: %w", path, err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(record); err != nil {
		return fmt.Errorf("failed to encode record: %w", err)
	}
	return nil
}

// LoadRecordedTxLoop loads a recorded TxLoop state from a JSON file.
func LoadRecordedTxLoop(path string) (*RecordedTxLoop, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open file %s: %w", path, err)
	}
	defer file.Close()

	var record RecordedTxLoop
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&record); err != nil {
		return nil, fmt.Errorf("failed to decode record: %w", err)
	}
	return &record, nil
}

func featuresToJSON(f *features.Features) map[string]uint64 {
	if f == nil {
		return nil
	}
	result := make(map[string]uint64)
	for gate, info := range *f {
		if info.Enabled {
			result[gate.Name] = info.ActivationSlot
		}
	}
	return result
}

func featuresFromJSON(activeFeatures map[string]uint64) *features.Features {
	f := features.NewFeaturesDefault()
	// Build a lookup from name to gate
	nameToGate := make(map[string]features.FeatureGate)
	for _, gate := range features.AllFeatureGates {
		nameToGate[gate.Name] = gate
	}
	for name, slot := range activeFeatures {
		if gate, ok := nameToGate[name]; ok {
			f.EnableFeature(gate, slot)
		}
	}
	return f
}

func accountsToMap(accts accounts.Accounts) map[string]*accounts.Account {
	result := make(map[string]*accounts.Account)
	for _, acct := range accts.AllAccounts() {
		key := base58.Encode(acct.Key[:])
		// Clone to capture PRE-TxLoop state (accounts are modified in place during TxLoop)
		result[key] = acct.Clone()
	}
	return result
}

func accountsFromMap(acctsMap map[string]*accounts.Account) accounts.Accounts {
	memAccts := accounts.NewMemAccountsWithLen(uint64(len(acctsMap)))
	for _, acct := range acctsMap {
		memAccts.SetAccountWithoutLock(acct.Key, acct)
	}
	return memAccts
}

// CapturePostTxLoopState captures the modified accounts after TxLoop completes.
// Call this right after TxLoop finishes in ProcessBlock.
func CapturePostTxLoopState(record *RecordedTxLoop, slotCtx *sealevel.SlotCtx) {
	record.ExpectedModifiedAccounts = make(map[solana.PublicKey]*accounts.Account)
	for pk := range slotCtx.ModifiedAccts {
		acct, err := slotCtx.GetAccount(pk)
		if err != nil {
			continue
		}
		record.ExpectedModifiedAccounts[pk] = acct.Clone()
	}
}

