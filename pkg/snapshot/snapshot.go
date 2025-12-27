package snapshot

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

func UnmarshalManifestFromSnapshot(filename string, accountsDbDir string) (*SnapshotManifest, error) {
	manifest := new(SnapshotManifest)

	if err := os.MkdirAll(accountsDbDir, 0775); err != nil {
		return nil, err
	}

	tarReader, closer, err := newSnapshotReader(filename)
	if err != nil {
		return nil, err
	}
	defer closer.Close()

	writer := new(bytes.Buffer)

	for {
		header, err := tarReader.Next()
		if err != nil {
			return nil, err
		}

		// identify manifest file, whose path is of the form "snapshots/SLOT/SLOT"
		if strings.Contains(header.Name, "snapshots/") {
			if strings.Count(header.Name, "/") == 2 {
				_, err := io.Copy(writer, tarReader)
				if err != nil {
					return nil, err
				}
				err = os.WriteFile(filepath.Join(accountsDbDir, "manifest"), writer.Bytes(), 0644)
				if err != nil {
					mlog.Log.Errorf("err copying manifest file out: %s\n", err)
					return nil, err
				}
				break
			}
		}
	}

	decoder := bin.NewBinDecoder(writer.Bytes())
	err = manifest.UnmarshalWithDecoder(decoder)

	return manifest, err
}

type appendVecCopyingTask struct {
	Filename                string
	TarBuffer               *bytes.Buffer
	FromIncrementalSnapshot bool
}

type indexEntryBuilderTask struct {
	Data     []byte
	FileSize uint64
	Slot     uint64
	FileId   uint64
}

type indexEntryCommitterTask struct {
	IndexEntries []accountsdb.AccountIndexEntry
	Pubkeys      []solana.PublicKey
}

const (
	snapshotTypeZst = iota
	snapshotTypeLz4
)

var (
	ZstdDecoderConcurrency = runtime.NumCPU()
)

func readerForCompressionType(snapshotType int, bmr *bufmonreader) (r io.Reader, err error) {
	if snapshotType == snapshotTypeZst {
		zstdReader, err := zstd.NewReader(bmr, zstd.WithDecoderConcurrency(ZstdDecoderConcurrency))
		if err != nil {
			return nil, err
		}
		r = zstdReader
	} else if snapshotType == snapshotTypeLz4 {
		r = lz4.NewReader(bmr)
	} else {
		panic("unknown snapshot type")
	}

	return r, nil
}

func parseSnapshotType(snapshotFileName string) int {
	var snapshotType int
	fileExt := filepath.Ext(snapshotFileName)

	if fileExt == ".zst" {
		snapshotType = snapshotTypeZst
	} else if fileExt == ".lz4" {
		snapshotType = snapshotTypeLz4
	} else {
		panic(fmt.Sprintf("unknown snapshot compression type - file ext: %s", fileExt))
	}

	return snapshotType
}

func newSnapshotReader(filename string) (*tar.Reader, io.Closer, error) {
	return newSnapshotReaderWithSave(filename, "")
}

// newSnapshotReaderWithSave creates a tar reader for a snapshot file or HTTP URL.
// If filename is an HTTP URL and savePath is non-empty, the data will be saved
// to disk while streaming (using io.TeeReader for parallel download+processing+saving).
func newSnapshotReaderWithSave(filename string, savePath string) (*tar.Reader, io.Closer, error) {
	snapshotType := parseSnapshotType(filename)
	var bmr *bufmonreader
	var err error
	if strings.HasPrefix(filename, "https://") || strings.HasPrefix(filename, "http://") {
		bmr, err = NewBufMonReaderHTTPWithSave(filename, savePath)
	} else {
		snapshotFile, err := os.Open(filename)
		if err != nil {
			return nil, nil, fmt.Errorf("open %s: %w", filename, err)
		}
		bmr, err = NewBufMonReaderFromFile(snapshotFile)
	}
	if err != nil {
		return nil, nil, err
	}
	reader, err := readerForCompressionType(snapshotType, bmr)
	if err != nil {
		return nil, nil, fmt.Errorf("opening compression reader: %v", err)
	}

	return tar.NewReader(reader), bmr, nil
}

func LoadManifestFromFile(filename string) (*SnapshotManifest, error) {
	manifestFile, err := os.Open(filename)
	if err != nil {
		mlog.Log.Errorf("failed to open %s\n", filename)
		return nil, err
	}
	manifestBytes, err := ioutil.ReadAll(manifestFile)
	if err != nil {
		return nil, err
	}

	manifest := new(SnapshotManifest)
	decoder := bin.NewBinDecoder(manifestBytes)
	err = manifest.UnmarshalWithDecoder(decoder)
	if err != nil {
		return nil, err
	}

	return manifest, nil
}
