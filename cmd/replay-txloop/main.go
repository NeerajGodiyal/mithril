package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/replay"
)

func main() {
	parallelism := flag.Int("parallelism", 0, "Number of parallel workers (0 = sequential)")
	flag.Parse()

	if flag.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags] <snapshot.txloop.json>\n", os.Args[0])
		flag.PrintDefaults()
		os.Exit(1)
	}

	snapshotPath := flag.Arg(0)

	if _, err := os.Stat(snapshotPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: snapshot file not found: %s\n", snapshotPath)
		os.Exit(1)
	}

	fmt.Printf("Loading TxLoop snapshot from %s\n", snapshotPath)

	result, err := replay.ReplayTxLoop(snapshotPath, *parallelism)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error replaying TxLoop: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\n=== TxLoop Replay Results ===\n")
	fmt.Printf("Transactions: %d\n", result.TxCount)
	fmt.Printf("Duration:     %v\n", result.Duration)
	if result.TxCount > 0 {
		fmt.Printf("Avg per tx:   %v\n", result.Duration/time.Duration(result.TxCount))
	}
	fmt.Printf("Total fees:   %d lamports\n", result.TxFeeAccumulator.TotalFees)

	// Report divergences
	hasDivergences := len(result.AccountDivergences) > 0 || len(result.MissingAccounts) > 0 || len(result.ExtraAccounts) > 0

	if hasDivergences {
		fmt.Printf("\n=== DIVERGENCES DETECTED ===\n")

		if len(result.MissingAccounts) > 0 {
			fmt.Printf("\nAccounts expected but not modified (%d):\n", len(result.MissingAccounts))
			for _, pk := range result.MissingAccounts {
				fmt.Printf("  - %s\n", pk)
			}
		}

		if len(result.ExtraAccounts) > 0 {
			fmt.Printf("\nAccounts modified but not expected (%d):\n", len(result.ExtraAccounts))
			for _, pk := range result.ExtraAccounts {
				fmt.Printf("  - %s\n", pk)
			}
		}

		if len(result.AccountDivergences) > 0 {
			fmt.Printf("\nAccount state divergences (%d):\n", len(result.AccountDivergences))
			for _, div := range result.AccountDivergences {
				fmt.Printf("  - %s.%s: expected=%s, actual=%s\n",
					div.Pubkey, div.Field, div.ExpectedValue, div.ActualValue)
			}
		}

		os.Exit(1)
	} else {
		fmt.Printf("\n✓ No divergences detected\n")
	}
}
