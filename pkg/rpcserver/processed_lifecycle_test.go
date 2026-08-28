package rpcserver

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
)

type blockingProcessedAccountReader struct {
	started chan struct{}
	release chan struct{}
}

func (r *blockingProcessedAccountReader) GetAccount(uint64, solana.PublicKey) (*accounts.Account, error) {
	close(r.started)
	<-r.release
	return nil, accountsdb.ErrNoAccount
}

func (*blockingProcessedAccountReader) ValidateAccountRead() error { return nil }

type blockingProcessedValidationReader struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Uint32
}

func (*blockingProcessedValidationReader) GetAccount(uint64, solana.PublicKey) (*accounts.Account, error) {
	return nil, accountsdb.ErrNoAccount
}

func (r *blockingProcessedValidationReader) ValidateAccountRead() error {
	if r.calls.Add(1) == 1 {
		close(r.started)
		<-r.release
	}
	return nil
}

func TestAccountResponseRejectsBankInvalidatedWhileReading(t *testing.T) {
	server := &RpcServer{}
	reader := &blockingProcessedAccountReader{started: make(chan struct{}), release: make(chan struct{})}
	server.SetSlotCtx(&sealevel.SlotCtx{Slot: 250, UnrootedRead: reader})
	params := mustRawParams(t, []interface{}{
		solana.PublicKey{1}.String(),
		map[string]interface{}{"commitment": "processed", "encoding": "base64"},
	})
	errCh := make(chan error, 1)
	go func() {
		_, err := server.GetAccountInfo(context.Background(), params)
		errCh <- err
	}()
	<-reader.started
	server.SetSlotCtx(nil)
	close(reader.release)
	if err := <-errCh; err == nil || !strings.Contains(err.Error(), "invalidated during getAccountInfo") {
		t.Fatalf("account response accepted an invalidated bank: %v", err)
	}
}

func TestSimulationRejectsBankInvalidatedWhileInFlight(t *testing.T) {
	server := &RpcServer{}
	reader := &blockingProcessedValidationReader{started: make(chan struct{}), release: make(chan struct{})}
	server.SetSlotCtx(&sealevel.SlotCtx{Slot: 250, BlockHeight: 200, UnrootedRead: reader})
	tx, _ := testLegacyTransaction(t)
	params := mustRawParams(t, []interface{}{
		tx.MustToBase64(),
		map[string]interface{}{"commitment": "processed", "encoding": "base64", "sigVerify": true},
	})
	errCh := make(chan error, 1)
	go func() {
		_, err := server.SimulateTransaction(context.Background(), params)
		errCh <- err
	}()
	<-reader.started
	server.SetSlotCtx(nil)
	close(reader.release)
	if err := <-errCh; err == nil || !strings.Contains(err.Error(), "invalidated during simulation") {
		t.Fatalf("simulation accepted an invalidated bank: %v", err)
	}
}

func TestSendRejectsBankInvalidatedWhileInFlight(t *testing.T) {
	server := &RpcServer{}
	reader := &blockingProcessedValidationReader{started: make(chan struct{}), release: make(chan struct{})}
	server.SetSlotCtx(&sealevel.SlotCtx{Slot: 250, BlockHeight: 200, UnrootedRead: reader})
	tx, _ := testLegacyTransaction(t)
	params := mustRawParams(t, []interface{}{
		tx.MustToBase64(),
		map[string]interface{}{"encoding": "base64", "skipPreflight": true},
	})
	errCh := make(chan error, 1)
	go func() {
		_, err := server.SendTransaction(context.Background(), params)
		errCh <- err
	}()
	<-reader.started
	server.SetSlotCtx(nil)
	close(reader.release)
	if err := <-errCh; err == nil || !strings.Contains(err.Error(), "invalidated during transaction validation") {
		t.Fatalf("send accepted an invalidated bank: %v", err)
	}
}

func TestPublishingNewTipDoesNotRevokeCapturedBank(t *testing.T) {
	server := &RpcServer{}
	server.SetSlotCtx(&sealevel.SlotCtx{Slot: 250})
	_, generation := server.getSlotCtxWithLifecycle()
	server.SetSlotCtx(&sealevel.SlotCtx{Slot: 251})
	if err := server.validateSlotCtxLifecycle(generation, "test"); err != nil {
		t.Fatalf("normal tip publication revoked an immutable captured bank: %v", err)
	}
}

func TestProcessedMetadataRejectsBankInvalidatedBeforePublication(t *testing.T) {
	tests := []struct {
		name string
		call func(*RpcServer) error
	}{
		{
			name: "latest blockhash",
			call: func(server *RpcServer) error {
				_, err := server.GetLatestBlockhash(context.Background(), mustRawParams(t, []interface{}{}))
				return err
			},
		},
		{
			name: "block height",
			call: func(server *RpcServer) error {
				_, err := server.GetBlockHeight(context.Background(), mustRawParams(t, []interface{}{}))
				return err
			},
		},
		{
			name: "epoch info",
			call: func(server *RpcServer) error {
				_, err := server.GetEpochInfo(context.Background(), mustRawParams(t, []interface{}{}))
				return err
			},
		},
		{
			name: "submitted status",
			call: func(server *RpcServer) error {
				_, err := server.GetSubmittedTransactionStatus(
					context.Background(),
					statusParams(t, []any{statusSig(1).String()}),
				)
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := &blockingProcessedValidationReader{started: make(chan struct{}), release: make(chan struct{})}
			server, _ := newStatusServer(t)
			server.epochSchedule = &sealevel.SysvarEpochSchedule{SlotsPerEpoch: 100, FirstNormalEpoch: 0}
			server.SetSlotCtx(&sealevel.SlotCtx{
				Slot:             250,
				BlockHeight:      200,
				Epoch:            2,
				Blockhash:        solana.Hash{9},
				TransactionCount: 321,
				UnrootedRead:     reader,
			})

			errCh := make(chan error, 1)
			go func() { errCh <- test.call(server) }()
			<-reader.started
			server.SetSlotCtx(nil)
			close(reader.release)
			if err := <-errCh; err == nil || !strings.Contains(err.Error(), "invalidated during") {
				t.Fatalf("published metadata from an invalidated bank: %v", err)
			}
		})
	}
}
