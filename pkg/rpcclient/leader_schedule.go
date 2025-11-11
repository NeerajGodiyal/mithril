package rpcclient

import (
	"context"

	"github.com/gagliardetto/solana-go"
)

func (fetcher *RpcClient) GetLeaderSchedule() (map[solana.PublicKey][]uint64, error) {
	leaderSchedule, err := fetcher.client.GetLeaderSchedule(context.Background())
	return leaderSchedule, err
}
