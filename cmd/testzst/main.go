package main

import (
	"bufio"
	"flag"
	"io"
	"os"

	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/snapshot"
	"github.com/klauspost/compress/zstd"
)

func main() {
	zstdFilename := flag.String("zstd-file", "", "Path to zstd compressed file")
	concurrency := flag.Int("zstd-decoder-concurrency", 1, "Number of decoder goroutines")
	outputFile := flag.String("output", "", "Optional output file to write decompressed data (measures decompress+write throughput)")

	flag.Parse()

	file, err := os.Open(*zstdFilename)
	if err != nil {
		mlog.Log.Errorf("opening zstdFilename=%s: %v", *zstdFilename, err)
		os.Exit(1)
	}
	bmr, err := snapshot.NewBufMonReaderFromFile(file)
	if err != nil {
		mlog.Log.Errorf("making BufMonReader: %v", err)
		os.Exit(1)
	}
	zstdReader, err := zstd.NewReader(bmr, zstd.WithDecoderConcurrency(*concurrency))
	if err != nil {
		mlog.Log.Errorf("zstd.NewReader: %v", err)
		os.Exit(1)
	}

	var dst io.Writer = io.Discard
	if *outputFile != "" {
		f, err := os.Create(*outputFile)
		if err != nil {
			mlog.Log.Errorf("creating output file %s: %v", *outputFile, err)
			os.Exit(1)
		}
		defer f.Close()
		bw := bufio.NewWriterSize(f, 4*1024*1024)
		defer bw.Flush()
		dst = bw
	}

	_, err = zstdReader.WriteTo(dst)
	if err != nil {
		mlog.Log.Errorf("WriteTo: %v", err)
		os.Exit(1)
	}
}
