//go:build unix

package safefile

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

var errPlatformUnsupported = errors.New("unsupported platform")

func openTrustedRegular(path string) (*os.File, error) {
	fd, err := unix.Open(
		path,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
		0,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, os.ErrInvalid
	}
	return file, nil
}
