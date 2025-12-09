package blockstream

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/block"
	b "github.com/Overclock-Validator/mithril/pkg/block"

	//"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/panjf2000/ants/v2"
)

type BlockSourceType int

const (
	BlockSourceRpc = iota
	BlockSourceFile
	BlockSourceOvercast
)

type BlockSourceOpts struct {
	RpcClient  *rpcclient.RpcClient
	SourceType BlockSourceType
	StartSlot  uint64
	EndSlot    uint64
	BlockDir   string
}

type BlockSource struct {
	rpcClient         *rpcclient.RpcClient
	streamChan        chan *b.Block
	startSlot         uint64
	endSlot           uint64
	currentSlot       uint64
	totalSlotsToFetch uint64
	numInitialSlots   uint64
	blockDir          string
	sourceType        BlockSourceType
}

type blockSourceTaskInfo struct {
	slot uint64
	idx  uint64
}

var blockBufferSize = 35

func NewBlockSource(opts *BlockSourceOpts) *BlockSource {
	streamChan := make(chan *b.Block, blockBufferSize)
	return &BlockSource{
		rpcClient:         opts.RpcClient,
		streamChan:        streamChan,
		totalSlotsToFetch: opts.EndSlot - opts.StartSlot,
		currentSlot:       opts.StartSlot,
		startSlot:         opts.StartSlot,
		endSlot:           opts.EndSlot,
		numInitialSlots:   min(uint64(blockBufferSize), opts.EndSlot-opts.StartSlot),
		blockDir:          opts.BlockDir,
		sourceType:        opts.SourceType,
	}
}

func (blockSource *BlockSource) tryGetBlockFromFile(slot uint64) (*block.Block, error) {
	if blockSource.blockDir == "" {
		return nil, fmt.Errorf("no block directory specified")
	}
	blockFilename := filepath.Join(filepath.Clean(blockSource.blockDir), fmt.Sprintf("%d.json", slot))
	file, err := os.Open(blockFilename)
	if err != nil {
		return nil, fmt.Errorf("error opening blockFilename=%s: %w", blockFilename, err)
	}

	// Create a decoder
	decoder := json.NewDecoder(file)

	out := &block.Block{}

	// Decode JSON into target
	err = decoder.Decode(out)
	if err != nil {
		return nil, fmt.Errorf("block decode error: %w", err)
	}
	out.FixupTxVersions()

	// we no longer need the file anymore after use, so close and delete from file system
	file.Close()
	os.Remove(blockFilename)

	return out, nil
}

func (blockSource *BlockSource) fetchAndParseBlock(slot uint64) (*b.Block, error) {
	var err error
	var blockResult *rpc.GetBlockResult
	var b *b.Block

	if blockSource.sourceType == BlockSourceRpc {
		b, err = blockSource.tryGetBlockFromFile(slot)
		if err != nil {
			for {
				blockResult, err = blockSource.rpcClient.GetBlockConfirmed(uint64(slot))
				if err == nil {
					break
				} else if err == rpcclient.SlotSkipped {
					return nil, err
				} else if isSlotNotAvailableErr(err) { // we're too early. wait for a bit.
					time.Sleep(500 * time.Millisecond)
				} else if isRateLimitedErr(err) {
					time.Sleep(2 * time.Second)
				} else {
					panic(fmt.Sprintf("error fetching block: %s\n", err))
				}
			}
			b = block.FromBlockResult(blockResult, slot, blockSource.rpcClient)
		}
	} else {
		if blockSource.sourceType == BlockSourceFile {
			b, err = blockSource.tryGetBlockFromFile(slot)
			if err != nil {
				for {
					blockResult, err = blockSource.rpcClient.GetBlockConfirmed(uint64(slot))
					if err == nil {
						break
					} else if err == rpcclient.SlotSkipped {
						return nil, err
					} else if isSlotNotAvailableErr(err) { // we're too early. wait for a bit.
						time.Sleep(500 * time.Millisecond)
					} else if isRateLimitedErr(err) {
						time.Sleep(2 * time.Second)
					} else {
						panic(fmt.Sprintf("error fetching block: %s\n", err))
					}
				}
				b = block.FromBlockResult(blockResult, slot, blockSource.rpcClient)
			}
		} else if blockSource.sourceType == BlockSourceOvercast {
			b, err = blockSource.tryGetBlockFromFile(slot)
			if err != nil {
				return nil, rpcclient.SlotSkipped
			}
		} else {
			panic("invalid source type - programming error")
		}
	}

	return b, nil
}

func (blockSource *BlockSource) DownloadInitialBlocks() {
	blocks := make([]*b.Block, blockSource.numInitialSlots)
	var wg sync.WaitGroup

	workerPool, _ := ants.NewPoolWithFunc(10, func(i interface{}) {
		defer wg.Done()
		taskInfo := i.(blockSourceTaskInfo)
		slot := taskInfo.slot
		idx := taskInfo.idx

		block, _ := blockSource.fetchAndParseBlock(slot)
		if block != nil {
			//global.SubmitBlockToForkChoiceService(block.Slot, block.Transactions)
		}
		blocks[idx] = block
	})

	var idx uint64
	for ; blockSource.currentSlot < blockSource.startSlot+blockSource.numInitialSlots; blockSource.currentSlot++ {
		ti := blockSourceTaskInfo{slot: blockSource.currentSlot, idx: idx}
		wg.Add(1)
		workerPool.Invoke(ti)
		idx++
	}
	wg.Wait()

	for i := uint64(0); i < blockSource.numInitialSlots; i++ {
		if blocks[i] != nil {
			blockSource.streamChan <- blocks[i]
		}
	}
}

func (blockSource *BlockSource) Start() {
	for ; blockSource.currentSlot < blockSource.endSlot; blockSource.currentSlot++ {
		newBlock, _ := blockSource.fetchAndParseBlock(blockSource.currentSlot)
		if newBlock != nil {
			//global.SubmitBlockToForkChoiceService(newBlock.Slot, newBlock.Transactions)
			blockSource.streamChan <- newBlock
		}
	}
	close(blockSource.streamChan)
}

func (blockSource *BlockSource) NextBlock() *b.Block {
	block := <-blockSource.streamChan
	return block
}
