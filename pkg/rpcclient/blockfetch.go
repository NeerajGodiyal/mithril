package rpcclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	//"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

func (fetcher *RpcClient) GetBlock(slot uint64) (*rpc.GetBlockResult, error) {
	return fetcher.client.GetBlock(context.TODO(), slot)
}

func (fetcher *RpcClient) GetBlockConfirmed(slot uint64) (*rpc.GetBlockResult, error) {
	includeRewards := false
	maxSupportedTxVer := uint64(0)

	result, err := fetcher.client.GetBlockWithOpts(
		context.TODO(),
		slot,
		&rpc.GetBlockOpts{
			MaxSupportedTransactionVersion: &maxSupportedTxVer,
			Encoding:                       solana.EncodingBase64,
			Commitment:                     rpc.CommitmentConfirmed,
			TransactionDetails:             rpc.TransactionDetailsFull,
			Rewards:                        &includeRewards,
		},
	)

	return result, err
}

var SlotSkipped = errors.New("slot skipped")

func (fetcher *RpcClient) GetBlockFinalized(slot uint64) (*rpc.GetBlockResult, error) {
	includeRewards := true
	maxSupportedTxVer := uint64(0)

	var result *rpc.GetBlockResult
	var err error

	for count := 0; count < 10; count++ {
		result, err = fetcher.client.GetBlockWithOpts(
			context.TODO(),
			slot,
			&rpc.GetBlockOpts{
				MaxSupportedTransactionVersion: &maxSupportedTxVer,
				Commitment:                     rpc.CommitmentFinalized,
				TransactionDetails:             rpc.TransactionDetailsFull,
				Rewards:                        &includeRewards,
			},
		)

		if err == nil {
			break
		} else {
			if strings.Contains(err.Error(), fmt.Sprintf("Slot %d was skipped", slot)) {
				return nil, SlotSkipped
			} else {
				//mlog.Log.Debugf("fetch block %d failed - retrying", slot)
			}
		}
	}

	return result, err
}

func (fetcher *RpcClient) GetRewardsForSlot(slot uint64) ([]rpc.BlockReward, error) {
	includeRewards := true
	maxSupportedTxVer := uint64(0)

	result, err := fetcher.client.GetBlockWithOpts(
		context.TODO(),
		slot,
		&rpc.GetBlockOpts{
			MaxSupportedTransactionVersion: &maxSupportedTxVer,
			Commitment:                     rpc.CommitmentFinalized,
			TransactionDetails:             rpc.TransactionDetailsNone,
			Rewards:                        &includeRewards,
		},
	)
	if err != nil {
		return nil, err
	}

	return result.Rewards, err
}

func (fetcher *RpcClient) GetNumRewardPartitions(slot uint64) (uint64, error) {
	includeRewards := true
	maxSupportedTxVer := uint64(0)

	var result *rpc.GetBlockResult
	var err error

	for count := uint64(0); count < 20; count++ {
		result, err = fetcher.client.GetBlockWithOpts(
			context.TODO(),
			slot+count,
			&rpc.GetBlockOpts{
				MaxSupportedTransactionVersion: &maxSupportedTxVer,
				Commitment:                     rpc.CommitmentFinalized,
				TransactionDetails:             rpc.TransactionDetailsNone,
				Rewards:                        &includeRewards,
			},
		)

		if err == nil {
			break
		} else if strings.Contains(err.Error(), fmt.Sprintf("Slot %d was skipped", slot+count)) {
			continue
		} else {
			panic(err)
		}
	}

	if result.NumRewardPartitions == nil {
		return 0, fmt.Errorf("no numRewardPartitions field present")
	}

	return *result.NumRewardPartitions, nil
}

func (fetcher *RpcClient) GetStakingRewardSlots(startSlot uint64, numPartitions uint64) ([]uint64, error) {
	result, err := fetcher.client.GetBlocksWithLimit(
		context.TODO(),
		startSlot+1,
		numPartitions,
		rpc.CommitmentFinalized)

	if err != nil {
		return nil, err
	}

	return *result, nil
}

func (fetcher *RpcClient) GetRewardSlots(slot uint64) ([]rpc.BlockReward, *uint64, error) {
	includeRewards := true
	maxSupportedTxVer := uint64(0)

	result, err := fetcher.client.GetBlockWithOpts(
		context.TODO(),
		slot,
		&rpc.GetBlockOpts{
			MaxSupportedTransactionVersion: &maxSupportedTxVer,
			Commitment:                     rpc.CommitmentFinalized,
			TransactionDetails:             rpc.TransactionDetailsNone,
			Rewards:                        &includeRewards,
		},
	)
	if err != nil {
		return nil, nil, err
	}

	return result.Rewards, result.NumRewardPartitions, err
}

func (fetcher *RpcClient) GetLatestBlockConfirmed() (*rpc.GetBlockResult, error) {
	result, err := fetcher.client.GetLatestBlockhash(context.TODO(), rpc.CommitmentConfirmed)
	if err != nil {
		return nil, err
	}

	slot := result.Context.Slot

	return fetcher.GetBlockConfirmed(slot)
}

func (fetcher *RpcClient) GetLatestBlockFinalized() (*rpc.GetBlockResult, error) {
	result, err := fetcher.client.GetLatestBlockhash(context.TODO(), rpc.CommitmentFinalized)
	if err != nil {
		return nil, err
	}

	slot := result.Context.Slot

	return fetcher.GetBlockFinalized(slot)
}

func (fetcher *RpcClient) GetLeaderForSlot(slot uint64) (solana.PublicKey, error) {
	leader, err := fetcher.client.GetSlotLeaders(context.TODO(), slot, 1)
	if err != nil {
		return solana.PublicKey{}, err
	}
	return leader[0], err
}

func (fetcher *RpcClient) GetBlockTime(slot uint64) (int64, error) {
	ts, err := fetcher.client.GetBlockTime(context.TODO(), slot)
	if err != nil {
		return 0, rpc.ErrNotConfirmed
	}
	return int64(*ts), err
}

func (fetcher *RpcClient) GetSlot() (uint64, error) {
	slot, err := fetcher.client.GetSlot(context.TODO(), rpc.CommitmentFinalized)
	if err != nil {
		return 0, rpc.ErrNotConfirmed
	}
	return slot, err
}

func (fetcher *RpcClient) DownloadBlocksToFile(outDir string, slot uint64, num int64) {
	var fetchForever bool
	if num < 0 {
		fetchForever = true
	}

	endSlot := slot + uint64(num)
	slotMu := &sync.Mutex{}
	wg := &sync.WaitGroup{}
	workers := 10
	wg.Add(int(workers))
	for i := 0; i < int(workers); i++ {
		go func() {
			defer wg.Done()
			for {
				slotMu.Lock()
				s := slot
				slot++
				slotMu.Unlock()
				if !fetchForever && s > endSlot {
					return
				}

				blockResult, err := fetcher.GetBlockFinalized(s)
				if err == SlotSkipped {
					continue
				} else if err != nil {
					mlog.Log.Errorf("error fetching slot=%d: %v", s, err)
					continue
				}
				blockFilename := filepath.Join(filepath.Clean(outDir), fmt.Sprintf("%d.json", s))
				saveBlockToFile(blockFilename, blockResult)
			}
		}()
	}
	wg.Wait()
}

func saveBlockToFile(filename string, b *rpc.GetBlockResult) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(b)
}
