package replay

import (
	consensusengine "github.com/Overclock-Validator/mithril/pkg/consensus"
)

// ConsensusOpts selects replay's protocol semantics and optionally carries the
// Alpenglow certificate engine. Alpenglow with a nil Engine uses delegated
// (RPC-attested) finality; a nil opts value is classic verifying replay.
type ConsensusOpts struct {
	Alpenglow bool
	Engine    consensusengine.Engine
}
