package snapshot

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestStatusCacheCandidateAtomicReplacement(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "status-cache")
	install := func(payload []byte) {
		t.Helper()
		candidate := newStatusCacheCandidate(destination)
		defer candidate.cleanup()
		header := &tar.Header{Name: "./snapshots/status_cache", Typeflag: tar.TypeReg, Size: int64(len(payload))}
		handled, written, err := candidate.capture(header, bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("capture: %v", err)
		}
		if !handled || written != int64(len(payload)) {
			t.Fatalf("capture handled=%t written=%d, want true/%d", handled, written, len(payload))
		}
		if err := candidate.commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	full := []byte("fullseed")
	install(full)
	assertStatusCacheFileBytes(t, destination, full)

	incremental := []byte("incremental-seed")
	candidate := newStatusCacheCandidate(destination)
	header := &tar.Header{Name: agaveStatusCacheArchiveMember, Typeflag: tar.TypeReg, Size: int64(len(incremental))}
	if _, _, err := candidate.capture(header, bytes.NewReader(incremental)); err != nil {
		t.Fatalf("capture incremental: %v", err)
	}
	// The full seed remains installed until the whole incremental archive has
	// completed and readTar calls commit.
	assertStatusCacheFileBytes(t, destination, full)
	candidate.cleanup()
	assertStatusCacheFileBytes(t, destination, full)

	install(incremental)
	assertStatusCacheFileBytes(t, destination, incremental)
}

func TestStatusCacheCandidateRejectsInvalidArchiveMembers(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "status-cache")
	candidate := newStatusCacheCandidate(destination)
	defer candidate.cleanup()

	handled, _, err := candidate.capture(&tar.Header{Name: "accounts/1.1", Size: 8}, bytes.NewReader(make([]byte, 8)))
	if err != nil || handled {
		t.Fatalf("unrelated member handled=%t err=%v", handled, err)
	}
	if err := candidate.commit(); err == nil {
		t.Fatal("expected missing status-cache error")
	}

	invalid := []struct {
		name   string
		header tar.Header
		body   []byte
	}{
		{"not regular", tar.Header{Name: agaveStatusCacheArchiveMember, Typeflag: tar.TypeSymlink, Size: 8}, make([]byte, 8)},
		{"too small", tar.Header{Name: agaveStatusCacheArchiveMember, Typeflag: tar.TypeReg, Size: 7}, make([]byte, 7)},
		{"truncated", tar.Header{Name: agaveStatusCacheArchiveMember, Typeflag: tar.TypeReg, Size: 9}, make([]byte, 8)},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			c := newStatusCacheCandidate(destination)
			defer c.cleanup()
			if _, _, err := c.capture(&tt.header, bytes.NewReader(tt.body)); err == nil {
				t.Fatal("expected capture error")
			}
		})
	}
}

func assertStatusCacheFileBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s contains %q, want %q", path, got, want)
	}
}
