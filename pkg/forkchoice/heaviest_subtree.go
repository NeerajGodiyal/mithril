package forkchoice

import (
	"bytes"
	"sort"

	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/gagliardetto/solana-go"
)

// SlotHashKey identifies one block version: (slot, hash). Ordering is slot, then
// hash bytes — matching Agave's (Slot, Hash) tuple ordering.
type SlotHashKey struct {
	Slot uint64
	Hash [32]byte
}

func (k SlotHashKey) less(o SlotHashKey) bool {
	if k.Slot != o.Slot {
		return k.Slot < o.Slot
	}
	return bytes.Compare(k.Hash[:], o.Hash[:]) < 0
}

// hsfForkInfo is the per-node state (Agave ForkInfo).
type hsfForkInfo struct {
	stakeVotedAt      uint64
	stakeVotedSubtree uint64
	height            int
	bestSlot          SlotHashKey // heaviest descendant (candidates only)
	deepestSlot       SlotHashKey // deepest descendant (ignores validity)
	parent            *SlotHashKey
	children          []SlotHashKey // sorted ascending (BTreeSet semantics)
	// Latest ancestor slot marked invalid (== own slot when self is the duplicate);
	// nil = candidate.
	latestInvalidAncestor *uint64
	isDuplicateConfirmed  bool
}

func (fi *hsfForkInfo) isCandidate() bool { return fi.latestInvalidAncestor == nil }

func (fi *hsfForkInfo) setDuplicateConfirmed() {
	fi.isDuplicateConfirmed = true
	fi.latestInvalidAncestor = nil
}

func (fi *hsfForkInfo) updateWithNewlyValidAncestor(validSlot uint64) {
	if fi.latestInvalidAncestor != nil && *fi.latestInvalidAncestor <= validSlot {
		fi.latestInvalidAncestor = nil
	}
}

func (fi *hsfForkInfo) updateWithNewlyInvalidAncestor(invalidSlot uint64) {
	if fi.isDuplicateConfirmed {
		panic("heaviest-subtree: cannot mark a duplicate-confirmed node invalid")
	}
	if fi.latestInvalidAncestor == nil || invalidSlot > *fi.latestInvalidAncestor {
		s := invalidSlot
		fi.latestInvalidAncestor = &s
	}
}

// StakeFn returns a validator's stake for votes at the given slot (epoch-aware
// lookups plug in here; a flat epoch-stakes map ignores the slot).
type StakeFn func(pk solana.PublicKey, slot uint64) uint64

// HeaviestSubtreeForkChoice is a faithful port of Agave's stake-weighted fork
// choice (core/src/consensus/heaviest_subtree_fork_choice.rs): per-node
// stake_voted_at/subtree, latest-vote-per-validator, upward aggregation, best
// child by subtree stake (tie-break lower key), duplicate marking via
// invalid/valid candidates. Not goroutine-safe; callers serialize.
type HeaviestSubtreeForkChoice struct {
	treeRoot    SlotHashKey
	forkInfos   map[SlotHashKey]*hsfForkInfo
	latestVotes map[solana.PublicKey]SlotHashKey
}

func NewHeaviestSubtreeForkChoice(root SlotHashKey) *HeaviestSubtreeForkChoice {
	h := &HeaviestSubtreeForkChoice{
		treeRoot:    root,
		forkInfos:   make(map[SlotHashKey]*hsfForkInfo),
		latestVotes: make(map[solana.PublicKey]SlotHashKey),
	}
	h.AddNewLeafSlot(root, nil)
	return h
}

func (h *HeaviestSubtreeForkChoice) TreeRoot() SlotHashKey { return h.treeRoot }

func (h *HeaviestSubtreeForkChoice) ContainsBlock(k SlotHashKey) bool {
	_, ok := h.forkInfos[k]
	return ok
}

// BestOverallSlot is the heaviest valid leaf: the fork to follow.
func (h *HeaviestSubtreeForkChoice) BestOverallSlot() SlotHashKey {
	return h.forkInfos[h.treeRoot].bestSlot
}

// DeepestOverallSlot is the deepest leaf regardless of validity.
func (h *HeaviestSubtreeForkChoice) DeepestOverallSlot() SlotHashKey {
	return h.forkInfos[h.treeRoot].deepestSlot
}

func (h *HeaviestSubtreeForkChoice) BestSlot(k SlotHashKey) (SlotHashKey, bool) {
	fi, ok := h.forkInfos[k]
	if !ok {
		return SlotHashKey{}, false
	}
	return fi.bestSlot, true
}

func (h *HeaviestSubtreeForkChoice) StakeVotedSubtree(k SlotHashKey) (uint64, bool) {
	fi, ok := h.forkInfos[k]
	if !ok {
		return 0, false
	}
	return fi.stakeVotedSubtree, true
}

func (h *HeaviestSubtreeForkChoice) StakeVotedAt(k SlotHashKey) (uint64, bool) {
	fi, ok := h.forkInfos[k]
	if !ok {
		return 0, false
	}
	return fi.stakeVotedAt, true
}

func (h *HeaviestSubtreeForkChoice) IsCandidate(k SlotHashKey) (bool, bool) {
	fi, ok := h.forkInfos[k]
	if !ok {
		return false, false
	}
	return fi.isCandidate(), true
}

func (h *HeaviestSubtreeForkChoice) IsDuplicateConfirmed(k SlotHashKey) (bool, bool) {
	fi, ok := h.forkInfos[k]
	if !ok {
		return false, false
	}
	return fi.isDuplicateConfirmed, true
}

func (h *HeaviestSubtreeForkChoice) LatestInvalidAncestor(k SlotHashKey) (uint64, bool) {
	fi, ok := h.forkInfos[k]
	if !ok || fi.latestInvalidAncestor == nil {
		return 0, false
	}
	return *fi.latestInvalidAncestor, true
}

// AddNewLeafSlot inserts a frozen block under parent (nil = the tree root itself)
// and propagates best/deepest up the tree. Re-adding an existing key is a no-op
// (repair of the same version after a dump).
func (h *HeaviestSubtreeForkChoice) AddNewLeafSlot(key SlotHashKey, parent *SlotHashKey) {
	if _, exists := h.forkInfos[key]; exists {
		return
	}
	var inheritedInvalid *uint64
	if parent != nil {
		if pfi, ok := h.forkInfos[*parent]; ok && pfi.latestInvalidAncestor != nil {
			s := *pfi.latestInvalidAncestor
			inheritedInvalid = &s
		}
	}
	h.forkInfos[key] = &hsfForkInfo{
		height:                1,
		bestSlot:              key, // a leaf's best/deepest is itself
		deepestSlot:           key,
		parent:                parent,
		latestInvalidAncestor: inheritedInvalid,
		// A parentless insert is the root, which is duplicate-confirmed by definition.
		isDuplicateConfirmed: parent == nil,
	}
	if parent == nil {
		return
	}
	pfi, ok := h.forkInfos[*parent]
	if !ok {
		panic("heaviest-subtree: parent must exist before its child is added")
	}
	pfi.children = insertSortedKey(pfi.children, key)
	h.propagateNewLeaf(key, *parent)
}

// AddVotes applies the latest votes (one per validator per batch), subtracting each
// validator's stake from its previous fork and adding it to the new one, then
// re-aggregates. Returns the new best overall slot.
func (h *HeaviestSubtreeForkChoice) AddVotes(votes []VoteKey, stakeAt StakeFn) SlotHashKey {
	ops := h.generateUpdateOperations(votes, stakeAt)
	h.processUpdateOperations(ops)
	return h.BestOverallSlot()
}

// VoteKey is one validator's latest vote target.
type VoteKey struct {
	Pubkey solana.PublicKey
	Key    SlotHashKey
}

// update-operation machinery (Agave UpdateLabel/UpdateOperation over a BTreeMap,
// processed greatest→smallest so descendants aggregate before ancestors).
const (
	labelAggregate = iota
	labelAdd
	labelMarkValid
	labelMarkInvalid
	labelSubtract
)

type opKey struct {
	key       SlotHashKey
	label     int
	labelSlot uint64 // MarkValid/MarkInvalid payload (part of ordering, like Rust)
}

type updateOps map[opKey]uint64 // value = stake for Add/Subtract, unused otherwise

func (h *HeaviestSubtreeForkChoice) generateUpdateOperations(votes []VoteKey, stakeAt StakeFn) updateOps {
	ops := make(updateOps)
	observed := make(map[solana.PublicKey]bool, len(votes))
	for _, v := range votes {
		if v.Key.Slot < h.treeRoot.Slot {
			continue // below root: provably a no-op for fork choice
		}
		if observed[v.Pubkey] {
			panic("heaviest-subtree: multiple votes for the same pubkey in one batch")
		}
		observed[v.Pubkey] = true

		if prev, ok := h.latestVotes[v.Pubkey]; ok {
			// Only newer votes count; equal slot only for a SMALLER hash (a duplicate
			// version of the same slot).
			if v.Key.Slot < prev.Slot ||
				(v.Key.Slot == prev.Slot && bytes.Compare(v.Key.Hash[:], prev.Hash[:]) >= 0) {
				continue
			}
			// Remove this validator's stake from the previous fork.
			if st := stakeAt(v.Pubkey, prev.Slot); st > 0 {
				ops[opKey{key: prev, label: labelSubtract}] += st
				h.insertAggregateOperations(ops, prev)
			}
		}
		h.latestVotes[v.Pubkey] = v.Key

		// Insert the Add and its aggregates even for zero stake, so op ordering is identical regardless of stake.
		ops[opKey{key: v.Key, label: labelAdd}] += stakeAt(v.Pubkey, v.Key.Slot)
		h.insertAggregateOperations(ops, v.Key)
	}
	return ops
}

func (h *HeaviestSubtreeForkChoice) insertAggregateOperations(ops updateOps, key SlotHashKey) {
	h.insertAggregateAcrossAncestors(ops, key, 0, 0)
}

// insertAggregateAcrossAncestors marks every ancestor for re-aggregation (stopping
// at the first already-marked one), optionally attaching a MarkValid/MarkInvalid.
func (h *HeaviestSubtreeForkChoice) insertAggregateAcrossAncestors(ops updateOps, key SlotHashKey, markLabel int, markSlot uint64) {
	for p := h.parentOf(key); p != nil; p = h.parentOf(*p) {
		if !h.insertOneAggregate(ops, *p, markLabel, markSlot) {
			break
		}
	}
}

func (h *HeaviestSubtreeForkChoice) insertOneAggregate(ops updateOps, key SlotHashKey, markLabel int, markSlot uint64) bool {
	agg := opKey{key: key, label: labelAggregate}
	if _, exists := ops[agg]; exists {
		return false
	}
	if markLabel == labelMarkValid || markLabel == labelMarkInvalid {
		ops[opKey{key: key, label: markLabel, labelSlot: markSlot}] = 0
	}
	ops[agg] = 0
	return true
}

// processUpdateOperations applies ops greatest→smallest (descendants before
// ancestors; per node: Subtract/MarkInvalid/MarkValid/Add before Aggregate).
func (h *HeaviestSubtreeForkChoice) processUpdateOperations(ops updateOps) {
	keys := make([]opKey, 0, len(ops))
	for k := range ops {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { // descending (Rust .rev() over BTreeMap)
		a, b := keys[i], keys[j]
		if a.key != b.key {
			return b.key.less(a.key)
		}
		if a.label != b.label {
			return a.label > b.label
		}
		return a.labelSlot > b.labelSlot
	})
	for _, k := range keys {
		switch k.label {
		case labelMarkValid:
			h.markForkValid(k.key, k.labelSlot)
		case labelMarkInvalid:
			h.markForkInvalid(k.key, k.labelSlot)
		case labelAggregate:
			h.aggregateSlot(k.key)
		case labelAdd:
			if fi, ok := h.forkInfos[k.key]; ok {
				fi.stakeVotedAt += ops[k]
				fi.stakeVotedSubtree += ops[k]
			}
		case labelSubtract:
			if fi, ok := h.forkInfos[k.key]; ok {
				fi.stakeVotedAt -= ops[k]
				fi.stakeVotedSubtree -= ops[k]
			}
		}
	}
}

// aggregateSlot recomputes one node from its children: subtree stake counts ALL
// children (even non-candidates, so their weight still backs shared ancestors);
// bestSlot considers candidates only, by subtree stake, tie-break lower key;
// deepest by height, then stake, then lower key; duplicate-confirmed bubbles up.
func (h *HeaviestSubtreeForkChoice) aggregateSlot(key SlotHashKey) {
	fi, ok := h.forkInfos[key]
	if !ok {
		return
	}
	stakeVotedSubtree := fi.stakeVotedAt
	deepestChildHeight := 0
	bestSlot := key
	deepestSlot := key
	isDupConfirmed := false
	bestChildStake := uint64(0)
	bestChildKey := key
	deepestChildStake := uint64(0)
	deepestChildKey := key
	for _, ck := range fi.children {
		childInfo := h.forkInfos[ck]
		isDupConfirmed = isDupConfirmed || childInfo.isDuplicateConfirmed
		stakeVotedSubtree += childInfo.stakeVotedSubtree
		if childInfo.isCandidate() &&
			(bestChildKey == key ||
				childInfo.stakeVotedSubtree > bestChildStake ||
				(childInfo.stakeVotedSubtree == bestChildStake && ck.less(bestChildKey))) {
			bestChildStake = childInfo.stakeVotedSubtree
			bestChildKey = ck
			bestSlot = childInfo.bestSlot
		}
		if deepestChildKey == key ||
			childInfo.height > deepestChildHeight ||
			(childInfo.height == deepestChildHeight && childInfo.stakeVotedSubtree > deepestChildStake) ||
			(childInfo.height == deepestChildHeight && childInfo.stakeVotedSubtree == deepestChildStake && ck.less(deepestChildKey)) {
			deepestChildHeight = childInfo.height
			deepestChildStake = childInfo.stakeVotedSubtree
			deepestChildKey = ck
			deepestSlot = childInfo.deepestSlot
		}
	}
	if isDupConfirmed && !fi.isDuplicateConfirmed {
		mlog.Log.Infof("fork choice: setting (%d) to duplicate confirmed", key.Slot)
		fi.setDuplicateConfirmed()
	}
	fi.stakeVotedSubtree = stakeVotedSubtree
	fi.height = deepestChildHeight + 1
	fi.bestSlot = bestSlot
	fi.deepestSlot = deepestSlot
}

func (h *HeaviestSubtreeForkChoice) markForkValid(key SlotHashKey, validSlot uint64) {
	if fi, ok := h.forkInfos[key]; ok {
		fi.updateWithNewlyValidAncestor(validSlot)
		if key.Slot == validSlot {
			fi.isDuplicateConfirmed = true
		}
	}
}

func (h *HeaviestSubtreeForkChoice) markForkInvalid(key SlotHashKey, invalidSlot uint64) {
	if fi, ok := h.forkInfos[key]; ok {
		fi.updateWithNewlyInvalidAncestor(invalidSlot)
	}
}

// MarkForkInvalidCandidate excludes the subtree rooted at key from best-slot
// selection (unconfirmed duplicate); its stake still backs shared ancestors.
func (h *HeaviestSubtreeForkChoice) MarkForkInvalidCandidate(key SlotHashKey) {
	fi, ok := h.forkInfos[key]
	if !ok {
		return
	}
	if fi.isDuplicateConfirmed {
		panic("heaviest-subtree: cannot mark a duplicate-confirmed fork invalid")
	}
	ops := make(updateOps)
	// The whole subtree INCLUDING key gets the mark (Agave subtree_diff includes
	// its root; the key itself becomes latest_invalid_ancestor == own slot).
	for _, node := range append([]SlotHashKey{key}, h.subtreeKeys(key)...) {
		h.insertOneAggregate(ops, node, labelMarkInvalid, key.Slot)
	}
	h.insertAggregateOperations(ops, key)
	h.processUpdateOperations(ops)
}

// MarkForkValidCandidate re-admits a fork after its version is duplicate-confirmed;
// returns the newly duplicate-confirmed ancestors.
func (h *HeaviestSubtreeForkChoice) MarkForkValidCandidate(key SlotHashKey) []SlotHashKey {
	var newlyConfirmed []SlotHashKey
	for cur := &key; cur != nil; cur = h.parentOf(*cur) {
		if fi, ok := h.forkInfos[*cur]; ok && !fi.isDuplicateConfirmed {
			newlyConfirmed = append(newlyConfirmed, *cur)
		}
	}
	ops := make(updateOps)
	for _, node := range append([]SlotHashKey{key}, h.subtreeKeys(key)...) {
		h.insertOneAggregate(ops, node, labelMarkValid, key.Slot)
	}
	h.insertAggregateOperations(ops, key)
	h.processUpdateOperations(ops)
	return newlyConfirmed
}

// SetTreeRoot advances the root, dropping everything not reachable from newRoot.
func (h *HeaviestSubtreeForkChoice) SetTreeRoot(newRoot SlotHashKey) {
	nfi, ok := h.forkInfos[newRoot]
	if !ok {
		panic("heaviest-subtree: new root does not exist in fork choice")
	}
	keep := make(map[SlotHashKey]bool)
	for _, k := range h.subtreeKeys(newRoot) {
		keep[k] = true
	}
	keep[newRoot] = true
	for k := range h.forkInfos {
		if !keep[k] {
			delete(h.forkInfos, k)
		}
	}
	nfi.parent = nil
	h.treeRoot = newRoot
}

// propagateNewLeaf pushes a just-added leaf's best/deepest status up the tree
// without a full re-aggregation.
func (h *HeaviestSubtreeForkChoice) propagateNewLeaf(key, parent SlotHashKey) {
	parentBest := h.forkInfos[parent].bestSlot
	if h.isBestChild(key) {
		ancestor := &parent
		for ancestor != nil {
			ancestorInfo := h.forkInfos[*ancestor]
			if ancestorInfo.bestSlot == parentBest {
				ancestorInfo.bestSlot = key
			} else {
				break
			}
			ancestor = ancestorInfo.parent
		}
	}
	ancestor := &parent
	currentChild := key
	currentHeight := 1
	for ancestor != nil {
		if !h.isDeepestChild(currentChild) {
			break
		}
		ancestorInfo := h.forkInfos[*ancestor]
		ancestorInfo.deepestSlot = key
		ancestorInfo.height = currentHeight + 1
		currentChild = *ancestor
		currentHeight = ancestorInfo.height
		ancestor = ancestorInfo.parent
	}
}

// isBestChild: heaviest among its parent's CANDIDATE children, ties to lower key
// (only siblings are candidacy-filtered; the node itself is not).
func (h *HeaviestSubtreeForkChoice) isBestChild(key SlotHashKey) bool {
	fi := h.forkInfos[key]
	if fi.parent == nil {
		return true
	}
	myStake := fi.stakeVotedSubtree
	for _, sibling := range h.forkInfos[*fi.parent].children {
		if sibling == key {
			continue
		}
		siblingInfo := h.forkInfos[sibling]
		if !siblingInfo.isCandidate() {
			continue
		}
		if siblingInfo.stakeVotedSubtree > myStake ||
			(siblingInfo.stakeVotedSubtree == myStake && sibling.less(key)) {
			return false
		}
	}
	return true
}

func (h *HeaviestSubtreeForkChoice) isDeepestChild(key SlotHashKey) bool {
	fi := h.forkInfos[key]
	if fi.parent == nil {
		return true
	}
	myHeight, myStake := fi.height, fi.stakeVotedSubtree
	for _, sibling := range h.forkInfos[*fi.parent].children {
		if sibling == key {
			continue
		}
		siblingInfo := h.forkInfos[sibling]
		if siblingInfo.height > myHeight ||
			(siblingInfo.height == myHeight && siblingInfo.stakeVotedSubtree > myStake) ||
			(siblingInfo.height == myHeight && siblingInfo.stakeVotedSubtree == myStake && sibling.less(key)) {
			return false
		}
	}
	return true
}

func (h *HeaviestSubtreeForkChoice) parentOf(key SlotHashKey) *SlotHashKey {
	if fi, ok := h.forkInfos[key]; ok {
		return fi.parent
	}
	return nil
}

// subtreeKeys returns all descendants of key (excluding key), BFS order.
func (h *HeaviestSubtreeForkChoice) subtreeKeys(key SlotHashKey) []SlotHashKey {
	var out []SlotHashKey
	queue := append([]SlotHashKey(nil), h.forkInfos[key].children...)
	for len(queue) > 0 {
		k := queue[0]
		queue = queue[1:]
		out = append(out, k)
		queue = append(queue, h.forkInfos[k].children...)
	}
	return out
}

func insertSortedKey(keys []SlotHashKey, k SlotHashKey) []SlotHashKey {
	i := sort.Search(len(keys), func(i int) bool { return !keys[i].less(k) })
	keys = append(keys, SlotHashKey{})
	copy(keys[i+1:], keys[i:])
	keys[i] = k
	return keys
}
