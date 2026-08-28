package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
)

type classicReplayMarker struct {
	Version      uint32 `json:"version"`
	RunID        string `json:"run_id"`
	StartingSlot uint64 `json:"starting_slot"`
}

type classicReplayGuard struct {
	path string
}

func classicReplayMarkerRequired(alpenglowMode, rootedDurableMode bool) bool {
	return !alpenglowMode && !rootedDurableMode
}

func classicReplayWasInterrupted(accountsPath string) (bool, error) {
	_, err := os.Lstat(filepath.Join(accountsPath, accountsdb.ClassicReplayMarkerName))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("inspect classic replay marker: %w", err)
}

func beginClassicReplay(accountsPath, runID string, startingSlot uint64) (*classicReplayGuard, error) {
	if accountsPath == "" || runID == "" {
		return nil, errors.New("classic replay marker requires an accounts path and run ID")
	}
	path := filepath.Join(accountsPath, accountsdb.ClassicReplayMarkerName)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, errors.New("an earlier classic replay did not shut down cleanly; re-bootstrap from a snapshot")
		}
		return nil, fmt.Errorf("create classic replay marker: %w", err)
	}
	payload, encodeErr := json.Marshal(classicReplayMarker{
		Version: 1, RunID: runID, StartingSlot: startingSlot,
	})
	writeErr := error(nil)
	if encodeErr != nil {
		writeErr = encodeErr
	} else if _, err := file.Write(payload); err != nil {
		writeErr = err
	} else if err := file.Sync(); err != nil {
		writeErr = err
	}
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return nil, fmt.Errorf("persist classic replay marker: %w", err)
	}
	if err := syncDirectory(accountsPath); err != nil {
		return nil, fmt.Errorf("persist classic replay marker directory: %w", err)
	}
	return &classicReplayGuard{path: path}, nil
}

func (guard *classicReplayGuard) Complete() error {
	if guard == nil || guard.path == "" {
		return errors.New("classic replay marker is unavailable")
	}
	if err := os.Remove(guard.path); err != nil {
		return fmt.Errorf("remove classic replay marker: %w", err)
	}
	if err := syncDirectory(filepath.Dir(guard.path)); err != nil {
		return fmt.Errorf("persist classic replay marker removal: %w", err)
	}
	guard.path = ""
	return nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
