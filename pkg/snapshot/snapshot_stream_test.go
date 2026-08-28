package snapshot

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

type snapshotRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn snapshotRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type snapshotTrackingBody struct {
	closed *atomic.Bool
}

func (b *snapshotTrackingBody) Read([]byte) (int, error) { return 0, io.EOF }
func (b *snapshotTrackingBody) Close() error {
	b.closed.Store(true)
	return nil
}

type snapshotBlockingBody struct {
	closed chan struct{}
	once   sync.Once
}

func (b *snapshotBlockingBody) Read([]byte) (int, error) {
	<-b.closed
	return 0, io.ErrClosedPipe
}

func (b *snapshotBlockingBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

type snapshotReadableBody struct {
	reader *bytes.Reader
	closed atomic.Bool
}

func (b *snapshotReadableBody) Read(p []byte) (int, error) {
	if b.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	return b.reader.Read(p)
}

func (b *snapshotReadableBody) Close() error {
	b.closed.Store(true)
	return nil
}

type snapshotErrorCloser struct{ err error }

func (c snapshotErrorCloser) Close() error { return c.err }

func TestSnapshotHTTPProbeHonorsCancellationAndSanitizesURL(t *testing.T) {
	client := &http.Client{Transport: snapshotRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	secret := "private-query-value"
	_, err := newBufMonReaderHTTPWithSave(ctx, "https://snapshot.invalid/snapshot.tar.zst?token="+secret, "", client, client)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("probe error = %v, want deadline exceeded", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "snapshot.invalid") {
		t.Fatalf("probe error exposed snapshot URL: %v", err)
	}
}

func TestSnapshotHTTPClosesNonOKBodiesAndSanitizesURL(t *testing.T) {
	var probeClosed atomic.Bool
	var downloadClosed atomic.Bool
	transport := snapshotRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.Method {
		case http.MethodHead:
			return &http.Response{
				StatusCode:    http.StatusOK,
				ContentLength: 1,
				Body:          &snapshotTrackingBody{closed: &probeClosed},
				Header:        make(http.Header),
				Request:       request,
			}, nil
		case http.MethodGet:
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Body:       &snapshotTrackingBody{closed: &downloadClosed},
				Header:     make(http.Header),
				Request:    request,
			}, nil
		default:
			return nil, fmt.Errorf("unexpected method %s", request.Method)
		}
	})
	client := &http.Client{Transport: transport}
	secret := "private-query-value"
	_, err := newBufMonReaderHTTPWithSave(context.Background(), "https://snapshot.invalid/archive.tar.zst?token="+secret, "", client, client)
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("download error = %v, want safe HTTP status", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "snapshot.invalid") {
		t.Fatalf("download error exposed snapshot URL: %v", err)
	}
	if !probeClosed.Load() || !downloadClosed.Load() {
		t.Fatalf("response bodies closed = probe:%t download:%t", probeClosed.Load(), downloadClosed.Load())
	}
}

func TestSnapshotHTTPResponseHeaderTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		server.Close()
	}()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 20 * time.Millisecond
	client := &http.Client{Transport: transport}

	start := time.Now()
	_, err := newBufMonReaderHTTPWithSave(context.Background(), server.URL+"/snapshot.tar.zst", "", client, client)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("header error = %v, want deadline exceeded", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("response header timeout did not stop promptly")
	}
}

func TestSnapshotDownloadBodyHasIdleTimeout(t *testing.T) {
	body := &snapshotBlockingBody{closed: make(chan struct{})}
	reader := newIdleTimeoutReadCloser(body, 20*time.Millisecond)
	start := time.Now()
	_, err := reader.Read(make([]byte, 1))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("idle read error = %v, want deadline exceeded", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("idle read did not stop promptly")
	}
}

func TestSnapshotDownloadIdleTimeoutIgnoresTimeBetweenReads(t *testing.T) {
	const timeout = 20 * time.Millisecond
	body := &snapshotReadableBody{reader: bytes.NewReader([]byte("ab"))}
	reader := newIdleTimeoutReadCloser(body, timeout)
	defer reader.Close()

	buffer := make([]byte, 1)
	if n, err := reader.Read(buffer); err != nil || n != 1 || buffer[0] != 'a' {
		t.Fatalf("first read = %d, %v, %q; want 1, nil, a", n, err, buffer)
	}
	time.Sleep(3 * timeout)
	if body.closed.Load() {
		t.Fatal("idle timeout closed the body while no read was blocked")
	}
	if n, err := reader.Read(buffer); err != nil || n != 1 || buffer[0] != 'b' {
		t.Fatalf("second read = %d, %v, %q; want 1, nil, b", n, err, buffer)
	}
}

func TestFinalizePartialDownloadInstallsCompletedFile(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "snapshot.tar.zst")
	payload := []byte("complete snapshot")
	if err := os.WriteFile(destination+PartialSuffix, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := FinalizePartialDownload(destination); err != nil {
		t.Fatalf("finalize partial: %v", err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("finalized payload = %q, want %q", got, payload)
	}
	if _, err := os.Stat(destination + PartialSuffix); !os.IsNotExist(err) {
		t.Fatalf("partial remains after finalize: %v", err)
	}
}

func TestFinalizePartialDownloadRequiresPartialFile(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "snapshot.tar.zst")
	if err := FinalizePartialDownload(destination); err == nil {
		t.Fatal("missing partial download was accepted")
	}
}

func TestSnapshotStreamCloserPropagatesCloseErrors(t *testing.T) {
	compressionErr := errors.New("compression close")
	monitorErr := errors.New("monitor close")
	stream := &snapshotStreamCloser{
		compressionCloser: snapshotErrorCloser{err: compressionErr},
		monitor:           &bufmonreader{c: snapshotErrorCloser{err: monitorErr}},
	}
	got := stream.Close()
	if !errors.Is(got, compressionErr) || !errors.Is(got, monitorErr) {
		t.Fatalf("close error = %v, want both close failures", got)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("second close = %v, want nil", err)
	}
}

func TestReadTarRejectsTruncatedStreamAndCleansPartial(t *testing.T) {
	payload := compressedSnapshotArchive(t)
	payload = payload[:len(payload)-4]
	server := snapshotArchiveServer(payload)
	defer server.Close()

	destination := filepath.Join(t.TempDir(), "snapshot-42-test.tar.zst")
	err := readTar(context.Background(), &sync.WaitGroup{}, server.URL+"/snapshot-42-test.tar.zst?token=private", nil, readTarOptions{savePath: destination})
	if err == nil {
		t.Fatal("truncated snapshot stream succeeded")
	}
	if strings.Contains(err.Error(), "token=private") {
		t.Fatalf("stream error exposed URL query: %v", err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("final file exists after truncated stream: %v", err)
	}
	if _, err := os.Stat(destination + PartialSuffix); !os.IsNotExist(err) {
		t.Fatalf("partial remains after truncated stream: %v", err)
	}
}

func TestReadTarFinishesAndAtomicallyInstallsStream(t *testing.T) {
	payload := compressedSnapshotArchive(t)
	server := snapshotArchiveServer(payload)
	defer server.Close()

	destination := filepath.Join(t.TempDir(), "snapshot-42-test.tar.zst")
	if err := readTar(context.Background(), &sync.WaitGroup{}, server.URL+"/snapshot-42-test.tar.zst?download=1", nil, readTarOptions{savePath: destination}); err != nil {
		t.Fatalf("read snapshot stream: %v", err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("saved snapshot length = %d, want %d", len(got), len(payload))
	}
	if _, err := os.Stat(destination + PartialSuffix); !os.IsNotExist(err) {
		t.Fatalf("partial remains after successful stream: %v", err)
	}
}

func TestSnapshotReaderAcceptsLZ4(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "snapshot-41-test.tar.lz4")
	file, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	lz4Writer := lz4.NewWriter(file)
	tarWriter := tar.NewWriter(lz4Writer)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "snapshots/41/41", Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lz4Writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	slot, err := selectedFullSnapshotSlot(filename)
	if err != nil || slot != 41 {
		t.Fatalf("selected full snapshot slot = %d, %v; want 41, nil", slot, err)
	}
	reader, closer, err := newSnapshotReader(context.Background(), filename)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	header, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	if header.Name != "snapshots/41/41" {
		t.Fatalf("archive entry = %q, want snapshots/41/41", header.Name)
	}
	base, end, err := selectedIncrementalSnapshotSlots("https://snapshot.invalid/incremental-snapshot-41-42-test.tar.lz4?download=1")
	if err != nil || base != 41 || end != 42 {
		t.Fatalf("selected incremental slots = %d, %d, %v; want 41, 42, nil", base, end, err)
	}
}

func compressedSnapshotArchive(t *testing.T) []byte {
	t.Helper()
	var archive bytes.Buffer
	tarWriter := tar.NewWriter(&archive)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "metadata", Mode: 0o644, Size: 0}); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}

	var compressed bytes.Buffer
	encoder, err := zstd.NewWriter(&compressed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encoder.Write(archive.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func snapshotArchiveServer(payload []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		if request.Method == http.MethodGet {
			_, _ = response.Write(payload)
		}
	}))
}
