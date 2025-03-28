package replay

import (
	"fmt"
	"sync"

	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
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
}

type blockStreamTaskInfo struct {
	slot uint64
	idx  uint64
}

func NewBlockStream(rpcClient *rpcclient.RpcClient, streamChan chan *Block, startSlot, endSlot uint64, numInitialSlots uint64) *blockStream {
	return &blockStream{rpcClient: rpcClient, streamChan: streamChan, totalSlotsToFetch: endSlot - startSlot,
		currentSlot: startSlot, startSlot: startSlot, endSlot: endSlot, numInitialSlots: min(numInitialSlots, endSlot-startSlot)}
}

func (blockStream *blockStream) fetchAndParseBlock(slot uint64) *Block {
	blockResult, err := blockStream.rpcClient.GetBlockFinalized(uint64(slot))
	if err == rpcclient.SlotSkipped {
		return nil
	} else if err != nil {
		panic(fmt.Sprintf("error fetching block: %s\n", err))
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
}
