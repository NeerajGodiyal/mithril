package safefile

import (
	"fmt"
	"os"
	"path/filepath"
)

// ValidateNoSymlinkPath requires every existing component of path, including
// the final component, to be a non-symlink. Ancestors must be directories.
func ValidateNoSymlinkPath(path string) error {
	return validateNoSymlinkPath(path, true)
}

// ValidateNoSymlinkAncestors applies the same policy to the parent of path.
// It is intended for a final object that may not exist yet.
func ValidateNoSymlinkAncestors(path string) error {
	return validateNoSymlinkPath(path, false)
}

func validateNoSymlinkPath(path string, includeLeaf bool) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrInvalidPath
	}
	current := path
	if !includeLeaf {
		current = filepath.Dir(current)
	}

	components := make([]string, 0, 8)
	for {
		components = append(components, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	for index := len(components) - 1; index >= 0; index-- {
		info, err := os.Lstat(components[index])
		if err != nil {
			return ErrUnavailable
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// Naming the component matters more here than anywhere else this
			// refusal fires: the offender is usually an ANCESTOR, not the file
			// the operator typed, and the bare sentinel gave them no way to
			// tell which. /var/run is a symlink to /run on every mainstream
			// distribution, so a perfectly ordinary
			// "/var/run/credentials/<unit>/secret" is refused while the same
			// file under "/run/..." is accepted — indistinguishable without
			// this, and the resolved path is the whole answer.
			if target, linkErr := os.Readlink(components[index]); linkErr == nil {
				return fmt.Errorf("%w: %s is a symlink to %s; use the resolved path",
					ErrSymlink, components[index], target)
			}
			return fmt.Errorf("%w: %s is a symlink", ErrSymlink, components[index])
		}
		if (index != 0 || !includeLeaf) && !info.IsDir() {
			return ErrUnavailable
		}
	}
	return nil
}
