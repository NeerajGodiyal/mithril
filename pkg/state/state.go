package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/mr-tron/base58"
)

const StateFileName = "mithril_state.json"

// MithrilState tracks the current state of the mithril node.
// The state file serves as an atomic marker of validity - AccountsDB is valid
// if and only if this file exists with Stage == "ready".
type MithrilState struct {
	Stage          string        `json:"stage"` // "ready", "downloading", "building", "corrupted"
	SnapshotSlot   uint64        `json:"snapshot_slot"`
	LastSlot       uint64        `json:"last_slot,omitempty"`
	LastBankhash   string        `json:"last_bankhash,omitempty"`
	FullSnapshot   *SnapshotInfo `json:"full_snapshot,omitempty"`
	IncrSnapshot   *SnapshotInfo `json:"incr_snapshot,omitempty"`
	BuildCompleted time.Time     `json:"build_completed_at,omitempty"`

	// Corruption tracking - set when integrity check fails
	CorruptionReason     string    `json:"corruption_reason,omitempty"`
	CorruptionDetectedAt time.Time `json:"corruption_detected_at,omitempty"`

	// Resume context - stored on shutdown to properly configure first block on resume
	// These fields capture the state at the end of the last successfully replayed slot
	LastAcctsLtHash          string `json:"last_accts_lt_hash,omitempty"`           // base64 encoded LtHash
	LastLamportsPerSignature uint64 `json:"last_lamports_per_sig,omitempty"`        // FeeRateGovernor.LamportsPerSignature
	LastPrevLamportsPerSig   uint64 `json:"last_prev_lamports_per_sig,omitempty"`   // FeeRateGovernor.PrevLamportsPerSignature
	LastNumSignatures        uint64 `json:"last_num_signatures,omitempty"`          // SlotCtx.NumSignatures

	// Blockhash context - required because appendvec file writes are not fsynced,
	// so RecentBlockhashes sysvar data in AccountsDB may be stale after restart.
	// These are all base58 encoded.
	LastRecentBlockhashes []BlockhashEntry `json:"last_recent_blockhashes,omitempty"` // 150 entries, newest first
	LastEvictedBlockhash  string           `json:"last_evicted_blockhash,omitempty"`  // 151st blockhash
	LastBlockhash         string           `json:"last_blockhash,omitempty"`          // blockhash of last replayed slot (parent for next)

	// Run tracking - for correlating logs with state
	LastRunID string    `json:"last_run_id,omitempty"` // Run ID from last replay session
	LastRunAt time.Time `json:"last_run_at,omitempty"` // When last replay session started
}

// BlockhashEntry represents a single entry in the RecentBlockhashes sysvar
type BlockhashEntry struct {
	Blockhash            string `json:"blockhash"`
	LamportsPerSignature uint64 `json:"lamports_per_sig"`
}

// SnapshotInfo contains metadata about a downloaded snapshot file.
type SnapshotInfo struct {
	Path     string `json:"path"`
	Slot     uint64 `json:"slot"`
	BaseSlot uint64 `json:"base_slot,omitempty"` // only for incrementals
}

// LoadState loads the state file from the accountsdb directory.
// Returns nil and no error if the file doesn't exist.
func LoadState(accountsDbDir string) (*MithrilState, error) {
	stateFile := filepath.Join(accountsDbDir, StateFileName)

	data, err := os.ReadFile(stateFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read state file: %w", err)
	}

	var state MithrilState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to parse state file: %w", err)
	}

	return &state, nil
}

// Save writes the state to the accountsdb directory.
func (s *MithrilState) Save(accountsDbDir string) error {
	stateFile := filepath.Join(accountsDbDir, StateFileName)

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	// Write to temp file first, then rename for atomicity
	tmpFile := stateFile + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write state file: %w", err)
	}

	if err := os.Rename(tmpFile, stateFile); err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("failed to rename state file: %w", err)
	}

	return nil
}

// IsReady returns true if the state indicates AccountsDB is valid and ready.
func (s *MithrilState) IsReady() bool {
	return s != nil && s.Stage == "ready"
}

// IsCorrupted returns true if the state indicates AccountsDB is corrupted.
func (s *MithrilState) IsCorrupted() bool {
	return s != nil && s.Stage == "corrupted"
}

// MarkCorrupted updates the state file to indicate AccountsDB is corrupted.
// This persists the corruption status so the next startup knows to rebuild.
func (s *MithrilState) MarkCorrupted(accountsDbDir string, reason string) error {
	s.Stage = "corrupted"
	s.CorruptionReason = reason
	s.CorruptionDetectedAt = time.Now()
	return s.Save(accountsDbDir)
}

// ResumeContext contains the context needed to properly resume replay from a saved state.
type ResumeContext struct {
	AcctsLtHash          string // base64 encoded
	LamportsPerSignature uint64
	PrevLamportsPerSig   uint64
	NumSignatures        uint64

	// Blockhash context
	RecentBlockhashes []BlockhashEntry // 150 entries, newest first
	EvictedBlockhash  string           // base58 encoded, 151st blockhash
	LastBlockhash     string           // base58 encoded, blockhash of last slot (parent for next)

	// Run tracking
	RunID        string    // Run ID for log correlation
	RunStartedAt time.Time // When this replay session started
}

// UpdateLastSlot updates the last slot and bankhash in the state file.
// This should be called after successfully committing a slot during replay.
func (s *MithrilState) UpdateLastSlot(accountsDbDir string, slot uint64, bankhash []byte) error {
	s.LastSlot = slot
	s.LastBankhash = base58.Encode(bankhash)
	return s.Save(accountsDbDir)
}

// UpdateLastSlotWithContext updates the last slot, bankhash, and resume context in the state file.
// This should be called on graceful shutdown to preserve the full context needed for resume.
func (s *MithrilState) UpdateLastSlotWithContext(accountsDbDir string, slot uint64, bankhash []byte, ctx *ResumeContext) error {
	s.LastSlot = slot
	s.LastBankhash = base58.Encode(bankhash)
	if ctx != nil {
		s.LastAcctsLtHash = ctx.AcctsLtHash
		s.LastLamportsPerSignature = ctx.LamportsPerSignature
		s.LastPrevLamportsPerSig = ctx.PrevLamportsPerSig
		s.LastNumSignatures = ctx.NumSignatures

		// Blockhash context - required because appendvec writes are not fsynced
		s.LastRecentBlockhashes = ctx.RecentBlockhashes
		s.LastEvictedBlockhash = ctx.EvictedBlockhash
		s.LastBlockhash = ctx.LastBlockhash

		// Run tracking - for correlating logs with state
		s.LastRunID = ctx.RunID
		s.LastRunAt = ctx.RunStartedAt
	}
	return s.Save(accountsDbDir)
}

// HasResumeContext returns true if the state has resume context stored.
// This indicates the state was saved during a graceful shutdown with full context.
func (s *MithrilState) HasResumeContext() bool {
	return s != nil && s.LastSlot > 0 && s.LastAcctsLtHash != ""
}

// GetResumeContext returns the stored resume context, or nil if not present.
func (s *MithrilState) GetResumeContext() *ResumeContext {
	if !s.HasResumeContext() {
		return nil
	}
	return &ResumeContext{
		AcctsLtHash:          s.LastAcctsLtHash,
		LamportsPerSignature: s.LastLamportsPerSignature,
		PrevLamportsPerSig:   s.LastPrevLamportsPerSig,
		NumSignatures:        s.LastNumSignatures,

		// Blockhash context
		RecentBlockhashes: s.LastRecentBlockhashes,
		EvictedBlockhash:  s.LastEvictedBlockhash,
		LastBlockhash:     s.LastBlockhash,

		// Run tracking (from previous session)
		RunID:        s.LastRunID,
		RunStartedAt: s.LastRunAt,
	}
}

// GetResumeSlot returns the slot to resume from.
// Returns LastSlot + 1 if replay has happened, otherwise SnapshotSlot + 1.
func (s *MithrilState) GetResumeSlot() uint64 {
	if s.LastSlot > 0 {
		return s.LastSlot + 1
	}
	return s.SnapshotSlot + 1
}

// GetCurrentSlot returns the most recent slot (LastSlot if replayed, else SnapshotSlot).
func (s *MithrilState) GetCurrentSlot() uint64 {
	if s.LastSlot > 0 {
		return s.LastSlot
	}
	return s.SnapshotSlot
}

// IsStale returns true if the state is significantly behind the given slot.
func (s *MithrilState) IsStale(latestSlot uint64, threshold uint64) bool {
	currentSlot := s.GetCurrentSlot()
	return latestSlot > currentSlot && (latestSlot-currentSlot) > threshold
}

// BankhashGetter is an interface for getting bankhashes by slot.
// This is used for integrity validation without creating import cycles.
type BankhashGetter interface {
	GetBankHashForSlot(slot uint64) ([]byte, error)
}

// ValidateAgainstBankhashDB checks if AccountsDB has been modified beyond state file.
// This detects cases where the process was killed (Ctrl+Z, kill -9) without
// updating the state file, leaving AccountsDB in an inconsistent state.
func (s *MithrilState) ValidateAgainstBankhashDB(bankhashDb BankhashGetter) error {
	if s.LastSlot == 0 {
		// No replay happened according to state file
		// Check if bankhash exists for snapshot_slot + 1
		checkSlot := s.SnapshotSlot + 1
		bankhash, err := bankhashDb.GetBankHashForSlot(checkSlot)
		if err == nil && len(bankhash) > 0 {
			return fmt.Errorf("state file shows no replay (last_slot=0), but bankhash_db has entry for slot %d - AccountsDB may be corrupted", checkSlot)
		}
	} else {
		// Replay happened, check for slots beyond last_slot
		checkSlot := s.LastSlot + 1
		bankhash, err := bankhashDb.GetBankHashForSlot(checkSlot)
		if err == nil && len(bankhash) > 0 {
			return fmt.Errorf("state file shows last_slot=%d, but bankhash_db has entry for slot %d - AccountsDB may be corrupted", s.LastSlot, checkSlot)
		}

		// Also verify last_slot's bankhash matches
		expectedBankhash, err := bankhashDb.GetBankHashForSlot(s.LastSlot)
		if err != nil || len(expectedBankhash) == 0 {
			return fmt.Errorf("state file shows last_slot=%d, but no bankhash found in bankhash_db", s.LastSlot)
		}
		if s.LastBankhash != "" && base58.Encode(expectedBankhash) != s.LastBankhash {
			return fmt.Errorf("bankhash mismatch for slot %d: state file has %s, bankhash_db has %s",
				s.LastSlot, s.LastBankhash, base58.Encode(expectedBankhash))
		}
	}
	return nil
}

// NewReadyState creates a new state marking the AccountsDB as ready.
func NewReadyState(snapshotSlot uint64, fullSnapshotPath string, incrSnapshotPath string, incrBaseSlot uint64, incrSlot uint64) *MithrilState {
	state := &MithrilState{
		Stage:          "ready",
		SnapshotSlot:   snapshotSlot,
		BuildCompleted: time.Now(),
	}

	if fullSnapshotPath != "" {
		state.FullSnapshot = &SnapshotInfo{
			Path: fullSnapshotPath,
			Slot: snapshotSlot,
		}
	}

	if incrSnapshotPath != "" {
		state.IncrSnapshot = &SnapshotInfo{
			Path:     incrSnapshotPath,
			BaseSlot: incrBaseSlot,
			Slot:     incrSlot,
		}
	}

	return state
}

// DeleteState removes the state file from the accountsdb directory.
func DeleteState(accountsDbDir string) error {
	stateFile := filepath.Join(accountsDbDir, StateFileName)
	err := os.Remove(stateFile)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete state file: %w", err)
	}
	return nil
}

// ValidateAccountsDbArtifacts checks if expected AccountsDB artifacts exist.
// This provides an extra layer of validation beyond just checking the state file.
func ValidateAccountsDbArtifacts(accountsDbDir string) error {
	requiredFiles := []string{
		"mithril_db",
		"bankhash_db",
		"accounts",
		"largest_file_id",
		"bank_hash",
		"manifest",
	}

	for _, file := range requiredFiles {
		path := filepath.Join(accountsDbDir, file)
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("missing required artifact: %s", file)
			}
			return fmt.Errorf("error checking artifact %s: %w", file, err)
		}
	}

	return nil
}

// CheckAndLoadValidState loads state and validates that AccountsDB is ready.
// Returns (state, nil) if valid, or (nil, nil) if state is invalid/missing.
// Returns (nil, error) only for unexpected errors.
func CheckAndLoadValidState(accountsDbDir string) (*MithrilState, error) {
	state, err := LoadState(accountsDbDir)
	if err != nil {
		mlog.Log.Infof("error loading state file: %v", err)
		return nil, nil
	}

	if state == nil {
		mlog.Log.Infof("no state file found in %s", accountsDbDir)
		return nil, nil
	}

	// Handle corrupted state with specific logging
	if state.IsCorrupted() {
		mlog.Log.Infof("previous corruption detected: %s", state.CorruptionReason)
		return nil, nil
	}

	if !state.IsReady() {
		mlog.Log.Infof("state file exists but stage is %q (not ready)", state.Stage)
		return nil, nil
	}

	// Extra validation: check that artifacts actually exist
	if err := ValidateAccountsDbArtifacts(accountsDbDir); err != nil {
		mlog.Log.Infof("state file says ready but artifacts invalid: %v", err)
		return nil, nil
	}

	mlog.Log.Infof("valid state found: snapshot_slot=%d, last_slot=%d", state.SnapshotSlot, state.LastSlot)
	return state, nil
}
