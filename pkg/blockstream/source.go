package blockstream

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	//"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/ws"
	laserstream "github.com/helius-labs/laserstream-sdk/go"
	"github.com/panjf2000/ants/v2"
)

type BlockEndpointType uint64

const (
	BlockEndpointTypeWebSocket = iota
	BlockEndpointTypeLaserStream
	BlockEndpointTypeRpc
)

type BlockSourceOpts struct {
	WsEndpoint   string
	LsEndpoint   string
	RpcEndpoint  string
	OutDir       string
	EndpointType BlockEndpointType
	LsApiKey     string
	RpcPoolFile  string
	Channel      chan *block.Block
	StartSlot    uint64
}

type BlockSource struct {
	wsClient          *ws.Client
	laserStreamClient *laserstream.Client
	rpcClient         *rpcclient.RpcClient
	rpcPool           *rpcConnPool
	outDir            string
	endpointType      BlockEndpointType
	output            chan *block.Block
	startSlot         uint64
}

func NewBlockSource(opts BlockSourceOpts) *BlockSource {
	var blockSrc *BlockSource
	var endpoint string

	switch opts.EndpointType {
	case BlockEndpointTypeWebSocket:
		{
			endpoint = strings.ReplaceAll(opts.WsEndpoint, "https://", "wss://")
			endpoint = strings.ReplaceAll(opts.WsEndpoint, "http://", "wss://")

			client, err := ws.Connect(context.Background(), endpoint)
			if err != nil {
				panic(err)
			}

			blockSrc = &BlockSource{wsClient: client, outDir: opts.OutDir, endpointType: opts.EndpointType}
		}

	case BlockEndpointTypeLaserStream:
		{
			lsClient := laserstream.NewClient(laserstream.LaserstreamConfig{
				Endpoint: opts.LsEndpoint,
				APIKey:   opts.LsApiKey,
			})
			rpcClient := rpcclient.NewRpcClient(opts.RpcEndpoint)
			blockSrc = &BlockSource{laserStreamClient: lsClient, rpcClient: rpcClient, outDir: opts.OutDir, endpointType: opts.EndpointType, output: opts.Channel}
		}

	case BlockEndpointTypeRpc:
		{
			addrs := parseRpcPoolFile(opts.RpcPoolFile)
			pool := newRpcConnPool(addrs)
			blockSrc = &BlockSource{rpcPool: pool, outDir: opts.OutDir, endpointType: opts.EndpointType, startSlot: opts.StartSlot}
		}

	default:
		panic(fmt.Sprintf("invalid endpoint type: %d", opts.EndpointType))
	}

	return blockSrc
}

func parseRpcPoolFile(path string) []string {
	file, err := os.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()

	var addrs []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		addrs = append(addrs, scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		log.Fatal(err)
	}

	return addrs
}

func (blockSrc *BlockSource) Start() {

	switch blockSrc.endpointType {
	case BlockEndpointTypeWebSocket:
		blockSrc.startStandardWebSocketStream()

	case BlockEndpointTypeLaserStream:
		blockSrc.startLaserStreamStream()

	case BlockEndpointTypeRpc:
		blockSrc.startRpcStream()

	default:
		panic("invalid subscription channel type")
	}
}

func (blockSrc *BlockSource) startStandardWebSocketStream() {
	ctx := context.Background()
	filter := ws.NewBlockSubscribeFilterAll()
	s, err := blockSrc.wsClient.BlockSubscribe(filter, &ws.BlockSubscribeOpts{Commitment: rpc.CommitmentFinalized})
	if err != nil {
		panic(err)
	}

	for {
		mlog.Log.Infof("receiving on websocket")
		recvd, err := s.Recv(ctx)
		if err != nil {
			mlog.Log.Errorf("error in block src: %s", err)
		}
		mlog.Log.Infof("got message on websocket")
		blockFilename := filepath.Join(filepath.Clean(blockSrc.outDir), fmt.Sprintf("%d.json", recvd.Value.Slot))
		err = blockSrc.saveBlockToFile(blockFilename, recvd.Value.Block)
		if err != nil {
			mlog.Log.Errorf("error saving block %d in block src: %s", recvd.Value.Slot, err)
		} else {
			mlog.Log.Infof("received block %d and saved to file %s", recvd.Value.Slot, blockFilename)
		}
	}
}

func (blockSrc *BlockSource) startLaserStreamStream() {
	includeTransactions := true
	includeAccounts := true
	commitmentLevel := laserstream.CommitmentLevel_FINALIZED

	req := &laserstream.SubscribeRequest{
		Blocks: map[string]*laserstream.SubscribeRequestFilterBlocks{
			"all-blocks": {
				IncludeTransactions: &includeTransactions,
				IncludeAccounts:     &includeAccounts,
			},
		},
		Commitment: &commitmentLevel,
	}

	err := blockSrc.laserStreamClient.Subscribe(req,
		func(update *laserstream.SubscribeUpdate) {
			block := block.FromLaserStream(update.GetBlock(), blockSrc.rpcClient)
			mlog.Log.Infof("got slot %d", block.Slot)
			blockSrc.output <- block
		},
		func(err error) {
			panic(err)
		},
	)
	if err != nil {
		panic(err)
	}
}

func (blockSrc *BlockSource) startRpcStream() {
	workerPool, _ := ants.NewPoolWithFunc(blockSrc.rpcPool.NumClients(), func(i interface{}) {
		slot := i.(uint64)
		block := blockSrc.fetchAndParseBlock(slot)
		blockFilename := filepath.Join(filepath.Clean(blockSrc.outDir), fmt.Sprintf("%d.json", slot))
		blockSrc.saveBlockToFile(blockFilename, block)
	})

	var err error
	slot := blockSrc.startSlot

	if slot == 0 {
		c := rpcclient.NewRpcClient("https://api.mainnet-beta.solana.com/")
		slot, err = c.GetSlot()
		if err != nil {
			panic(err)
		}
	}

	for {
		workerPool.Invoke(slot)
		slot++
	}
}

func (blockSrc *BlockSource) fetchAndParseBlock(slot uint64) *rpc.GetBlockResult {
	var blockResult *rpc.GetBlockResult
	var err error

	client := blockSrc.rpcPool.Take()
	defer blockSrc.rpcPool.Release(client)

	for {
		blockResult, err = client.GetBlockConfirmed(uint64(slot))
		if err == nil {
			return blockResult
		} else if err == rpcclient.SlotSkipped {
			return nil
		} else if strings.Contains(err.Error(), "Block not available for slot") { // we're too early. wait for a bit.
			time.Sleep(500 * time.Millisecond)
		} else {
			panic(fmt.Sprintf("error fetching block: %s\n", err))
		}
	}
}

func (blockSrc *BlockSource) saveBlockToFile(filename string, b *rpc.GetBlockResult) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(b)
}
