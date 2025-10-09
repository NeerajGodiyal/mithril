package block

import (
	"math"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/lthash"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/rewards"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

type BlockRewardsInfo struct {
	Leader      solana.PublicKey
	Lamports    uint64
	PostBalance uint64
}

type Block struct {
	Slot                                uint64
	ParentSlot                          uint64
	BlockHeight                         uint64
	Epoch                               uint64
	Transactions                        []*solana.Transaction
	BankHash                            [32]byte
	EpochAcctsHash                      []byte
	EahWorkaroundBankhash               []byte
	HasEahWorkaround                    bool
	ParentBankhash                      [32]byte
	AcctsLtHash                         *lthash.LtHash
	NumSignatures                       uint64
	PrevNumSignatures                   uint64
	InitialPreviousLamportsPerSignature uint64
	Blockhash                           [32]byte
	ExpectedBankhash                    [32]byte
	TxMetas                             []*rpc.TransactionMeta
	Leader                              solana.PublicKey
	BlockReward                         *BlockRewardsInfo
	LastBlockhash                       [32]byte
	UnixTimestamp                       int64
	VoteAccts                           map[solana.PublicKey]uint64
	VoteTimestamps                      map[solana.PublicKey]sealevel.BlockTimestamp
	TotalEpochStake                     uint64
	Features                            *features.Features
	UpdatedAccts                        []solana.PublicKey
	ParentEpochUpdatedAccts             []*accounts.Account
	EpochUpdatedAccts                   []*accounts.Account
	PartitionedRewardsInfo              *rewards.PartitionedRewardDistributionInfo
	Rewards                             []rpc.BlockReward
	NumRewardPartitions                 uint64
	LatestEvictedBlockhash              [32]byte
	PrevFeeRateGovernor                 *sealevel.FeeRateGovernor
	FeeRateGovernor                     *sealevel.FeeRateGovernor
}

func FromBlockResult(blockResult *rpc.GetBlockResult, slot uint64, rpcc *rpcclient.RpcClient) (*Block, error) {
	block := new(Block)

	for _, tx := range blockResult.Transactions {
		txParsed, err := tx.GetTransaction()
		if err != nil {
			return nil, err
		}
		block.Transactions = append(block.Transactions, txParsed)
		block.TxMetas = append(block.TxMetas, tx.Meta)
	}

	block.Blockhash = blockResult.Blockhash
	block.LastBlockhash = blockResult.PreviousBlockhash

	if blockResult.BlockTime == nil {
		mlog.Log.Infof("slot %d had nil BlockTime field", slot)
	} else {
		block.UnixTimestamp = int64(*blockResult.BlockTime)
	}
	block.BlockHeight = *blockResult.BlockHeight

	block.Rewards = blockResult.Rewards
	if blockResult.NumRewardPartitions != nil {
		block.NumRewardPartitions = *blockResult.NumRewardPartitions
	} else {
		block.NumRewardPartitions = math.MaxUint64
	}
	blockReward := blockRewardRewards(blockResult.Rewards)
	if blockReward != nil {
		block.BlockReward = &BlockRewardsInfo{Leader: blockReward.Pubkey, Lamports: uint64(blockReward.Lamports), PostBalance: blockReward.PostBalance}
	} else {
		if rpcc != nil {
			leaderForSlot, err := rpcc.GetLeaderForSlot(slot)
			if err == nil {
				block.BlockReward = &BlockRewardsInfo{Leader: leaderForSlot}
			}
		}
	}

	for _, tx := range block.Transactions {
		block.NumSignatures += uint64(tx.Message.Header.NumRequiredSignatures)
	}

	return block, nil
}

func blockRewardRewards(rewards []rpc.BlockReward) *rpc.BlockReward {
	for _, reward := range rewards {
		if string(reward.RewardType) == "Fee" {
			return &reward
		}
	}

	return nil
}
