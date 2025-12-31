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
	"time"

	//"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

func (fetcher *RpcClient) GetBlock(slot uint64) (*rpc.GetBlockResult, error) {
	return fetcher.client.GetBlock(context.TODO(), slot)
}

func (fetcher *RpcClient) GetBlockConfirmed(slot uint64) (*rpc.GetBlockResult, error) {
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
				Commitment:                     rpc.CommitmentConfirmed,
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

var SlotSkipped = errors.New("slot skipped")

// GetBlockConfirmedOnce fetches a block with a single RPC attempt (no internal retry).
// Use this with rate-limited parallel fetching where the scheduler handles retries.
// Uses a 30-second timeout to prevent worker stalls on hung RPC connections.
func (fetcher *RpcClient) GetBlockConfirmedOnce(slot uint64) (*rpc.GetBlockResult, error) {
	includeRewards := true
	maxSupportedTxVer := uint64(0)

	// Use a timeout to prevent indefinite hangs on slow/stuck RPC connections
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := fetcher.client.GetBlockWithOpts(
		ctx,
		slot,
		&rpc.GetBlockOpts{
			MaxSupportedTransactionVersion: &maxSupportedTxVer,
			Commitment:                     rpc.CommitmentConfirmed,
			TransactionDetails:             rpc.TransactionDetailsFull,
			Rewards:                        &includeRewards,
		},
	)

	if err != nil {
		if strings.Contains(err.Error(), fmt.Sprintf("Slot %d was skipped", slot)) {
			return nil, SlotSkipped
		}
		return nil, err
	}

	return result, nil
}

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

	for attempt := uint64(0); attempt < 20; attempt++ {
		result, err = fetcher.client.GetBlockWithOpts(
			context.TODO(),
			slot+attempt,
			&rpc.GetBlockOpts{
				MaxSupportedTransactionVersion: &maxSupportedTxVer,
				Commitment:                     rpc.CommitmentFinalized,
				TransactionDetails:             rpc.TransactionDetailsNone,
				Rewards:                        &includeRewards,
			},
		)

		if err == nil {
			break
		} else if strings.Contains(err.Error(), fmt.Sprintf("Slot %d was skipped", slot+attempt)) {
			continue
		} else {
			if attempt < 19 {
				waitTime := time.Duration(1<<attempt) * time.Second // 1s, 2s, 4s, 8s...
				if waitTime > 30*time.Second {
					waitTime = 30 * time.Second
				}
				mlog.Log.Infof("might be too early for slot %d, retrying in %v (attempt %d/10)", slot, waitTime, attempt+1)
				time.Sleep(waitTime)
			}
		}
	}

	if result == nil {
		return 0, fmt.Errorf("unable to fetch numRewardPartitions")
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
	slot, err := fetcher.client.GetSlot(context.TODO(), rpc.CommitmentConfirmed)
	if err != nil {
		return 0, rpc.ErrNotConfirmed
	}
	return slot, err
}

// GetSlotWithTimeout returns the current slot with a timeout.
// Useful for health probes where we don't want to block indefinitely.
func (fetcher *RpcClient) GetSlotWithTimeout(timeout time.Duration) (uint64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	slot, err := fetcher.client.GetSlot(ctx, rpc.CommitmentConfirmed)
	if err != nil {
		return 0, err
	}
	return slot, nil
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
