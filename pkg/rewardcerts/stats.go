package rewardcerts

import "fmt"

// FooterCertReadiness describes whether accumulated Votor votes can produce
// skip/notar reward certificates for a leader slot's footer.
type FooterCertReadiness struct {
	LeaderSlot     uint64
	RewardSlot     uint64
	PendingSlots   int
	LatestVoteSlot uint64
	SkipVotes      int
	NotarVotes     int
	SkipReady      bool
	NotarReady     bool
}

func (r FooterCertReadiness) Format(listenerActive bool) string {
	listener := "disabled"
	if listenerActive {
		listener = "active"
	}
	certs := "none"
	switch {
	case r.SkipReady && r.NotarReady:
		certs = "skip+notar"
	case r.SkipReady:
		certs = "skip"
	case r.NotarReady:
		certs = "notar"
	}
	return fmt.Sprintf(
		" reward_cert_listener=%s reward_cert_pending=%d reward_cert_latest_vote_slot=%d footer_reward_slot=%d footer_skip_votes=%d footer_notar_votes=%d footer_certs=%s",
		listener, r.PendingSlots, r.LatestVoteSlot, r.RewardSlot, r.SkipVotes, r.NotarVotes, certs,
	)
}

// ReadinessForLeaderSlot inspects accumulated votes for the reward window that
// leaderSlot embeds in its footer (leaderSlot - SlotsForReward).
func (b *Builder) ReadinessForLeaderSlot(leaderSlot uint64) FooterCertReadiness {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := FooterCertReadiness{
		LeaderSlot:   leaderSlot,
		PendingSlots: len(b.votes),
	}
	for slot := range b.votes {
		if slot > out.LatestVoteSlot {
			out.LatestVoteSlot = slot
		}
	}

	rewardSlot, ok := rewardSlotForLeader(leaderSlot)
	if !ok {
		return out
	}
	out.RewardSlot = rewardSlot

	entry, ok := b.votes[rewardSlot]
	if !ok {
		return out
	}
	out.SkipVotes = entry.skip.votesSeen()
	out.NotarVotes = entry.notar.votesSeen()
	out.SkipReady = out.SkipVotes > 0
	out.NotarReady = out.NotarVotes > 0
	return out
}
