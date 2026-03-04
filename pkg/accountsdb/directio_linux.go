//go:build linux

package accountsdb

import (
	"fmt"
	"os"
	"syscall"
)

// OpenDirect opens a file with O_DIRECT and pre-allocates disk space via fallocate.
func OpenDirect(path string, flag int, perm os.FileMode, size int64) (*os.File, error) {
	f, err := os.OpenFile(path, flag|syscall.O_DIRECT, perm)
	if err != nil {
		return nil, err
	}
	if err := syscall.Fallocate(int(f.Fd()), 0, 0, size); err != nil {
		f.Close()
		return nil, fmt.Errorf("fallocate size=%d: %w", size, err)
	}
	return f, nil
}
