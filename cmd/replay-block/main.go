package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/replay"
)

func main() {
	inputDir := flag.String("input", "", "Input directory containing accounts TAR and block JSON")
	parallelism := flag.Int("parallelism", 0, "Number of parallel workers (0 = sequential)")
	slot := flag.Uint64("slot", 0, "Slot number (used to find files if not specified)")
	flag.Parse()

	if *inputDir == "" {
		fmt.Fprintf(os.Stderr, "Usage: %s -input <dir> [-parallelism <n>] [-slot <slot>]\n", os.Args[0])
		flag.PrintDefaults()
		os.Exit(1)
	}

	// Find the TAR and block files
	tarPath, blockPath, detectedSlot, err := findSnapshotFiles(*inputDir, *slot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error finding snapshot files: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Loading block from %s...\n", blockPath)
	blk, err := loadBlock(blockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load block: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Block %d has %d transactions\n", detectedSlot, len(blk.Transactions))

	// Create temp directory for AccountsDb
	tmpDir, err := os.MkdirTemp("", fmt.Sprintf("replay-block-%d-", detectedSlot))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create temp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)

	fmt.Printf("Importing accounts from %s...\n", tarPath)
	db, err := accountsdb.ImportAccountsFromTar(tarPath, tmpDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to import accounts: %v\n", err)
		os.Exit(1)
	}
	defer db.CloseDb()

	// Run replay
	fmt.Printf("Replaying block %d with parallelism=%d...\n", detectedSlot, *parallelism)
	start := time.Now()

	result, err := replay.ReplayBlock(db, blk, *parallelism)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Replay failed: %v\n", err)
		os.Exit(1)
	}

	duration := time.Since(start)

	fmt.Printf("\n=== Replay Results ===\n")
	fmt.Printf("Slot:         %d\n", detectedSlot)
	fmt.Printf("Transactions: %d\n", len(blk.Transactions))
	fmt.Printf("Duration:     %v\n", duration)
	if len(blk.Transactions) > 0 {
		fmt.Printf("Avg per tx:   %v\n", duration/time.Duration(len(blk.Transactions)))
	}
	fmt.Printf("Total fees:   %d lamports\n", result.TotalFees)

	if result.Divergences > 0 {
		fmt.Printf("\n⚠ %d divergences detected\n", result.Divergences)
		os.Exit(1)
	} else {
		fmt.Printf("\n✓ No divergences detected\n")
	}
}

func findSnapshotFiles(inputDir string, slot uint64) (tarPath, blockPath string, detectedSlot uint64, err error) {
	if slot != 0 {
		tarPath = filepath.Join(inputDir, fmt.Sprintf("accounts-%d.tar.gz", slot))
		blockPath = filepath.Join(inputDir, fmt.Sprintf("block-%d.json", slot))
		detectedSlot = slot

		if _, err := os.Stat(tarPath); os.IsNotExist(err) {
			return "", "", 0, fmt.Errorf("accounts TAR not found: %s", tarPath)
		}
		if _, err := os.Stat(blockPath); os.IsNotExist(err) {
			return "", "", 0, fmt.Errorf("block JSON not found: %s", blockPath)
		}
		return tarPath, blockPath, detectedSlot, nil
	}

	// Try to find files by pattern
	entries, err := os.ReadDir(inputDir)
	if err != nil {
		return "", "", 0, fmt.Errorf("failed to read input directory: %w", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		var s uint64
		if n, _ := fmt.Sscanf(name, "accounts-%d.tar.gz", &s); n == 1 {
			tarPath = filepath.Join(inputDir, name)
			blockPath = filepath.Join(inputDir, fmt.Sprintf("block-%d.json", s))
			detectedSlot = s

			if _, err := os.Stat(blockPath); err == nil {
				return tarPath, blockPath, detectedSlot, nil
			}
		}
	}

	return "", "", 0, fmt.Errorf("no snapshot files found in %s", inputDir)
}

func loadBlock(path string) (*block.Block, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var blk block.Block
	if err := json.NewDecoder(file).Decode(&blk); err != nil {
		return nil, err
	}
	return &blk, nil
}
