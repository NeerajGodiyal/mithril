//go:build unix

package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Overclock-Validator/mithril/internal/safefile"
	"golang.org/x/sys/unix"
)

func validateControlDirectory(path string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return errors.New("control state directory must not traverse a symlink")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("control state directory is unavailable")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("control state directory must be a real directory")
	}
	if !safefile.OwnerTrusted(info) {
		return errors.New("control state directory owner is not trusted")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("control state directory permissions are too broad")
	}
	return nil
}

func lockControlState(ctx context.Context, path string) (func(), error) {
	fd, err := unix.Open(
		path,
		unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
		0o600,
	)
	if err != nil {
		return nil, errors.New("open control state lock")
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open control state lock")
	}
	closeFile := func() {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = file.Close()
	}
	if err := file.Chmod(0o600); err != nil {
		closeFile()
		return nil, errors.New("set control state lock permissions")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !safefile.OwnerTrusted(info) ||
		info.Mode().Perm()&0o077 != 0 {
		closeFile()
		return nil, errors.New("control state lock is not trusted")
	}

	timer := time.NewTicker(10 * time.Millisecond)
	defer timer.Stop()
	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return closeFile, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			closeFile()
			return nil, errors.New("lock control state")
		}
		select {
		case <-ctx.Done():
			closeFile()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func readControlFile(path string, maxBytes int64) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, os.ErrNotExist
		}
		return nil, errors.New("open control operation state")
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open control operation state")
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !safefile.OwnerTrusted(info) ||
		info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("control operation state is not trusted")
	}
	if info.Size() > maxBytes {
		return nil, errors.New("control operation state exceeds its size limit")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(raw)) > maxBytes {
		return nil, errors.New("read control operation state")
	}
	return raw, nil
}

func writeControlFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return errors.New("open control state directory")
	}
	defer root.Close()

	base := filepath.Base(path)
	temp := fmt.Sprintf(".%s.tmp-%d", base, time.Now().UnixNano())
	file, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return errors.New("create control state temporary file")
	}
	complete := false
	defer func() {
		if !complete {
			_ = file.Close()
			_ = root.Remove(temp)
		}
	}()
	if err := file.Chmod(mode); err != nil {
		return errors.New("set control state file permissions")
	}
	if _, err := file.Write(data); err != nil {
		return errors.New("write control state file")
	}
	if err := file.Sync(); err != nil {
		return errors.New("sync control state file")
	}
	if err := file.Close(); err != nil {
		return errors.New("close control state file")
	}
	if err := root.Rename(temp, base); err != nil {
		return errors.New("replace control state file")
	}
	if err := syncControlDirectory(dir); err != nil {
		return err
	}
	complete = true
	return nil
}

func syncControlDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return errors.New("open control state directory")
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return errors.New("sync control state directory")
	}
	return nil
}
