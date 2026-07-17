package alpenglow

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/gagliardetto/solana-go"
)

const voteHistoryVersion = 1

var ErrVoteHistoryNotFound = errors.New("alpenglow vote history not found")

// VoteHistory is the safety record for votes emitted by one validator
// identity. It deliberately includes the local consensus observations used by
// Agave when resuming (notarized blocks and ParentReady edges), in addition to
// the anti-equivocation vote sets.
type VoteHistory struct {
	Version              uint32                          `json:"version"`
	NodePubkey           solana.PublicKey                `json:"node_pubkey"`
	Root                 uint64                          `json:"root"`
	Voted                map[uint64]bool                 `json:"voted"`
	VotedNotar           map[uint64]solana.Hash          `json:"voted_notar"`
	VotedNotarFallback   map[uint64]map[solana.Hash]bool `json:"voted_notar_fallback"`
	VotedSkipFallback    map[uint64]bool                 `json:"voted_skip_fallback"`
	Skipped              map[uint64]bool                 `json:"skipped"`
	ItsOver              map[uint64]bool                 `json:"its_over"`
	VotesCast            map[uint64][]Vote               `json:"votes_cast"`
	NotarizedBlocks      map[BlockID]bool                `json:"-"`
	ParentReadyBySlot    map[uint64]map[BlockID]bool     `json:"-"`
	PersistedNotarized   []BlockID                       `json:"notarized_blocks,omitempty"`
	PersistedParentReady []persistedParentReady          `json:"parent_ready,omitempty"`
}

type persistedParentReady struct {
	Slot    uint64    `json:"slot"`
	Parents []BlockID `json:"parents"`
}

type savedVoteHistory struct {
	Version   uint32           `json:"version"`
	Node      solana.PublicKey `json:"node_pubkey"`
	Data      json.RawMessage  `json:"data"`
	Signature []byte           `json:"signature"`
}

func NewVoteHistory(node solana.PublicKey, root uint64) *VoteHistory {
	h := &VoteHistory{Version: voteHistoryVersion, NodePubkey: node, Root: root}
	h.ensureMaps()
	return h
}

func (h *VoteHistory) ensureMaps() {
	if h.Voted == nil {
		h.Voted = make(map[uint64]bool)
	}
	if h.VotedNotar == nil {
		h.VotedNotar = make(map[uint64]solana.Hash)
	}
	if h.VotedNotarFallback == nil {
		h.VotedNotarFallback = make(map[uint64]map[solana.Hash]bool)
	}
	if h.VotedSkipFallback == nil {
		h.VotedSkipFallback = make(map[uint64]bool)
	}
	if h.Skipped == nil {
		h.Skipped = make(map[uint64]bool)
	}
	if h.ItsOver == nil {
		h.ItsOver = make(map[uint64]bool)
	}
	if h.VotesCast == nil {
		h.VotesCast = make(map[uint64][]Vote)
	}
	if h.NotarizedBlocks == nil {
		h.NotarizedBlocks = make(map[BlockID]bool)
	}
	if h.ParentReadyBySlot == nil {
		h.ParentReadyBySlot = make(map[uint64]map[BlockID]bool)
	}
}

func (h *VoteHistory) VotedAt(slot uint64) bool {
	return slot >= h.Root && h.Voted[slot]
}

func (h *VoteHistory) NotarizedVote(slot uint64) (solana.Hash, bool) {
	if slot < h.Root {
		return solana.Hash{}, false
	}
	hash, ok := h.VotedNotar[slot]
	return hash, ok
}

func (h *VoteHistory) HasNotarFallback(block BlockID) bool {
	return slotHashSetContains(h.VotedNotarFallback, block)
}

func (h *VoteHistory) HasSkipFallback(slot uint64) bool {
	return slot >= h.Root && h.VotedSkipFallback[slot]
}

func (h *VoteHistory) HasSkipped(slot uint64) bool {
	return slot >= h.Root && h.Skipped[slot]
}

func (h *VoteHistory) IsOver(slot uint64) bool {
	return slot >= h.Root && h.ItsOver[slot]
}

func (h *VoteHistory) BadWindow(slot uint64) bool {
	return h.HasSkipped(slot) || len(h.VotedNotarFallback[slot]) != 0 || h.HasSkipFallback(slot)
}

func (h *VoteHistory) IsBlockNotarized(block BlockID) bool {
	return h.NotarizedBlocks[block]
}

func (h *VoteHistory) IsParentReady(slot uint64, parent BlockID) bool {
	return h.ParentReadyBySlot[slot][parent]
}

func (h *VoteHistory) ValidateVote(vote Vote) error {
	if err := vote.ValidateBasic(); err != nil {
		return err
	}
	if vote.Slot < h.Root {
		return fmt.Errorf("vote slot %d is behind vote-history root %d", vote.Slot, h.Root)
	}
	switch vote.Type {
	case VoteTypeNotarize:
		if h.Voted[vote.Slot] {
			return fmt.Errorf("slot %d already has a notarize-or-skip vote", vote.Slot)
		}
	case VoteTypeFinalize:
		if h.Skipped[vote.Slot] {
			return fmt.Errorf("slot %d has a skip-family vote and cannot be finalized", vote.Slot)
		}
		if h.ItsOver[vote.Slot] {
			return fmt.Errorf("slot %d already has a finalization vote", vote.Slot)
		}
	case VoteTypeSkip:
		if h.Voted[vote.Slot] {
			return fmt.Errorf("slot %d already has a notarize-or-skip vote", vote.Slot)
		}
	case VoteTypeNotarizeFallback:
		if !h.Voted[vote.Slot] || h.ItsOver[vote.Slot] {
			return fmt.Errorf("slot %d is not eligible for notarize-fallback", vote.Slot)
		}
		if h.VotedNotarFallback[vote.Slot][vote.BlockHash] {
			return fmt.Errorf("slot %d already has notarize-fallback for %s", vote.Slot, vote.BlockHash)
		}
	case VoteTypeSkipFallback:
		if !h.Voted[vote.Slot] || h.ItsOver[vote.Slot] || h.VotedSkipFallback[vote.Slot] {
			return fmt.Errorf("slot %d is not eligible for skip-fallback", vote.Slot)
		}
	case VoteTypeGenesis:
		return fmt.Errorf("genesis votes are managed by migration, not the live voter")
	}
	return nil
}

func (h *VoteHistory) AddVote(vote Vote) error {
	h.ensureMaps()
	if err := h.ValidateVote(vote); err != nil {
		return err
	}
	switch vote.Type {
	case VoteTypeNotarize:
		h.Voted[vote.Slot] = true
		h.VotedNotar[vote.Slot] = vote.BlockHash
	case VoteTypeFinalize:
		h.ItsOver[vote.Slot] = true
	case VoteTypeSkip:
		h.Voted[vote.Slot] = true
		h.Skipped[vote.Slot] = true
	case VoteTypeNotarizeFallback:
		h.Skipped[vote.Slot] = true
		if h.VotedNotarFallback[vote.Slot] == nil {
			h.VotedNotarFallback[vote.Slot] = make(map[solana.Hash]bool)
		}
		h.VotedNotarFallback[vote.Slot][vote.BlockHash] = true
	case VoteTypeSkipFallback:
		h.Skipped[vote.Slot] = true
		h.VotedSkipFallback[vote.Slot] = true
	}
	h.VotesCast[vote.Slot] = append(h.VotesCast[vote.Slot], vote)
	return nil
}

func (h *VoteHistory) AddBlockNotarized(block BlockID) {
	h.ensureMaps()
	if block.Slot >= h.Root {
		h.NotarizedBlocks[block] = true
	}
}

// AddParentReady returns true for the first ParentReady observation at slot.
func (h *VoteHistory) AddParentReady(slot uint64, parent BlockID) bool {
	h.ensureMaps()
	if slot < h.Root {
		return false
	}
	first := len(h.ParentReadyBySlot[slot]) == 0
	if h.ParentReadyBySlot[slot] == nil {
		h.ParentReadyBySlot[slot] = make(map[BlockID]bool)
	}
	h.ParentReadyBySlot[slot][parent] = true
	return first
}

func (h *VoteHistory) HighestParentReady() (uint64, BlockID, bool) {
	var bestSlot uint64
	var best BlockID
	found := false
	for slot, parents := range h.ParentReadyBySlot {
		for parent := range parents {
			if !found || slot > bestSlot || (slot == bestSlot && blockLess(parent, best)) {
				bestSlot, best, found = slot, parent, true
			}
		}
	}
	return bestSlot, best, found
}

func (h *VoteHistory) VotesAfter(slot uint64) []Vote {
	slots := make([]uint64, 0, len(h.VotesCast))
	for voteSlot := range h.VotesCast {
		if voteSlot > slot {
			slots = append(slots, voteSlot)
		}
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i] < slots[j] })
	var votes []Vote
	for _, voteSlot := range slots {
		votes = append(votes, h.VotesCast[voteSlot]...)
	}
	return votes
}

func (h *VoteHistory) SetRoot(root uint64) {
	if root <= h.Root {
		return
	}
	h.Root = root
	pruneSlotMap(h.Voted, root)
	pruneSlotMap(h.VotedNotar, root)
	pruneSlotMap(h.VotedNotarFallback, root)
	pruneSlotMap(h.VotedSkipFallback, root)
	pruneSlotMap(h.Skipped, root)
	pruneSlotMap(h.ItsOver, root)
	pruneSlotMap(h.VotesCast, root)
	pruneSlotMap(h.ParentReadyBySlot, root)
	for block := range h.NotarizedBlocks {
		if block.Slot < root {
			delete(h.NotarizedBlocks, block)
		}
	}
}

func pruneSlotMap[T any](m map[uint64]T, root uint64) {
	for slot := range m {
		if slot < root {
			delete(m, slot)
		}
	}
}

func slotHashSetContains(m map[uint64]map[solana.Hash]bool, block BlockID) bool {
	return m[block.Slot][block.Hash]
}

func (h *VoteHistory) preparePersistedViews() {
	h.PersistedNotarized = h.PersistedNotarized[:0]
	for block := range h.NotarizedBlocks {
		h.PersistedNotarized = append(h.PersistedNotarized, block)
	}
	sort.Slice(h.PersistedNotarized, func(i, j int) bool { return blockLess(h.PersistedNotarized[i], h.PersistedNotarized[j]) })
	h.PersistedParentReady = h.PersistedParentReady[:0]
	slots := make([]uint64, 0, len(h.ParentReadyBySlot))
	for slot := range h.ParentReadyBySlot {
		slots = append(slots, slot)
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i] < slots[j] })
	for _, slot := range slots {
		parents := make([]BlockID, 0, len(h.ParentReadyBySlot[slot]))
		for parent := range h.ParentReadyBySlot[slot] {
			parents = append(parents, parent)
		}
		sort.Slice(parents, func(i, j int) bool { return blockLess(parents[i], parents[j]) })
		h.PersistedParentReady = append(h.PersistedParentReady, persistedParentReady{Slot: slot, Parents: parents})
	}
}

func (h *VoteHistory) restorePersistedViews() {
	h.ensureMaps()
	for _, block := range h.PersistedNotarized {
		h.NotarizedBlocks[block] = true
	}
	for _, ready := range h.PersistedParentReady {
		for _, parent := range ready.Parents {
			h.AddParentReady(ready.Slot, parent)
		}
	}
	// These are transport-only views; avoid retaining duplicate state in RAM.
	h.PersistedNotarized = nil
	h.PersistedParentReady = nil
}

func VoteHistoryFilename(dir string, node solana.PublicKey) string {
	return filepath.Join(dir, fmt.Sprintf("vote_history-%s.mithril.json", node))
}

// SaveVoteHistory signs the exact serialized history with the validator
// identity and atomically replaces the previous file before any vote is sent
// to the network.
func SaveVoteHistory(dir string, h *VoteHistory, identity ed25519.PrivateKey) error {
	if h == nil {
		return fmt.Errorf("save vote history: nil history")
	}
	if len(identity) != ed25519.PrivateKeySize {
		return fmt.Errorf("save vote history: invalid identity key size %d", len(identity))
	}
	node := solana.PublicKey(identity.Public().(ed25519.PublicKey))
	if node != h.NodePubkey {
		return fmt.Errorf("save vote history: identity %s does not match history %s", node, h.NodePubkey)
	}
	h.Version = voteHistoryVersion
	h.preparePersistedViews()
	data, err := json.Marshal(h)
	h.PersistedNotarized = nil
	h.PersistedParentReady = nil
	if err != nil {
		return fmt.Errorf("serialize vote history: %w", err)
	}
	envelope := savedVoteHistory{
		Version:   voteHistoryVersion,
		Node:      node,
		Data:      data,
		Signature: ed25519.Sign(identity, data),
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("serialize saved vote history: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create vote-history directory: %w", err)
	}
	filename := VoteHistoryFilename(dir, node)
	temporary := filename + ".new"
	if err := os.WriteFile(temporary, encoded, 0o600); err != nil {
		return fmt.Errorf("write vote history: %w", err)
	}
	if err := os.Rename(temporary, filename); err != nil {
		return fmt.Errorf("replace vote history: %w", err)
	}
	return nil
}

func LoadVoteHistory(dir string, node solana.PublicKey) (*VoteHistory, error) {
	filename := VoteHistoryFilename(dir, node)
	encoded, err := os.ReadFile(filename)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrVoteHistoryNotFound, filename)
		}
		return nil, fmt.Errorf("read vote history: %w", err)
	}
	var envelope savedVoteHistory
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		return nil, fmt.Errorf("decode saved vote history: %w", err)
	}
	if envelope.Version != voteHistoryVersion || envelope.Node != node {
		return nil, fmt.Errorf("saved vote history identity/version mismatch")
	}
	if !ed25519.Verify(ed25519.PublicKey(node[:]), envelope.Data, envelope.Signature) {
		return nil, fmt.Errorf("saved vote history signature is invalid")
	}
	var h VoteHistory
	if err := json.Unmarshal(envelope.Data, &h); err != nil {
		return nil, fmt.Errorf("decode vote history: %w", err)
	}
	if h.Version != voteHistoryVersion || h.NodePubkey != node {
		return nil, fmt.Errorf("vote history identity/version mismatch")
	}
	h.restorePersistedViews()
	return &h, nil
}
