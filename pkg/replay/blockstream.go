package replay

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/panjf2000/ants/v2"
)

type blockStream struct {
	rpcClient         *rpcclient.RpcClient
	streamChan        chan *Block
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

type blockStreamTaskInfo struct {
	slot uint64
	idx  uint64
}

func NewBlockStream(rpcClient *rpcclient.RpcClient, streamChan chan *Block, startSlot, endSlot uint64, numInitialSlots uint64, blockDir string) *blockStream {
	return &blockStream{
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

func (blockStream *blockStream) tryGetBlockResultFromFile(slot uint64) (*rpc.GetBlockResult, error) {
	if blockStream.blockDir == "" {
		return nil, fmt.Errorf("no block directory specified")
	}
	blockFilename := filepath.Join(filepath.Clean(blockStream.blockDir), fmt.Sprintf("%d.json", slot))
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

func (blockStream *blockStream) fetchAndParseBlock(slot uint64) *Block {
	var blockResult *rpc.GetBlockResult
	var err error

	if blockStream.useRpc {
		blockResult, err = blockStream.rpcClient.GetBlockFinalized(uint64(slot))
		if err == rpcclient.SlotSkipped {
			return nil
		} else if err != nil {
			panic(fmt.Sprintf("error fetching block: %s\n", err))
		}
	} else {
		blockResult, err = blockStream.tryGetBlockResultFromFile(slot)
		if err != nil {
			mlog.Log.Errorf("block cache miss for slot=%d: %v", slot, err)
			blockResult, err = blockStream.rpcClient.GetBlockFinalized(uint64(slot))
			if err == rpcclient.SlotSkipped {
				return nil
			} else if err != nil {
				panic(fmt.Sprintf("error fetching block: %s\n", err))
			}
		}
	}

	block, err := newBlockFromBlockResult(blockResult)
	if err != nil {
		panic(fmt.Sprintf("error creating block from BlockResult: %s\n", err))
	}

	block.Slot = slot
	return block
}

func (blockStream *blockStream) downloadInitialBlocks() {
	blocks := make([]*Block, blockStream.numInitialSlots)
	var wg sync.WaitGroup

	workerPool, _ := ants.NewPoolWithFunc(10, func(i interface{}) {
		defer wg.Done()
		taskInfo := i.(blockStreamTaskInfo)
		slot := taskInfo.slot
		idx := taskInfo.idx

		block := blockStream.fetchAndParseBlock(slot)
		blocks[idx] = block
	})

	var idx uint64
	for ; blockStream.currentSlot < blockStream.startSlot+blockStream.numInitialSlots; blockStream.currentSlot++ {
		ti := blockStreamTaskInfo{slot: blockStream.currentSlot, idx: idx}
		wg.Add(1)
		workerPool.Invoke(ti)
		idx++
	}
	wg.Wait()

	for i := uint64(0); i < blockStream.numInitialSlots; i++ {
		if blocks[i] != nil {
			blockStream.streamChan <- blocks[i]
		}
	}
}

func (blockStream *blockStream) startAsyncBlockStream() {
	for ; blockStream.currentSlot < blockStream.endSlot; blockStream.currentSlot++ {
		newBlock := blockStream.fetchAndParseBlock(blockStream.currentSlot)
		if newBlock != nil {
			blockStream.streamChan <- newBlock
		}
	}
	close(blockStream.streamChan)
}
