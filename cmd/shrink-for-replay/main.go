package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/replay"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

func main() {
	accountsDbPath := flag.String("accountsdb", "", "Path to existing AccountsDb directory")
	rpcEndpoint := flag.String("rpc", "https://api.mainnet-beta.solana.com", "RPC endpoint for fetching blocks")
	outputDir := flag.String("output", "", "Output directory for snapshot files")
	flag.Parse()

	if *accountsDbPath == "" || *outputDir == "" {
		fmt.Fprintf(os.Stderr, "Usage: %s -accountsdb <path> -output <dir> [-rpc <endpoint>]\n", os.Args[0])
		flag.PrintDefaults()
		os.Exit(1)
	}

	// Create output directory
	if err := os.MkdirAll(*outputDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create output directory: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Opening AccountsDb at %s...\n", *accountsDbPath)
	db, err := accountsdb.OpenDb(*accountsDbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open AccountsDb: %v\n", err)
		os.Exit(1)
	}
	db.InitCaches()
	defer db.CloseDb()

	// Detect latest slot and fetch the next one
	latestSlot, err := db.GetLatestSlot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to get latest slot: %v\n", err)
		os.Exit(1)
	}
	if latestSlot == 0 {
		fmt.Fprintf(os.Stderr, "No slots found in AccountsDb\n")
		os.Exit(1)
	}
	targetSlot := latestSlot + 1
	fmt.Printf("Latest slot in AccountsDb: %d, fetching next slot: %d\n", latestSlot, targetSlot)

	fmt.Printf("Fetching block %d from RPC...\n", targetSlot)
	rpcClient := rpcclient.NewRpcClient(*rpcEndpoint)
	blockResult, err := rpcClient.GetBlockFinalized(targetSlot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to fetch block: %v\n", err)
		os.Exit(1)
	}

	// Convert to Block struct
	blk := block.FromBlockResult(blockResult, targetSlot, rpcClient)
	fmt.Printf("Block has %d transactions\n", len(blk.Transactions))

	// Get account dependencies
	fmt.Println("Scanning block for account dependencies...")
	deps := replay.GetBlockAccountDependenciesDetailed(blk)
	fmt.Printf("Found %d account dependencies\n", len(deps))

	// Collect pubkeys
	pubkeys := make([]solana.PublicKey, len(deps))
	programPubkeys := make([]solana.PublicKey, 0)
	for i, dep := range deps {
		pubkeys[i] = dep.Pubkey
		if dep.IsProgram {
			programPubkeys = append(programPubkeys, dep.Pubkey)
		}
	}

	// Fetch accounts to find ProgramData addresses (using latestSlot, the parent slot)
	fmt.Println("Fetching program accounts to resolve ProgramData dependencies...")
	programDataAddrs := resolveProgramDataAddresses(db, latestSlot, programPubkeys)
	fmt.Printf("Found %d ProgramData accounts\n", len(programDataAddrs))

	// Add ProgramData addresses to the list
	allPubkeys := append(pubkeys, programDataAddrs...)
	allPubkeys = deduplicatePubkeys(allPubkeys)
	fmt.Printf("Total accounts to export: %d\n", len(allPubkeys))

	// Export accounts to TAR at latestSlot (the parent slot, state before targetSlot)
	tarPath := filepath.Join(*outputDir, fmt.Sprintf("accounts-%d.tar.gz", targetSlot))
	fmt.Printf("Exporting accounts at slot %d to %s...\n", latestSlot, tarPath)
	if err := accountsdb.ExportAccountsToTar(db, latestSlot, allPubkeys, tarPath); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to export accounts: %v\n", err)
		os.Exit(1)
	}

	// Save block as JSON
	blockPath := filepath.Join(*outputDir, fmt.Sprintf("block-%d.json", targetSlot))
	fmt.Printf("Saving block to %s...\n", blockPath)
	if err := saveBlock(blockPath, blk); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to save block: %v\n", err)
		os.Exit(1)
	}

	// Also save the raw RPC block result for completeness
	rpcBlockPath := filepath.Join(*outputDir, fmt.Sprintf("block-%d-rpc.json", targetSlot))
	if err := saveRpcBlock(rpcBlockPath, blockResult); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to save RPC block: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("\nSnapshot created successfully!")
	fmt.Printf("  Accounts: %s\n", tarPath)
	fmt.Printf("  Block:    %s\n", blockPath)
	fmt.Printf("\nTo replay, run:\n")
	fmt.Printf("  replay-block -input %s\n", *outputDir)
}

func resolveProgramDataAddresses(db *accountsdb.AccountsDb, slot uint64, programPubkeys []solana.PublicKey) []solana.PublicKey {
	var programDataAddrs []solana.PublicKey

	for _, pk := range programPubkeys {
		acct, err := db.GetAccount(slot, pk)
		if err != nil {
			continue
		}

		// Check if this is a BPFLoaderUpgradeable program
		if !acct.Executable {
			continue
		}

		// Try to parse as UpgradeableLoaderState
		state, err := sealevel.UnmarshalUpgradeableLoaderState(acct.Data)
		if err != nil {
			continue
		}

		if state.Type == sealevel.UpgradeableLoaderStateTypeProgram {
			programDataAddrs = append(programDataAddrs, state.Program.ProgramDataAddress)
		}
	}

	return programDataAddrs
}

func deduplicatePubkeys(pubkeys []solana.PublicKey) []solana.PublicKey {
	seen := make(map[solana.PublicKey]bool)
	result := make([]solana.PublicKey, 0, len(pubkeys))
	for _, pk := range pubkeys {
		if !seen[pk] {
			seen[pk] = true
			result = append(result, pk)
		}
	}
	return result
}

func saveBlock(path string, blk *block.Block) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(blk)
}

func saveRpcBlock(path string, blockResult *rpc.GetBlockResult) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(blockResult)
}
