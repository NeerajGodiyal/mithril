package blockstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	//"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/overcast"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/ws"
	laserstream "github.com/helius-labs/laserstream-sdk/go"
	"github.com/panjf2000/ants/v2"
	"google.golang.org/grpc"
)

type BackgroundBlockDownloaderSourceType uint64

const (
	BackgroundBlockDownloaderSourceWebSocket = iota
	BackgroundBlockDownloaderSourceLaserStream
	BackgroundBlockDownloaderSourceRpc
	BackgroundBlockDownloaderSourceOvercast
)

type BackgroundBlockDownloaderOpts struct {
	WsEndpoint       string
	LsEndpoint       string
	RpcEndpoints     []string // List of RPC endpoints for load balancing
	OvercastEndpoint string
	OutDir           string
	SourceType       BackgroundBlockDownloaderSourceType
	LsApiKey         string
	Channel          chan *block.Block
	StartSlot        uint64
}

type BackgroundBlockDownloader struct {
	wsClient          *ws.Client
	laserStreamClient *laserstream.Client
	rpcClient         *rpcclient.RpcClient
	rpcPool           *rpcConnPool
	overcastClient    *overcast.SlotStreamClient
	outDir            string
	overcastEndpoint  string
	sourceType        BackgroundBlockDownloaderSourceType
	output            chan *block.Block
	startSlot         uint64
}

var (
	errFileAlreadyExists = errors.New("errFileAlreadyExists")
)

func NewBlockDownloader(opts BackgroundBlockDownloaderOpts) *BackgroundBlockDownloader {
	var downloader *BackgroundBlockDownloader
	var endpoint string

	switch opts.SourceType {
	case BackgroundBlockDownloaderSourceWebSocket:
		{
			endpoint = strings.ReplaceAll(opts.WsEndpoint, "https://", "wss://")
			endpoint = strings.ReplaceAll(opts.WsEndpoint, "http://", "wss://")

			client, err := ws.Connect(context.Background(), endpoint)
			if err != nil {
				panic(err)
			}

			downloader = &BackgroundBlockDownloader{wsClient: client, outDir: opts.OutDir, sourceType: opts.SourceType}
		}

	case BackgroundBlockDownloaderSourceLaserStream:
		{
			lsClient := laserstream.NewClient(laserstream.LaserstreamConfig{
				Endpoint: opts.LsEndpoint,
				APIKey:   opts.LsApiKey,
			})
			rpcClient := rpcclient.NewRpcClient(opts.RpcEndpoints[0])
			downloader = &BackgroundBlockDownloader{laserStreamClient: lsClient, rpcClient: rpcClient, outDir: opts.OutDir, sourceType: opts.SourceType, output: opts.Channel}
		}

	case BackgroundBlockDownloaderSourceRpc:
		{
			pool := newRpcConnPool(opts.RpcEndpoints)
			os.Mkdir(opts.OutDir, 0777)
			downloader = &BackgroundBlockDownloader{rpcPool: pool, outDir: opts.OutDir, sourceType: opts.SourceType, startSlot: opts.StartSlot}
		}

	case BackgroundBlockDownloaderSourceOvercast:
		{
			pool := newRpcConnPool(opts.RpcEndpoints)
			os.Mkdir(opts.OutDir, 0777)

			downloader = &BackgroundBlockDownloader{
				rpcPool:          pool,
				startSlot:        opts.StartSlot,
				overcastEndpoint: opts.OvercastEndpoint,
				outDir:           opts.OutDir,
				sourceType:       opts.SourceType}
		}

	default:
		panic(fmt.Sprintf("invalid endpoint type: %d", opts.SourceType))
	}

	return downloader
}

func (downloader *BackgroundBlockDownloader) Start() {

	switch downloader.sourceType {
	case BackgroundBlockDownloaderSourceWebSocket:
		downloader.startStandardWebSocketStream()

	case BackgroundBlockDownloaderSourceLaserStream:
		downloader.startLaserStreamStream()

	case BackgroundBlockDownloaderSourceRpc:
		downloader.startRpcStream()

	case BackgroundBlockDownloaderSourceOvercast:
		go downloader.startRpcDownloadForOvercastCatchup()
		downloader.startOvercastStream()

	default:
		panic("invalid subscription channel type")
	}
}

func (downloader *BackgroundBlockDownloader) startStandardWebSocketStream() {
	ctx := context.Background()
	filter := ws.NewBlockSubscribeFilterAll()
	s, err := downloader.wsClient.BlockSubscribe(filter, &ws.BlockSubscribeOpts{Commitment: rpc.CommitmentFinalized})
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
		blockFilename := filepath.Join(filepath.Clean(downloader.outDir), fmt.Sprintf("%d.json", recvd.Value.Slot))
		err = downloader.saveRpcBlockToFile(blockFilename, recvd.Value.Block, recvd.Value.Slot, false)
		if err != nil {
			mlog.Log.Errorf("error saving block %d in block src: %s", recvd.Value.Slot, err)
		} else {
			mlog.Log.Infof("received block %d and saved to file %s", recvd.Value.Slot, blockFilename)
		}
	}
}

func (downloader *BackgroundBlockDownloader) startLaserStreamStream() {
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

	err := downloader.laserStreamClient.Subscribe(req,
		func(update *laserstream.SubscribeUpdate) {
			block := block.FromLaserStream(update.GetBlock(), downloader.rpcClient)
			mlog.Log.Infof("got slot %d", block.Slot)
			downloader.output <- block
		},
		func(err error) {
			panic(err)
		},
	)
	if err != nil {
		panic(err)
	}
}

func (downloader *BackgroundBlockDownloader) startRpcStream() {
	workerPool, _ := ants.NewPoolWithFunc(downloader.rpcPool.NumClients(), func(i interface{}) {
		slot := i.(uint64)
		block := downloader.fetchAndParseBlockFromRpc(slot)
		if block != nil {
			blockFilename := filepath.Join(filepath.Clean(downloader.outDir), fmt.Sprintf("%d.json", slot))
			downloader.saveRpcBlockToFile(blockFilename, block, slot, false)
		}
	})

	var err error
	slot := downloader.startSlot

	if slot == 0 {
		c := downloader.rpcPool.Take()
		slot, err = c.GetSlot()
		downloader.rpcPool.Release(c)
		if err != nil {
			panic(err)
		}
	}

	for {
		workerPool.Invoke(slot)
		slot++
	}
}

func (downloader *BackgroundBlockDownloader) startRpcDownloadForOvercastCatchup() {
	done := make(chan uint64)

	workerPool, _ := ants.NewPoolWithFunc(downloader.rpcPool.NumClients(), func(i interface{}) {
		slot := i.(uint64)
		block := downloader.fetchAndParseBlockFromRpc(slot)
		if block != nil {
			blockFilename := filepath.Join(filepath.Clean(downloader.outDir), fmt.Sprintf("%d.json", slot))
			err := downloader.saveRpcBlockToFile(blockFilename, block, slot, true)
			if err == errFileAlreadyExists {
				done <- slot
			} else if err != nil {
				mlog.Log.Infof("error saving slot %d to file: %s", slot, err)
			}
		}
	})

	var err error
	slot := downloader.startSlot

	if slot == 0 {
		c := downloader.rpcPool.Take()
		slot, err = c.GetSlot()
		downloader.rpcPool.Release(c)
		if err != nil {
			panic(err)
		}
	}

	for {
		select {
		case slot := <-done:
			{
				mlog.Log.Infof("stopping catchup rpc block fetch (final slot %d)\n", slot)
				time.Sleep(30 * time.Second)
				workerPool.Release()
				ants.Release()
				return
			}
		default:
			{
				workerPool.Invoke(slot)
				slot++
			}
		}
	}
}

func (downloader *BackgroundBlockDownloader) startOvercastStream() {
	conn, err := grpc.NewClient(downloader.overcastEndpoint, grpc.WithInsecure())
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	client := overcast.NewSlotStreamClient(conn)
	stream, err := client.StreamSlots(context.Background(), &overcast.SlotStreamRequest{})
	if err != nil {
		panic(err)
	}

	workerPool, _ := ants.NewPoolWithFunc(4, func(i interface{}) {
		resp := i.(*overcast.SlotResponse)
		blockFilename := filepath.Join(filepath.Clean(downloader.outDir), fmt.Sprintf("%d.json", resp.Slot))
		err = downloader.saveOvercastBlockResponseToFile(blockFilename, resp)
		if err != nil {
			mlog.Log.Infof("error writing block %d to file", resp.Slot)
		}
	})

	for {
		resp, err := stream.Recv()
		if err == nil {
			workerPool.Invoke(resp)
		}
	}
}

func (downloader *BackgroundBlockDownloader) fetchAndParseBlockFromRpc(slot uint64) *rpc.GetBlockResult {
	var blockResult *rpc.GetBlockResult
	var err error

	client := downloader.rpcPool.Take()
	defer downloader.rpcPool.Release(client)

	for {
		// Use single-attempt fetch to avoid inner retry loop bypassing rate limits
		blockResult, err = client.GetBlockConfirmedOnce(uint64(slot))
		if err == nil {
			return blockResult
		} else if err == rpcclient.SlotSkipped {
			return nil
		} else if isSlotNotAvailableErr(err) { // we're too early. wait for a bit.
			time.Sleep(500 * time.Millisecond)
		} else if isRateLimitedErr(err) {
			time.Sleep(2 * time.Second)
		} else if isTransientNetworkErr(err) {
			time.Sleep(1 * time.Second)
		} else {
			panic(fmt.Sprintf("error fetching block: %s\n", err))
		}
	}
}

func (downloader *BackgroundBlockDownloader) saveRpcBlockToFile(filename string, b *rpc.GetBlockResult, slot uint64, checkFileExists bool) error {
	if checkFileExists {
		_, err := os.Stat(filename)
		if !os.IsNotExist(err) {
			return errFileAlreadyExists
		}
	}

	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	convertedBlock := block.FromBlockResult(b, slot, downloader.rpcClient)
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(convertedBlock)
}

func (downloader *BackgroundBlockDownloader) saveOvercastBlockResponseToFile(filename string, overcastBlock *overcast.SlotResponse) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	b := block.FromOvercastStreamMsg(overcastBlock)
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(b)
}
