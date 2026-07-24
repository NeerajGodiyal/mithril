package global

import (
	"sync"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/epochstakes"
	"github.com/Overclock-Validator/mithril/pkg/leaderschedule"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func testVoteKeyedSchedule(
	node solana.PublicKey,
	vote solana.PublicKey,
	epoch uint64,
	epochSchedule *sealevel.SysvarEpochSchedule,
) *leaderschedule.LeaderSchedule {
	return leaderschedule.New(
		map[solana.PublicKey]*epochstakes.VoteAccount{
			vote: {NodePubkey: node},
		},
		map[solana.PublicKey]uint64{vote: 1},
		epochSchedule,
		epoch,
		epochSchedule.SlotsInEpoch(epoch),
		1,
	)
}

func TestEpochLeaderVoteAccountsCoexist(t *testing.T) {
	SetLeaderSchedule(nil)
	t.Cleanup(func() { SetLeaderSchedule(nil) })

	node := solana.PublicKey{9}
	oldVote := solana.PublicKey{1}
	newVote := solana.PublicKey{2}
	epochSchedule := &sealevel.SysvarEpochSchedule{
		SlotsPerEpoch:            32,
		LeaderScheduleSlotOffset: 32,
	}
	SetLeaderScheduleForEpoch(0, testVoteKeyedSchedule(node, oldVote, 0, epochSchedule))
	SetLeaderScheduleForEpoch(1, testVoteKeyedSchedule(node, newVote, 1, epochSchedule))

	gotNode, gotVote, ok := LeaderForSlotWithVoteAccount(31)
	require.True(t, ok)
	require.Equal(t, node, gotNode)
	require.Equal(t, oldVote, gotVote)

	gotNode, gotVote, ok = LeaderForSlotWithVoteAccount(32)
	require.True(t, ok)
	require.Equal(t, node, gotNode)
	require.Equal(t, newVote, gotVote)
}

func TestLeaderVoteAccountReplacementReturnsCoherentPair(t *testing.T) {
	SetLeaderSchedule(nil)
	t.Cleanup(func() { SetLeaderSchedule(nil) })

	nodeA, voteA := solana.PublicKey{1}, solana.PublicKey{11}
	nodeB, voteB := solana.PublicKey{2}, solana.PublicKey{12}
	epochSchedule := &sealevel.SysvarEpochSchedule{
		SlotsPerEpoch:            32,
		LeaderScheduleSlotOffset: 32,
	}
	scheduleA := testVoteKeyedSchedule(nodeA, voteA, 0, epochSchedule)
	scheduleB := testVoteKeyedSchedule(nodeB, voteB, 0, epochSchedule)
	SetLeaderScheduleForEpoch(0, scheduleA)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 1_000 {
			SetLeaderScheduleForEpoch(0, scheduleA)
			SetLeaderScheduleForEpoch(0, scheduleB)
		}
	}()
	for range 2_000 {
		node, vote, ok := LeaderForSlotWithVoteAccount(0)
		require.True(t, ok)
		require.True(t,
			(node == nodeA && vote == voteA) || (node == nodeB && vote == voteB),
			"incoherent leader pair node=%s vote=%s", node, vote,
		)
	}
	wg.Wait()
}
