package snapshot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/progress"
	"github.com/Overclock-Validator/mithril/pkg/rpcclient"
	"github.com/Overclock-Validator/mithril/pkg/snapshotdl"
	"github.com/panjf2000/ants/v2"
)

// fmtDuration formats a duration to 3 decimal places in the most appropriate unit
func fmtDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%.3fms", float64(d.Microseconds())/1000)
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	minutes := int(d.Minutes())
	seconds := int(d.Seconds()) % 60
	return fmt.Sprintf("%dm%02ds", minutes, seconds)
}

func validateIncrementalSnapshotPreflight(
	ctx context.Context,
	fullSnapshotFile string,
	referenceSlot int,
	fullSnapshotSlot uint64,
	fullManifest *SnapshotManifest,
	snapCfg snapshotdl.SnapshotConfig,
) error {
	if fresh, err := snapshotdl.GetReferenceSlotContext(ctx, snapCfg); err == nil && fresh > referenceSlot {
		referenceSlot = fresh
	} else if err != nil && (referenceSlot <= 0 || uint64(referenceSlot) <= fullSnapshotSlot) {
		return fmt.Errorf("refresh incremental freshness reference: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	source, locatedBase, locatedEnd, err := snapshotdl.GetIncrementalSnapshotURLContext(
		ctx, fullSnapshotFile, referenceSlot, int(fullSnapshotSlot), snapCfg,
	)
	if err != nil {
		return err
	}
	base, end, err := selectedIncrementalSnapshotSlots(source)
	if err != nil {
		return err
	}
	if locatedBase != int(fullSnapshotSlot) || base != fullSnapshotSlot || locatedEnd != int(end) {
		return fmt.Errorf("selected incremental slots %d/%d do not match full snapshot slot %d", base, end, fullSnapshotSlot)
	}
	manifest, _, err := readManifestFromSnapshot(ctx, source)
	if err != nil {
		return err
	}
	if manifest == nil || manifest.Bank == nil || manifest.AccountsDb == nil {
		return fmt.Errorf("incremental snapshot manifest is missing required fields")
	}
	if manifest.LtHash == nil {
		return fmt.Errorf("incremental snapshot manifest has no AccountsLtHash identity")
	}
	if manifest.Bank.Slot != end {
		return fmt.Errorf("incremental snapshot manifest root %d does not match selected slot %d", manifest.Bank.Slot, end)
	}
	if err := validateSnapshotArchiveHash(source, manifest); err != nil {
		return err
	}
	return validateIncrementalManifestBase(fullManifest, manifest)
}

// BuildAccountsDbAuto builds the accounts database from full + incremental snapshots.
func BuildAccountsDbAuto(
	ctx context.Context,
	fullSnapshotFile string,
	snapshotDownloadPath string,
	fullSnapshotSlot int,
	referenceSlot int,
	accountsDbDir string,
	rpcEndpoints []string,
	blockDir string,
	snapCfg snapshotdl.SnapshotConfig,
	dp *progress.DualProgress,
) (*accountsdb.AccountsDb, *SnapshotManifest, error) {
	selectedFullSlot, err := selectedFullSnapshotSlot(fullSnapshotFile)
	if err != nil {
		return nil, nil, err
	}
	if fullSnapshotSlot >= 0 && selectedFullSlot != uint64(fullSnapshotSlot) {
		return nil, nil, fmt.Errorf("full snapshot filename slot %d does not match selected slot %d", selectedFullSlot, fullSnapshotSlot)
	}

	mlog.Log.Infof("Parsing full snapshot manifest...")
	manifest, _, err := readManifestFromSnapshot(ctx, fullSnapshotFile)
	if err != nil {
		return nil, nil, fmt.Errorf("reading snapshot manifest: %v", err)
	}
	if manifest == nil || manifest.Bank == nil || manifest.AccountsDb == nil {
		return nil, nil, fmt.Errorf("full snapshot manifest is missing required fields")
	}
	if manifest.Bank.Slot != selectedFullSlot {
		return nil, nil, fmt.Errorf("full snapshot manifest root %d does not match selected slot %d", manifest.Bank.Slot, selectedFullSlot)
	}
	if err := validateSnapshotArchiveHash(fullSnapshotFile, manifest); err != nil {
		return nil, nil, err
	}
	mlog.Log.Infof("Parsed full snapshot manifest")
	if OnFullSnapshotManifestParsed != nil {
		OnFullSnapshotManifestParsed(manifest)
	}
	mlog.Log.Infof("Validating an incremental snapshot matching full slot %d...", selectedFullSlot)
	if err := validateIncrementalSnapshotPreflight(
		ctx, fullSnapshotFile, referenceSlot, selectedFullSlot, manifest, snapCfg,
	); err != nil {
		return nil, nil, fmt.Errorf("validate matching incremental snapshot: %w", err)
	}

	// A matching pair exists. The existing retry path below reselects the
	// freshest incremental after the full archive finishes extracting.
	if err := CleanAccountsDbDir(accountsDbDir); err != nil {
		return nil, nil, err
	}

	start := time.Now()

	appendVecsOutputDir := filepath.Join(accountsDbDir, "accounts")
	if err = os.MkdirAll(appendVecsOutputDir, 0775); err != nil {
		return nil, nil, err
	}
	if err = syncSnapshotDirectory(accountsDbDir); err != nil {
		return nil, nil, err
	}
	logSnapshotBootstrapTuning()

	defer ants.Release()

	incrementalManifest := &SnapshotManifest{}
	var largestFileId atomic.Uint64
	wg := &sync.WaitGroup{}

	numShards := snapshotIndexShards()
	logsDir, cleanupIndexWorkDir, err := prepareSnapshotIndexWorkDir(accountsDbDir)
	if err != nil {
		return nil, nil, err
	}
	defer cleanupIndexWorkDir()
	sl, err := NewShardLoggerWithError(numShards, logsDir)
	if err != nil {
		return nil, nil, fmt.Errorf("create shard logger: %w", err)
	}
	defer sl.Abort()

	// Create stake pubkey collector for building stake index during appendvec processing
	stakeCollector := &stakeIndexCollector{
		entries: make([]accountsdb.StakeIndexEntry, 0, 1000000), // Pre-allocate for ~1M stake accounts
	}

	pools, err := initWorkerPools(wg, sl, manifest, incrementalManifest, accountsDbDir, &largestFileId, stakeCollector)
	if err != nil {
		return nil, nil, fmt.Errorf("initializing worker pools: %w", err)
	}
	defer pools.Release()

	// Determine save path for full snapshot if streaming from HTTP
	var fullSavePath string
	if snapCfg.MaxFullSnapshots > 0 && (strings.HasPrefix(fullSnapshotFile, "http://") || strings.HasPrefix(fullSnapshotFile, "https://")) {
		if snapshotDownloadPath != "" {
			// Ensure snapshot download directory exists
			if err := os.MkdirAll(snapshotDownloadPath, 0o755); err != nil {
				return nil, nil, fmt.Errorf("failed to create snapshot download directory %s: %w", snapshotDownloadPath, err)
			}
			// Extract filename from URL and create save path
			filename, err := snapshotArchiveFilename(fullSnapshotFile)
			if err != nil {
				return nil, nil, err
			}
			fullSavePath = filepath.Join(snapshotDownloadPath, filename)
			mlog.Log.Infof("Will save full snapshot to %s while streaming", fullSavePath)
		}
	}

	// Start progress display if provided
	if dp != nil {
		// Flush mlog's buffered writer AND OS buffers before starting progress bars
		// This prevents late-flushing logs from breaking cursor positioning
		mlog.Flush()
		os.Stdout.Sync()
		os.Stderr.Sync()
		dp.Start()
	}

	err = readTar(ctx, wg, fullSnapshotFile, pools, readTarOptions{
		savePath:        fullSavePath,
		progress:        dp,
		statusCachePath: retainedStatusCachePath(accountsDbDir),
		statusCacheRoot: &manifest.Bank.Slot,
	})
	// A successful archive read is not a successful build until all three
	// worker stages have drained without error.
	workerErr := waitForSnapshotWorkers(wg, pools)
	if err == nil {
		err = workerErr
	}
	if err != nil {
		if dp != nil {
			dp.Interrupt(err)
		}
		return nil, nil, fmt.Errorf("processing full snapshot: %w", err)
	}

	// Stop progress display after full snapshot is processed
	if dp != nil {
		dp.Stop()
	}

	// Log full snapshot processing time to debug log only (noise reduction)
	mlog.Log.Debugf("done processing full snapshot in %s.", fmtDuration(time.Since(start)))

	// Note: ShardLogger is NOT closed here - we use one logger for both phases
	// and flush once at the end. This avoids the bug where pools still reference
	// the old ShardLogger after reinit.

	// Refresh the freshness reference to the CURRENT chain tip before picking
	// an incremental: the tip advanced during the full download/build, and the
	// callers' initial reference may even be the full slot itself — against
	// which any incremental looks fresh and the staleness gate never fires
	// (the bug that bootstrapped 81k slots behind on the Alpenglow cluster).
	if fresh, rerr := snapshotdl.GetReferenceSlotContext(ctx, snapCfg); rerr == nil && fresh > referenceSlot {
		if referenceSlot > 0 && fresh > referenceSlot+1000 {
			mlog.Log.Infof("refreshed incremental freshness reference: %d -> %d (tip advanced during full snapshot build)", referenceSlot, fresh)
		}
		referenceSlot = fresh
	} else if rerr != nil {
		mlog.Log.Warnf("could not refresh reference slot for incremental freshness gating: %v (using %d)", rerr, referenceSlot)
	}

	// Get incremental snapshot URL (tries same source first, then searches if needed)
	mlog.Log.Infof("finding incremental snapshot matching full slot %d...", fullSnapshotSlot)
	incrSnapshotDlStart := time.Now()
	incrementalSnapshotPath, _, incrSlot, err := snapshotdl.GetIncrementalSnapshotURLContext(ctx, fullSnapshotFile, referenceSlot, fullSnapshotSlot, snapCfg)
	if err != nil {
		// Return error instead of fatal exit so caller can handle gracefully
		errMsg := fmt.Sprintf("failed to find incremental snapshot: %v", err)
		if strings.Contains(err.Error(), "threshold") {
			errMsg += fmt.Sprintf("\n  Hint: every available incremental is staler than snapshot.incremental_threshold (current: %d slots)."+
				"\n  Either raise the threshold to accept a staler incremental — and pair it with a raised"+
				"\n  block.repair_catchup_max_gap_slots so turbine repair fills the remaining gap after bootstrap —"+
				"\n  or wait for the cluster to publish a fresher incremental.", snapCfg.IncrementalThreshold)
		} else if strings.Contains(err.Error(), "no rpc nodes") || strings.Contains(err.Error(), "no nodes found") {
			errMsg += "\n  Hint: Check RPC endpoints connectivity or try again later"
		}
		return nil, nil, fmt.Errorf("%s", errMsg)
	}
	mlog.Log.Debugf("found incremental snapshot URL in %s: %s", fmtDuration(time.Since(incrSnapshotDlStart)), incrementalSnapshotPath)

	// Retry loop for incremental snapshot download
	// If download fails mid-way (not context cancellation), re-discover sources and retry
	maxIncrRetries := 3
	var selected validatedSnapshotSelection
	var incrementalSelectionErr error
	for incrAttempt := range maxIncrRetries {
		if ctx.Err() != nil {
			return nil, nil, fmt.Errorf("attempting to download incremental snapshot: %w", ctx.Err())
		}
		if incrAttempt > 0 {
			// Re-discover incremental snapshot URL (sources may have changed)
			mlog.Log.Infof("Incremental download failed, re-discovering sources (attempt %d/%d)...", incrAttempt+1, maxIncrRetries)
			incrementalSnapshotPath, _, incrSlot, err = snapshotdl.GetIncrementalSnapshotURLContext(ctx, fullSnapshotFile, referenceSlot, fullSnapshotSlot, snapCfg)
			if err != nil {
				mlog.Log.Errorf("Failed to re-discover incremental snapshot: %v", err)
				continue
			}
			mlog.Log.Infof("Found new incremental snapshot URL: %s (slot %d)", incrementalSnapshotPath, incrSlot)
		}

		mlog.Log.FileOnlyf("Parsing incremental snapshot manifest...")
		incrementalBase, selectedIncrementalSlot, parseErr := selectedIncrementalSnapshotSlots(incrementalSnapshotPath)
		if parseErr != nil {
			incrementalSelectionErr = parseErr
			mlog.Log.Errorf("invalid incremental snapshot selection: %v", parseErr)
			continue
		}
		if incrementalBase != selectedFullSlot || selectedIncrementalSlot != uint64(incrSlot) {
			incrementalSelectionErr = fmt.Errorf("incremental snapshot slots %d/%d do not match selected slots %d/%d",
				incrementalBase, selectedIncrementalSlot, selectedFullSlot, incrSlot)
			mlog.Log.Errorf("%v", incrementalSelectionErr)
			continue
		}
		incrementalManifestCopy, manifestBytes, err := readManifestFromSnapshot(ctx, incrementalSnapshotPath)
		if err != nil {
			incrementalSelectionErr = err
			mlog.Log.Errorf("reading incremental snapshot manifest: %v", err)
			continue
		}
		if incrementalManifestCopy == nil || incrementalManifestCopy.Bank == nil || incrementalManifestCopy.AccountsDb == nil {
			incrementalSelectionErr = fmt.Errorf("incremental snapshot manifest is missing required fields")
			mlog.Log.Errorf("%v", incrementalSelectionErr)
			continue
		}
		if incrementalManifestCopy.LtHash == nil {
			incrementalSelectionErr = fmt.Errorf("incremental snapshot manifest has no AccountsLtHash identity")
			mlog.Log.Errorf("%v", incrementalSelectionErr)
			continue
		}
		if incrementalManifestCopy.Bank.Slot != selectedIncrementalSlot {
			incrementalSelectionErr = fmt.Errorf("incremental snapshot manifest root %d does not match selected slot %d",
				incrementalManifestCopy.Bank.Slot, selectedIncrementalSlot)
			mlog.Log.Errorf("%v", incrementalSelectionErr)
			continue
		}
		if err := validateSnapshotArchiveHash(incrementalSnapshotPath, incrementalManifestCopy); err != nil {
			incrementalSelectionErr = err
			mlog.Log.Errorf("invalid incremental snapshot archive: %v", err)
			continue
		}
		if err := validateIncrementalManifestBase(manifest, incrementalManifestCopy); err != nil {
			incrementalSelectionErr = err
			mlog.Log.Errorf("invalid incremental snapshot base: %v", err)
			continue
		}
		selection := validatedSnapshotSelection{manifest: incrementalManifestCopy, bytes: manifestBytes, hasArchiveHash: true}
		selection.archiveHash, err = snapshotArchiveHash(incrementalSnapshotPath)
		if err != nil {
			incrementalSelectionErr = err
			mlog.Log.Errorf("invalid incremental snapshot hash: %v", err)
			continue
		}
		// Copy the manifest so the worker pool's pointer has the value.
		*incrementalManifest = *incrementalManifestCopy
		mlog.Log.FileOnlyf("Parsed incremental snapshot manifest")
		if OnIncrementalManifestParsed != nil {
			OnIncrementalManifestParsed(incrementalManifestCopy)
		}

		// Determine save path for incremental snapshot if streaming from HTTP
		var incrSavePath string
		if snapCfg.MaxFullSnapshots > 0 && (strings.HasPrefix(incrementalSnapshotPath, "http://") || strings.HasPrefix(incrementalSnapshotPath, "https://")) {
			if snapshotDownloadPath != "" {
				// Ensure snapshot download directory exists (may not exist if full was local)
				if err := os.MkdirAll(snapshotDownloadPath, 0o755); err != nil {
					return nil, nil, fmt.Errorf("failed to create snapshot download directory %s: %w", snapshotDownloadPath, err)
				}
				// Extract filename from URL and create save path
				filename, err := snapshotArchiveFilename(incrementalSnapshotPath)
				if err != nil {
					return nil, nil, err
				}
				incrSavePath = filepath.Join(snapshotDownloadPath, filename)
				mlog.Log.FileOnlyf("Will save incremental snapshot to %s while streaming", incrSavePath)
			}
		}

		err = readTar(ctx, wg, incrementalSnapshotPath, pools, readTarOptions{
			savePath:        incrSavePath,
			isIncremental:   true,
			statusCachePath: retainedStatusCachePath(accountsDbDir),
			statusCacheRoot: &incrementalManifestCopy.Bank.Slot,
		})
		workerErr = waitForSnapshotWorkers(wg, pools)
		if workerErr != nil {
			return nil, nil, fmt.Errorf("processing incremental snapshot workers: %w", workerErr)
		}
		// Check if we should retry
		if err == nil {
			selected = selection
			incrementalSelectionErr = nil
			break // Success
		}
		incrementalSelectionErr = err
		// Download failed mid-way, will retry with re-discovery
		mlog.Log.Errorf("Incremental download failed: %v", err)
	}
	if selected.manifest == nil {
		if incrementalSelectionErr == nil {
			incrementalSelectionErr = fmt.Errorf("no incremental snapshot passed validation")
		}
		return nil, nil, incrementalSelectionErr
	}

	// Show indexing progress for shard flush
	indexProgress := progress.NewIndexingProgress("Flush (shard logs)")
	indexProgress.Start(numShards)
	if err := sl.CloseWithProgress(ctx, func(completed, total int) {
		indexProgress.Update(completed, total)
	}); err != nil {
		return nil, nil, fmt.Errorf("closing shard logger: %w", err)
	}
	indexDir := filepath.Join(accountsDbDir, "mithril_db")
	index, err := ingestSSTFiles(indexDir, logsDir)
	if err != nil {
		return nil, nil, fmt.Errorf("initializing pebble from SST files: %w", err)
	}
	if err := index.Close(); err != nil {
		return nil, nil, fmt.Errorf("close snapshot account index: %w", err)
	}

	accountsDb, err := finalizeSnapshotBootstrap(
		ctx, accountsDbDir, largestFileId.Load(), selected, stakeCollector.entries,
	)
	if err != nil {
		return nil, nil, err
	}

	rpcClient := rpcclient.NewRpcClient(rpcEndpoints[0])
	latestSlot, err := rpcClient.GetSlot()
	_, incrSlot = snapshotdl.ExtractIncrementalSnapshotSlots(incrementalSnapshotPath)

	if err != nil || latestSlot == 0 {
		mlog.Log.Infof("Node currently at slot %d (unable to fetch chain tip)", incrSlot)
	} else if latestSlot > uint64(incrSlot) {
		mlog.Log.Infof("Node currently at slot %d, chain tip at slot %d (%d slots behind)", incrSlot, latestSlot, latestSlot-uint64(incrSlot))
	} else {
		mlog.Log.Infof("Node currently at slot %d, chain tip at slot %d", incrSlot, latestSlot)
	}

	return accountsDb, incrementalManifest, nil
}
