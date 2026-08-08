//go:build unix

package controlaudit

import (
	"os"

	"golang.org/x/sys/unix"
)

func createStoreObject(path string) (*os.File, error) {
	fd, err := unix.Open(
		path,
		unix.O_RDWR|unix.O_APPEND|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CREAT|unix.O_EXCL,
		0o600,
	)
	if err != nil {
		return nil, err
	}
	return lockedStoreFile(fd, path, unix.LOCK_EX|unix.LOCK_NB)
}

func openExistingStoreObject(path string, writable bool) (*os.File, error) {
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	lock := unix.LOCK_SH | unix.LOCK_NB
	if writable {
		flags = unix.O_RDWR | unix.O_APPEND | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
		lock = unix.LOCK_EX | unix.LOCK_NB
	}
	fd, err := unix.Open(path, flags, 0)
	if err != nil {
		return nil, err
	}
	return lockedStoreFile(fd, path, lock)
}

func lockedStoreFile(fd int, path string, lock int) (*os.File, error) {
	if err := unix.Flock(fd, lock); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, os.ErrInvalid
	}
	return file, nil
}
