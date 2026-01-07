// This package acts as a singleton for storage and retrieval of data shared between replay and RPC

package global

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/Overclock-Validator/mithril/pkg/epochstakes"
	"github.com/Overclock-Validator/mithril/pkg/forkchoice"
	"github.com/Overclock-Validator/mithril/pkg/leaderschedule"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
)

const (
	// StakeCacheFileName is the filename for persisted stake cache.
	// Currently uses JSON for easier debugging. Consider binary format
	// later for performance if cache size becomes a bottleneck.
	StakeCacheFileName = "stake_cache.json"

	// StakePubkeyIndexFileName is the binary index of stake account pubkeys.
	// Append-only during replay, rewritten at epoch boundaries.
	// Each entry is 32 bytes (raw pubkey). Used for fast cache rebuild
	// when stake_cache.json is stale.
	StakePubkeyIndexFileName = "stake_pubkeys.idx"
)

type GlobalCtx struct {
	latestBlockhash            [32]byte
	blockHeight                uint64
	slot                       uint64
	epoch                      uint64
	transactionCount           uint64
	stakeCache                 map[solana.PublicKey]*sealevel.Delegation
	stakeCacheSlots            map[solana.PublicKey]uint64 // Tracks which slot each stake cache entry came from
	pendingNewStakePubkeys     []solana.PublicKey          // New stake pubkeys to append to index after block commit
	voteCache                  map[solana.PublicKey]*sealevel.VoteStateVersions
	epochStakes                *epochstakes.EpochStakesCache
	epochAuthorizedVoters      *epochstakes.EpochAuthorizedVotersCache
	forkChoice                 *forkchoice.ForkChoiceService
	slotsConfirmed             map[uint64]struct{}
	leaderSchedule             *leaderschedule.LeaderSchedule
	calcUnixTimeForClockSysvar bool
	manageLeaderSchedule       bool
	manageBlockHeight          bool
	stakeCacheMutex            sync.Mutex
	voteCacheMutex             sync.Mutex
	slotsConfirmedMutex        sync.Mutex
	mu                         sync.Mutex
}

var instance GlobalCtx

func SetLatestBlockHash(blockHash [32]byte) {
	instance.SetLatestBlockhash(blockHash)
}

func SetBlockHeight(blockHeight uint64) {
	instance.SetBlockHeight(blockHeight)
}

func SetSlot(slot uint64) {
	instance.SetSlot(slot)
}

func SetEpoch(epoch uint64) {
	instance.SetEpoch(epoch)
}

func SetForkChoice(forkChoice *forkchoice.ForkChoiceService) {
	instance.forkChoice = forkChoice
}

func SubmitBlockToForkChoiceService(slot uint64, txs []*solana.Transaction) {
	instance.forkChoice.SubmitBlock(slot, txs)
}

func BankhashConfirmedForSlot(slot uint64, bankHash solana.Hash) int {
	return instance.forkChoice.IsBankhashCorrect(slot, bankHash)
}

func IncrTransactionCount(num uint64) {
	instance.IncrTransactionCount(num)
}

// PutStakeCacheItem adds or updates a stake cache entry without slot tracking.
// Use this during replay where operations are strictly sequential.
// If this is a new pubkey (not already in cache), it's added to pendingNewStakePubkeys
// for later append to the index file.
func PutStakeCacheItem(pubkey solana.PublicKey, delegation *sealevel.Delegation) {
	instance.stakeCacheMutex.Lock()
	defer instance.stakeCacheMutex.Unlock()
	if instance.stakeCache == nil {
		instance.stakeCache = make(map[solana.PublicKey]*sealevel.Delegation)
	}
	if instance.stakeCacheSlots == nil {
		instance.stakeCacheSlots = make(map[solana.PublicKey]uint64)
	}
	// Track new pubkeys for index append
	_, exists := instance.stakeCache[pubkey]
	if !exists {
		instance.pendingNewStakePubkeys = append(instance.pendingNewStakePubkeys, pubkey)
	}
	instance.stakeCache[pubkey] = delegation
	// During replay, always use max slot to indicate "most recent"
	instance.stakeCacheSlots[pubkey] = ^uint64(0)
}

// PutStakeCacheItemBulk adds a stake cache entry during bulk population.
// Does NOT track new pubkeys (avoids enqueuing entire cache on rebuild).
// Use this when loading cache from hints, snapshot, or AccountsDB scan.
func PutStakeCacheItemBulk(pubkey solana.PublicKey, delegation *sealevel.Delegation) {
	instance.stakeCacheMutex.Lock()
	defer instance.stakeCacheMutex.Unlock()
	if instance.stakeCache == nil {
		instance.stakeCache = make(map[solana.PublicKey]*sealevel.Delegation)
	}
	if instance.stakeCacheSlots == nil {
		instance.stakeCacheSlots = make(map[solana.PublicKey]uint64)
	}
	instance.stakeCache[pubkey] = delegation
	instance.stakeCacheSlots[pubkey] = ^uint64(0)
}

// PutStakeCacheItemWithSlot adds or updates a stake cache entry with slot tracking.
// Only updates if slot >= existing slot (latest wins). Use during parallel snapshot loading.
func PutStakeCacheItemWithSlot(pubkey solana.PublicKey, delegation *sealevel.Delegation, slot uint64) {
	instance.stakeCacheMutex.Lock()
	defer instance.stakeCacheMutex.Unlock()
	if instance.stakeCache == nil {
		instance.stakeCache = make(map[solana.PublicKey]*sealevel.Delegation)
	}
	if instance.stakeCacheSlots == nil {
		instance.stakeCacheSlots = make(map[solana.PublicKey]uint64)
	}
	// Only update if this slot is >= existing slot (latest wins)
	existingSlot, exists := instance.stakeCacheSlots[pubkey]
	if !exists || slot >= existingSlot {
		instance.stakeCache[pubkey] = delegation
		instance.stakeCacheSlots[pubkey] = slot
	}
}

// StakeCacheBatchEntry represents a single entry for batch insertion into the stake cache.
type StakeCacheBatchEntry struct {
	Pubkey     solana.PublicKey
	Delegation *sealevel.Delegation
	Slot       uint64
}

// PutStakeCacheItemsBatch inserts multiple stake cache entries under a single lock.
// Use this during parallel appendvec processing to avoid per-entry locking overhead.
// Only updates if slot >= existing slot (latest wins).
func PutStakeCacheItemsBatch(entries []StakeCacheBatchEntry) {
	if len(entries) == 0 {
		return
	}
	instance.stakeCacheMutex.Lock()
	defer instance.stakeCacheMutex.Unlock()
	if instance.stakeCache == nil {
		instance.stakeCache = make(map[solana.PublicKey]*sealevel.Delegation)
	}
	if instance.stakeCacheSlots == nil {
		instance.stakeCacheSlots = make(map[solana.PublicKey]uint64)
	}
	for _, entry := range entries {
		existingSlot, exists := instance.stakeCacheSlots[entry.Pubkey]
		if !exists || entry.Slot >= existingSlot {
			instance.stakeCache[entry.Pubkey] = entry.Delegation
			instance.stakeCacheSlots[entry.Pubkey] = entry.Slot
		}
	}
}

func DeleteStakeCacheItem(pubkey solana.PublicKey) {
	instance.stakeCacheMutex.Lock()
	defer instance.stakeCacheMutex.Unlock()
	delete(instance.stakeCache, pubkey)
	delete(instance.stakeCacheSlots, pubkey)
}

func PutEpochAuthorizedVoter(voteAcct solana.PublicKey, authorizedVoter solana.PublicKey) {
	if instance.epochAuthorizedVoters == nil {
		instance.epochAuthorizedVoters = epochstakes.NewEpochAuthorizedVotersCache()
	}
	instance.epochAuthorizedVoters.PutEntry(voteAcct, authorizedVoter)
}

func EpochAuthorizedVoters() *epochstakes.EpochAuthorizedVotersCache {
	return instance.epochAuthorizedVoters
}

func PutSlotConfirmed(slot uint64) {
	if instance.slotsConfirmed == nil {
		instance.slotsConfirmed = make(map[uint64]struct{})
	}
	instance.slotsConfirmedMutex.Lock()
	defer instance.slotsConfirmedMutex.Unlock()
	instance.slotsConfirmed[slot] = struct{}{}
}

func SlotConfirmed(slot uint64) bool {
	instance.slotsConfirmedMutex.Lock()
	defer instance.slotsConfirmedMutex.Unlock()
	_, exists := instance.slotsConfirmed[slot]
	return exists
}

func StakeCache() map[solana.PublicKey]*sealevel.Delegation {
	return instance.stakeCache
}

func HasStakeCacheItem(pubkey solana.PublicKey) bool {
	instance.stakeCacheMutex.Lock()
	defer instance.stakeCacheMutex.Unlock()
	_, exists := instance.stakeCache[pubkey]
	return exists
}

func PutVoteCacheItem(pubkey solana.PublicKey, voteState *sealevel.VoteStateVersions) {
	if instance.voteCache == nil {
		instance.voteCache = make(map[solana.PublicKey]*sealevel.VoteStateVersions)
	}
	instance.voteCacheMutex.Lock()
	defer instance.voteCacheMutex.Unlock()
	instance.voteCache[pubkey] = voteState
}

func VoteCacheItem(pubkey solana.PublicKey) *sealevel.VoteStateVersions {
	return instance.voteCache[pubkey]
}

func DeleteVoteCacheItem(pubkey solana.PublicKey) {
	instance.voteCacheMutex.Lock()
	defer instance.voteCacheMutex.Unlock()
	delete(instance.voteCache, pubkey)
}

func VoteCache() map[solana.PublicKey]*sealevel.VoteStateVersions {
	return instance.voteCache
}

func PutEpochStakesEntry(epoch uint64, pubkey solana.PublicKey, stake uint64, voteAcct *epochstakes.VoteAccount) {
	if instance.epochStakes == nil {
		instance.epochStakes = epochstakes.NewEpochStakesCache()
	}
	instance.epochStakes.PutEntry(epoch, pubkey, stake, voteAcct)
}

func EpochStakes(epoch uint64) map[solana.PublicKey]uint64 {
	return instance.epochStakes.EpochStakes(epoch)
}

func PutEpochTotalStake(epoch uint64, totalStake uint64) {
	if instance.epochStakes == nil {
		instance.epochStakes = epochstakes.NewEpochStakesCache()
	}
	instance.epochStakes.PutTotalEpochStake(epoch, totalStake)
}

func EpochTotalStake(epoch uint64) uint64 {
	return instance.epochStakes.TotalStake(epoch)
}

func StakeForVoteAcct(epoch uint64, voteAcct solana.PublicKey) uint64 {
	epochStakes := instance.epochStakes.EpochStakes(epoch)
	return epochStakes[voteAcct]
}

func EpochStakesVoteAccts(epoch uint64) map[solana.PublicKey]*epochstakes.VoteAccount {
	return instance.epochStakes.EpochStakesAccts(epoch)
}

func LatestBlockHash() [32]byte {
	return instance.LatestBlockhash()
}

func BlockHeight() uint64 {
	return instance.BlockHeight()
}

func Slot() uint64 {
	return instance.Slot()
}

func Epoch() uint64 {
	return instance.Epoch()
}

func TransactionCount() uint64 {
	return instance.TransactionCount()
}

func SetCalcUnixTimeForClockSysvar(calcUnixTime bool) {
	instance.calcUnixTimeForClockSysvar = calcUnixTime
}

func CalcUnixTimeForClockSysvar() bool {
	return instance.calcUnixTimeForClockSysvar
}

func SetManageLeaderSchedule(manageLeaderSchedule bool) {
	instance.manageLeaderSchedule = manageLeaderSchedule
}

func ManageLeaderSchedule() bool {
	return instance.manageLeaderSchedule
}

func SetManageBlockHeight(manageBlockHeight bool) {
	instance.manageBlockHeight = true
}

func ManageBlockHeight() bool {
	return instance.manageBlockHeight
}

func SetLeaderSchedule(ls *leaderschedule.LeaderSchedule) {
	instance.leaderSchedule = ls
}

// MergeLeaderSchedule merges the given schedule into the existing global schedule.
// Used at epoch boundaries to add the next epoch's schedule without losing the current one.
// If no existing schedule, this behaves like SetLeaderSchedule.
func MergeLeaderSchedule(ls *leaderschedule.LeaderSchedule) {
	if instance.leaderSchedule == nil {
		instance.leaderSchedule = ls
		return
	}
	instance.leaderSchedule.Merge(ls)
}

func LeaderSchedule() *leaderschedule.LeaderSchedule {
	return instance.leaderSchedule
}

func LeaderForSlot(slot uint64) (solana.PublicKey, bool) {
	if instance.leaderSchedule == nil {
		return solana.PublicKey{}, false
	}
	return instance.leaderSchedule.LeaderForSlot(slot)
}

func (globctx *GlobalCtx) SetLatestBlockhash(blockhash [32]byte) {
	globctx.mu.Lock()
	defer globctx.mu.Unlock()
	globctx.latestBlockhash = blockhash
}

func (globctx *GlobalCtx) SetBlockHeight(blockHeight uint64) {
	globctx.mu.Lock()
	defer globctx.mu.Unlock()
	globctx.blockHeight = blockHeight
}

func (globctx *GlobalCtx) SetSlot(slot uint64) {
	globctx.mu.Lock()
	defer globctx.mu.Unlock()
	globctx.slot = slot
}

func (globctx *GlobalCtx) SetEpoch(epoch uint64) {
	globctx.mu.Lock()
	defer globctx.mu.Unlock()
	globctx.epoch = epoch
}

func (globctx *GlobalCtx) IncrTransactionCount(num uint64) {
	globctx.mu.Lock()
	defer globctx.mu.Unlock()
	globctx.transactionCount += num
}

func (globctx *GlobalCtx) LatestBlockhash() [32]byte {
	globctx.mu.Lock()
	defer globctx.mu.Unlock()
	return globctx.latestBlockhash
}

func (globctx *GlobalCtx) BlockHeight() uint64 {
	globctx.mu.Lock()
	defer globctx.mu.Unlock()
	return globctx.blockHeight
}

func (globctx *GlobalCtx) Slot() uint64 {
	globctx.mu.Lock()
	defer globctx.mu.Unlock()
	return globctx.slot
}

func (globctx *GlobalCtx) Epoch() uint64 {
	globctx.mu.Lock()
	defer globctx.mu.Unlock()
	return globctx.epoch
}

func (globctx *GlobalCtx) TransactionCount() uint64 {
	globctx.mu.Lock()
	defer globctx.mu.Unlock()
	return globctx.transactionCount
}

// StakeCacheEntry is the JSON-serializable form of a stake delegation (legacy format).
type StakeCacheEntry struct {
	Pubkey             string  `json:"pubkey"`
	VoterPubkey        string  `json:"voter_pubkey"`
	StakeLamports      uint64  `json:"stake_lamports"`
	ActivationEpoch    uint64  `json:"activation_epoch"`
	DeactivationEpoch  uint64  `json:"deactivation_epoch"`
	WarmupCooldownRate float64 `json:"warmup_cooldown_rate"`
	CreditsObserved    uint64  `json:"credits_observed"`
}

// StakeCacheFile is the top-level structure for stake_cache.json with metadata (legacy format).
type StakeCacheFile struct {
	Slot       uint64            `json:"slot"`        // Slot when cache was saved
	Bankhash   string            `json:"bankhash"`    // Bankhash for fork validation
	EntryCount int               `json:"entry_count"` // Expected number of entries
	Entries    []StakeCacheEntry `json:"entries"`
}

// SaveStakeCache persists the stake cache to disk for resume.
// Uses atomic write (temp file + rename) to prevent corruption.
// Includes metadata (slot, bankhash, entry count) for validation on load.
func SaveStakeCache(accountsDbDir string, slot uint64, bankhash string) error {
	instance.stakeCacheMutex.Lock()
	defer instance.stakeCacheMutex.Unlock()

	if instance.stakeCache == nil || len(instance.stakeCache) == 0 {
		return nil // Nothing to save
	}

	entries := make([]StakeCacheEntry, 0, len(instance.stakeCache))
	for pubkey, delegation := range instance.stakeCache {
		entries = append(entries, StakeCacheEntry{
			Pubkey:             pubkey.String(),
			VoterPubkey:        delegation.VoterPubkey.String(),
			StakeLamports:      delegation.StakeLamports,
			ActivationEpoch:    delegation.ActivationEpoch,
			DeactivationEpoch:  delegation.DeactivationEpoch,
			WarmupCooldownRate: delegation.WarmupCooldownRate,
			CreditsObserved:    delegation.CreditsObserved,
		})
	}

	cacheFile := StakeCacheFile{
		Slot:       slot,
		Bankhash:   bankhash,
		EntryCount: len(entries),
		Entries:    entries,
	}

	data, err := json.Marshal(cacheFile)
	if err != nil {
		return fmt.Errorf("failed to marshal stake cache: %w", err)
	}

	cacheFilePath := filepath.Join(accountsDbDir, StakeCacheFileName)
	tmpFile := cacheFilePath + ".tmp"

	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write stake cache file: %w", err)
	}

	if err := os.Rename(tmpFile, cacheFilePath); err != nil {
		os.Remove(tmpFile)
		return fmt.Errorf("failed to rename stake cache file: %w", err)
	}

	return nil
}

// LoadStakeCache loads the stake cache from disk.
// Returns (loaded_count, nil) on success, (0, nil) if file doesn't exist.
// Returns error if file is corrupt or has invalid entries (triggers scan fallback).
// The expectedSlot and expectedBankhash parameters validate the cache is from the correct AccountsDB state.
// Pass empty string for expectedBankhash to skip bankhash validation.
func LoadStakeCache(accountsDbDir string, expectedSlot uint64, expectedBankhash string) (int, error) {
	cacheFilePath := filepath.Join(accountsDbDir, StakeCacheFileName)

	data, err := os.ReadFile(cacheFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // File doesn't exist, not an error
		}
		return 0, fmt.Errorf("failed to read stake cache file: %w", err)
	}

	var cacheFile StakeCacheFile
	if err := json.Unmarshal(data, &cacheFile); err != nil {
		return 0, fmt.Errorf("failed to parse stake cache file: %w", err)
	}

	// Validate metadata - slot must match to ensure cache is from correct state
	if cacheFile.Slot != expectedSlot {
		return 0, fmt.Errorf("stake cache slot mismatch: file=%d expected=%d (stale cache?)", cacheFile.Slot, expectedSlot)
	}

	// Validate bankhash if provided - detects fork divergence at same slot
	if expectedBankhash != "" && cacheFile.Bankhash != expectedBankhash {
		return 0, fmt.Errorf("stake cache bankhash mismatch: file=%s expected=%s (different fork?)", cacheFile.Bankhash, expectedBankhash)
	}

	// Validate entry count matches
	if cacheFile.EntryCount != len(cacheFile.Entries) {
		return 0, fmt.Errorf("stake cache entry count mismatch: header=%d actual=%d (corrupt file?)", cacheFile.EntryCount, len(cacheFile.Entries))
	}

	instance.stakeCacheMutex.Lock()
	defer instance.stakeCacheMutex.Unlock()

	// Unconditionally clear before loading to avoid stale entries
	instance.stakeCache = make(map[solana.PublicKey]*sealevel.Delegation)
	instance.stakeCacheSlots = make(map[solana.PublicKey]uint64)

	loadedCount := 0
	skippedCount := 0
	for _, entry := range cacheFile.Entries {
		pubkey, err := solana.PublicKeyFromBase58(entry.Pubkey)
		if err != nil {
			skippedCount++
			continue
		}
		voterPubkey, err := solana.PublicKeyFromBase58(entry.VoterPubkey)
		if err != nil {
			skippedCount++
			continue
		}

		instance.stakeCache[pubkey] = &sealevel.Delegation{
			VoterPubkey:        voterPubkey,
			StakeLamports:      entry.StakeLamports,
			ActivationEpoch:    entry.ActivationEpoch,
			DeactivationEpoch:  entry.DeactivationEpoch,
			WarmupCooldownRate: entry.WarmupCooldownRate,
			CreditsObserved:    entry.CreditsObserved,
		}
		// Seed slot tracking so PutStakeCacheItemWithSlot works correctly during replay
		instance.stakeCacheSlots[pubkey] = expectedSlot
		loadedCount++
	}

	// If any entries were skipped, treat as corrupt and return error to trigger scan
	if skippedCount > 0 {
		// Clear partial load
		instance.stakeCache = make(map[solana.PublicKey]*sealevel.Delegation)
		instance.stakeCacheSlots = make(map[solana.PublicKey]uint64)
		return 0, fmt.Errorf("stake cache has %d corrupt entries out of %d (forcing scan)", skippedCount, len(cacheFile.Entries))
	}

	return loadedCount, nil
}

// StakeCacheExists checks if the stake cache file exists on disk.
func StakeCacheExists(accountsDbDir string) bool {
	cacheFile := filepath.Join(accountsDbDir, StakeCacheFileName)
	_, err := os.Stat(cacheFile)
	return err == nil
}

// LoadStakeCachePubkeysOnly loads just the pubkeys from a stake cache file (ignoring slot validation).
// Used as a hint for faster AccountsDB scanning when the cache is stale.
// Returns the list of pubkeys and the slot the cache was saved at.
func LoadStakeCachePubkeysOnly(accountsDbDir string) ([]solana.PublicKey, uint64, error) {
	cacheFilePath := filepath.Join(accountsDbDir, StakeCacheFileName)

	data, err := os.ReadFile(cacheFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil // File doesn't exist
		}
		return nil, 0, fmt.Errorf("failed to read stake cache file: %w", err)
	}

	var cacheFile StakeCacheFile
	if err := json.Unmarshal(data, &cacheFile); err != nil {
		return nil, 0, fmt.Errorf("failed to parse stake cache file: %w", err)
	}

	pubkeys := make([]solana.PublicKey, 0, len(cacheFile.Entries))
	for _, entry := range cacheFile.Entries {
		pubkey, err := solana.PublicKeyFromBase58(entry.Pubkey)
		if err != nil {
			continue // Skip invalid pubkeys
		}
		pubkeys = append(pubkeys, pubkey)
	}

	return pubkeys, cacheFile.Slot, nil
}

// ClearStakeCache clears the in-memory stake cache.
// Used before loading from file or scanning AccountsDB.
func ClearStakeCache() {
	instance.stakeCacheMutex.Lock()
	defer instance.stakeCacheMutex.Unlock()
	instance.stakeCache = make(map[solana.PublicKey]*sealevel.Delegation)
	instance.stakeCacheSlots = make(map[solana.PublicKey]uint64)
}

// StakeCacheSize returns the number of entries in the stake cache.
func StakeCacheSize() int {
	instance.stakeCacheMutex.Lock()
	defer instance.stakeCacheMutex.Unlock()
	return len(instance.stakeCache)
}

// FlushPendingStakePubkeys appends any new stake pubkeys to the index file.
// Call this after each block commit to persist new stake accounts.
// Returns the number of pubkeys appended, or error if append failed.
// IMPORTANT: Only clears the pending list after successful write to avoid data loss.
func FlushPendingStakePubkeys(accountsDbDir string) (int, error) {
	instance.stakeCacheMutex.Lock()
	pending := instance.pendingNewStakePubkeys
	// Don't clear yet - only clear after successful write
	instance.stakeCacheMutex.Unlock()

	if len(pending) == 0 {
		return 0, nil
	}

	indexPath := filepath.Join(accountsDbDir, StakePubkeyIndexFileName)

	// Open file for append (create if not exists)
	f, err := os.OpenFile(indexPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return 0, fmt.Errorf("failed to open stake pubkey index for append: %w", err)
	}

	// Write each pubkey as raw 32 bytes
	for _, pubkey := range pending {
		if _, err := f.Write(pubkey[:]); err != nil {
			f.Close()
			return 0, fmt.Errorf("failed to append pubkey to index: %w", err)
		}
	}

	// Sync to ensure data is on disk before clearing pending list
	if err := f.Sync(); err != nil {
		f.Close()
		return 0, fmt.Errorf("failed to sync stake pubkey index: %w", err)
	}

	if err := f.Close(); err != nil {
		return 0, fmt.Errorf("failed to close stake pubkey index: %w", err)
	}

	// Only clear pending list after successful write+sync+close
	instance.stakeCacheMutex.Lock()
	instance.pendingNewStakePubkeys = nil
	instance.stakeCacheMutex.Unlock()

	return len(pending), nil
}

// ClearPendingStakePubkeys discards any pending new stake pubkeys.
// Call this when a block replay fails and we need to rollback.
func ClearPendingStakePubkeys() {
	instance.stakeCacheMutex.Lock()
	defer instance.stakeCacheMutex.Unlock()
	instance.pendingNewStakePubkeys = nil
}

// PendingStakePubkeysCount returns the number of new stake pubkeys pending append.
func PendingStakePubkeysCount() int {
	instance.stakeCacheMutex.Lock()
	defer instance.stakeCacheMutex.Unlock()
	return len(instance.pendingNewStakePubkeys)
}

// LoadStakePubkeyIndex reads the binary index file and returns all stake pubkeys.
// Returns empty slice if file doesn't exist.
func LoadStakePubkeyIndex(accountsDbDir string) ([]solana.PublicKey, error) {
	indexPath := filepath.Join(accountsDbDir, StakePubkeyIndexFileName)

	data, err := os.ReadFile(indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // File doesn't exist, not an error
		}
		return nil, fmt.Errorf("failed to read stake pubkey index: %w", err)
	}

	// Each pubkey is 32 bytes
	if len(data)%32 != 0 {
		return nil, fmt.Errorf("stake pubkey index has invalid size: %d bytes (not multiple of 32)", len(data))
	}

	numPubkeys := len(data) / 32
	pubkeys := make([]solana.PublicKey, 0, numPubkeys)
	seen := make(map[solana.PublicKey]struct{}, numPubkeys)

	for i := 0; i < len(data); i += 32 {
		var pubkey solana.PublicKey
		copy(pubkey[:], data[i:i+32])
		// Deduplicate (index is append-only, may have duplicates)
		if _, exists := seen[pubkey]; !exists {
			seen[pubkey] = struct{}{}
			pubkeys = append(pubkeys, pubkey)
		}
	}

	return pubkeys, nil
}

// SaveStakePubkeyIndex rewrites the index file with all current stake cache pubkeys.
// Call this at epoch boundaries to compact the index and remove closed accounts.
func SaveStakePubkeyIndex(accountsDbDir string) error {
	instance.stakeCacheMutex.Lock()
	// Collect all pubkeys from current cache
	pubkeys := make([]solana.PublicKey, 0, len(instance.stakeCache))
	for pubkey := range instance.stakeCache {
		pubkeys = append(pubkeys, pubkey)
	}
	instance.stakeCacheMutex.Unlock()

	indexPath := filepath.Join(accountsDbDir, StakePubkeyIndexFileName)

	// Write to temp file first, then rename (atomic)
	tempPath := indexPath + ".tmp"
	f, err := os.Create(tempPath)
	if err != nil {
		return fmt.Errorf("failed to create temp stake pubkey index: %w", err)
	}

	// Write all pubkeys as raw 32 bytes
	for _, pubkey := range pubkeys {
		if _, err := f.Write(pubkey[:]); err != nil {
			f.Close()
			os.Remove(tempPath)
			return fmt.Errorf("failed to write pubkey to index: %w", err)
		}
	}

	if err := f.Close(); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("failed to close temp stake pubkey index: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tempPath, indexPath); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("failed to rename stake pubkey index: %w", err)
	}

	return nil
}

// StakePubkeyIndexExists checks if the stake pubkey index file exists.
func StakePubkeyIndexExists(accountsDbDir string) bool {
	indexPath := filepath.Join(accountsDbDir, StakePubkeyIndexFileName)
	_, err := os.Stat(indexPath)
	return err == nil
}

// DeleteStakePubkeyIndex removes the stake pubkey index file.
func DeleteStakePubkeyIndex(accountsDbDir string) error {
	indexPath := filepath.Join(accountsDbDir, StakePubkeyIndexFileName)
	err := os.Remove(indexPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
