package replay

import (
	"fmt"
	//"time"

	//"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/gagliardetto/solana-go"
)

// Type wrappers around indices.
type acct int
type tx int

func mustAccountMetaList(t *solana.Transaction) []*solana.AccountMeta {
	if t.Message.IsVersioned() && !t.Message.IsResolved() {
		panic(fmt.Sprintf("topsort planner: assumes versioned (IsVersioned was %t) txs are resolved (IsResolved was %t)", t.Message.IsVersioned(), t.Message.IsResolved()))
	}
	ams, err := t.AccountMetaList()
	if err != nil {
		panic(fmt.Sprintf("topsort planner: AccountMetaList failed: %v", err))
	}
	return ams
}

func blockToDependencyGraph(b *block.Block) (adjacencyList [][]tx, inDegree []int) {
	//start := time.Now()
	// Map between pubkeys and account indices
	var acctToPk []solana.PublicKey
	pkToAcct := make(map[solana.PublicKey]acct)

	for i := range b.Transactions {
		tx := b.Transactions[i]
		ams := mustAccountMetaList(tx)
		for _, am := range ams {
			if _, exists := pkToAcct[am.PublicKey]; !exists {
				pkToAcct[am.PublicKey] = acct(len(acctToPk))
				acctToPk = append(acctToPk, am.PublicKey)
			}
		}
	}

	acctToReaderTxs := make(map[acct][]tx, len(acctToPk))
	acctToWriterTxs := make(map[acct][]tx, len(acctToPk))
	adjacencyList = make([][]tx, len(b.TxMetas))
	inDegree = make([]int, len(b.TxMetas))
	for txIdx := range b.Transactions {
		/* Given S < T (S occurs before T) in a sequential execution, we
		   use these restrictions on the parallel execution to reproduce
		   the sequential execution

		   S      | T      | parallel ordering required
		   reads  | reads  | any order
		   writes | reads  | S < T
		   reads  | writes | S < T
		   writes | writes | S < T
		*/
		t := tx(txIdx)

		//txSig := b.Transactions[txIdx].Signatures[0]
		////mlog.Log.Debugf("printing input accounts for txIdx=%d txSig=%s", txIdx, txSig)
		txn := b.Transactions[txIdx]
		var readonlyAccts, writableAccts []solana.PublicKey
		{
			ams := mustAccountMetaList(txn)
			var rs, ws int
			for _, am := range ams {
				if am.IsWritable {
					ws++
				} else {
					rs++
				}
			}
			readonlyAccts, writableAccts = make([]solana.PublicKey, 0, rs), make([]solana.PublicKey, 0, ws)
			for _, am := range ams {
				if am.IsWritable {
					writableAccts = append(writableAccts, am.PublicKey)
				} else {
					readonlyAccts = append(readonlyAccts, am.PublicKey)
				}
			}
		}

		for _, roAcct := range readonlyAccts {
			////mlog.Log.Debugf("- roAcct=%s", roAcct.String())
			acct, exists := pkToAcct[roAcct]
			if !exists {
				panic(fmt.Sprintf("invariant error: did not record account index for pk=%s in previous loop?", roAcct.String()))
			}

			// Add an edge for S writes < T reads
			for _, s := range acctToWriterTxs[acct] {
				if s >= t {
					break
				}
				if len(adjacencyList[int(s)]) > 0 && adjacencyList[int(s)][len(adjacencyList[int(s)])-1] == t {
					continue
				}
				adjacencyList[int(s)] = append(adjacencyList[int(s)], t)
				inDegree[int(t)]++
			}
			// Add T as a reader of this account.
			acctToReaderTxs[acct] = append(acctToReaderTxs[acct], t)
		}

		for _, writeAcct := range writableAccts {
			////mlog.Log.Debugf("- writeAcct=%s", writeAcct.String())
			acct, exists := pkToAcct[writeAcct]
			if !exists {
				panic(fmt.Sprintf("invariant error: expected pkToAcct to contain all public keys of accounts used in block; missing public key=%s", writeAcct.String()))
			}
			// Add an edge for S reads < T writes
			for _, s := range acctToReaderTxs[acct] {
				if s >= t {
					break
				}
				if len(adjacencyList[int(s)]) > 0 && adjacencyList[int(s)][len(adjacencyList[int(s)])-1] == t {
					continue
				}
				adjacencyList[int(s)] = append(adjacencyList[int(s)], t)
				inDegree[int(t)]++
			}
			// Add an edge for S writes < T writes
			for _, s := range acctToWriterTxs[acct] {
				if s >= t {
					break
				}
				if len(adjacencyList[int(s)]) > 0 && adjacencyList[int(s)][len(adjacencyList[int(s)])-1] == t {
					continue
				}
				adjacencyList[int(s)] = append(adjacencyList[int(s)], t)
				inDegree[int(t)]++
			}
			// Add T as a writer of this account.
			acctToWriterTxs[acct] = append(acctToWriterTxs[acct], t)
		}
	}
	return adjacencyList, inDegree
}

// TopsortPlanner returns a list of list of ints.
// The ints are indices into the b.Transactions slices.
// Each list of indices do not have write-after-write or read-after-write conflicts.
func TopsortPlanner(b *block.Block) [][]int {
	adjList, inDegree := blockToDependencyGraph(b)

	// Output a topological sorting of the transactions
	topSorted := 0
	var topSortLevels [][]int
	var roots []int
	for t, deg := range inDegree {
		if deg == 0 {
			roots = append(roots, t)
		}
	}
	for topSorted < len(b.TxMetas) {
		topSortLevels = append(topSortLevels, roots)
		topSorted += len(roots)
		// Remove roots from graph.
		var nextRoots []int
		for _, root := range roots {
			for _, dependentTx := range adjList[int(root)] {
				inDegree[int(dependentTx)]--
				if inDegree[int(dependentTx)] == 0 {
					nextRoots = append(nextRoots, int(dependentTx))
				}
			}
		}
		roots = nextRoots
	}

	//mlog.Log.Infof("planner finished in %s", time.Since(start))
	return topSortLevels
}

// TopsortPlanner outputs ints on out channel which have had their dependencies satisfied and can be run. On completion, return the int to the done channel.
func TopsortPlannerStream(b *block.Block, out chan int, done chan int) {
	adjList, inDegree := blockToDependencyGraph(b)

	sent := 0
	// Output a topological sorting of the transactions
	for t, deg := range inDegree {
		if deg == 0 {
			out <- t
			sent++
		}
	}

	for sent < len(b.Transactions) {
		in := <-done
		for _, dependentTx := range adjList[int(in)] {
			inDegree[int(dependentTx)]--
			if inDegree[int(dependentTx)] == 0 {
				out <- int(dependentTx)
				sent++
			}
		}
	}
	close(out)
}
