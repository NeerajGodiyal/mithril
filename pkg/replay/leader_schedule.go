package replay

import (
	"errors"
	"fmt"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/leaderschedule"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
)

// ErrLeaderScheduleFailed is returned when leader schedule cannot be fetched from any endpoint
var ErrLeaderScheduleFailed = errors.New("leader schedule fetch failed from all RPC endpoints")

// prepareLeaderSchedule fetches the leader schedule for the given epoch.
// It tries the primary RPC client first, then falls back to backup endpoints.
// Returns an error instead of panicking to allow graceful shutdown.
func prepareLeaderSchedule(epoch uint64, epochSchedule *sealevel.SysvarEpochSchedule, rpcClient *rpcclient.RpcClient) error {
	return prepareLeaderScheduleWithBackups(epoch, epochSchedule, rpcClient, nil)
}

// prepareLeaderScheduleWithBackups tries the primary client, then backup endpoints in order.
func prepareLeaderScheduleWithBackups(epoch uint64, epochSchedule *sealevel.SysvarEpochSchedule, rpcClient *rpcclient.RpcClient, backupEndpoints []string) error {
	firstSlotInEpoch := epochSchedule.FirstSlotInEpoch(epoch)

	// Try primary endpoint first
	leaderMap, err := fetchLeaderScheduleWithRetry(rpcClient, 10)
	if err == nil {
		leaderSchedule := leaderschedule.NewLeaderScheduleFromKeyedSlots(leaderMap, firstSlotInEpoch)
		global.SetLeaderSchedule(leaderSchedule)
		return nil
	}

	lastErr := err
	mlog.Log.Errorf("leader schedule fetch failed on primary %s: %v", rpcClient.Endpoint(), err)

	// Try backup endpoints
	for i, endpoint := range backupEndpoints {
		mlog.Log.Infof("trying backup RPC endpoint #%d for leader schedule: %s", i+1, endpoint)
		backupClient := rpcclient.NewRpcClient(endpoint)
		leaderMap, err := fetchLeaderScheduleWithRetry(backupClient, 5) // Fewer retries on backups
		if err == nil {
			mlog.Log.Infof("leader schedule fetched from backup endpoint %s", endpoint)
			leaderSchedule := leaderschedule.NewLeaderScheduleFromKeyedSlots(leaderMap, firstSlotInEpoch)
			global.SetLeaderSchedule(leaderSchedule)
			return nil
		}
		lastErr = err
		mlog.Log.Errorf("leader schedule fetch failed on backup %s: %v", endpoint, err)
	}

	// All endpoints failed
	return fmt.Errorf("%w: last error: %v", ErrLeaderScheduleFailed, lastErr)
}

// fetchLeaderScheduleWithRetry attempts to fetch leader schedule with exponential backoff
func fetchLeaderScheduleWithRetry(rpcClient *rpcclient.RpcClient, maxAttempts int) (map[solana.PublicKey][]uint64, error) {
	var leaderMap map[solana.PublicKey][]uint64
	var err error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		leaderMap, err = rpcClient.GetLeaderSchedule()
		if err == nil {
			return leaderMap, nil
		}
		// Retry with exponential backoff
		if attempt < maxAttempts-1 {
			waitTime := time.Duration(1<<attempt) * time.Second // 1s, 2s, 4s, 8s...
			if waitTime > 30*time.Second {
				waitTime = 30 * time.Second
			}
			mlog.Log.Infof("leader schedule fetch from %s failed, retrying in %v (attempt %d/%d): %v",
				rpcClient.Endpoint(), waitTime, attempt+1, maxAttempts, err)
			time.Sleep(waitTime)
		}
	}

	return nil, fmt.Errorf("failed after %d attempts: %w", maxAttempts, err)
}

// fetchLeaderScheduleForSlotWithRetry fetches leader schedule for the epoch containing the given slot.
// Pass firstSlotInEpoch to get the schedule for a specific epoch.
// RPC method: getLeaderSchedule with slot parameter
func fetchLeaderScheduleForSlotWithRetry(rpcClient *rpcclient.RpcClient, slot uint64, maxAttempts int) (map[solana.PublicKey][]uint64, error) {
	var leaderMap map[solana.PublicKey][]uint64
	var err error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		leaderMap, err = rpcClient.GetLeaderScheduleForSlot(slot)
		if err == nil {
			return leaderMap, nil
		}
		// Retry with exponential backoff
		if attempt < maxAttempts-1 {
			waitTime := time.Duration(1<<attempt) * time.Second // 1s, 2s, 4s, 8s...
			if waitTime > 30*time.Second {
				waitTime = 30 * time.Second
			}
			mlog.Log.Debugf("leader schedule fetch for slot %d from %s failed, retrying in %v (attempt %d/%d): %v",
				slot, rpcClient.Endpoint(), waitTime, attempt+1, maxAttempts, err)
			time.Sleep(waitTime)
		}
	}

	return nil, fmt.Errorf("failed after %d attempts: %w", maxAttempts, err)
}
