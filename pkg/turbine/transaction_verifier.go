package turbine

import (
	"context"
	"fmt"
	"runtime"
	"sync"

	"github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/txverify"
	"github.com/gagliardetto/solana-go"
)

var errNilTransaction = fmt.Errorf("nil transaction")

type transactionVerifyJob struct {
	tx   *solana.Transaction
	err  *error
	done *sync.WaitGroup
}

type transactionVerifier struct {
	jobs    chan transactionVerifyJob
	verify  func(*solana.Transaction) error
	workers int
	close   sync.Once
	worker  sync.WaitGroup
}

func newTransactionVerifier(workers, queueDepth int, verify func(*solana.Transaction) error) *transactionVerifier {
	if workers < 1 {
		workers = 1
	}
	if queueDepth < 1 {
		queueDepth = 1
	}
	if verify == nil {
		verify = txverify.VerifyTransaction
	}
	v := &transactionVerifier{
		jobs:    make(chan transactionVerifyJob, queueDepth),
		verify:  verify,
		workers: workers,
	}
	v.worker.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer v.worker.Done()
			for job := range v.jobs {
				func() {
					defer job.done.Done()
					*job.err = verifyTransactionSafely(v.verify, job.tx)
				}()
			}
		}()
	}
	return v
}

func (v *transactionVerifier) closeAndWait() {
	if v == nil {
		return
	}
	v.close.Do(func() {
		close(v.jobs)
		v.worker.Wait()
	})
}

func verifyTransactionSafely(verify func(*solana.Transaction) error, tx *solana.Transaction) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("signature verifier panic: %v", recovered)
		}
	}()
	return verify(tx)
}

func (v *transactionVerifier) verifyBlock(blk *block.Block) error {
	return v.verifyBlockContext(context.Background(), blk)
}

func (v *transactionVerifier) verifyBlockContext(ctx context.Context, blk *block.Block) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if blk == nil || len(blk.Transactions) == 0 {
		return nil
	}
	// Admit one worker-wave at a time. A monster block still occupies every
	// verifier lane, but cannot park tens of thousands of jobs ahead of a newly
	// completed small block in the shared bounded queue.
	for chunkStart := 0; chunkStart < len(blk.Transactions); chunkStart += v.workers {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunkEnd := min(chunkStart+v.workers, len(blk.Transactions))
		errs := make([]error, chunkEnd-chunkStart)
		var done sync.WaitGroup
		for txIdx := chunkStart; txIdx < chunkEnd; txIdx++ {
			if err := ctx.Err(); err != nil {
				done.Wait()
				return err
			}
			tx := blk.Transactions[txIdx]
			errIdx := txIdx - chunkStart
			if tx == nil {
				errs[errIdx] = errNilTransaction
				continue
			}
			done.Add(1)
			select {
			case v.jobs <- transactionVerifyJob{tx: tx, err: &errs[errIdx], done: &done}:
			case <-ctx.Done():
				done.Done()
				done.Wait()
				return ctx.Err()
			}
		}
		done.Wait()
		if err := ctx.Err(); err != nil {
			return err
		}
		for errIdx, err := range errs {
			if err != nil {
				return formatTransactionVerificationError(blk, chunkStart+errIdx, err)
			}
		}
	}
	return nil
}

func formatTransactionVerificationError(blk *block.Block, txIdx int, err error) error {
	txSig := "<missing>"
	version := solana.MessageVersionLegacy
	if blk != nil && txIdx >= 0 && txIdx < len(blk.Transactions) && blk.Transactions[txIdx] == nil {
		return fmt.Errorf("slot %d transaction %d is nil", blk.Slot, txIdx)
	}
	if blk != nil && txIdx >= 0 && txIdx < len(blk.Transactions) && blk.Transactions[txIdx] != nil {
		tx := blk.Transactions[txIdx]
		version = tx.Message.GetVersion()
		if len(tx.Signatures) > 0 {
			txSig = tx.Signatures[0].String()
		}
	}
	return fmt.Errorf("slot %d transaction %d %s version=%d failed signature verification: %w", blk.Slot, txIdx, txSig, version, err)
}

var (
	defaultTransactionVerifierOnce sync.Once
	defaultTransactionVerifier     *transactionVerifier
)

func validateBlockTransactionsContext(ctx context.Context, blk *block.Block) error {
	defaultTransactionVerifierOnce.Do(func() {
		workers := max(1, (runtime.GOMAXPROCS(0)+1)/2)
		defaultTransactionVerifier = newTransactionVerifier(workers, 2*workers, nil)
	})
	return defaultTransactionVerifier.verifyBlockContext(ctx, blk)
}

func validateBlockTransactions(blk *block.Block) error {
	return validateBlockTransactionsContext(context.Background(), blk)
}
