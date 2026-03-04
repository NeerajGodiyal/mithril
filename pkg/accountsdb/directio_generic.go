//go:build !linux

package accountsdb

import (
	"os"
)

// Fallback for non-linux. See directio_linux.go.
func OpenDirect(path string, flag int, perm os.FileMode, size int64) (*os.File, error) {
	return os.OpenFile(path, flag, perm)
}
