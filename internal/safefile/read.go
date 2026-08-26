package safefile

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

var (
	ErrInvalidPath    = errors.New("path must be a clean absolute path")
	ErrUnavailable    = errors.New("file is unavailable")
	ErrSymlink        = errors.New("path must not traverse a symlink")
	ErrNotRegular     = errors.New("file must be a regular file")
	ErrChanged        = errors.New("file changed while opening")
	ErrUntrustedOwner = errors.New("file owner is not trusted")
	ErrPermissions    = errors.New("file permissions are too broad")
	ErrTooLarge       = errors.New("file is too large")
	ErrUnreadable     = errors.New("file is unreadable")
	ErrUnsupported    = errors.New("trusted file reads are unsupported on this platform")
	ErrInvalidOptions = errors.New("trusted file read policy is invalid")
)

const maxTrustedFileBytes = 1 << 30

// ReadOptions bounds a trusted regular-file read.
type ReadOptions struct {
	MaxBytes               int64
	ForbiddenPerm          os.FileMode
	RejectAncestorSymlinks bool
}

// OpenStableRegular opens a regular file without following any symlink and
// confirms that the opened object is the one observed at path.
func OpenStableRegular(path string) (*os.File, error) {
	file, _, err := openStableRegular(path, true)
	return file, err
}

// ReadTrustedRegular reads one trusted file object without following its final
// symlink. RejectAncestorSymlinks extends that rule to every path component.
// The opened object is checked against the path observation, owner and
// permission policy, and the configured size bound.
func ReadTrustedRegular(path string, options ReadOptions) ([]byte, error) {
	if options.MaxBytes <= 0 || options.MaxBytes > maxTrustedFileBytes ||
		(options.ForbiddenPerm != 0o022 && options.ForbiddenPerm != 0o077) {
		return nil, ErrInvalidOptions
	}
	file, info, err := openStableRegular(path, options.RejectAncestorSymlinks)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	if !OwnerTrusted(info) {
		return nil, ErrUntrustedOwner
	}
	if info.Mode().Perm()&options.ForbiddenPerm != 0 {
		return nil, ErrPermissions
	}
	if info.Size() > options.MaxBytes {
		return nil, ErrTooLarge
	}

	data, err := io.ReadAll(io.LimitReader(file, options.MaxBytes+1))
	if err != nil {
		return nil, ErrUnreadable
	}
	if int64(len(data)) > options.MaxBytes {
		return nil, ErrTooLarge
	}
	return data, nil
}

func openStableRegular(path string, rejectAncestorSymlinks bool) (*os.File, os.FileInfo, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, nil, ErrInvalidPath
	}
	if rejectAncestorSymlinks {
		if err := ValidateNoSymlinkPath(path); err != nil {
			return nil, nil, err
		}
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, nil, ErrUnavailable
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 {
		return nil, nil, ErrSymlink
	}
	if !pathInfo.Mode().IsRegular() {
		return nil, nil, ErrNotRegular
	}
	file, err := openTrustedRegular(path)
	if err != nil {
		if errors.Is(err, errPlatformUnsupported) {
			return nil, nil, ErrUnsupported
		}
		return nil, nil, ErrUnavailable
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, ErrUnreadable
	}
	if !os.SameFile(pathInfo, info) {
		_ = file.Close()
		return nil, nil, ErrChanged
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, nil, ErrNotRegular
	}
	return file, info, nil
}
