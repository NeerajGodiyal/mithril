package replay

import (
	"encoding/binary"
	"testing"

	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/gagliardetto/solana-go"
)

var (
	benchmarkTransactionMessageHash [32]byte
	benchmarkExecutionPlan          blockTransactionExecutionPlan
)

func benchmarkUniqueTransactions(count int) []*solana.Transaction {
	transactions := make([]*solana.Transaction, count)
	for index := range transactions {
		data := make([]byte, 4)
		binary.LittleEndian.PutUint32(data, uint32(index))
		transactions[index] = &solana.Transaction{
			Message: solana.Message{
				Header:          solana.MessageHeader{NumRequiredSignatures: 1},
				AccountKeys:     []solana.PublicKey{{1}, {2}},
				RecentBlockhash: solana.Hash{3},
				Instructions: []solana.CompiledInstruction{{
					ProgramIDIndex: 1,
					Accounts:       []uint16{0},
					Data:           data,
				}},
			},
		}
	}
	return transactions
}

func BenchmarkTransactionMessageIdentityLifecycle32K(benchmark *testing.B) {
	transactions := benchmarkUniqueTransactions(32_000)

	benchmark.Run("six_hash_passes", func(benchmark *testing.B) {
		benchmark.ReportAllocs()
		benchmark.ResetTimer()
		for range benchmark.N {
			for range 6 {
				for _, tx := range transactions {
					messageHash, err := TransactionMessageHash(tx)
					if err != nil {
						benchmark.Fatal(err)
					}
					benchmarkTransactionMessageHash = messageHash
				}
			}
		}
	})

	benchmark.Run("prepared_process_path", func(benchmark *testing.B) {
		benchmark.ReportAllocs()
		benchmark.ResetTimer()
		for iteration := range benchmark.N {
			block := &b.Block{Slot: uint64(iteration + 1), Transactions: transactions}
			plan, err := planBlockTransactionExecution(block)
			if err != nil {
				benchmark.Fatal(err)
			}
			if !plan.messageIdentities.MatchesBlock(block) ||
				!plan.messageIdentities.MatchesBlock(block) {
				benchmark.Fatal("prepared plan lost its block binding")
			}
			benchmarkExecutionPlan = plan
		}
	})

	preparedBlock := &b.Block{Slot: 1, Transactions: transactions}
	if _, err := preparedBlock.PrepareTransactionMessageIdentities(); err != nil {
		benchmark.Fatal(err)
	}
	benchmark.Run("cached_duplicate_plan", func(benchmark *testing.B) {
		benchmark.ReportAllocs()
		benchmark.ResetTimer()
		for range benchmark.N {
			plan, err := planBlockTransactionExecution(preparedBlock)
			if err != nil {
				benchmark.Fatal(err)
			}
			benchmarkExecutionPlan = plan
		}
	})
}
