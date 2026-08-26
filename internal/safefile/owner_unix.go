//go:build unix

package safefile

import (
	"os"
	"syscall"
)

// OwnerTrusted reports whether a file is controlled by root or by this
// process's user. A root process therefore accepts only root-owned files.
func OwnerTrusted(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return ownerAllowed(stat.Uid, uint32(os.Geteuid()))
}

func ownerAllowed(owner, process uint32) bool {
	return owner == 0 || owner == process
}
