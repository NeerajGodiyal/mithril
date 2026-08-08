package notifier

import (
	"bufio"
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
		err := filepath.WalkDir(path, func(name string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !entry.Type().IsRegular() {
				return nil
			}
			switch strings.ToLower(filepath.Ext(name)) {
			case ".yml", ".yaml", ".json", ".toml":
			default:
				return nil
			}
			found, scanErr := scanPlaceholders(name)
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
func scanPlaceholders(name string) ([]string, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var found []string
	scanner := bufio.NewScanner(io.LimitReader(file, maxDeployScanBytes))
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for line := 1; scanner.Scan(); line++ {
		if strings.Contains(scanner.Text(), DeployPlaceholder) {
			found = append(found, fmt.Sprintf("%s:%d", name, line))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return found, nil
}
