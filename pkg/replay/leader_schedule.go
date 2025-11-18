package replay

import (
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/leaderschedule"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
)

func prepareLeaderSchedule(epoch uint64, epochSchedule *sealevel.SysvarEpochSchedule, rpcClient *rpcclient.RpcClient) {
	firstSlotInEpoch := epochSchedule.FirstSlotInEpoch(epoch)
	leaderMap, err := rpcClient.GetLeaderSchedule()
	if err != nil {
		panic(fmt.Sprintf("unable to fetch leader schedule: %s", err))
	}

	leaderSchedule := leaderschedule.NewLeaderScheduleFromKeyedSlots(leaderMap, firstSlotInEpoch)
	global.SetLeaderSchedule(leaderSchedule)
}
