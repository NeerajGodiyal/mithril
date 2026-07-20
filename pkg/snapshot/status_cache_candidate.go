package snapshot

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/Overclock-Validator/mithril/pkg/txstatus"
)

const (
	agaveStatusCacheArchiveMember = "snapshots/status_cache"
	maxAgaveStatusCacheSize       = int64(32 * 1024 * 1024 * 1024)
)

func retainedStatusCachePath(accountsDbDir string) string {
	return filepath.Join(accountsDbDir, txstatus.SnapshotSeedFileName)
}

// statusCacheCandidate retains an extracted member under a temporary name
// until the complete archive has been consumed. A truncated incremental can
// therefore never replace the valid seed retained from its full snapshot.
type statusCacheCandidate struct {
	destination string
	temporary   string
	found       bool
}

func newStatusCacheCandidate(destination string) *statusCacheCandidate {
	return &statusCacheCandidate{destination: destination}
}

func (c *statusCacheCandidate) capture(header *tar.Header, src io.Reader) (handled bool, written int64, err error) {
	if c == nil || c.destination == "" || header == nil {
		return false, 0, nil
	}
	name := path.Clean(strings.TrimPrefix(header.Name, "./"))
	if name != agaveStatusCacheArchiveMember {
		return false, 0, nil
	}
	if c.found {
		return true, 0, fmt.Errorf("snapshot archive contains duplicate %s members", agaveStatusCacheArchiveMember)
	}
	if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
		return true, 0, fmt.Errorf("snapshot %s member is not a regular file", agaveStatusCacheArchiveMember)
	}
	if header.Size < 8 {
		return true, 0, fmt.Errorf("snapshot %s member is too small: %d bytes", agaveStatusCacheArchiveMember, header.Size)
	}
	if header.Size > maxAgaveStatusCacheSize {
		return true, 0, fmt.Errorf("snapshot %s member is too large: %d bytes (max %d)", agaveStatusCacheArchiveMember, header.Size, maxAgaveStatusCacheSize)
	}

	dir := filepath.Dir(c.destination)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return true, 0, fmt.Errorf("create status-cache destination directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".snapshot-status-cache-*.partial")
	if err != nil {
		return true, 0, fmt.Errorf("create temporary status cache: %w", err)
	}
	tmpPath := tmp.Name()
	keep := false
	closed := false
	defer func() {
		if !closed {
			if closeErr := tmp.Close(); err == nil && closeErr != nil {
				err = fmt.Errorf("close temporary status cache: %w", closeErr)
			}
		}
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()

	written, err = io.Copy(tmp, src)
	if err != nil {
		return true, written, fmt.Errorf("extract snapshot status cache: %w", err)
	}
	if written != header.Size {
		return true, written, fmt.Errorf("extract snapshot status cache: copied %d bytes, expected %d", written, header.Size)
	}
	if err = tmp.Sync(); err != nil {
		return true, written, fmt.Errorf("sync temporary status cache: %w", err)
	}
	if err = tmp.Chmod(0o644); err != nil {
		return true, written, fmt.Errorf("chmod temporary status cache: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return true, written, fmt.Errorf("close temporary status cache: %w", err)
	}
	closed = true
	c.temporary = tmpPath
	c.found = true
	keep = true
	return true, written, nil
}

func (c *statusCacheCandidate) commit() error {
	if c == nil || c.destination == "" {
		return nil
	}
	if !c.found || c.temporary == "" {
		return fmt.Errorf("snapshot archive is missing required %s member", agaveStatusCacheArchiveMember)
	}
	if err := os.Rename(c.temporary, c.destination); err != nil {
		return fmt.Errorf("install snapshot status cache: %w", err)
	}
	c.temporary = ""
	dir, err := os.Open(filepath.Dir(c.destination))
	if err != nil {
		return fmt.Errorf("open status-cache destination directory: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync status-cache destination directory: %w", err)
	}
	return nil
}

func (c *statusCacheCandidate) cleanup() {
	if c == nil || c.temporary == "" {
		return
	}
	_ = os.Remove(c.temporary)
	c.temporary = ""
}
