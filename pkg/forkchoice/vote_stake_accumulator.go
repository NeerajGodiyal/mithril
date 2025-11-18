package forkchoice

import (
	"github.com/gagliardetto/solana-go"
)

type voteStakeAccumulator struct {
	stakePerHash       map[solana.Hash]uint64
	supermajorityStake uint64
	slot               uint64
}

func newVoteStakeAccumulator(totalStake uint64, slot uint64) *voteStakeAccumulator {
	return &voteStakeAccumulator{
		stakePerHash: make(map[solana.Hash]uint64),
		// calculate supermajority stake weight threshold (2/3)
		supermajorityStake: (totalStake*2)/3 + 1,
		slot:               slot,
	}
}

func (accumulator *voteStakeAccumulator) add(hash solana.Hash, stake uint64) {
	accumulator.stakePerHash[hash] += stake
}

func (accumulator *voteStakeAccumulator) hashHasSupermajority(hash solana.Hash) bool {
	stake := accumulator.stakePerHash[hash]
	return stake >= accumulator.supermajorityStake
}
