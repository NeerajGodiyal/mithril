package snapshotdl

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Overclock-Validator/solana-snapshot-finder-go/pkg/config"
	"github.com/Overclock-Validator/solana-snapshot-finder-go/pkg/rpc"
	"github.com/Overclock-Validator/solana-snapshot-finder-go/pkg/snapshot"
)

func DownloadSnapshot(endpoint string, path string) (string, int, error) {
	cfg := config.Config{RPCAddress: endpoint, SnapshotPath: path, NumOfRetries: 5,
		MinDownloadSpeed: 100, MaxLatency: 10, WorkerCount: 100}

	referenceSlot, err := rpc.GetReferenceSlot(cfg.RPCAddress)
	if err != nil {
		return "", 0, fmt.Errorf("error getting reference slot: %s", err)
	}

	nodes := rpc.FetchRPCNodes(cfg)
	if len(nodes) == 0 {
		return "", 0, fmt.Errorf("no rpc nodes available")
	}

	results := rpc.EvaluateNodesWithVersions(nodes, cfg, referenceSlot)
	bestRPCs := rpc.SortBestRPCs(results)

	var finalPath string
	for _, rpc := range bestRPCs {
		finalPath, err = snapshot.DownloadSnapshot(rpc, cfg, "snapshot-", referenceSlot)
		if err == nil {
			break
		} else {
			fmt.Printf("failed to download snapshot from %s. trying next.\n", rpc)
		}
	}

	return finalPath, referenceSlot, nil
}

func DownloadIncrementalSnapshot(endpoint string, path string, referenceSlot int) (string, int, error) {
	cfg := config.Config{RPCAddress: endpoint, SnapshotPath: path, NumOfRetries: 5,
		MinDownloadSpeed: 100, MaxLatency: 10, WorkerCount: 100}

	var err error
	nodes := rpc.FetchRPCNodes(cfg)
	if len(nodes) == 0 {
		return "", 0, fmt.Errorf("no rpc nodes available")
	}

	results := rpc.EvaluateNodesWithVersions(nodes, cfg, referenceSlot)
	bestRPCs := rpc.SortBestRPCs(results)

	var finalPath string
	for _, rpc := range bestRPCs {
		finalPath, err = snapshot.DownloadSnapshot(rpc, cfg, "incremental", referenceSlot)
		if err == nil {
			break
		} else {
			fmt.Printf("failed to download snapshot from %s. trying next.\n", rpc)
		}
	}

	finalPathParts := strings.Split(finalPath, "/")
	finalPathFn := finalPathParts[len(finalPathParts)-1]
	incrSlotNum := extractIncrementalSnapshotSlot(finalPathFn)

	return finalPath, incrSlotNum, nil
}

// expected format of incr snapshot fn: incremental-snapshot-361569776-XXXX-....
func extractIncrementalSnapshotSlot(path string) int {
	parts := strings.Split(path, "-")
	if len(parts) < 4 {
		panic(fmt.Sprintf("invalid incremental snapshot filename format: %s", path))
	}
	slotStr := parts[3]
	slot, err := strconv.Atoi(slotStr)
	if err != nil {
		panic(err)
	}
	return slot
}
