package notifier

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DeployPlaceholder marks a value the shipped monitoring templates expect a
// deployment to replace.
const DeployPlaceholder = "replace-at-deploy"

const (
	maxDeployScanBytes = 4 << 20
	maxDeployFindings  = 32
)

// VerifyDeployConfig reports every remaining placeholder under the given paths.
//
// An unreplaced placeholder is not cosmetic. The shipped deadman receiver
// points at an unroutable host, so Alertmanager retries it every minute
// forever, and the resulting delivery-failure counter makes the pipeline-health
// alert page continuously. Failing to start is the quieter outcome.
func VerifyDeployConfig(paths ...string) error {
	var findings []string
	for _, path := range paths {
		if path == "" {
			continue
		}
		resolvedRoot, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("inspect monitoring config: %w", err)
		}
		err = filepath.WalkDir(resolvedRoot, func(name string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			var scanName string
			if entry.Type()&os.ModeSymlink != 0 {
				resolved, resolveErr := filepath.EvalSymlinks(name)
				if resolveErr != nil {
					return resolveErr
				}
				target, statErr := os.Stat(resolved)
				if statErr != nil {
					return statErr
				}
				if target.IsDir() {
					return fmt.Errorf("%s is a symlinked directory; pass its resolved path explicitly", name)
				}
				if !target.Mode().IsRegular() {
					return nil
				}
				scanName = resolved
			}
			if entry.IsDir() {
				return nil
			}
			switch strings.ToLower(filepath.Ext(name)) {
			case ".yml", ".yaml", ".json", ".toml":
			default:
				return nil
			}
			if scanName == "" {
				scanName = name
			}
			if entry.Type()&os.ModeSymlink == 0 && !entry.Type().IsRegular() {
				return nil
			}
			found, scanErr := scanPlaceholders(scanName, name)
			if scanErr != nil {
				return scanErr
			}
			findings = append(findings, found...)
			return nil
		})
		if err != nil {
			return fmt.Errorf("inspect monitoring config: %w", err)
		}
	}
	if len(findings) == 0 {
		return nil
	}
	sort.Strings(findings)
	if len(findings) > maxDeployFindings {
		findings = append(findings[:maxDeployFindings], "...")
	}
	return fmt.Errorf(
		"monitoring config still contains %q; replace every one before starting:\n  %s",
		DeployPlaceholder, strings.Join(findings, "\n  "))
}

// scanPlaceholders returns "file:line" for each placeholder. Only the location
// is reported: the surrounding line can carry an endpoint or a credential.
func scanPlaceholders(name, displayName string) ([]string, error) {
	pathInfo, err := os.Stat(name)
	if err != nil {
		return nil, err
	}
	if !pathInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", displayName)
	}
	if pathInfo.Size() > maxDeployScanBytes {
		return nil, fmt.Errorf("%s is too large to verify", displayName)
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(pathInfo, openedInfo) {
		return nil, fmt.Errorf("%s changed while opening", displayName)
	}

	data, err := io.ReadAll(io.LimitReader(file, maxDeployScanBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxDeployScanBytes {
		return nil, fmt.Errorf("%s is too large to verify", displayName)
	}

	var found []string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for line := 1; scanner.Scan(); line++ {
		if strings.Contains(scanner.Text(), DeployPlaceholder) {
			found = append(found, fmt.Sprintf("%s:%d", displayName, line))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return found, nil
}
