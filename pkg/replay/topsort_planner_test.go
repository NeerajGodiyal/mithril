package replay

import (
	"encoding/json"
	"slices"
	"sort"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/block"
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

// Graph test cases, many taken from https://github.com/apfitzge/prio-graph
type graphTestCase struct {
	name            string
	b               *block.Block
	sortedTxIndices [][]int
}

var tests = []graphTestCase{
	{
		"ReadAfterWriteSequential",
		&block.Block{
			Transactions: testTxs(2),
			TxMetas: []*rpc.TransactionMeta{
				testTxMeta(nil, []byte{0}),
				testTxMeta([]byte{0}, nil),
			},
		},
		[][]int{{0}, {1}},
	},
	{
		"WriteAfterReadSequential",
		&block.Block{
			Transactions: testTxs(2),
			TxMetas: []*rpc.TransactionMeta{
				testTxMeta([]byte{0}, nil),
				testTxMeta(nil, []byte{0}),
			},
		},
		[][]int{{0}, {1}},
	},
	{
		"ReadonlyExecuteAllParallel",
		&block.Block{
			Transactions: testTxs(3),
			TxMetas: []*rpc.TransactionMeta{
				testTxMeta([]byte{0}, nil),
				testTxMeta([]byte{0}, nil),
				testTxMeta([]byte{0}, nil),
			},
		},
		[][]int{{0, 1, 2}},
	},
	{
		"ChainedTxsExecuteSequentially",
		&block.Block{
			Transactions: testTxs(3),
			TxMetas: []*rpc.TransactionMeta{
				testTxMeta(nil, []byte{0}),
				testTxMeta(nil, []byte{0}),
				testTxMeta(nil, []byte{0}),
			},
		},
		[][]int{{0}, {1}, {2}},
	},
	{
		"DisjointWritesExecuteAllParallel",
		&block.Block{
			Transactions: testTxs(3),
			TxMetas: []*rpc.TransactionMeta{
				testTxMeta(nil, []byte{0}),
				testTxMeta(nil, []byte{1}),
				testTxMeta(nil, []byte{2}),
			},
		},
		[][]int{{0, 1, 2}},
	},
	{
		"MultipleChains",
		&block.Block{
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
		},
		[][]int{{0, 2, 5}, {1, 4}, {3, 6}, {7}},
	},
	{
		"Join",
		&block.Block{
			Transactions: testTxs(6),
			TxMetas: []*rpc.TransactionMeta{
				testTxMeta(nil, []byte{0}),
				testTxMeta(nil, []byte{1}),
				testTxMeta(nil, []byte{1}),
				testTxMeta(nil, []byte{0}),
				testTxMeta(nil, []byte{0, 1}),
				testTxMeta(nil, []byte{0, 1}),
			},
		},
		[][]int{{0, 1}, {3, 2}, {4}, {5}},
	},
	{
		"Fork",
		&block.Block{
			Transactions: testTxs(6),
			TxMetas: []*rpc.TransactionMeta{
				testTxMeta(nil, []byte{0, 1}),
				testTxMeta(nil, []byte{0, 1}),
				testTxMeta(nil, []byte{1}),
				testTxMeta(nil, []byte{1}),
				testTxMeta(nil, []byte{0}),
				testTxMeta(nil, []byte{0}),
			},
		},
		[][]int{{0}, {1}, {2, 4}, {3, 5}},
	},
	{
		"ForkAndJoin",
		&block.Block{
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
		},
		[][]int{{0}, {1}, {2, 4}, {3}, {5}, {6, 7}, {8}},
	},
}

func runStream(b *block.Block) []int {
	do := make(chan int, len(b.Transactions))
	done := make(chan int, len(b.Transactions))
	go TopsortPlannerStream(b, do, done)
	var sort []int
	for len(sort) < len(b.Transactions) {
		task := <-do
		sort = append(sort, task)
		done <- task
	}
	return sort
}

func flatten(x [][]int) []int {
	var out []int
	for _, y := range x {
		out = append(out, y...)
	}
	return out
}

func TestTopsort(t *testing.T) {
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Run("Batch", func(t *testing.T) {
				got := TopsortPlanner(test.b)
				if diff := cmp.Diff(test.sortedTxIndices, got); diff != "" {
					t.Errorf("-want +got:\n%s", diff)
				}
			})
			t.Run("Streaming", func(t *testing.T) {
				got := runStream(test.b)
				if diff := cmp.Diff(flatten(test.sortedTxIndices), got); diff != "" {
					t.Errorf("-want +got:\n%s", diff)
				}
			})
		})
	}
}

// runStreamChains runs TopsortPlannerStreamWithChains single-threaded,
// processing one chain at a time and signaling done with the first index.
// Returns the chains in the order they were dispatched.
func runStreamChains(b *block.Block) [][]int {
	do := make(chan WorkItem, len(b.Transactions))
	done := make(chan int, len(b.Transactions))
	go TopsortPlannerStreamWithChains(b, do, done)
	var chains [][]int
	totalTxs := 0
	for totalTxs < len(b.Transactions) {
		item := <-do
		chains = append(chains, item.Indices)
		totalTxs += len(item.Indices)
		done <- item.Indices[0]
	}
	return chains
}

// verifyDependencyOrder checks that the flattened order of tx indices respects
// all dependency edges: no tx appears before any of its prerequisites.
func verifyDependencyOrder(t *testing.T, b *block.Block, order []int) {
	t.Helper()
	adjList, _ := blockToDependencyGraph(b)
	// Build position map: txIdx -> position in execution order
	pos := make(map[int]int, len(order))
	for i, idx := range order {
		pos[idx] = i
	}
	for u, deps := range adjList {
		for _, v := range deps {
			if pos[u] >= pos[int(v)] {
				t.Errorf("dependency violation: tx %d (pos %d) must come before tx %d (pos %d)", u, pos[u], int(v), pos[int(v)])
			}
		}
	}
}

func TestTopsortChains(t *testing.T) {
	// Run all existing test cases through the chain planner and verify:
	// 1. All tx indices appear exactly once
	// 2. Flattened order respects all dependencies
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			chains := runStreamChains(test.b)
			flat := flatten(chains)

			// Check all indices present exactly once
			if len(flat) != len(test.b.Transactions) {
				t.Fatalf("expected %d txs, got %d", len(test.b.Transactions), len(flat))
			}
			seen := make(map[int]bool)
			for _, idx := range flat {
				if seen[idx] {
					t.Errorf("duplicate tx index %d", idx)
				}
				seen[idx] = true
			}
			for i := range test.b.Transactions {
				if !seen[i] {
					t.Errorf("missing tx index %d", i)
				}
			}

			verifyDependencyOrder(t, test.b, flat)
		})
	}
}

type chainTestCase struct {
	name           string
	b              *block.Block
	expectedChains [][]int // expected chains in dispatch order
}

var chainTests = []chainTestCase{
	{
		// 0→1→2→3: single linear chain, all coalesced
		"LinearChain",
		&block.Block{
			Transactions: testTxs(4),
			TxMetas: []*rpc.TransactionMeta{
				testTxMeta(nil, []byte{0}),
				testTxMeta(nil, []byte{0}),
				testTxMeta(nil, []byte{0}),
				testTxMeta(nil, []byte{0}),
			},
		},
		[][]int{{0, 1, 2, 3}},
	},
	{
		// 0→1, 1→2, 1→3: chain [0,1] then singletons [2] and [3]
		// tx0 writes A, tx1 writes A+B, tx2 writes A, tx3 writes B
		// adjList[0]=[1], adjList[1]=[2,3], so chain breaks at 1 (out-degree 2)
		"ForkAtMiddle",
		&block.Block{
			Transactions: testTxs(4),
			TxMetas: []*rpc.TransactionMeta{
				testTxMeta(nil, []byte{0}),
				testTxMeta(nil, []byte{0, 1}),
				testTxMeta(nil, []byte{0}),
				testTxMeta(nil, []byte{1}),
			},
		},
		[][]int{{0, 1}, {2}, {3}},
	},
	{
		// 0→2, 1→2, 2→3: singletons [0],[1] then chain [2,3]
		// tx0 writes A, tx1 writes B, tx2 writes A+B, tx3 writes A
		"JoinThenChain",
		&block.Block{
			Transactions: testTxs(4),
			TxMetas: []*rpc.TransactionMeta{
				testTxMeta(nil, []byte{0}),
				testTxMeta(nil, []byte{1}),
				testTxMeta(nil, []byte{0, 1}),
				testTxMeta(nil, []byte{0}),
			},
		},
		[][]int{{0}, {1}, {2, 3}},
	},
	{
		// Diamond: 0→1, 0→2, 1→3, 2→3 — all singletons
		// tx0 writes A+B, tx1 writes A, tx2 writes B, tx3 writes A+B
		"Diamond",
		&block.Block{
			Transactions: testTxs(4),
			TxMetas: []*rpc.TransactionMeta{
				testTxMeta(nil, []byte{0, 1}),
				testTxMeta(nil, []byte{0}),
				testTxMeta(nil, []byte{1}),
				testTxMeta(nil, []byte{0, 1}),
			},
		},
		[][]int{{0}, {1}, {2}, {3}},
	},
	{
		// Two independent linear chains: 0→1→2 on acct A, 3→4→5 on acct B
		"DisjointLinearChains",
		&block.Block{
			Transactions: testTxs(6),
			TxMetas: []*rpc.TransactionMeta{
				testTxMeta(nil, []byte{0}),
				testTxMeta(nil, []byte{0}),
				testTxMeta(nil, []byte{0}),
				testTxMeta(nil, []byte{1}),
				testTxMeta(nil, []byte{1}),
				testTxMeta(nil, []byte{1}),
			},
		},
		[][]int{{0, 1, 2}, {3, 4, 5}},
	},
	{
		// Single tx
		"SingleTx",
		&block.Block{
			Transactions: testTxs(1),
			TxMetas: []*rpc.TransactionMeta{
				testTxMeta(nil, []byte{0}),
			},
		},
		[][]int{{0}},
	},
	{
		// All independent — each is its own chain
		"AllIndependent",
		&block.Block{
			Transactions: testTxs(4),
			TxMetas: []*rpc.TransactionMeta{
				testTxMeta(nil, []byte{0}),
				testTxMeta(nil, []byte{1}),
				testTxMeta(nil, []byte{2}),
				testTxMeta(nil, []byte{3}),
			},
		},
		[][]int{{0}, {1}, {2}, {3}},
	},
}

func TestTopsortChainDecomposition(t *testing.T) {
	for _, test := range chainTests {
		t.Run(test.name, func(t *testing.T) {
			chains := runStreamChains(test.b)
			if diff := cmp.Diff(test.expectedChains, chains); diff != "" {
				t.Errorf("chains -want +got:\n%s", diff)
			}

			// Also verify dependency order
			flat := flatten(chains)
			verifyDependencyOrder(t, test.b, flat)
		})
	}
}

func mustMarshal(b *block.Block) []byte {
	bBytes, err := json.Marshal(b)
	if err != nil {
		panic(err)
	}
	return bBytes
}

func unwrap(txs []tx) []int {
	i := make([]int, len(txs))
	for ti, tx := range txs {
		i[ti] = int(tx)
	}
	return i
}

func FuzzBlockToDependencyGraph(f *testing.F) {
	for _, test := range tests {
		f.Add(mustMarshal(test.b))
	}

	f.Fuzz(func(t *testing.T, blockBytes []byte) {
		b := &block.Block{}
		err := json.Unmarshal(blockBytes, b)
		if err != nil {
			t.Skip("skipping unmarshalable block")
		}
		if len(b.Transactions) != len(b.TxMetas) {
			t.Skip("skipping malformed block, not all txs have txmetas")
		}
		for _, tx := range b.Transactions {
			if tx.Message.IsResolved() {
				t.Skip("skipping resolved tx")
			}
		}

		adjList, inDegrees := blockToDependencyGraph(b)
		if len(adjList) != len(b.Transactions) {
			t.Errorf("len(adjList)=%d != len(b.Transactions)=%d", len(adjList), len(b.Transactions))
		}
		if len(adjList) != len(inDegrees) {
			t.Errorf("len(adjList)=%d != len(inDegrees)=%d", len(adjList), len(inDegrees))
		}
		for node, inDegree := range inDegrees {
			if inDegree < 0 {
				t.Errorf("node=%d had negative inDegree=%d", node, inDegree)
			}
		}

		for u, vs := range adjList {
			for _, v := range vs {
				if int(v) < u {
					t.Errorf("a tx v=%d later in the block depended on a tx u=%d earlier in the block", v, u)
				}
			}
			vs0 := unwrap(vs)
			if !sort.IntsAreSorted(vs0) {
				t.Errorf("neighbors list wasn't sorted")
			}
			uncompactedVs := make([]int, len(vs0))
			copy(uncompactedVs, vs0)
			if len(uncompactedVs) != len(slices.Compact(vs0)) {
				t.Errorf("node=%d neighbors list=%+v had duplicates %v", u, uncompactedVs, slices.Compact(vs0))
			}
		}
	})
}
