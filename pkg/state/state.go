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
const HistoryFileName = "mithril_state.history.jsonl"

// CurrentStateSchemaVersion is the current version of the state file format.
// Increment this when making breaking changes to the state file structure.
const CurrentStateSchemaVersion uint32 = 1

// MithrilState tracks the current state of the mithril node.
// The state file serves as an atomic marker of validity - AccountsDB is valid
// if and only if this file exists with Stage == "ready".
type MithrilState struct {
	// Schema version for state file format migrations
	StateSchemaVersion uint32 `json:"state_schema_version"`

	Stage          string        `json:"stage"` // "ready", "downloading", "building", "corrupted"
	SnapshotSlot   uint64        `json:"snapshot_slot"`
	SnapshotEpoch  uint64        `json:"snapshot_epoch,omitempty"`
	LastSlot       uint64        `json:"last_slot,omitempty"`
	LastEpoch      uint64        `json:"last_epoch,omitempty"`
	LastBankhash   string        `json:"last_bankhash,omitempty"`
	FullSnapshot   *SnapshotInfo `json:"full_snapshot,omitempty"`
	IncrSnapshot   *SnapshotInfo `json:"incr_snapshot,omitempty"`
	BuildCompleted time.Time     `json:"build_completed_at,omitempty"`

	// Build metadata - tracks how and when AccountsDB was built
	BuildStartedAt time.Time `json:"build_started_at,omitempty"` // When bootstrap started
	BuildMode      string    `json:"build_mode,omitempty"`       // "auto", "snapshot", "new-snapshot", "accountsdb"

	// Cluster safety - prevents mainnet/testnet mixups
	Cluster     string `json:"cluster,omitempty"`      // "mainnet-beta", "testnet", "devnet"
	GenesisHash string `json:"genesis_hash,omitempty"` // Base58 genesis hash from RPC

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

	// SlotHashes context - same issue as RecentBlockhashes, appendvec writes not fsynced.
	// SlotHashes is used by vote program to verify vote slot→hash mappings.
	LastSlotHashes []SlotHashEntry `json:"last_slot_hashes,omitempty"` // up to 512 entries, newest first

	// Run tracking - for correlating logs with state
	LastRunID string    `json:"last_run_id,omitempty"` // Run ID from last replay session
	LastRunAt time.Time `json:"last_run_at,omitempty"` // When last replay session started

	// Writer info - tracks which binary version last wrote to this state
	LastWriterVersion string `json:"last_writer_version,omitempty"` // Semver tag (e.g., "v0.1.0" or "dev")
	LastWriterCommit  string `json:"last_writer_commit,omitempty"`  // Git commit hash of writer binary

	// Legacy field name - kept for backwards compatibility during migration
	// TODO: Remove after v1.0 release
	LastCommit string `json:"last_commit,omitempty"` // Deprecated: use last_writer_commit

	// Shutdown tracking - records why the last session ended
	LastShutdownReason string    `json:"last_shutdown_reason,omitempty"` // human-readable reason
	LastShutdownAt     time.Time `json:"last_shutdown_at,omitempty"`     // when shutdown occurred
}

// Shutdown reason constants - these are stored in the state file and should be
// human-readable without needing to look up what they mean.
const (
	ShutdownReasonNormal         = "graceful shutdown (Ctrl+C)"
	ShutdownReasonStall          = "block fetch stalled - no RPC progress for 5+ minutes"
	ShutdownReasonLeaderSchedule = "leader schedule fetch failed from all RPC endpoints"
	ShutdownReasonError          = "replay error"       // Will be suffixed with actual error
	ShutdownReasonCompleted      = "replay completed - reached end slot"
)

// BlockhashEntry represents a single entry in the RecentBlockhashes sysvar
type BlockhashEntry struct {
	Blockhash            string `json:"blockhash"`
	LamportsPerSignature uint64 `json:"lamports_per_sig"`
}

// SlotHashEntry represents a single entry in the SlotHashes sysvar
type SlotHashEntry struct {
	Slot uint64 `json:"slot"`
	Hash string `json:"hash"` // base58 encoded
}

// SnapshotInfo contains metadata about a downloaded snapshot file.
type SnapshotInfo struct {
	Path     string `json:"path"`
	Slot     uint64 `json:"slot"`
	BaseSlot uint64 `json:"base_slot,omitempty"` // only for incrementals
}

// LoadState loads the state file from the accountsdb directory.
// Returns nil and no error if the file doesn't exist.
// Handles migration from older schema versions automatically.
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

	// Migrate from older schema versions
	if state.StateSchemaVersion == 0 {
		// Version 0 → 1 migration:
		// - Migrate LastCommit to LastWriterCommit
		if state.LastCommit != "" && state.LastWriterCommit == "" {
			state.LastWriterCommit = state.LastCommit
		}
		state.StateSchemaVersion = 1
		// Note: We don't save here - the state will be saved on next update
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
	Epoch                uint64 // epoch of the last replayed slot

	// Blockhash context
	RecentBlockhashes []BlockhashEntry // 150 entries, newest first
	EvictedBlockhash  string           // base58 encoded, 151st blockhash
	LastBlockhash     string           // base58 encoded, blockhash of last slot (parent for next)

	// SlotHashes context - vote program uses this to verify slot→hash mappings
	SlotHashes []SlotHashEntry // up to 512 entries, newest first

	// Run tracking
	RunID        string    // Run ID for log correlation
	RunStartedAt time.Time // When this replay session started

	// Writer info
	WriterVersion string // Semver tag (e.g., "v0.1.0" or "dev")
	WriterCommit  string // Git commit hash

	// Shutdown tracking
	ShutdownReason string // Why the session ended (see ShutdownReason* constants)
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
		s.LastEpoch = ctx.Epoch

		// Blockhash context - required because appendvec writes are not fsynced
		s.LastRecentBlockhashes = ctx.RecentBlockhashes
		s.LastEvictedBlockhash = ctx.EvictedBlockhash
		s.LastBlockhash = ctx.LastBlockhash

		// SlotHashes context - same issue, vote program needs accurate slot→hash mappings
		s.LastSlotHashes = ctx.SlotHashes

		// Run tracking - for correlating logs with state
		s.LastRunID = ctx.RunID
		s.LastRunAt = ctx.RunStartedAt

		// Writer info
		s.LastWriterVersion = ctx.WriterVersion
		s.LastWriterCommit = ctx.WriterCommit
		// Also set legacy field for backwards compatibility
		s.LastCommit = ctx.WriterCommit

		// Shutdown tracking
		if ctx.ShutdownReason != "" {
			s.LastShutdownReason = ctx.ShutdownReason
			s.LastShutdownAt = time.Now()
		}
	}

	// Ensure schema version is set
	s.StateSchemaVersion = CurrentStateSchemaVersion

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
		Epoch:                s.LastEpoch,

		// Blockhash context
		RecentBlockhashes: s.LastRecentBlockhashes,
		EvictedBlockhash:  s.LastEvictedBlockhash,
		LastBlockhash:     s.LastBlockhash,

		// SlotHashes context
		SlotHashes: s.LastSlotHashes,

		// Run tracking (from previous session)
		RunID:        s.LastRunID,
		RunStartedAt: s.LastRunAt,

		// Writer info (from previous session)
		WriterVersion: s.LastWriterVersion,
		WriterCommit:  s.getWriterCommit(),
	}
}

// getWriterCommit returns the writer commit, preferring the new field but falling back to legacy.
func (s *MithrilState) getWriterCommit() string {
	if s.LastWriterCommit != "" {
		return s.LastWriterCommit
	}
	return s.LastCommit // Legacy field fallback
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

// NewReadyStateOpts contains options for creating a new ready state.
type NewReadyStateOpts struct {
	SnapshotSlot     uint64
	SnapshotEpoch    uint64
	FullSnapshotPath string
	IncrSnapshotPath string
	IncrBaseSlot     uint64
	IncrSlot         uint64
	BuildMode        string // "auto", "snapshot", "new-snapshot", "accountsdb"
	BuildStartedAt   time.Time
	Cluster          string // "mainnet-beta", "testnet", "devnet"
	GenesisHash      string // Base58 genesis hash
	WriterVersion    string // Semver tag
	WriterCommit     string // Git commit hash
}

// NewReadyState creates a new state marking the AccountsDB as ready.
func NewReadyState(snapshotSlot uint64, snapshotEpoch uint64, fullSnapshotPath string, incrSnapshotPath string, incrBaseSlot uint64, incrSlot uint64) *MithrilState {
	return NewReadyStateWithOpts(NewReadyStateOpts{
		SnapshotSlot:     snapshotSlot,
		SnapshotEpoch:    snapshotEpoch,
		FullSnapshotPath: fullSnapshotPath,
		IncrSnapshotPath: incrSnapshotPath,
		IncrBaseSlot:     incrBaseSlot,
		IncrSlot:         incrSlot,
	})
}

// NewReadyStateWithOpts creates a new state with full options including cluster and version info.
func NewReadyStateWithOpts(opts NewReadyStateOpts) *MithrilState {
	state := &MithrilState{
		StateSchemaVersion: CurrentStateSchemaVersion,
		Stage:              "ready",
		SnapshotSlot:       opts.SnapshotSlot,
		SnapshotEpoch:      opts.SnapshotEpoch,
		BuildCompleted:     time.Now(),
		BuildStartedAt:     opts.BuildStartedAt,
		BuildMode:          opts.BuildMode,
		Cluster:            opts.Cluster,
		GenesisHash:        opts.GenesisHash,
		LastWriterVersion:  opts.WriterVersion,
		LastWriterCommit:   opts.WriterCommit,
		LastCommit:         opts.WriterCommit, // Also set legacy field
	}

	if opts.FullSnapshotPath != "" {
		state.FullSnapshot = &SnapshotInfo{
			Path: opts.FullSnapshotPath,
			Slot: opts.SnapshotSlot,
		}
	}

	if opts.IncrSnapshotPath != "" {
		state.IncrSnapshot = &SnapshotInfo{
			Path:     opts.IncrSnapshotPath,
			BaseSlot: opts.IncrBaseSlot,
			Slot:     opts.IncrSlot,
		}
	}

	return state
}

// ValidateGenesisHash checks if the stored genesis hash matches the expected value.
// Returns an error if there's a mismatch (prevents mainnet/testnet mixups).
// Returns nil if state has no genesis hash stored (first run after upgrade).
func (s *MithrilState) ValidateGenesisHash(expectedGenesisHash string) error {
	if s.GenesisHash == "" {
		// No genesis hash stored (older state file) - allow but log warning
		return nil
	}
	if s.GenesisHash != expectedGenesisHash {
		return fmt.Errorf("genesis hash mismatch: state has %s, RPC returned %s - refusing to use AccountsDB built for a different cluster",
			s.GenesisHash, expectedGenesisHash)
	}
	return nil
}

// SetClusterInfo updates the cluster and genesis hash in the state.
// Should be called on first run after upgrade if genesis hash is missing.
func (s *MithrilState) SetClusterInfo(cluster, genesisHash string) {
	s.Cluster = cluster
	s.GenesisHash = genesisHash
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

	return state, nil
}
