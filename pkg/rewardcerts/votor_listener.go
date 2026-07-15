package rewardcerts

import (
	"github.com/Overclock-Validator/mithril/pkg/alpenglow"
)

// ObserveVotorMessage records skip/notar votes from a decoded Votor message.
func (b *Builder) ObserveVotorMessage(msg alpenglow.Message) {
	if msg.Vote == nil {
		return
	}
	switch msg.Vote.Vote.Type {
	case alpenglow.VoteTypeSkip, alpenglow.VoteTypeNotarize:
		b.AddVote(*msg.Vote)
	}
}
