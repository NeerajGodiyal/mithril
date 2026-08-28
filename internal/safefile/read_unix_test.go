//go:build unix

package safefile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestReadTrustedRegularRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.fifo")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := ReadTrustedRegular(path, ReadOptions{MaxBytes: 1024, ForbiddenPerm: 0o077})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotRegular) {
			t.Fatalf("FIFO error = %v, want ErrNotRegular", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("FIFO read blocked")
	}
}

func TestReadTrustedRegularPolicy(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "trusted")
	if err := os.WriteFile(path, []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadTrustedRegular(path, ReadOptions{
		MaxBytes:               5,
		ForbiddenPerm:          0o077,
		RejectAncestorSymlinks: true,
	})
	if err != nil || string(got) != "value" {
		t.Fatalf("trusted read = %q, %v", got, err)
	}
	if _, err := ReadTrustedRegular(path, ReadOptions{MaxBytes: 5}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("empty permission policy error = %v", err)
	}
	if _, err := ReadTrustedRegular(path, ReadOptions{MaxBytes: maxTrustedFileBytes + 1, ForbiddenPerm: 0o077}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("unbounded read policy error = %v", err)
	}

	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTrustedRegular(path, ReadOptions{MaxBytes: 5, ForbiddenPerm: 0o077}); !errors.Is(err, ErrPermissions) {
		t.Fatalf("broad permissions error = %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTrustedRegular(path, ReadOptions{MaxBytes: 4, ForbiddenPerm: 0o077}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize error = %v", err)
	}

	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTrustedRegular(link, ReadOptions{MaxBytes: 5, ForbiddenPerm: 0o022}); !errors.Is(err, ErrSymlink) {
		t.Fatalf("symlink error = %v", err)
	}

	realParent := filepath.Join(dir, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	nestedPath := filepath.Join(realParent, "nested")
	if err := os.WriteFile(nestedPath, []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	parentLink := filepath.Join(dir, "parent-link")
	if err := os.Symlink(realParent, parentLink); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTrustedRegular(
		filepath.Join(parentLink, "nested"),
		ReadOptions{
			MaxBytes:               5,
			ForbiddenPerm:          0o077,
			RejectAncestorSymlinks: true,
		},
	); !errors.Is(err, ErrSymlink) {
		t.Fatalf("ancestor symlink error = %v", err)
	}
}

func TestReadTrustedRegularRejectsWritableAncestor(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writable := filepath.Join(dir, "writable")
	if err := os.Mkdir(writable, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(writable, "trusted")
	if err := os.WriteFile(path, []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writable, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTrustedRegular(path, ReadOptions{
		MaxBytes:               5,
		ForbiddenPerm:          0o077,
		RejectAncestorSymlinks: true,
	}); !errors.Is(err, ErrPermissions) {
		t.Fatalf("writable ancestor error = %v", err)
	}
}
