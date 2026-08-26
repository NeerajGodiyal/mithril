// Package txstatus records the fate of transactions this node submitted.
//
// It deliberately does not model arbitrary signature history. A verifying node
// that never indexed the whole chain cannot answer "what happened to signature
// X" without retained submission evidence, and pretending otherwise would be a
// lie a caller might act on. The index therefore only ever grows from a
// submission and reports StatusUnknown when it has no retained evidence.
//
// The package is neutral by construction: it imports neither pkg/rpcserver nor
// pkg/replay, so both can depend on it without a cycle.
package txstatus

import (
	"time"

	"github.com/gagliardetto/solana-go"
)

// Status is the lifecycle of a transaction we submitted.
//
// StatusUnknown is not a failure. It means the index has no evidence, which is
// the correct answer before landing and after eviction, and is what a caller
// must see rather than a fabricated negative.
type Status uint8

const (
	StatusUnknown Status = iota
	// StatusSubmissionUnknown means forwarding began but no network delivery
	// was proven. An I/O error is ambiguous, so callers must not treat this as
	// either submitted or failed.
	StatusSubmissionUnknown
	// StatusSubmitted means we handed it to the network and have not yet
	// observed it in a replayed block.
	StatusSubmitted
	// StatusLanded means replay observed it executing successfully in a block
	// that is not yet rooted, so it can still be unwound.
	StatusLanded
	// StatusLandedFailed means replay observed an execution failure in an
	// unrooted block. It remains non-terminal because that fork can unwind.
	StatusLandedFailed
	// StatusRooted means it landed in a slot that has since been rooted. This
	// is terminal.
	StatusRooted
	// StatusFailed means the rooted transaction executed with an error. It is
	// terminal.
	StatusFailed
	// StatusUnwound means it landed on a fork that was later abandoned. It may
	// land again, so this is not terminal.
	StatusUnwound
	// StatusExpired means its known block-height validity window passed without
	// it landing in the node's observed fork. It is provisional because replay
	// may unwind to a lower fork and later observe the transaction. Durable-nonce
	// and older-blockhash submissions have no inferred deadline.
	StatusExpired
)

func (s Status) String() string {
	switch s {
	case StatusSubmissionUnknown:
		return "submission_unknown"
	case StatusSubmitted:
		return "submitted"
	case StatusLanded:
		return "landed"
	case StatusLandedFailed:
		return "landed_failed"
	case StatusRooted:
		return "rooted"
	case StatusFailed:
		return "failed"
	case StatusUnwound:
		return "unwound"
	case StatusExpired:
		return "expired"
	default:
		return "unknown"
	}
}

// Terminal reports whether the status can still change through normal
// progress. A caller polling for an outcome can stop once this is true.
func (s Status) Terminal() bool {
	switch s {
	case StatusRooted, StatusFailed:
		return true
	default:
		return false
	}
}

// Receipt is what the node knows about one transaction it submitted. Values
// are returned by copy so a caller cannot reach back into indexed state.
type Receipt struct {
	Signature solana.Signature `json:"signature"`
	Status    Status           `json:"-"`
	// StatusName carries the status over the wire; Status itself is an
	// internal integer whose values must not be depended on externally.
	StatusName string `json:"status"`

	SubmittedAt     time.Time   `json:"submitted_at"`
	RecentBlockhash solana.Hash `json:"recent_blockhash"`
	// LastValidBlockHeight is set only when the transaction uses the exact
	// latest blockhash published by this node. Older hashes and durable nonces
	// cannot be assigned a deadline safely from the transaction bytes alone.
	LastValidBlockHeight *uint64 `json:"last_valid_block_height,omitempty"`

	// LandedSlot is the slot replay observed it in, or zero if it has not
	// landed. It is cleared on unwind, because a receipt must never claim a
	// slot on an abandoned fork.
	LandedSlot uint64 `json:"landed_slot,omitempty"`
	// ExecutionError is the on-chain error when Status is StatusLandedFailed or
	// StatusFailed, and empty otherwise.
	ExecutionError string `json:"execution_error,omitempty"`

	UpdatedAt          time.Time `json:"updated_at"`
	statusBeforeExpiry Status
}

// Sink is the narrow interface the rest of the node writes through. Keeping it
// this small is what lets RPC and replay both depend on this package without
// either depending on the other.
type Sink interface {
	// SubmissionAttempted records that forwarding began. It is intentionally
	// separate from Forwarded because an I/O failure cannot prove non-delivery.
	SubmissionAttempted(signature solana.Signature, recentBlockhash solana.Hash, lastValidBlockHeight *uint64) error
	// Forwarded records that at least one network send completed successfully.
	Forwarded(signature solana.Signature)
	// Landed records replay observing the transaction in a block. A non-empty
	// executionError means it executed and failed.
	Landed(signature solana.Signature, slot uint64, executionError string)
	// Rooted promotes every receipt that landed at or below throughSlot.
	Rooted(throughSlot uint64)
	// Unwound reverts every non-rooted receipt that landed at or above
	// fromSlot, because that fork was abandoned.
	Unwound(fromSlot uint64)
	// DurableRewound lowers the rooted watermark after an explicit durable
	// rollback. Callers then unwind the abandoned slot range normally.
	DurableRewound(throughSlot uint64)
	// ObserveBlockHeight advances the node's block height so submissions with a
	// known validity window can expire even when nothing of ours lands.
	ObserveBlockHeight(blockHeight uint64)
	// RewindBlockHeight restores receipts whose deadline is valid again after
	// replay moves to a lower fork.
	RewindBlockHeight(blockHeight uint64)
}

// Reader is the query half. It is separate from Sink so a component that only
// answers questions cannot also rewrite history.
type Reader interface {
	// Lookup reports what is known about a signature. The boolean is false
	// when the current index has no retained evidence, which is NOT a failure
	// and does not prove the transaction was never submitted.
	Lookup(signature solana.Signature) (Receipt, bool)
}

// Store is both halves, which is what the node actually installs.
type Store interface {
	Sink
	Reader
}
