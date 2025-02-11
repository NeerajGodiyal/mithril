package replay

import (
	"bytes"
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/rewards"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go/rpc"
	"k8s.io/klog/v2"
)

func handlePartitionedEpochRewardsForSlot(acctsDb *accountsdb.AccountsDb, rpcc *rpcclient.RpcClient, rewardInfo []rpc.BlockReward, currentSlot uint64) {
	distributedLamports := rewards.DistributePartitionedEpochRewards(acctsDb, rewardInfo, currentSlot)

	epochRewardsAcct, err := acctsDb.GetAccount(currentSlot, sealevel.SysvarEpochRewardsAddr)
	if err != nil {
		panic(fmt.Sprintf("unable to get EpochRewards from acctsdb: %s", err))
	}

	var epochRewards sealevel.SysvarEpochRewards

	decoder := bin.NewBinDecoder(epochRewardsAcct.Data)
	epochRewards.MustUnmarshalWithDecoder(decoder)

	epochRewards.Distribute(distributedLamports)

	// set epoch rewards to inactive after all partitioned rewards are distributed for this epoch
	nextBlock, err := rpcc.GetBlockFinalized(uint64(currentSlot + 1))
	if err != nil {
		klog.Fatalf("error fetching block: %s\n", err)
	}

	if len(nextBlock.Rewards) == 0 {
		epochRewards.Active = false
	}

	writer := new(bytes.Buffer)
	encoder := bin.NewBinEncoder(writer)
	epochRewards.MustMarshalWithEncoder(encoder)
	copy(epochRewardsAcct.Data, writer.Bytes())

	err = acctsDb.StoreAccounts([]*accounts.Account{epochRewardsAcct}, currentSlot)
	if err != nil {
		panic(fmt.Sprintf("unable to update EpochRewards sysvar to acctsdb: %s", err))
	}
}
