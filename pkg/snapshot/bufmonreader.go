package snapshot

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
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
	expected   int64
	onProgress ProgressCallback
}

func NewBufMonReader(name string, r io.ReadCloser, totalSize int64) *bufmonreader {
	return &bufmonreader{
		name:      name,
		b:         r,
		c:         r,
		totalSize: totalSize,
		expected:  totalSize,
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
		expected:  info.Size(),
	}, nil
}

func NewBufMonReaderHTTP(ctx context.Context, url string) (*bufmonreader, error) {
	return NewBufMonReaderHTTPWithSave(ctx, url, "")
}

// PartialSuffix is appended to snapshot files during download to mark them as incomplete.
// Once download completes successfully, the file is atomically renamed to remove this suffix.
const PartialSuffix = ".partial"

const (
	snapshotProbeTimeout          = 30 * time.Second
	snapshotResponseHeaderTimeout = 30 * time.Second
	snapshotDownloadIdleTimeout   = 2 * time.Minute
)

var (
	snapshotProbeHTTPClient    = newSnapshotHTTPClient(snapshotProbeTimeout)
	snapshotDownloadHTTPClient = newSnapshotHTTPClient(0)
)

func newSnapshotHTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	transport.ResponseHeaderTimeout = snapshotResponseHeaderTimeout
	return &http.Client{Transport: transport, Timeout: timeout}
}

// NewBufMonReaderHTTPWithSave streams from HTTP URL and optionally saves to disk.
// If savePath is non-empty, the data will be written to a .partial file while streaming.
// Use FinalizePartialDownload after successful processing to rename to the final path.
// Returns: (*bufmonreader, error)
func NewBufMonReaderHTTPWithSave(ctx context.Context, url string, savePath string) (*bufmonreader, error) {
	return newBufMonReaderHTTPWithSave(ctx, url, savePath, snapshotProbeHTTPClient, snapshotDownloadHTTPClient)
}

func newBufMonReaderHTTPWithSave(
	ctx context.Context,
	url string,
	savePath string,
	probeClient *http.Client,
	downloadClient *http.Client,
) (*bufmonreader, error) {
	probeCtx, cancelProbe := context.WithTimeout(ctx, snapshotProbeTimeout)
	defer cancelProbe()
	probeRequest, err := http.NewRequestWithContext(probeCtx, http.MethodHead, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create snapshot probe request: invalid URL")
	}
	resp, err := probeClient.Do(probeRequest)
	if err != nil {
		return nil, safeSnapshotRequestError("probe", probeCtx, err)
	}
	probeSize := resp.ContentLength
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("snapshot probe returned HTTP %d", resp.StatusCode)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create snapshot download request: invalid URL")
	}
	resp, err = downloadClient.Do(req)
	if err != nil {
		return nil, safeSnapshotRequestError("download", ctx, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("snapshot download returned HTTP %d", resp.StatusCode)
	}
	expectedSize := resp.ContentLength
	if expectedSize < 0 {
		expectedSize = probeSize
	}
	totalSize := expectedSize

	body := newIdleTimeoutReadCloser(resp.Body, snapshotDownloadIdleTimeout)
	var reader io.Reader = body
	var closer io.Closer = body

	// If savePath is provided, use TeeReader to write to disk while streaming.
	// Write to .partial file first for crash safety.
	if savePath != "" {
		partialPath := savePath + PartialSuffix
		// Note: Don't log here - caller logs before progress bar starts to avoid breaking cursor positioning
		outFile, err := os.Create(partialPath)
		if err != nil {
			_ = body.Close()
			return nil, fmt.Errorf("creating save file %s: %v", partialPath, err)
		}

		// TeeReader splits the stream: data goes to both the tar reader AND the file
		reader = io.TeeReader(body, outFile)

		// Create a multi-closer that closes both the HTTP body and the file
		closer = &multiCloser{closers: []io.Closer{body, outFile}}
	}

	return &bufmonreader{
		name:      "remote snapshot",
		b:         reader,
		c:         closer,
		totalSize: totalSize,
		expected:  expectedSize,
	}, nil
}

type idleTimeoutReadCloser struct {
	body     io.ReadCloser
	timeout  time.Duration
	timedOut atomic.Bool
}

func newIdleTimeoutReadCloser(body io.ReadCloser, timeout time.Duration) *idleTimeoutReadCloser {
	return &idleTimeoutReadCloser{body: body, timeout: timeout}
}

func (r *idleTimeoutReadCloser) Read(p []byte) (int, error) {
	if r.timeout <= 0 {
		return r.body.Read(p)
	}
	expired := make(chan struct{})
	timer := time.AfterFunc(r.timeout, func() {
		r.timedOut.Store(true)
		close(expired)
		_ = r.body.Close()
	})
	n, err := r.body.Read(p)
	if !timer.Stop() {
		<-expired
	}
	if r.timedOut.Load() {
		return n, context.DeadlineExceeded
	}
	return n, err
}

func (r *idleTimeoutReadCloser) Close() error {
	return r.body.Close()
}

func safeSnapshotRequestError(operation string, ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("snapshot %s: %w", operation, ctxErr)
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return fmt.Errorf("snapshot %s: %w", operation, context.DeadlineExceeded)
	}
	return fmt.Errorf("snapshot %s request failed", operation)
}

// FinalizePartialDownload atomically renames a completed .partial file to its final name.
// Call after successfully processing a snapshot saved with NewBufMonReaderHTTPWithSave.
// No-op if savePath is empty.
func FinalizePartialDownload(savePath string) error {
	if savePath == "" {
		return nil
	}
	partialPath := savePath + PartialSuffix
	partial, err := os.OpenFile(partialPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open completed snapshot download: %w", err)
	}
	if err := partial.Sync(); err != nil {
		_ = partial.Close()
		return fmt.Errorf("sync completed snapshot download: %w", err)
	}
	if err := partial.Close(); err != nil {
		return fmt.Errorf("close completed snapshot download: %w", err)
	}
	if err := os.Rename(partialPath, savePath); err != nil {
		return fmt.Errorf("failed to finalize snapshot %s: %w", savePath, err)
	}
	dir, err := os.Open(filepath.Dir(savePath))
	if err != nil {
		return fmt.Errorf("open snapshot download directory: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync snapshot download directory: %w", err)
	}
	mlog.Log.FileOnlyf("Finalized snapshot download: %s", savePath)
	return nil
}

// CleanupPartialDownload removes a .partial file if it exists.
// Call on error/cancellation to clean up incomplete downloads.
func CleanupPartialDownload(savePath string) {
	if savePath == "" {
		return
	}
	partialPath := savePath + PartialSuffix
	if _, err := os.Stat(partialPath); err == nil {
		mlog.Log.Infof("Cleaning up partial download: %s", partialPath)
		os.Remove(partialPath)
	}
}

// multiCloser closes multiple io.Closers
type multiCloser struct {
	closers []io.Closer
}

func (mc *multiCloser) Close() error {
	var errs []error
	for _, c := range mc.closers {
		errs = append(errs, c.Close())
	}
	return errors.Join(errs...)
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

func (x *bufmonreader) validateComplete() error {
	if x.expected >= 0 && x.bytesRead != x.expected {
		return fmt.Errorf("snapshot stream ended after %d bytes, expected %d", x.bytesRead, x.expected)
	}
	return nil
}

func (x *bufmonreader) Read(p []byte) (int, error) {
	n, err := x.b.Read(p)
	x.bytesRead += int64(n)

	// Call progress callback if set
	if x.onProgress != nil {
		x.onProgress(x.bytesRead, x.totalSize)
	}
	return n, err
}

func (x *bufmonreader) Close() error {
	return x.c.Close()
}
