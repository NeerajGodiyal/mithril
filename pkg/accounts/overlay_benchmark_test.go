package accounts

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gagliardetto/solana-go"
)

const overlayPublicationBenchmarkBatchCount = 1 << 15

type overlayPublicationBenchmarkBatch struct {
	accountStates [2]*Account
	touched       [2]bool
}

type overlayPublicationBenchmarkStore interface {
	GetAccount(pubkey *[32]byte) (*Account, error)
	SetTransactionAccounts(accountStates []*Account, touched []bool) error
}

type globalOverlayBenchmarkStore struct {
	mu     sync.RWMutex
	delta  map[[32]byte]*Account
	parent Accounts
}

func (o *globalOverlayBenchmarkStore) GetAccount(pubkey *[32]byte) (*Account, error) {
	o.mu.RLock()
	if acct, ok := o.delta[*pubkey]; ok {
		o.mu.RUnlock()
		return acct, nil
	}
	acct, err := o.parent.GetAccount(pubkey)
	o.mu.RUnlock()
	return acct, err
}

func (o *globalOverlayBenchmarkStore) SetTransactionAccounts(accountStates []*Account, touched []bool) error {
	if err := validateTransactionAccountBatch(accountStates, touched); err != nil {
		return err
	}
	o.mu.Lock()
	for idx, acct := range accountStates {
		if touched[idx] {
			o.delta[acct.Key] = transactionAccountForStorage(acct)
		}
	}
	o.mu.Unlock()
	return nil
}

func overlayPublicationBenchmarkKey(n uint64) solana.PublicKey {
	var key solana.PublicKey
	binary.LittleEndian.PutUint64(key[:8], n)
	binary.LittleEndian.PutUint64(key[8:16], n*0x9e3779b97f4a7c15)
	binary.LittleEndian.PutUint64(key[16:24], n^0xa0761d6478bd642f)
	binary.LittleEndian.PutUint64(key[24:], n*0xe7037ed1a0b428db)
	return key
}

func overlayPublicationBenchmarkFixture(b *testing.B) (MemAccounts, []overlayPublicationBenchmarkBatch) {
	parent := NewMemAccountsWithLen(overlayPublicationBenchmarkBatchCount * 2)
	batches := make([]overlayPublicationBenchmarkBatch, overlayPublicationBenchmarkBatchCount)
	for batchIdx := range batches {
		for accountIdx := range batches[batchIdx].accountStates {
			key := overlayPublicationBenchmarkKey(uint64(batchIdx*2 + accountIdx + 1))
			acct := &Account{Key: key, Lamports: uint64(batchIdx + 1)}
			if err := parent.SetAccountWithoutLock(key, acct); err != nil {
				b.Fatal(err)
			}
			batches[batchIdx].accountStates[accountIdx] = acct
			batches[batchIdx].touched[accountIdx] = true
		}
	}
	return parent, batches
}

func BenchmarkOverlayAccountsParallelReadPublish(b *testing.B) {
	parent, batches := overlayPublicationBenchmarkFixture(b)
	benchmarks := []struct {
		name string
		new  func() overlayPublicationBenchmarkStore
	}{
		{
			name: "global",
			new: func() overlayPublicationBenchmarkStore {
				return &globalOverlayBenchmarkStore{
					delta:  make(map[[32]byte]*Account, overlayPublicationBenchmarkBatchCount*2),
					parent: parent,
				}
			},
		},
		{
			name: "sharded",
			new: func() overlayPublicationBenchmarkStore {
				return NewOverlayAccountsWithLen(parent, overlayPublicationBenchmarkBatchCount*2)
			},
		},
	}

	for _, benchmark := range benchmarks {
		b.Run(benchmark.name, func(b *testing.B) {
			overlay := benchmark.new()
			var nextLane atomic.Uint64
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				lane := nextLane.Add(1) - 1
				batchIdx := int((lane * 4099) & (overlayPublicationBenchmarkBatchCount - 1))
				for pb.Next() {
					batch := &batches[batchIdx]
					for _, acct := range batch.accountStates {
						key := [32]byte(acct.Key)
						if _, err := overlay.GetAccount(&key); err != nil {
							panic(err)
						}
					}
					if err := overlay.SetTransactionAccounts(batch.accountStates[:], batch.touched[:]); err != nil {
						panic(err)
					}
					batchIdx = (batchIdx + 1) & (overlayPublicationBenchmarkBatchCount - 1)
				}
			})
			b.ReportMetric(2, "accounts/op")
		})
	}
}
