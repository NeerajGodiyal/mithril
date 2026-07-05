package block

import (
	"fmt"
	"math"

	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

func FromBlockResult(blockResult *rpc.GetBlockResult, slot uint64, rpcc *rpcclient.RpcClient) *Block {
	return fromBlockResult(blockResult, slot, rpcc, nil)
}

func FromBlockAGResult(blockResult *rpcclient.GetBlockAGResult, slot uint64, rpcc *rpcclient.RpcClient) *Block {
	return fromBlockResult(&blockResult.GetBlockResult, slot, rpcc, blockResult.AlpenglowFooter)
}

func fromBlockResult(blockResult *rpc.GetBlockResult, slot uint64, rpcc *rpcclient.RpcClient, alpenglowFooter *rpcclient.AlpenglowFooterRPC) *Block {
	block := new(Block)
	block.Slot = slot

	for _, tx := range blockResult.Transactions {
		txParsed, err := tx.GetTransaction()
		if err != nil {
			panic(fmt.Sprintf("parsing tx from rpc returned err: %s", err))
		}
		block.Transactions = append(block.Transactions, txParsed)
		block.TxMetas = append(block.TxMetas, tx.Meta)
		block.Versions = append(block.Versions, uint8(txParsed.Message.GetVersion()))
	}

	block.Blockhash = blockResult.Blockhash
	block.LastBlockhash = blockResult.PreviousBlockhash

	if blockResult.BlockTime == nil {
		mlog.Log.Infof("slot %d had nil BlockTime field", slot)
	} else {
		block.UnixTimestamp = int64(*blockResult.BlockTime)
	}

	block.Rewards = blockResult.Rewards
	if blockResult.NumRewardPartitions != nil {
		block.NumRewardPartitions = *blockResult.NumRewardPartitions
	} else {
		block.NumRewardPartitions = math.MaxUint64
	}
	blockReward := blockRewardRewards(blockResult.Rewards)

	if !global.ManageLeaderSchedule() {
		if blockReward != nil {
			block.BlockReward = &BlockRewardsInfo{Leader: blockReward.Pubkey, Lamports: uint64(blockReward.Lamports), PostBalance: blockReward.PostBalance}
			block.Leader = blockReward.Pubkey
		} else {
			if rpcc != nil {
				leaderForSlot, err := rpcc.GetLeaderForSlot(slot)
				if err == nil {
					block.BlockReward = &BlockRewardsInfo{Leader: leaderForSlot}
					block.Leader = leaderForSlot
				}
			}
		}
	}

	for _, tx := range block.Transactions {
		block.NumSignatures += uint64(tx.Message.Header.NumRequiredSignatures)
	}

	applyAlpenglowFooterFromRPC(block, alpenglowFooter)

	return block
}

func applyAlpenglowFooterFromRPC(block *Block, footer *rpcclient.AlpenglowFooterRPC) {
	if footer == nil {
		return
	}
	block.HasAlpenglowFooter = true
	block.FooterProducerTimeNanos = footer.BlockProducerTimeNanos
	skipCert, notarCert, finalCert, err := rpcclient.DecodeAlpenglowFooterCerts(footer)
	if err != nil {
		panic(fmt.Sprintf("slot %d alpenglow footer decode: %s", block.Slot, err))
	}
	block.SkipRewardCert = skipCert
	block.NotarRewardCert = notarCert
	block.BlockFinalCert = finalCert
	if footer.BankHash != "" {
		bankHash, err := solana.HashFromBase58(footer.BankHash)
		if err != nil {
			panic(fmt.Sprintf("slot %d alpenglow footer bank hash: %s", block.Slot, err))
		}
		block.ExpectedBankhash = bankHash
		block.HasExpectedBankhash = true
	}
}

func blockRewardRewards(rewards []rpc.BlockReward) *rpc.BlockReward {
	for _, reward := range rewards {
		if string(reward.RewardType) == "Fee" {
			return &reward
		}
	}

	return nil
}
