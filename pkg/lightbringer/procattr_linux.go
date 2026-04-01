//go:build linux

package lightbringer

import "syscall"

// childSysProcAttr returns SysProcAttr with Pdeathsig set.
// On Linux, Pdeathsig makes the kernel send SIGTERM to the child when the
// parent's spawning thread dies — surviving os.Exit, SIGKILL, panic, etc.
func childSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGTERM,
	}
}
