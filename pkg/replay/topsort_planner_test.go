package replay

import (
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/google/go-cmp/cmp"
)

func testSig(b byte) solana.Signature {
	var sig [64]byte
	for i := 0; i < 64; i++ {
		sig[i] = b
	}
	return sig
}

func testPk(b byte) solana.PublicKey {
	var pk [32]byte
	for i := 0; i < 32; i++ {
		pk[i] = b
	}
	return pk
}

func testTx(sigbyte byte) *solana.Transaction {
	return &solana.Transaction{
		Signatures: []solana.Signature{testSig(sigbyte)},
	}
}

func testTxs(n int) []*solana.Transaction {
	out := make([]*solana.Transaction, n)
	for i := range out {
		out[i] = testTx(byte(i))
	}
	return out
}

func testTxMeta(readAcctBytes []byte, writeAcctBytes []byte) *rpc.TransactionMeta {
	tm := &rpc.TransactionMeta{}
	for _, r := range readAcctBytes {
		tm.LoadedAddresses.ReadOnly = append(tm.LoadedAddresses.ReadOnly, testPk(r))
	}
	for _, w := range writeAcctBytes {
		tm.LoadedAddresses.Writable = append(tm.LoadedAddresses.Writable, testPk(w))
	}
	return tm
}

func TestTopsortReadAfterWriteSequential(t *testing.T) {
	b := &Block{
		Transactions: testTxs(2),
		TxMetas: []*rpc.TransactionMeta{
			testTxMeta(nil, []byte{0}),
			testTxMeta([]byte{0}, nil),
		},
	}
	want := [][]int{{0}, {1}}

	got := TopsortPlanner(b)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("-want +got:\n%s", diff)
	}
}

func TestTopsortWriteAfterReadSequential(t *testing.T) {
	b := &Block{
		Transactions: testTxs(2),
		TxMetas: []*rpc.TransactionMeta{
			testTxMeta([]byte{0}, nil),
			testTxMeta(nil, []byte{0}),
		},
	}
	want := [][]int{{0}, {1}}

	got := TopsortPlanner(b)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("-want +got:\n%s", diff)
	}
}

func TestTopsortReadonlyExecuteAllParallel(t *testing.T) {
	b := &Block{
		Transactions: testTxs(3),
		TxMetas: []*rpc.TransactionMeta{
			testTxMeta([]byte{0}, nil),
			testTxMeta([]byte{0}, nil),
			testTxMeta([]byte{0}, nil),
		},
	}
	want := [][]int{{0, 1, 2}}

	got := TopsortPlanner(b)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("-want +got:\n%s", diff)
	}
}

func TestTopsortChainedTxsExecuteSequentially(t *testing.T) {
	b := &Block{
		Transactions: testTxs(3),
		TxMetas: []*rpc.TransactionMeta{
			testTxMeta(nil, []byte{0}),
			testTxMeta(nil, []byte{0}),
			testTxMeta(nil, []byte{0}),
		},
	}
	want := [][]int{{0}, {1}, {2}}

	got := TopsortPlanner(b)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("-want +got:\n%s", diff)
	}
}

func TestTopsortDisjointWritesExecuteAllParallel(t *testing.T) {
	b := &Block{
		Transactions: testTxs(3),
		TxMetas: []*rpc.TransactionMeta{
			testTxMeta(nil, []byte{0}),
			testTxMeta(nil, []byte{1}),
			testTxMeta(nil, []byte{2}),
		},
	}
	want := [][]int{{0, 1, 2}}

	got := TopsortPlanner(b)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("-want +got:\n%s", diff)
	}
}

func TestTopsortMultipleChains(t *testing.T) {
	b := &Block{
		Transactions: testTxs(8),
		TxMetas: []*rpc.TransactionMeta{
			testTxMeta(nil, []byte{0}),
			testTxMeta(nil, []byte{0}),
			testTxMeta(nil, []byte{1}),
			testTxMeta(nil, []byte{0}),
			testTxMeta(nil, []byte{1}),
			testTxMeta(nil, []byte{2}),
			testTxMeta(nil, []byte{1}),
			testTxMeta(nil, []byte{0}),
		},
	}
	want := [][]int{{0, 2, 5}, {1, 4}, {3, 6}, {7}}

	got := TopsortPlanner(b)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("-want +got:\n%s", diff)
	}
}

func TestTopsortJoin(t *testing.T) {
	b := &Block{
		Transactions: testTxs(6),
		TxMetas: []*rpc.TransactionMeta{
			testTxMeta(nil, []byte{0}),
			testTxMeta(nil, []byte{1}),
			testTxMeta(nil, []byte{1}),
			testTxMeta(nil, []byte{0}),
			testTxMeta(nil, []byte{0, 1}),
			testTxMeta(nil, []byte{0, 1}),
		},
	}
	want := [][]int{{0, 1}, {3, 2}, {4}, {5}}

	got := TopsortPlanner(b)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("-want +got:\n%s", diff)
	}
}

func TestTopsortFork(t *testing.T) {
	b := &Block{
		Transactions: testTxs(6),
		TxMetas: []*rpc.TransactionMeta{
			testTxMeta(nil, []byte{0, 1}),
			testTxMeta(nil, []byte{0, 1}),
			testTxMeta(nil, []byte{1}),
			testTxMeta(nil, []byte{1}),
			testTxMeta(nil, []byte{0}),
			testTxMeta(nil, []byte{0}),
		},
	}
	want := [][]int{{0}, {1}, {2, 4}, {3, 5}}

	got := TopsortPlanner(b)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("-want +got:\n%s", diff)
	}
}

func TestTopsortForkAndJoin(t *testing.T) {
	b := &Block{
		Transactions: testTxs(9),
		TxMetas: []*rpc.TransactionMeta{
			testTxMeta(nil, []byte{0, 1}),
			testTxMeta(nil, []byte{0, 1}),
			testTxMeta(nil, []byte{1}),
			testTxMeta(nil, []byte{1}),
			testTxMeta(nil, []byte{0}),
			testTxMeta(nil, []byte{0, 1}),
			testTxMeta(nil, []byte{1}),
			testTxMeta(nil, []byte{0}),
			testTxMeta(nil, []byte{0}),
		},
	}
	want := [][]int{{0}, {1}, {2, 4}, {3}, {5}, {6, 7}, {8}}

	got := TopsortPlanner(b)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("-want +got:\n%s", diff)
	}
}
