package rpcclient

import (
	"context"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

func (fetcher *RpcClient) GetLeaderSchedule() (map[solana.PublicKey][]uint64, error) {
	leaderSchedule, err := fetcher.client.GetLeaderSchedule(context.Background())
	return leaderSchedule, err
}

// GetLeaderScheduleForEpoch fetches the leader schedule for a specific epoch.
// This is needed when validating historical epochs (during catchup) since the
// default GetLeaderSchedule returns the current epoch's schedule.
func (fetcher *RpcClient) GetLeaderScheduleForEpoch(epoch uint64) (map[solana.PublicKey][]uint64, error) {
	return fetcher.client.GetLeaderScheduleWithOpts(context.Background(), &rpc.GetLeaderScheduleOpts{
		Epoch: &epoch,
	})
}
