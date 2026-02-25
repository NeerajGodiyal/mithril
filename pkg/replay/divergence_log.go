package replay

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/version"
)

// DivergenceRecord is a structured record written to divergence.jsonl on any divergence event.
type DivergenceRecord struct {
	Timestamp      string `json:"ts"`
	RunID          string `json:"run_id"`
	Slot           uint64 `json:"slot"`
	ParentSlot     uint64 `json:"parent_slot"`
	TxIndex        int    `json:"tx_index"`
	TxSig          string `json:"tx_sig"`
	Leader         string `json:"leader"`
	Path           string `json:"path"`
	DivergenceType string `json:"divergence_type"`
	LocalErr       string `json:"local_err"`
	OnchainErr     string `json:"onchain_err"`
	GitCommit      string `json:"git_commit"`
	GitBranch      string `json:"git_branch"`
}

var (
	divergenceLogMu   sync.Mutex
	divergenceLogFile *os.File
)

// WriteDivergenceRecord appends a single JSON record to divergence.jsonl in the run log directory.
// The file is lazily opened on first call. Each write is flushed and synced immediately.
func WriteDivergenceRecord(rec DivergenceRecord) {
	rec.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	rec.RunID = CurrentRunID
	rec.GitCommit = version.GitCommit
	rec.GitBranch = version.GitBranch

	data, err := json.Marshal(rec)
	if err != nil {
		mlog.Log.Errorf("failed to marshal divergence record: %v", err)
		return
	}
	data = append(data, '\n')

	divergenceLogMu.Lock()
	defer divergenceLogMu.Unlock()

	if divergenceLogFile == nil {
		logDir := mlog.GetLogDir()
		if logDir == "" {
			mlog.Log.Errorf("divergence record: log directory not initialized")
			return
		}
		path := filepath.Join(logDir, "divergence.jsonl")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			mlog.Log.Errorf("failed to open divergence.jsonl: %v", err)
			return
		}
		divergenceLogFile = f
	}

	if _, err := divergenceLogFile.Write(data); err != nil {
		mlog.Log.Errorf("failed to write divergence record: %v", err)
		return
	}
	divergenceLogFile.Sync()
}

// divergenceArtifactPointer logs a terminal pointer to the divergence artifact file.
func divergenceArtifactPointer() {
	mlog.Log.Errorf("  -> divergence artifact: %s", filepath.Join(mlog.GetLogDir(), "divergence.jsonl"))
}

// errString returns a string representation of an error, or empty string for nil.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// fmtErr returns a string from fmt.Sprintf("%+v", v), or empty string for nil.
func fmtErr(v interface{}) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%+v", v)
}
