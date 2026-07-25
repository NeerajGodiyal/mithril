package replay

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/epochstakes"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/leaderschedule"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func testExactLeaderVoteSchedule(
	node solana.PublicKey,
	vote solana.PublicKey,
) *leaderschedule.LeaderSchedule {
	epochSchedule := &sealevel.SysvarEpochSchedule{
		SlotsPerEpoch:            32,
		LeaderScheduleSlotOffset: 32,
	}
	return leaderschedule.New(
		map[solana.PublicKey]*epochstakes.VoteAccount{
			vote: {NodePubkey: node},
		},
		map[solana.PublicKey]uint64{vote: 1},
		epochSchedule,
		0,
		1,
		1,
	)
}

func TestLeaderVotePubkeyUsesExactScheduleMetadata(t *testing.T) {
	global.SetLeaderSchedule(nil)
	t.Cleanup(func() { global.SetLeaderSchedule(nil) })

	node := solana.PublicKey{9}
	selectedVote := solana.PublicKey{1}
	otherVote := solana.PublicKey{2}
	global.SetLeaderSchedule(testExactLeaderVoteSchedule(node, selectedVote))

	got, err := leaderVotePubkey(
		0,
		map[solana.PublicKey]*epochstakes.VoteAccount{
			selectedVote: {NodePubkey: node},
			otherVote:    {NodePubkey: node},
		},
		node,
	)
	require.NoError(t, err)
	require.Equal(t, selectedVote, got)
}

func TestLeaderVotePubkeyRejectsScheduledNodeMismatch(t *testing.T) {
	global.SetLeaderSchedule(nil)
	t.Cleanup(func() { global.SetLeaderSchedule(nil) })

	scheduledNode := solana.PublicKey{9}
	blockLeader := solana.PublicKey{10}
	selectedVote := solana.PublicKey{1}
	global.SetLeaderSchedule(testExactLeaderVoteSchedule(scheduledNode, selectedVote))

	_, err := leaderVotePubkey(0, nil, blockLeader)
	require.ErrorContains(t, err, "does not match block leader")
}

func TestLeaderVotePubkeyNodeFallbackRequiresUniqueMatch(t *testing.T) {
	global.SetLeaderSchedule(nil)
	t.Cleanup(func() { global.SetLeaderSchedule(nil) })

	node := solana.PublicKey{9}
	voteA := solana.PublicKey{1}
	voteB := solana.PublicKey{2}

	got, err := leaderVotePubkey(
		0,
		map[solana.PublicKey]*epochstakes.VoteAccount{
			voteA: {NodePubkey: node},
		},
		node,
	)
	require.NoError(t, err)
	require.Equal(t, voteA, got)

	_, err = leaderVotePubkey(
		0,
		map[solana.PublicKey]*epochstakes.VoteAccount{
			voteA: {NodePubkey: node},
			voteB: {NodePubkey: node},
		},
		node,
	)
	require.ErrorContains(t, err, "ambiguous")

	_, err = leaderVotePubkey(0, nil, node)
	require.ErrorContains(t, err, "not found")
}
