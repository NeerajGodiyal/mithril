//go:build !linux

package lightbringer

import "syscall"

// childSysProcAttr returns SysProcAttr without Pdeathsig.
// Pdeathsig is Linux-only. On other platforms (macOS for development),
// child cleanup relies on the signal handler and deferred Stop().
func childSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}
