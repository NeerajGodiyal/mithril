package replay

import (
	"bytes"
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/rewards"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/Overclock-Validator/mithril/pkg/safemath"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

func beginPartitionedEpochRewardsDistribution(acctsDb *accountsdb.AccountsDb, slotCtx *sealevel.SlotCtx, stakeHistory *sealevel.SysvarStakeHistory, epochSchedule *sealevel.SysvarEpochSchedule, rpcc *rpcclient.RpcClient, blockResult *rpc.GetBlockResult, epoch uint64, slot uint64) (*rewards.PartitionedRewardDistributionInfo, []solana.PublicKey) {
	rewardPks, voteRewardsDistributed := rewards.DistributeVotingRewards(acctsDb, blockResult.Rewards, slot)
	partitionedRewardsInfo := rewards.RetrievePartitionedStakingRewardsInfo(rpcc, epochSchedule, epoch, slot)

	totalRewards, err := safemath.CheckedAddU64(voteRewardsDistributed, partitionedRewardsInfo.TotalStakingRewards)
	if err != nil {
		panic("overflow calculating total rewards")
	}

	newWarmupCooldownRateEpoch := sealevel.NewWarmupCooldownRateEpochWithSlotCtx(slotCtx, epochSchedule)
	points := rewards.CalculateRewardPointsPartitioned(acctsDb, slotCtx, slot, stakeHistory, newWarmupCooldownRateEpoch)

	newEpochRewards := sealevel.SysvarEpochRewards{DistributionStartingBlockHeight: slot + 1,
		NumPartitions: *blockResult.NumRewardPartitions, ParentBlockhash: blockResult.PreviousBlockhash,
		TotalRewards: totalRewards, DistributedRewards: voteRewardsDistributed, TotalPoints: points, Active: true}

	epochRewardsAcct, err := acctsDb.GetAccount(slot, sealevel.SysvarEpochRewardsAddr)
	if err != nil {
		panic(fmt.Sprintf("unable to get EpochRewards from acctsdb: %s", err))
	}

	writer := new(bytes.Buffer)
	encoder := bin.NewBinEncoder(writer)
	newEpochRewards.MustMarshalWithEncoder(encoder)
	copy(epochRewardsAcct.Data, writer.Bytes())

	err = acctsDb.StoreAccounts([]*accounts.Account{epochRewardsAcct}, slot)
	if err != nil {
		panic(fmt.Sprintf("unable to update EpochRewards sysvar to acctsdb: %s", err))
	}

	rewardPks = append(rewardPks, sealevel.SysvarEpochRewardsAddr)

	return partitionedRewardsInfo, rewardPks
}

func distributePartitionedEpochRewardsForSlot(acctsDb *accountsdb.AccountsDb, rewardInfo []rpc.BlockReward, currentSlot uint64, lastRewardsDistributionSlot uint64) []solana.PublicKey {
	rewardPks, distributedLamports := rewards.DistributeStakingRewards(acctsDb, rewardInfo, currentSlot)

	if distributedLamports == 0 {
		return nil
	}

	epochRewardsAcct, err := acctsDb.GetAccount(currentSlot, sealevel.SysvarEpochRewardsAddr)
	if err != nil {
		panic(fmt.Sprintf("unable to get EpochRewards from acctsdb: %s", err))
	}

	var epochRewards sealevel.SysvarEpochRewards

	decoder := bin.NewBinDecoder(epochRewardsAcct.Data)
	epochRewards.MustUnmarshalWithDecoder(decoder)

	epochRewards.Distribute(distributedLamports)

	if currentSlot == lastRewardsDistributionSlot {
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

	rewardPks = append(rewardPks, sealevel.SysvarEpochRewardsAddr)
	return rewardPks
}
