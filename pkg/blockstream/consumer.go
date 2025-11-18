package blockstream

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/panjf2000/ants/v2"
)

type BlockConsumer struct {
	rpcClient         *rpcclient.RpcClient
	streamChan        chan *b.Block
	startSlot         uint64
	endSlot           uint64
	currentSlot       uint64
	totalSlotsToFetch uint64
	numInitialSlots   uint64
	// A directory containing files named SLOT.json that have
	// JSON marshaled *rpc.BlockResult in them. If nonempty,
	// will check for blocks here before using RPC.
	blockDir string
	useRpc   bool
}

type blockConsumerTaskInfo struct {
	slot uint64
	idx  uint64
}

func NewBlockConsumer(rpcClient *rpcclient.RpcClient, streamChan chan *b.Block, startSlot, endSlot uint64, numInitialSlots uint64, blockDir string) *BlockConsumer {
	return &BlockConsumer{
		rpcClient:         rpcClient,
		streamChan:        streamChan,
		totalSlotsToFetch: endSlot - startSlot,
		currentSlot:       startSlot,
		startSlot:         startSlot,
		endSlot:           endSlot,
		numInitialSlots:   min(numInitialSlots, endSlot-startSlot),
		blockDir:          blockDir,
		useRpc:            blockDir == "",
	}
}

func (blockConsumer *BlockConsumer) tryGetBlockResultFromFile(slot uint64) (*rpc.GetBlockResult, error) {
	if blockConsumer.blockDir == "" {
		return nil, fmt.Errorf("no block directory specified")
	}
	blockFilename := filepath.Join(filepath.Clean(blockConsumer.blockDir), fmt.Sprintf("%d.json", slot))
	file, err := os.Open(blockFilename)
	if err != nil {
		return nil, fmt.Errorf("error opening blockFilename=%s: %w", blockFilename, err)
	}
	defer file.Close()

	// Create a decoder
	decoder := json.NewDecoder(file)

	out := &rpc.GetBlockResult{}
	// Decode JSON into target
	err = decoder.Decode(out)
	if err != nil {
		return nil, fmt.Errorf("decode error: %w", err)
	}

	return out, nil
}

func (blockConsumer *BlockConsumer) fetchAndParseBlock(slot uint64) *b.Block {
	var blockResult *rpc.GetBlockResult
	var err error

	if blockConsumer.useRpc {
		blockResult, err = blockConsumer.rpcClient.GetBlockConfirmed(uint64(slot))
		if err == rpcclient.SlotSkipped {
			return nil
		} else if err != nil {
			panic(fmt.Sprintf("error fetching block: %s\n", err))
		}
	} else {
		blockResult, err = blockConsumer.tryGetBlockResultFromFile(slot)
		if err != nil {
			for {
				blockResult, err = blockConsumer.rpcClient.GetBlockConfirmed(uint64(slot))
				if err == nil {
					break
				} else if err == rpcclient.SlotSkipped {
					return nil
				} else if strings.Contains(err.Error(), "Block not available for slot") { // we're too early. wait for a bit.
					time.Sleep(500 * time.Millisecond)
				} else {
					panic(fmt.Sprintf("error fetching block: %s\n", err))
				}
			}
		}
	}

	// skipped slot
	if blockResult.BlockTime == nil {
		return nil
	}

	block, err := b.FromBlockResult(blockResult, slot, blockConsumer.rpcClient)
	if err != nil {
		panic(fmt.Sprintf("error creating block from BlockResult: %s\n", err))
	}

	block.Slot = slot
	return block
}

func (blockConsumer *BlockConsumer) DownloadInitialBlocks() {
	blocks := make([]*b.Block, blockConsumer.numInitialSlots)
	var wg sync.WaitGroup

	workerPool, _ := ants.NewPoolWithFunc(10, func(i interface{}) {
		defer wg.Done()
		taskInfo := i.(blockConsumerTaskInfo)
		slot := taskInfo.slot
		idx := taskInfo.idx

		block := blockConsumer.fetchAndParseBlock(slot)
		global.SubmitBlockToForkChoiceService(block.Slot, block.Transactions)
		blocks[idx] = block
	})

	var idx uint64
	for ; blockConsumer.currentSlot < blockConsumer.startSlot+blockConsumer.numInitialSlots; blockConsumer.currentSlot++ {
		ti := blockConsumerTaskInfo{slot: blockConsumer.currentSlot, idx: idx}
		wg.Add(1)
		workerPool.Invoke(ti)
		idx++
	}
	wg.Wait()

	for i := uint64(0); i < blockConsumer.numInitialSlots; i++ {
		if blocks[i] != nil {
			blockConsumer.streamChan <- blocks[i]
		}
	}
}

func (blockConsumer *BlockConsumer) StartAsyncBlockStream() {
	for ; blockConsumer.currentSlot < blockConsumer.endSlot; blockConsumer.currentSlot++ {
		newBlock := blockConsumer.fetchAndParseBlock(blockConsumer.currentSlot)
		if newBlock != nil {
			global.SubmitBlockToForkChoiceService(newBlock.Slot, newBlock.Transactions)
			blockConsumer.streamChan <- newBlock
		}
	}
	close(blockConsumer.streamChan)
}
