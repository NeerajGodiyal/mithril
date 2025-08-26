package blocksub

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	//"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/ws"
	laserstream "github.com/helius-labs/laserstream-sdk/go"
)

type BlockEndpointType uint64

const (
	BlockEndpointTypeWebSocket = iota
	BlockEndpointTypeLaserStream
)

type BlockSubscriberOpts struct {
	Endpoint     string
	OutDir       string
	EndpointType BlockEndpointType
	ApiKey       string
}

type BlockSubscriber struct {
	wsClient          *ws.Client
	laserStreamClient *laserstream.Client
	outDir            string
	latestBlock       uint64
	endpointType      BlockEndpointType
}

func NewBlockSubscriber(opts BlockSubscriberOpts) *BlockSubscriber {
	var subscriber *BlockSubscriber
	var endpoint string

	switch opts.EndpointType {
	case BlockEndpointTypeWebSocket:
		{
			endpoint = strings.ReplaceAll(opts.Endpoint, "https://", "wss://")
			endpoint = strings.ReplaceAll(opts.Endpoint, "http://", "wss://")

			client, err := ws.Connect(context.Background(), endpoint)
			if err != nil {
				panic(err)
			}

			subscriber = &BlockSubscriber{wsClient: client, outDir: opts.OutDir, endpointType: opts.EndpointType}
		}

	case BlockEndpointTypeLaserStream:
		{
			client := laserstream.NewClient(laserstream.LaserstreamConfig{
				Endpoint: opts.Endpoint,
				APIKey:   opts.ApiKey,
			})
			subscriber = &BlockSubscriber{laserStreamClient: client, outDir: opts.OutDir, endpointType: opts.EndpointType}
		}

	default:
		panic(fmt.Sprintf("invalid endpoint type: %d", opts.EndpointType))
	}

	mlog.Log.Infof("connecting to endpoint websocket on %s", endpoint)
	return subscriber
}

func (subscriber *BlockSubscriber) Start() {

	switch subscriber.endpointType {
	case BlockEndpointTypeWebSocket:
		subscriber.handleStandardWebSocketSubscription()

	case BlockEndpointTypeLaserStream:
		subscriber.handleLaserStreamSubscription()

	default:
		panic("invalid subscription channel type")
	}
}

func (subscriber *BlockSubscriber) handleStandardWebSocketSubscription() {
	ctx := context.Background()
	filter := ws.NewBlockSubscribeFilterAll()
	s, err := subscriber.wsClient.BlockSubscribe(filter, &ws.BlockSubscribeOpts{Commitment: rpc.CommitmentFinalized})
	if err != nil {
		panic(err)
	}

	for {
		mlog.Log.Infof("receiving on websocket")
		recvd, err := s.Recv(ctx)
		if err != nil {
			mlog.Log.Errorf("error in block subscriber: %s", err)
		}
		mlog.Log.Infof("got message on websocket")
		blockFilename := filepath.Join(filepath.Clean(subscriber.outDir), fmt.Sprintf("%d.json", recvd.Value.Slot))
		err = subscriber.saveBlockToFile(blockFilename, recvd.Value.Block)
		if err != nil {
			mlog.Log.Errorf("error saving block %d in block subscriber: %s", recvd.Value.Slot, err)
		} else {
			mlog.Log.Infof("received block %d and saved to file %s", recvd.Value.Slot, blockFilename)
		}
		subscriber.latestBlock++
	}
}

func (subscriber *BlockSubscriber) handleLaserStreamSubscription() {
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

	err := subscriber.laserStreamClient.Subscribe(req,
		func(update *laserstream.SubscribeUpdate) {
			mlog.Log.Infof("Received block %d", update.GetBlock().Slot)
			update.GetBlock()
		},
		func(err error) {
			panic(err)
		},
	)
	if err != nil {
		panic(err)
	}
}

func (subscriber *BlockSubscriber) LatestBlock() uint64 {
	return subscriber.latestBlock
}

func (subscriber *BlockSubscriber) saveBlockToFile(filename string, b *rpc.GetBlockResult) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(b)
}
