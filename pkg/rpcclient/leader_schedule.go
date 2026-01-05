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

// GetLeaderScheduleForSlot fetches the leader schedule for the epoch containing the given slot.
// IMPORTANT: The solana-go library's GetLeaderScheduleOpts.Epoch field is misleadingly named -
// it actually expects a SLOT, not an epoch number. The RPC method getLeaderSchedule takes a
// slot parameter and returns the schedule for the epoch containing that slot.
// Pass firstSlotInEpoch to get the schedule for a specific epoch.
func (fetcher *RpcClient) GetLeaderScheduleForSlot(slot uint64) (map[solana.PublicKey][]uint64, error) {
	return fetcher.client.GetLeaderScheduleWithOpts(context.Background(), &rpc.GetLeaderScheduleOpts{
		Epoch: &slot, // Despite the name, this field is passed as the slot parameter to RPC
	})
}
