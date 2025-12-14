package replay

import (
	"fmt"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/leaderschedule"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
)

func prepareLeaderSchedule(epoch uint64, epochSchedule *sealevel.SysvarEpochSchedule, rpcClient *rpcclient.RpcClient) {
	firstSlotInEpoch := epochSchedule.FirstSlotInEpoch(epoch)

	var leaderMap map[solana.PublicKey][]uint64
	var err error

	// Retry with exponential backoff on rate limit
	for attempt := 0; attempt < 10; attempt++ {
		leaderMap, err = rpcClient.GetLeaderSchedule()
		if err == nil {
			break
		}
		// Check if rate limited - retry
		if attempt < 9 {
			waitTime := time.Duration(1<<attempt) * time.Second // 1s, 2s, 4s, 8s...
			if waitTime > 30*time.Second {
				waitTime = 30 * time.Second
			}
			mlog.Log.Infof("rate limited fetching leader schedule, retrying in %v (attempt %d/10)", waitTime, attempt+1)
			time.Sleep(waitTime)
		}
	}

	if err != nil {
		panic(fmt.Sprintf("unable to fetch leader schedule after 10 attempts: %s", err))
	}

	leaderSchedule := leaderschedule.NewLeaderScheduleFromKeyedSlots(leaderMap, firstSlotInEpoch)
	global.SetLeaderSchedule(leaderSchedule)
}
