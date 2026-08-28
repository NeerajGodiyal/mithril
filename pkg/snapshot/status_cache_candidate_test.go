package snapshot

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStatusCacheCandidateAtomicReplacement(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "status-cache")
	install := func(slot uint64) {
		t.Helper()
		payload := encodeRootOnlyStatusCache(slot)
		candidate := newStatusCacheCandidate(destination)
		defer candidate.cleanup()
		header := &tar.Header{Name: "./snapshots/status_cache", Typeflag: tar.TypeReg, Size: int64(len(payload))}
		handled, written, err := candidate.capture(context.Background(), header, bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("capture: %v", err)
		}
		if !handled || written != int64(len(payload)) {
			t.Fatalf("capture handled=%t written=%d, want true/%d", handled, written, len(payload))
		}
		if err := candidate.commit(context.Background(), &slot); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	full := encodeRootOnlyStatusCache(41)
	install(41)
	assertStatusCacheFileBytes(t, destination, full)

	incremental := encodeRootOnlyStatusCache(42)
	candidate := newStatusCacheCandidate(destination)
	header := &tar.Header{Name: agaveStatusCacheArchiveMember, Typeflag: tar.TypeReg, Size: int64(len(incremental))}
	if _, _, err := candidate.capture(context.Background(), header, bytes.NewReader(incremental)); err != nil {
		t.Fatalf("capture incremental: %v", err)
	}
	// The full seed remains installed until the whole incremental archive has
	// completed and readTar calls commit.
	assertStatusCacheFileBytes(t, destination, full)
	candidate.cleanup()
	assertStatusCacheFileBytes(t, destination, full)

	install(42)
	assertStatusCacheFileBytes(t, destination, incremental)
}

func TestStatusCacheCandidateRejectsInvalidArchiveMembers(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "status-cache")
	candidate := newStatusCacheCandidate(destination)
	defer candidate.cleanup()

	handled, _, err := candidate.capture(context.Background(), &tar.Header{Name: "accounts/1.1", Size: 8}, bytes.NewReader(make([]byte, 8)))
	if err != nil || handled {
		t.Fatalf("unrelated member handled=%t err=%v", handled, err)
	}
	root := uint64(42)
	if err := candidate.commit(context.Background(), &root); err == nil {
		t.Fatal("expected missing status-cache error")
	}

	invalid := []struct {
		name   string
		header tar.Header
		body   []byte
	}{
		{"not regular", tar.Header{Name: agaveStatusCacheArchiveMember, Typeflag: tar.TypeSymlink, Size: 8}, make([]byte, 8)},
		{"too small", tar.Header{Name: agaveStatusCacheArchiveMember, Typeflag: tar.TypeReg, Size: 7}, make([]byte, 7)},
		{"too large", tar.Header{Name: agaveStatusCacheArchiveMember, Typeflag: tar.TypeReg, Size: maxAgaveStatusCacheSize + 1}, nil},
		{"truncated", tar.Header{Name: agaveStatusCacheArchiveMember, Typeflag: tar.TypeReg, Size: 9}, make([]byte, 8)},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			c := newStatusCacheCandidate(destination)
			defer c.cleanup()
			if _, _, err := c.capture(context.Background(), &tt.header, bytes.NewReader(tt.body)); err == nil {
				t.Fatal("expected capture error")
			}
		})
	}
}

func TestStatusCacheStreamValidationIsBoundedAndCancellable(t *testing.T) {
	valid := encodeRootOnlyStatusCache(42)
	root, err := validateStatusCacheStream(context.Background(), bytes.NewReader(valid), int64(len(valid)), 42)
	if err != nil || root != 42 {
		t.Fatalf("valid status cache root = %d, %v", root, err)
	}

	trailing := append(append([]byte(nil), valid...), 0)
	if _, err := validateStatusCacheStream(context.Background(), bytes.NewReader(trailing), int64(len(trailing)), 42); err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("trailing-byte error = %v", err)
	}

	tooMany := binary.LittleEndian.AppendUint64(nil, maxAgaveStatusCacheSlotDeltas+1)
	tooMany = append(tooMany, make([]byte, int((maxAgaveStatusCacheSlotDeltas+1)*17))...)
	if _, err := validateStatusCacheStream(context.Background(), bytes.NewReader(tooMany), int64(len(tooMany)), maxAgaveStatusCacheSlotDeltas); err == nil || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Fatalf("slot-count error = %v", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := validateStatusCacheStream(cancelled, bytes.NewReader(valid), int64(len(valid)), 42); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled validation error = %v", err)
	}
}

func TestStatusCacheStreamRejectsMalformedResult(t *testing.T) {
	data := malformedStatusCacheResult(42)
	if _, err := validateStatusCacheStream(context.Background(), bytes.NewReader(data), int64(len(data)), 42); err == nil || !strings.Contains(err.Error(), "unknown transaction result tag") {
		t.Fatalf("malformed result error = %v", err)
	}
}

func TestStatusCacheStreamRejectsInvalidSlotStructure(t *testing.T) {
	tests := []struct {
		name         string
		deltas       []statusCacheTestDelta
		expectedRoot uint64
		want         string
	}{
		{
			name:         "nonroot",
			deltas:       []statusCacheTestDelta{{slot: 42, rooted: false}},
			expectedRoot: 42,
			want:         "is not rooted",
		},
		{
			name: "duplicate slot",
			deltas: []statusCacheTestDelta{
				{slot: 41, rooted: true},
				{slot: 41, rooted: true},
			},
			expectedRoot: 42,
			want:         "repeated slot 41",
		},
		{
			name: "duplicate nonroot slot",
			deltas: []statusCacheTestDelta{
				{slot: 41, rooted: true},
				{slot: 41, rooted: false},
			},
			expectedRoot: 42,
			want:         "repeated slot 41",
		},
		{
			name:         "slot beyond snapshot root",
			deltas:       []statusCacheTestDelta{{slot: 43, rooted: true}},
			expectedRoot: 42,
			want:         "exceeds snapshot root 42",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := encodeStatusCacheTestDeltas(tt.deltas)
			_, err := validateStatusCacheStream(
				context.Background(), bytes.NewReader(data), int64(len(data)), tt.expectedRoot,
			)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validation error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestStatusCacheStreamRootGapsRequireSlotHistory(t *testing.T) {
	deltas := make([]statusCacheTestDelta, maxAgaveStatusCacheSlotDeltas)
	for index := range deltas {
		// Gaps can be legitimate skipped slots. The status-cache bytes alone
		// cannot distinguish those from an omitted rooted bank.
		deltas[index] = statusCacheTestDelta{slot: uint64(index * 2), rooted: true}
	}
	expectedRoot := deltas[len(deltas)-1].slot
	data := encodeStatusCacheTestDeltas(deltas)
	root, err := validateStatusCacheStream(
		context.Background(), bytes.NewReader(data), int64(len(data)), expectedRoot,
	)
	if err != nil || root != expectedRoot {
		t.Fatalf("structural validation root = %d, %v; want %d", root, err, expectedRoot)
	}
}

func TestStatusCacheCandidateValidationFailurePreservesInstalledSeed(t *testing.T) {
	installed := []byte("installed-status-cache")
	root := uint64(42)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name         string
		payload      []byte
		ctx          context.Context
		expectedRoot *uint64
	}{
		{name: "malformed", payload: malformedStatusCacheResult(42), ctx: context.Background(), expectedRoot: &root},
		{name: "trailing", payload: append(encodeRootOnlyStatusCache(42), 0), ctx: context.Background(), expectedRoot: &root},
		{name: "wrong root", payload: encodeRootOnlyStatusCache(41), ctx: context.Background(), expectedRoot: &root},
		{name: "cancelled", payload: encodeRootOnlyStatusCache(42), ctx: cancelled, expectedRoot: &root},
		{name: "missing manifest root", payload: encodeRootOnlyStatusCache(42), ctx: context.Background()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			destination := filepath.Join(dir, "status-cache")
			if err := os.WriteFile(destination, installed, 0o644); err != nil {
				t.Fatalf("write installed seed: %v", err)
			}
			candidate := newStatusCacheCandidate(destination)
			header := &tar.Header{Name: agaveStatusCacheArchiveMember, Typeflag: tar.TypeReg, Size: int64(len(tt.payload))}
			if _, _, err := candidate.capture(context.Background(), header, bytes.NewReader(tt.payload)); err != nil {
				t.Fatalf("capture: %v", err)
			}
			if err := candidate.commit(tt.ctx, tt.expectedRoot); err == nil {
				t.Fatal("expected commit error")
			}
			assertStatusCacheFileBytes(t, destination, installed)
			candidate.cleanup()
			temporaries, err := filepath.Glob(filepath.Join(dir, ".snapshot-status-cache-*.partial"))
			if err != nil {
				t.Fatalf("glob temporaries: %v", err)
			}
			if len(temporaries) != 0 {
				t.Fatalf("failed commit left temporary files after cleanup: %v", temporaries)
			}
		})
	}
}

func TestStatusCacheCaptureCancellationRemovesTemporary(t *testing.T) {
	dir := t.TempDir()
	candidate := newStatusCacheCandidate(filepath.Join(dir, "status-cache"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	header := &tar.Header{Name: agaveStatusCacheArchiveMember, Typeflag: tar.TypeReg, Size: 8}
	if _, _, err := candidate.capture(ctx, header, bytes.NewReader(make([]byte, 8))); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled capture error = %v", err)
	}
	temporaries, err := filepath.Glob(filepath.Join(dir, ".snapshot-status-cache-*.partial"))
	if err != nil {
		t.Fatalf("glob temporaries: %v", err)
	}
	if len(temporaries) != 0 {
		t.Fatalf("cancelled capture left temporary files: %v", temporaries)
	}
}

type statusCacheTestDelta struct {
	slot   uint64
	rooted bool
}

func encodeStatusCacheTestDeltas(deltas []statusCacheTestDelta) []byte {
	data := binary.LittleEndian.AppendUint64(nil, uint64(len(deltas)))
	for _, delta := range deltas {
		data = binary.LittleEndian.AppendUint64(data, delta.slot)
		if delta.rooted {
			data = append(data, 1)
		} else {
			data = append(data, 0)
		}
		data = binary.LittleEndian.AppendUint64(data, 0)
	}
	return data
}

func encodeRootOnlyStatusCache(slot uint64) []byte {
	return encodeStatusCacheTestDeltas([]statusCacheTestDelta{{slot: slot, rooted: true}})
}

func malformedStatusCacheResult(slot uint64) []byte {
	data := binary.LittleEndian.AppendUint64(nil, 1)
	data = binary.LittleEndian.AppendUint64(data, slot)
	data = append(data, 1)
	data = binary.LittleEndian.AppendUint64(data, 1)
	data = append(data, make([]byte, 32)...)
	data = binary.LittleEndian.AppendUint64(data, 0)
	data = binary.LittleEndian.AppendUint64(data, 1)
	data = append(data, make([]byte, 20)...)
	return binary.LittleEndian.AppendUint32(data, 99)
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
