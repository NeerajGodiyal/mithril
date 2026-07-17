package replay

import (
	"testing"

	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/tpu/txfixture"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

func TestPrepareDependencyPlannerBlockSkipsUnneededClone(t *testing.T) {
	tx, err := solana.TransactionFromBytes(txfixture.MustSignedTransferWire(1))
	if err != nil {
		t.Fatalf("decode fixture transaction: %v", err)
	}

	for _, test := range []struct {
		name          string
		live          bool
		parallelism   int
		wantSameBlock bool
	}{
		{name: "live parallel", live: true, parallelism: 4, wantSameBlock: true},
		{name: "live sequential", live: true, parallelism: 0, wantSameBlock: true},
		{name: "rpc sequential", live: false, parallelism: 0, wantSameBlock: true},
		{name: "rpc parallel", live: false, parallelism: 4, wantSameBlock: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			block := &b.Block{
				Slot:           99,
				FromLiveStream: test.live,
				Transactions:   []*solana.Transaction{tx},
				TxMetas:        []*rpc.TransactionMeta{{Fee: 5}},
			}
			prepared, err := prepareDependencyPlannerBlock(block, test.parallelism)
			if err != nil {
				t.Fatalf("prepare block: %v", err)
			}
			if gotSame := prepared == block; gotSame != test.wantSameBlock {
				t.Fatalf("same block = %t, want %t", gotSame, test.wantSameBlock)
			}
			if test.wantSameBlock {
				return
			}
			if prepared.Transactions[0] == block.Transactions[0] {
				t.Fatal("parallel RPC replay did not clone its unresolved transaction")
			}
			if prepared.TxMetas[0] == block.TxMetas[0] {
				t.Fatal("parallel RPC replay did not copy its transaction metadata")
			}

			block.Transactions[0].Message.RecentBlockhash[0] ^= 0xff
			if prepared.Transactions[0].Message.RecentBlockhash == block.Transactions[0].Message.RecentBlockhash {
				t.Fatal("prepared transaction changed when the resolved transaction was mutated")
			}
		})
	}
}
