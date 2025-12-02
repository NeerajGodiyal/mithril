package snapshotdl

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/solana-snapshot-finder-go/pkg/config"
	"github.com/Overclock-Validator/solana-snapshot-finder-go/pkg/rpc"
	"github.com/Overclock-Validator/solana-snapshot-finder-go/pkg/snapshot"
)

func DownloadSnapshot(endpoint string, path string) (string, int, int, error) {
	cfg := config.Config{RPCAddress: endpoint, SnapshotPath: path, NumOfRetries: 5,
		MinDownloadSpeed: 100, MaxLatency: 10, WorkerCount: 100, FullThreshold: 100000}

	referenceSlot, err := rpc.GetReferenceSlot(cfg.RPCAddress)
	if err != nil {
		return "", 0, 0, fmt.Errorf("error getting reference slot: %s", err)
	}

	nodes := rpc.FetchRPCNodes(cfg)
	if len(nodes) == 0 {
		return "", 0, 0, fmt.Errorf("no rpc nodes available")
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

	snapshotSlot := extractFullSnapshotSlot(finalPath)

	return finalPath, referenceSlot, snapshotSlot, nil
}

func DownloadIncrementalSnapshot(endpoint string, path string, referenceSlot int, fullSnapshotSlot int) (string, int, int, error) {
	cfg := config.Config{RPCAddress: endpoint, SnapshotPath: path, NumOfRetries: 5,
		MinDownloadSpeed: 100, MaxLatency: 10, WorkerCount: 100, FullThreshold: 100000}

	var err error
	nodes := rpc.FetchRPCNodes(cfg)
	if len(nodes) == 0 {
		return "", 0, 0, fmt.Errorf("no rpc nodes available")
	}

	results := rpc.EvaluateNodesWithVersions(nodes, cfg, referenceSlot)
	bestRPCs := rpc.SortBestRPCs(results)

	var finalPath string
	var incrSlotNum, slotNum int
	for _, rpc := range bestRPCs {
		finalPath, err = snapshot.DownloadSnapshot(rpc, cfg, "incremental", referenceSlot)
		if err == nil {
			slotNum, incrSlotNum = ExtractIncrementalSnapshotSlots(finalPath)
			if slotNum == fullSnapshotSlot {
				break
			} else {
				mlog.Log.Infof("downloaded incremental snapshot for slot %d, need %d. trying another download source.", slotNum, fullSnapshotSlot)
			}
		} else {
			fmt.Printf("failed to download snapshot from %s. trying next.\n", rpc)
		}
	}

	return finalPath, slotNum, incrSlotNum, nil
}

// expected format of incr snapshot fn: snapshot-362871357-2m6Qctcrk54WooSWdHdu8Wjz49vzp6hpT5gXTJJUgT2C.tar.zst
func extractFullSnapshotSlot(path string) int {
	parts := strings.Split(path, "/")
	filename := parts[len(parts)-1]

	parts = strings.Split(filename, "-")
	if len(parts) < 3 {
		panic(fmt.Sprintf("invalid full snapshot filename format: %s", path))
	}

	slotStr := parts[1]
	slot, err := strconv.Atoi(slotStr)
	if err != nil {
		panic(err)
	}

	return slot
}

// expected format of incr snapshot fn: incremental-snapshot-361569776-XXXX-....
func ExtractIncrementalSnapshotSlots(path string) (int, int) {
	parts := strings.Split(path, "/")
	filename := parts[len(parts)-1]

	parts = strings.Split(filename, "-")
	if len(parts) < 4 {
		panic(fmt.Sprintf("invalid incremental snapshot filename format: %s", path))
	}

	slotStr := parts[2]
	slot, err := strconv.Atoi(slotStr)
	if err != nil {
		panic(err)
	}

	incrSlotStr := parts[3]
	incrSlot, err := strconv.Atoi(incrSlotStr)
	if err != nil {
		panic(err)
	}
	return slot, incrSlot
}
