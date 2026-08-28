//go:build linux

package accountsdb

import (
	"os"

	"golang.org/x/sys/unix"
)

func syncAccountsFilesystem(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return unix.Syncfs(int(dir.Fd()))
}
