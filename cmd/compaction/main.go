package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/snapshot"
	"github.com/cockroachdb/pebble"
	bin "github.com/gagliardetto/binary"
)

type SlotFileID struct {
	Slot   uint64
	FileID uint64
}

// calculateFileWastedBytes reads an append vec file, parses all entries,
// and checks each one against the pebble DB to determine wasted bytes.
func calculateFileWastedBytes(db *pebble.DB, appendVecPath string, manifestSize uint64, sfid SlotFileID) (int, uint64, error) {
	missingPubkey := 0
	data, err := os.ReadFile(appendVecPath)
	if err != nil {
		return 0, 0, err
	}
	fileSize := uint64(len(data))

	pubkeys, entries, err := accountsdb.BuildIndexEntriesFromAppendVecs(data, manifestSize, sfid.Slot, sfid.FileID)
	if err != nil {
		return 0, 0, err
	}
	wastedBytes := 0

	for i, e := range entries {
		pubkey := pubkeys[i]
		offset := e.Offset

		accountSize := int(fileSize) - int(offset)
		if i < len(entries)-1 {
			accountSize = int(entries[i+1].Offset) - int(offset)
		}

		// Look up this pubkey in the pebble DB
		value, closer, err := db.Get(pubkey[:])
		if err == pebble.ErrNotFound {
			missingPubkey++
			wastedBytes += accountSize
			continue
		}
		if err != nil {
			closer.Close()
			return 0, 0, err
		}

		// Check if the DB entry points to this file/offset
		var dbEntry accountsdb.AccountIndexEntry
		if len(value) != 24 {
			closer.Close()
			return 0, 0, fmt.Errorf("unexpected value length %d", len(value))
		}
		var valueBytes [24]byte
		copy(valueBytes[:], value)
		dbEntry.Unmarshal(&valueBytes)
		closer.Close()

		// If the DB points to a different location, these bytes are wasted
		if dbEntry.Slot != sfid.Slot || dbEntry.FileId != sfid.FileID || dbEntry.Offset != offset {
			wastedBytes += accountSize
		}
	}

	return wastedBytes, fileSize, nil
}

// calculateTotalWastedBytes scans a directory of append vec files and calculates
// total wasted bytes using worker goroutines.
func calculateTotalWastedBytes(db *pebble.DB, appendVecDir string, m ManifestLookup, numWorkers int) (int, uint64, error) {
	// Find all append vec files in the directory
	entries, err := os.ReadDir(appendVecDir)
	if err != nil {
		return 0, 0, err
	}

	type workItem struct {
		path string
		sfid SlotFileID
	}
	workChan := make(chan workItem)

	// Atomic counters for results
	var totalWasted atomic.Int64
	var totalSize atomic.Uint64
	var processed atomic.Uint64
	var wg sync.WaitGroup

	for range numWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for item := range workChan {
				mSize, found := m.fileSizes[item.sfid]
				if !found {
					log.Printf("missing file size in manifest: %+v", item.sfid)
					continue
				}
				wasted, size, err := calculateFileWastedBytes(db, item.path, mSize, item.sfid)
				if err != nil {
					log.Printf("calculating waste in %s: %v", item.path, err)
					continue
				}

				totalWasted.Add(int64(wasted))
				totalSize.Add(size)

				p := processed.Add(1)
				if p%1000 == 0 {
					log.Printf(
						"Processed %dk files, waste %d (%.2f GB), total %d (%.2f GB)",
						p/1000,
						totalWasted.Load(),
						float64(totalWasted.Load())/(1<<30),
						totalSize.Load(),
						float64(totalSize.Load())/(1<<30),
					)
				}
			}
		}()
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		// Parse filename: SLOT.FILEID
		var slot, fileID uint64
		_, err := fmt.Sscanf(entry.Name(), "%d.%d", &slot, &fileID)
		if err != nil {
			// Skip files that don't match the pattern
			continue
		}

		sfid := SlotFileID{Slot: slot, FileID: fileID}
		path := filepath.Join(appendVecDir, entry.Name())
		workChan <- workItem{path, sfid}
	}
	close(workChan)

	wg.Wait()

	return int(totalWasted.Load()), totalSize.Load(), nil
}

// ManifestLookup provides fast lookups of append vec file sizes
type ManifestLookup struct {
	fileSizes map[SlotFileID]uint64
}

func NewManifestLookup(manifestPath string) (*ManifestLookup, error) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, err
	}

	manifest := &snapshot.SnapshotManifest{}
	if err := manifest.UnmarshalWithDecoder(bin.NewBinDecoder(data)); err != nil {
		return nil, err
	}

	lookup := &ManifestLookup{
		fileSizes: make(map[SlotFileID]uint64),
	}

	samples := 0
	for slot, appendVecs := range manifest.AccountsDb.Storages {
		for _, av := range appendVecs.AcctVecs {
			sfid := SlotFileID{Slot: slot, FileID: av.Id}
			lookup.fileSizes[sfid] = av.FileSize
			if samples < 10 {
				log.Printf("slot=%d fileid=%d", slot, av.Id)
				samples++
			}
		}
	}

	log.Printf("Loaded manifest with %d append vec entries", len(lookup.fileSizes))
	return lookup, nil
}

func main() {
	dbPath := flag.String("db", "", "Path to pebble DB directory")
	appendVecDir := flag.String("appendvecs", "", "Path to directory containing append vec files")
	appendVecFile := flag.String("appendvec", "", "Path to single append vec to inspect")
	manifestPath := flag.String("manifest", "", "Path to manifest file (optional, for accurate file sizes)")
	numWorkers := flag.Int("workers", 16, "Number of worker goroutines")
	flag.Parse()

	if *dbPath == "" || ((*appendVecDir == "") == (*appendVecFile == "")) {
		log.Fatal("Please provide -db and -appendvecs xor -appendvec")
	}

	// Open the database
	db, err := pebble.Open(*dbPath, &pebble.Options{})
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	m, err := NewManifestLookup(*manifestPath)
	if err != nil {
		log.Fatalf("manifest path=%s: %v", *manifestPath, err)
	}

	if *appendVecDir != "" {
		log.Printf("Calculating wasted bytes in %s...", *appendVecDir)
		totalWasted, totalSize, err := calculateTotalWastedBytes(db, *appendVecDir, *m, *numWorkers)
		if err != nil {
			log.Fatalf("Error calculating wasted bytes: %v", err)
		}

		wastePercent := float64(totalWasted) / float64(totalSize) * 100
		log.Printf("=== Results ===")
		log.Printf("Total file size: %d (%.2f GB)", totalSize, float64(totalSize)/(1<<30))
		log.Printf("Total wasted bytes: %d (%.2f GB)", totalWasted, float64(totalWasted)/(1<<30))
		log.Printf("Waste percentage: %.2f%%", wastePercent)
	} else if *appendVecFile != "" {
		log.Printf("Calculating wasted bytes in %s...", *appendVecFile)
		var slot, fileID uint64
		_, err = fmt.Sscanf(filepath.Base(*appendVecFile), "%d.%d", &slot, &fileID)
		if err != nil {
			log.Fatalf("parsing slot/file from %s: %v", *appendVecFile, err)
		}
		sfid := SlotFileID{slot, fileID}
		mSize, found := m.fileSizes[sfid]
		if !found {
			log.Fatalf("not found in manifest: %v", sfid)
		}
		log.Printf("mSize=%d", mSize)
		wasted, total, err := calculateFileWastedBytes(db, *appendVecFile, mSize, sfid)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf(
			"wasted %d (%.2f GB) total %d (%.2f GB)",
			wasted, float64(wasted)/(1<<30),
			total, float64(total)/(1<<30),
		)
	}
}
