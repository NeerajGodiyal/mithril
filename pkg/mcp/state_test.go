package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corestate "github.com/Overclock-Validator/mithril/pkg/state"
)

type cancelAfterRead struct {
	reader io.Reader
	cancel context.CancelFunc
}

func (r *cancelAfterRead) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.cancel()
	return n, err
}

func TestParseStateSimulatorFormat(t *testing.T) {
	var s ShutdownState
	js := `{"last_slot":285000100,"last_epoch":660,"last_bankhash":"Sim","snapshot_slot":284500000,"stage":"live","last_shutdown_reason":"Normal","cluster":"mainnet-beta","manifest_transaction_count":284700}`
	if err := json.Unmarshal([]byte(js), &s); err != nil {
		t.Fatal(err)
	}
	if s.LastSlot == nil || *s.LastSlot != 285000100 {
		t.Errorf("last_slot = %v", s.LastSlot)
	}
	if r, ok := s.parsedShutdownReason(); !ok || r != shutdownNormal {
		t.Errorf("reason = %v/%v", r, ok)
	}
	if s.isErrorShutdown() {
		t.Error("normal shutdown should not be error")
	}
	if s.Cluster == nil || *s.Cluster != "mainnet-beta" {
		t.Errorf("cluster = %v", s.Cluster)
	}
}

func TestParseStateRealFormat(t *testing.T) {
	var s ShutdownState
	js := `{"state_schema_version":2,"stage":"ready","snapshot_slot":419496100,"snapshot_epoch":970,"build_mode":"auto","build_started_at":"2026-07-20T01:00:00Z","build_completed_at":"2026-07-20T01:10:00Z","cluster":"mainnet-beta","genesis_hash":"` + testHash + `","last_shutdown_reason":"graceful shutdown (Ctrl+C)"}`
	if err := json.Unmarshal([]byte(js), &s); err != nil {
		t.Fatal(err)
	}
	if s.StateSchemaVersion == nil || *s.StateSchemaVersion != 2 {
		t.Errorf("schema version = %v", s.StateSchemaVersion)
	}
	if r, ok := s.parsedShutdownReason(); !ok || r != shutdownNormal {
		t.Errorf("real graceful reason = %v/%v", r, ok)
	}
	summary := summarizeShutdownState(&s)
	if summary.SnapshotEpoch == nil || *summary.SnapshotEpoch != 970 || summary.BuildStartedAt == nil || summary.BuildCompletedAt == nil {
		t.Fatalf("bootstrap evidence missing from summary: %+v", summary)
	}
}

func TestStateSummaryOmitsCoreZeroTimestamps(t *testing.T) {
	data, err := json.Marshal(corestate.NewReadyState(42, 3, "", "", 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	var state ShutdownState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	summary := summarizeShutdownState(&state)
	if summary.BuildStartedAt != nil || summary.CorruptionDetectedAt != nil || summary.LastShutdownAt != nil {
		t.Fatalf("zero timestamps must be absent from MCP evidence: %+v", summary)
	}
	if summary.BuildCompletedAt == nil {
		t.Fatal("real build completion timestamp was omitted")
	}
}

func TestParseStateReplayError(t *testing.T) {
	var s ShutdownState
	if err := json.Unmarshal([]byte(`{"last_shutdown_reason":"replay error: bank hash mismatch at slot 12345"}`), &s); err != nil {
		t.Fatal(err)
	}
	if r, ok := s.parsedShutdownReason(); !ok || r != shutdownError {
		t.Errorf("replay error = %v/%v", r, ok)
	}
	if !s.isErrorShutdown() {
		t.Error("replay error should be error shutdown")
	}
}

func TestParseStateUnknownReason(t *testing.T) {
	var s ShutdownState
	if err := json.Unmarshal([]byte(`{"last_shutdown_reason":"weird future reason"}`), &s); err != nil {
		t.Fatal(err)
	}
	if *s.LastShutdownReason != "weird future reason" {
		t.Error("raw reason should be preserved")
	}
	if _, ok := s.parsedShutdownReason(); ok {
		t.Error("unknown reason should not parse")
	}
}

func TestParseStatePreservesExtras(t *testing.T) {
	var s ShutdownState
	js := `{"last_slot":100,"future_field":"preserved","manifest_accts_lt_hash":"blob"}`
	if err := json.Unmarshal([]byte(js), &s); err != nil {
		t.Fatal(err)
	}
	if s.LastSlot == nil || *s.LastSlot != 100 {
		t.Errorf("last_slot = %v", s.LastSlot)
	}
	if _, ok := s.Extra["future_field"]; !ok {
		t.Error("future_field should be in extra")
	}
	if _, ok := s.Extra["manifest_accts_lt_hash"]; !ok {
		t.Error("lt_hash field should be in extra")
	}
	// Round-trip: extras survive re-marshal (flatten).
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "future_field") || !strings.Contains(string(out), "manifest_accts_lt_hash") {
		t.Errorf("marshal should re-flatten extras: %s", out)
	}
}

func TestParseStateDoesNotProcessOmittedExtraValues(t *testing.T) {
	const raw = `{"future_field":{"token":"raw-secret","url":"https://rpc.example/raw-secret"}}`
	var state ShutdownState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatal(err)
	}
	if got := string(state.Extra["future_field"]); !strings.Contains(got, "raw-secret") {
		t.Fatalf("omitted value was transformed during parsing: %s", got)
	}
	summary, err := json.Marshal(summarizeShutdownState(&state))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(summary), "raw-secret") {
		t.Fatalf("omitted value leaked through summary: %s", summary)
	}
}

func TestReadStateSizeGuard(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.json")
	if err := os.WriteFile(p, make([]byte, maxStateFileBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readShutdownStateContext(context.Background(), p); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("expected too-large error, got %v", err)
	}
}

func TestReadStateParseErrorDoesNotEchoLargeNumericToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "invalid.json")
	token := strings.Repeat("9", 64*1024)
	if err := os.WriteFile(path, []byte(`{"last_slot":`+token+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := readShutdownStateContext(context.Background(), path)
	if err == nil || err.Error() != "failed to parse state file" || strings.Contains(err.Error(), token[:64]) {
		t.Fatalf("state parse error exposed input: %v", err)
	}
}

func TestParseStateOperationalEvidenceFields(t *testing.T) {
	var s ShutdownState
	js := `{"stage":"corrupted","last_block_height":42,"corruption_reason":"bankhash marker mismatch","corruption_detected_at":"2026-07-13T01:02:03Z"}`
	if err := json.Unmarshal([]byte(js), &s); err != nil {
		t.Fatal(err)
	}
	if s.LastBlockHeight == nil || *s.LastBlockHeight != 42 {
		t.Fatalf("last_block_height = %v", s.LastBlockHeight)
	}
	if s.CorruptionReason == nil || *s.CorruptionReason != "bankhash marker mismatch" {
		t.Fatalf("corruption_reason = %v", s.CorruptionReason)
	}
	if s.CorruptionDetectedAt == nil || *s.CorruptionDetectedAt != "2026-07-13T01:02:03Z" {
		t.Fatalf("corruption_detected_at = %v", s.CorruptionDetectedAt)
	}
	for _, key := range []string{"last_block_height", "corruption_reason", "corruption_detected_at"} {
		if _, ok := s.Extra[key]; ok {
			t.Errorf("named evidence field %q must not also appear in Extra", key)
		}
	}
}

func TestReadStateRejectsNonRegularFile(t *testing.T) {
	_, err := readShutdownStateContext(context.Background(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("expected regular-file error, got %v", err)
	}
}

func TestReadStateMissingReturnsNil(t *testing.T) {
	s, err := readShutdownStateContext(context.Background(), filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || s != nil {
		t.Errorf("missing file should be (nil,nil), got %v/%v", s, err)
	}
}

func TestReadStateHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readShutdownStateContext(ctx, filepath.Join(t.TempDir(), "state.json")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read error = %v, want context.Canceled", err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	reader := &contextReader{
		ctx: ctx,
		reader: &cancelAfterRead{
			reader: strings.NewReader(strings.Repeat("x", 1024)),
			cancel: cancel,
		},
	}
	if _, err := io.ReadAll(reader); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-read cancellation error = %v, want context.Canceled", err)
	}
}

func TestStateToolReturnsBoundedSummaryForLargeProductionState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mithril_state.json")
	secret := "TOP_SECRET_STATE_BLOB"
	data, err := json.Marshal(map[string]any{
		"state_schema_version":  2,
		"stage":                 "ready",
		"last_slot":             100,
		"computed_epoch_stakes": secret + strings.Repeat("x", 2*1024*1024),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	session := startInMemorySession(t, Config{StatePath: path})
	text, isError := callToolText(t, session, "mithril_read_shutdown_state", nil)
	if isError {
		t.Fatalf("bounded state summary was rejected: %s", text)
	}
	if strings.Contains(text, secret) || strings.Contains(text, strings.Repeat("x", 4096)) {
		t.Fatal("state tool leaked a large omitted field")
	}
	for _, want := range []string{`"schema_supported":true`, `"omitted_extra_field_count":1`, `"computed_epoch_stakes"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("state summary missing %s: %s", want, text)
		}
	}
}
