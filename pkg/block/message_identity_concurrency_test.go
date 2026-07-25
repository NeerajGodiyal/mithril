package block

import (
	"sync"
	"testing"

	"github.com/gagliardetto/solana-go"
)

func TestConcurrentTransactionMessageIdentityPreparationSharesOneResult(t *testing.T) {
	block := &Block{Transactions: []*solana.Transaction{
		identityTestTransaction(1),
		identityTestTransaction(2),
	}}
	const workers = 16
	start := make(chan struct{})
	results := make(chan *PreparedTransactionMessageIdentities, workers)
	errors := make(chan error, workers)
	var wait sync.WaitGroup
	wait.Add(workers)
	for range workers {
		go func() {
			defer wait.Done()
			<-start
			prepared, err := block.PrepareTransactionMessageIdentities()
			results <- prepared
			errors <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errors)

	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent preparation: %v", err)
		}
	}
	var first *PreparedTransactionMessageIdentities
	for prepared := range results {
		if first == nil {
			first = prepared
			continue
		}
		if prepared != first {
			t.Fatal("concurrent preparation published more than one identity set")
		}
	}
}
