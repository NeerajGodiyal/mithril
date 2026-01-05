package main

import (
	"archive/tar"
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	bin "github.com/gagliardetto/binary"
	"github.com/klauspost/compress/zstd"

	"github.com/Overclock-Validator/mithril/pkg/snapshot"
)

type SlotFileID struct {
	Slot   uint64
	FileID uint64
}

// ManifestLookup provides fast lookups of append vec file sizes
type ManifestLookup struct {
	fileSizes map[SlotFileID]uint64
}

// NewManifestLookupFromData creates a lookup table from manifest data
func NewManifestLookupFromData(data []byte) (*ManifestLookup, error) {
	manifest := &snapshot.SnapshotManifest{}
	if err := manifest.UnmarshalWithDecoder(bin.NewBinDecoder(data)); err != nil {
		return nil, err
	}

	lookup := &ManifestLookup{
		fileSizes: make(map[SlotFileID]uint64),
	}

	for slot, appendVecs := range manifest.AccountsDb.Storages {
		for _, av := range appendVecs.AcctVecs {
			sfid := SlotFileID{Slot: slot, FileID: av.Id}
			lookup.fileSizes[sfid] = av.FileSize
		}
	}

	log.Printf("Loaded manifest with %d append vec entries", len(lookup.fileSizes))
	return lookup, nil
}

// isAppendVec identifies appendvec files, whose path is of the form "accounts/SLOT.ID"
func isAppendVec(filename string) bool {
	return strings.Contains(filename, "accounts/") && strings.Contains(filename, ".")
}

func processSnapshot(snapshotPath string) error {
	file, err := os.Open(snapshotPath)
	if err != nil {
		return fmt.Errorf("opening snapshot file: %w", err)
	}
	defer file.Close()

	// Decompress zstd
	decoder, err := zstd.NewReader(file)
	if err != nil {
		return fmt.Errorf("creating zstd decoder: %w", err)
	}
	defer decoder.Close()

	// Create tar reader
	tarReader := tar.NewReader(decoder)

	var manifestLookup *ManifestLookup
	totalFiles := 0
	totalGarbageBytes := int64(0)
	matchingFiles := 0

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar header: %w", err)
		}

		// identify manifest file, whose path is of the form "snapshots/SLOT/SLOT"
		if manifestLookup == nil && strings.Contains(header.Name, "snapshots/") {
			writer := &bytes.Buffer{}
			if strings.Count(header.Name, "/") == 2 {
				_, err := io.Copy(writer, tarReader)
				if err != nil {
					return err
				}
				manifestLookup, err = NewManifestLookupFromData(writer.Bytes())
				if err != nil {
					return fmt.Errorf("parsing manifest: %w", err)
				}
			}
			continue
		}

		// Check if this is an append vec
		if isAppendVec(header.Name) {
			totalFiles++

			// Parse slot and file ID from filename
			var slot, fileID uint64
			basename := header.Name
			if idx := strings.LastIndex(header.Name, "/"); idx >= 0 {
				basename = header.Name[idx+1:]
			}

			n, err := fmt.Sscanf(basename, "%d.%d", &slot, &fileID)
			if n != 2 || err != nil {
				log.Printf("Warning: could not parse filename %s: %v", header.Name, err)
				// Skip this file's content
				io.Copy(io.Discard, tarReader)
				continue
			}

			if manifestLookup == nil {
				return fmt.Errorf("found append vec %s before manifest was loaded", header.Name)
			}

			sfid := SlotFileID{Slot: slot, FileID: fileID}
			expectedSize, found := manifestLookup.fileSizes[sfid]
			if !found {
				return fmt.Errorf("slot=%d fileid=%d not found in manifest", slot, fileID)
			}

			tarSize := header.Size
			garbageBytes := tarSize - int64(expectedSize)

			if garbageBytes > 0 {
				totalGarbageBytes += garbageBytes
				if totalFiles <= 10 {
					log.Printf("File: %s, Tar size: %d, Manifest size: %d, Garbage: %d",
						basename, tarSize, expectedSize, garbageBytes)
				}
			} else if garbageBytes < 0 {
				log.Printf("Warning: File %s has tar size %d < manifest size %d",
					basename, tarSize, expectedSize)
			}

			matchingFiles++

			if totalFiles%1000 == 0 {
				log.Printf("Processed %d append vec files, total garbage: %d bytes (%.2f GB)",
					totalFiles, totalGarbageBytes, float64(totalGarbageBytes)/(1<<30))
			}

			continue
		}

		log.Printf("non manifest, non av file: %s", header.Name)
	}

	log.Printf("\n=== Results ===")
	log.Printf("Total append vec files processed: %d", totalFiles)
	log.Printf("Files matched with manifest: %d", matchingFiles)
	log.Printf("Total garbage bytes (tar - manifest): %d (%.2f GB)",
		totalGarbageBytes, float64(totalGarbageBytes)/(1<<30))

	if manifestLookup != nil {
		log.Printf("Manifest contained %d file entries", len(manifestLookup.fileSizes))
	}

	return nil
}

func main() {
	snapshotPath := flag.String("snapshot", "", "Path to snapshot tar.zst file")
	flag.Parse()

	if *snapshotPath == "" {
		log.Fatal("Please provide -snapshot flag with path to snapshot file")
	}

	if err := processSnapshot(*snapshotPath); err != nil {
		log.Fatalf("Error processing snapshot: %v", err)
	}
}
