//go:build unix

package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func assertSpecialFileRejectedPromptly(t *testing.T, path string, call func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- call() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("special file was accepted")
		}
	case <-time.After(time.Second):
		// Unblock a regressed FIFO reader before failing the test process.
		if fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK, 0); err == nil {
			_ = unix.Close(fd)
		}
		t.Fatal("special-file open did not return promptly")
	}
}

func TestSpecialFilesDoNotBlockReaders(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipe")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("confined file", func(t *testing.T) {
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		assertSpecialFileRejectedPromptly(t, path, func() error {
			file, _, err := openRootRegularFile(root, filepath.Base(path))
			if file != nil {
				_ = file.Close()
			}
			return err
		})
	})

	t.Run("state file", func(t *testing.T) {
		assertSpecialFileRejectedPromptly(t, path, func() error {
			_, err := readShutdownStateContext(context.Background(), path)
			return err
		})
	})
}
