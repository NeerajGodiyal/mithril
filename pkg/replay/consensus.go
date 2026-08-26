package replay

import (
	"fmt"

	consensusengine "github.com/Overclock-Validator/mithril/pkg/consensus"
	"github.com/Overclock-Validator/mithril/pkg/state"
)

// ConsensusOpts selects replay's protocol semantics and optionally carries the
// Alpenglow certificate engine. Alpenglow with a nil Engine uses delegated
// (RPC-attested) finality; a nil opts value is classic verifying replay.
type ConsensusOpts struct {
	Alpenglow bool
	Engine    consensusengine.Engine
	// RootedDurable selects manifest-backed fold/recovery semantics. Alpenglow
	// always uses it; classic clusters may use it only with finalized RPC input.
	RootedDurable bool
	// FinalizedRPC means the block scheduler fetches and proves skips using only
	// finalized RPC commitment, so each observed classic block advances the
	// finality watermark without an Alpenglow certificate engine.
	FinalizedRPC bool
	// RootedEvents enables manifest-selected rooted event sidecars. It is
	// independent from transaction submission or signing.
	RootedEvents bool

	// TransactionStatusCheckpointAfterCommit performs advisory sidecar
	// retention after AccountsDB has durably selected a checkpoint reference.
	// Replay logs and ignores its error because the account fold is committed.
	TransactionStatusCheckpointAfterCommit func(*state.TransactionStatusCheckpointRef) error

	// RootedEventAfterCommit performs advisory retention after AccountsDB has
	// durably selected an event sidecar reference.
	RootedEventAfterCommit func(*state.RootedEventBatchRef) error
}

func validateConsensusReplayProfile(accountsRooted bool, opts *ConsensusOpts, verifier VerifierConfig, useLightbringer, useTurbine bool) error {
	rooted := opts != nil && opts.RootedDurable
	if rooted != accountsRooted {
		return fmt.Errorf("replay rooted-durable profile %t does not match AccountsDB profile %t", rooted, accountsRooted)
	}
	if opts == nil {
		return nil
	}
	if opts.RootedEvents && !opts.RootedDurable {
		return fmt.Errorf("rooted events require rooted-durable replay")
	}
	if opts.Alpenglow {
		if !opts.RootedDurable {
			return fmt.Errorf("Alpenglow replay requires rooted-durable storage")
		}
		if opts.FinalizedRPC {
			return fmt.Errorf("Alpenglow replay cannot use classic finalized-RPC semantics")
		}
		if useLightbringer || !useTurbine {
			return fmt.Errorf("Alpenglow replay requires the Turbine block source for exact block identity")
		}
		return nil
	}
	if opts.FinalizedRPC {
		if !opts.RootedDurable {
			return fmt.Errorf("classic finalized-RPC replay requires rooted-durable storage")
		}
		if useLightbringer || useTurbine {
			return fmt.Errorf("classic finalized-RPC replay requires the RPC block source")
		}
	}
	if opts.RootedDurable {
		if !opts.FinalizedRPC {
			return fmt.Errorf("classic rooted-durable replay requires finalized RPC input")
		}
		if !verifier.Enabled || !verifier.Required {
			return fmt.Errorf("classic rooted-durable replay requires the trailing verifier enabled and required")
		}
	}
	return nil
}
