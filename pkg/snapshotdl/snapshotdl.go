package snapshotdl

import (
	"fmt"

	"github.com/Overclock-Validator/solana-snapshot-finder-go/pkg/config"
	"github.com/Overclock-Validator/solana-snapshot-finder-go/pkg/rpc"
	"github.com/Overclock-Validator/solana-snapshot-finder-go/pkg/snapshot"
)

func DownloadSnapshot(endpoint string, path string) (string, error) {
	cfg := config.Config{RPCAddress: endpoint, SnapshotPath: path, NumOfRetries: 5,
		MinDownloadSpeed: 100, MaxLatency: 10, WorkerCount: 100}

	referenceSlot, err := rpc.GetReferenceSlot(cfg.RPCAddress)
	if err != nil {
		return "", fmt.Errorf("error getting reference slot: %s", err)
	}

	nodes := rpc.FetchRPCNodes(cfg)
	if len(nodes) == 0 {
		return "", fmt.Errorf("no rpc nodes available")
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

	return finalPath, nil
}
