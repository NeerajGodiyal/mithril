package snapshot

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/mlog"
)

// ProgressCallback is called with (bytesRead, totalBytes) to report download progress
type ProgressCallback func(bytesRead, totalBytes int64)

type bufmonreader struct {
	name       string
	b          io.Reader
	c          io.Closer
	bytesRead  int64
	totalSize  int64
	startTime  time.Time
	onProgress ProgressCallback
}

const gib = 1 << 30

func NewBufMonReader(name string, r io.ReadCloser, totalSize int64) *bufmonreader {
	return &bufmonreader{
		name:      name,
		b:         r,
		totalSize: totalSize,
		startTime: time.Now(),
	}
}

func NewBufMonReaderFromFile(file *os.File) (*bufmonreader, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}

	return &bufmonreader{
		name:      file.Name(),
		b:         bufio.NewReader(file),
		c:         file,
		totalSize: info.Size(),
		startTime: time.Now(),
	}, nil
}

func NewBufMonReaderHTTP(url string) (*bufmonreader, error) {
	return NewBufMonReaderHTTPWithSave(url, "")
}

// NewBufMonReaderHTTPWithSave streams from HTTP URL and optionally saves to disk.
// If savePath is non-empty, the data will be written to disk while streaming.
// Returns: (*bufmonreader, error)
func NewBufMonReaderHTTPWithSave(url string, savePath string) (*bufmonreader, error) {
	resp, err := http.Head(url)
	if err != nil {
		return nil, fmt.Errorf("HEAD %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HEAD %s: had not-ok status: %s", url, resp.Status)
	}
	totalSize := resp.ContentLength
	resp.Body.Close()

	resp, err = http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: had not-ok status: %s", url, resp.Status)
	}

	var reader io.Reader = resp.Body
	var closer io.Closer = resp.Body

	// If savePath is provided, use TeeReader to write to disk while streaming
	if savePath != "" {
		mlog.Log.Infof("Saving snapshot to %s while streaming...", savePath)
		outFile, err := os.Create(savePath)
		if err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("creating save file %s: %v", savePath, err)
		}

		// TeeReader splits the stream: data goes to both the tar reader AND the file
		reader = io.TeeReader(resp.Body, outFile)

		// Create a multi-closer that closes both the HTTP body and the file
		closer = &multiCloser{closers: []io.Closer{resp.Body, outFile}}
	}

	return &bufmonreader{
		name:      url,
		b:         reader,
		c:         closer,
		totalSize: totalSize,
		startTime: time.Now(),
	}, nil
}

// multiCloser closes multiple io.Closers
type multiCloser struct {
	closers []io.Closer
}

func (mc *multiCloser) Close() error {
	var firstErr error
	for _, c := range mc.closers {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// SetProgressCallback sets an optional callback to receive progress updates.
// The callback is invoked on each Read() with (bytesRead, totalBytes).
func (x *bufmonreader) SetProgressCallback(cb ProgressCallback) {
	x.onProgress = cb
}

// TotalSize returns the total size of the data being read
func (x *bufmonreader) TotalSize() int64 {
	return x.totalSize
}

func (x *bufmonreader) Read(p []byte) (int, error) {
	n, err := x.b.Read(p)
	before := x.bytesRead
	x.bytesRead += int64(n)

	// Call progress callback if set
	if x.onProgress != nil {
		x.onProgress(x.bytesRead, x.totalSize)
	} else if (x.bytesRead / gib) > (before / gib) {
		// Fallback to logging every GiB if no callback is set
		percentComplete := float64(x.bytesRead) / float64(x.totalSize) * 100

		elapsed := time.Since(x.startTime)
		var remaining time.Duration
		if x.bytesRead > 0 {
			bytesPerSec := float64(x.bytesRead) / elapsed.Seconds()
			remainingSecs := float64(x.totalSize-x.bytesRead) / bytesPerSec
			remaining = time.Duration(remainingSecs * float64(time.Second))
		}

		mlog.Log.Infof("Processed %.2f of %.2f GiB (%.1f%%) of %s. Est. remaining: %s",
			float64(x.bytesRead)/float64(gib),
			float64(x.totalSize)/float64(gib),
			percentComplete,
			x.name,
			remaining.Round(time.Second))
	}
	return n, err
}

func (x *bufmonreader) Close() error {
	return x.c.Close()
}
