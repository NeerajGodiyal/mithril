package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/overcast"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const (
	lightbringerProbeMessages      = 3
	lightbringerProbeTimeout       = 6 * time.Second
	lightbringerClockSkewTolerance = time.Second
	lightbringerIngestWindow       = 5 * time.Minute
	lightbringerMemoryWindow       = 15 * time.Minute
	lightbringerMemoryFreshness    = 30 * time.Second
	maxInfluxSummaryBytes          = 64 * 1024
	maxProbeErrorRunes             = 256
)

func resolveSafeDialTargets(ctx context.Context, addr string, resolver ipResolver) ([]string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid address %q: %w", addr, err)
	}
	if strings.TrimSpace(host) == "" {
		return nil, fmt.Errorf("invalid address %q: host is empty", addr)
	}
	portNum, err := strconv.Atoi(port)
	if err != nil || portNum < 1 || portNum > 65535 {
		return nil, fmt.Errorf("invalid address %q: port must be between 1 and 65535", addr)
	}
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip) {
			return nil, fmt.Errorf("address resolves to blocked range: %s", ip)
		}
		if !ip.IsLoopback() {
			return nil, errors.New("lightbringer insecure gRPC is permitted only on loopback")
		}
		return []string{net.JoinHostPort(ip.String(), port)}, nil
	}
	addrs, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve address hostname %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("address hostname %q resolved to no addresses", host)
	}
	targets := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if isBlockedIP(a.IP) {
			return nil, fmt.Errorf("hostname resolves to blocked range: %s", a.IP)
		}
		if !a.IP.IsLoopback() {
			return nil, errors.New("lightbringer insecure gRPC hostname must resolve only to loopback")
		}
		targets = append(targets, net.JoinHostPort(a.IP.String(), port))
	}
	return targets, nil
}

func pinnedGRPCDialer(targets []string) func(context.Context, string) (net.Conn, error) {
	return func(ctx context.Context, _ string) (net.Conn, error) {
		dialer := &net.Dialer{}
		var lastErr error
		for _, target := range targets {
			conn, err := dialer.DialContext(ctx, "tcp", target)
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("no validated Lightbringer dial targets")
		}
		return nil, lastErr
	}
}

type streamProbeInput struct{}

type streamProbeSample struct {
	Slot       uint64 `json:"slot"`
	ParentSlot uint64 `json:"parent_slot"`
}

type streamProbeOutput struct {
	GRPCAddr         string              `json:"grpc_addr"`
	State            string              `json:"state"` // active | no_progress | incomplete | backend_error | unreachable | inconclusive
	Reachable        bool                `json:"reachable"`
	CompleteSample   bool                `json:"complete_sample"`
	ActivityObserved bool                `json:"activity_observed"`
	SlotsSeen        int                 `json:"slots_seen"`
	DistinctSlots    int                 `json:"distinct_slots"`
	Samples          []streamProbeSample `json:"samples"`
	TerminalError    string              `json:"terminal_error,omitempty"`
}

func boundedProbeError(prefix string, err error) string {
	msg := redactUntrustedText(err.Error())
	if prefix != "" {
		msg = prefix + ": " + msg
	}
	if rs := []rune(msg); len(rs) > maxProbeErrorRunes {
		msg = string(rs[:maxProbeErrorRunes-1]) + "…"
	}
	return msg
}

// emptyStreamState distinguishes a probe that simply observed no data before
// its deadline from an explicit backend/transport failure. A successfully
// created client stream does not prove the server was reached: gRPC establishes
// it lazily on Recv, so deadline/cancellation and clean EOF remain inconclusive.
func emptyStreamState(ctx context.Context, err error) string {
	if ctx.Err() != nil || errors.Is(err, io.EOF) {
		return "inconclusive"
	}
	switch status.Code(err) {
	case codes.Canceled, codes.DeadlineExceeded, codes.ResourceExhausted:
		return "inconclusive"
	case codes.Unavailable:
		return "unreachable"
	default:
		return "backend_error"
	}
}

func callerContextError(ctx context.Context, transportErr error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	code := status.Code(transportErr)
	if code != codes.Canceled && code != codes.DeadlineExceeded {
		return nil
	}
	// gRPC can observe the shared deadline just before the parent context's
	// timer publishes Err(). Use the caller's own deadline to close that race.
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

// lightbringerStreamProbe samples the sidecar stream for liveness. Overcast
// publishes slots as they complete, so delivery order is not chain order.
func lightbringerStreamProbe(ctx context.Context, grpcAddr string) (streamProbeOutput, error) {
	out := streamProbeOutput{
		GRPCAddr: grpcAddr,
		State:    "inconclusive",
		Samples:  []streamProbeSample{},
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	// Keep policy failures distinct from an offline sidecar.
	pctx, cancel := context.WithTimeout(ctx, lightbringerProbeTimeout)
	defer cancel()
	targets, err := resolveSafeDialTargets(pctx, grpcAddr, net.DefaultResolver)
	if err != nil {
		return out, err
	}

	conn, err := grpc.NewClient(grpcAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(pinnedGRPCDialer(targets)),
	)
	if err != nil {
		out.TerminalError = boundedProbeError("dial config error", err)
		out.State = "unreachable"
		return out, nil
	}
	defer conn.Close()

	stream, err := overcast.NewSlotStreamClient(conn).StreamSlots(pctx, &overcast.SlotStreamRequest{})
	if err != nil {
		if ctxErr := callerContextError(ctx, err); ctxErr != nil {
			return out, ctxErr
		}
		out.TerminalError = boundedProbeError("stream open failed", err)
		out.State = emptyStreamState(pctx, err)
		out.Reachable = out.State == "backend_error"
		return out, nil
	}

	for i := 0; i < lightbringerProbeMessages; i++ {
		resp, err := stream.Recv()
		if err != nil {
			if ctxErr := callerContextError(ctx, err); ctxErr != nil {
				return out, ctxErr
			}
			out.TerminalError = boundedProbeError("stream ended before probe completed", err)
			if len(out.Samples) == 0 {
				out.State = emptyStreamState(pctx, err)
			} else if status.Code(err) == codes.ResourceExhausted {
				out.State = "inconclusive"
			}
			break
		}
		out.Samples = append(out.Samples, streamProbeSample{Slot: resp.GetSlot(), ParentSlot: resp.GetParentSlot()})
		out.State = "incomplete"
	}
	out.SlotsSeen = len(out.Samples)
	out.Reachable = len(out.Samples) > 0 || out.State == "backend_error"
	out.CompleteSample = len(out.Samples) == lightbringerProbeMessages
	seen := make(map[uint64]struct{}, len(out.Samples))
	for _, sample := range out.Samples {
		seen[sample.Slot] = struct{}{}
	}
	out.DistinctSlots = len(seen)
	out.ActivityObserved = len(out.Samples) > 0
	if !out.CompleteSample {
		return out, nil
	}
	if out.DistinctSlots >= 2 {
		out.State = "active"
	} else {
		out.State = "no_progress"
	}
	return out, nil
}

type ingestHealthInput struct{}

func appendObservationNote(note *string, message string) {
	if *note != "" {
		*note += "; "
	}
	*note += message
}

type ingestHealthOutput struct {
	InfluxURL                          string   `json:"influxdb_url"`
	ObservationState                   string   `json:"observation_state"` // observed | stale | no_completion_data
	ClockSkewAdjusted                  bool     `json:"clock_skew_adjusted,omitempty"`
	LastCompletionAgeSec               *float64 `json:"last_completion_age_seconds,omitempty"`
	LatestCompletedSlot                *uint64  `json:"latest_completed_slot,omitempty"`
	WindowSeconds                      int      `json:"window_seconds"`
	CompletedSlots                     uint64   `json:"completed_slots"`
	CompletionRatePerSecond            float64  `json:"completion_rate_per_second"`
	WindowRepairStartedSlots           uint64   `json:"window_repair_started_slots"`
	WindowRepairedCompletedSlots       uint64   `json:"window_repaired_completed_slots"`
	WindowUnresolvedRepairSlots        uint64   `json:"window_unresolved_repair_slots"`
	WindowRepairSharePercent           *float64 `json:"window_repair_share_percent,omitempty"`
	WindowCompletedRepairDurationP95Ms *float64 `json:"window_completed_repair_duration_p95_ms,omitempty"`
	WindowOldestUnresolvedRepairAgeSec *float64 `json:"window_oldest_unresolved_repair_age_seconds,omitempty"`
	Note                               string   `json:"note,omitempty"`
}

const lightbringerCompletionSQL = `WITH recent_completions AS (
  SELECT slot, MIN(time) AS completion_time
  FROM slot
  WHERE kind = 'completion' AND time >= now() - INTERVAL '5 minutes'
  GROUP BY slot
),
recent_repairs AS (
  SELECT slot, MIN(time) AS repair_time
  FROM slot
  WHERE kind = 'repair_initiate' AND time >= now() - INTERVAL '5 minutes'
  GROUP BY slot
),
repair_outcomes AS (
  SELECT
    repairs.slot,
    repairs.repair_time,
    completions.completion_time,
    CASE WHEN completions.completion_time >= repairs.repair_time
      THEN CAST((completions.completion_time - repairs.repair_time) AS BIGINT) / 1000000.0
    END AS repair_duration_ms
  FROM recent_repairs AS repairs
  LEFT JOIN recent_completions AS completions ON completions.slot = repairs.slot
),
latest_completion AS (
  SELECT time, slot
  FROM slot
  WHERE kind = 'completion'
  ORDER BY time DESC
  LIMIT 1
),
window_summary AS (
  SELECT
    (SELECT COUNT(*) FROM recent_completions) AS completed_slots,
    COUNT(slot) AS repair_started_slots,
    COUNT(CASE WHEN repair_duration_ms IS NOT NULL THEN 1 END) AS repaired_completed_slots,
    COUNT(CASE WHEN repair_duration_ms IS NULL THEN 1 END) AS unresolved_repair_slots,
    APPROX_PERCENTILE_CONT(repair_duration_ms, 0.95) AS repair_duration_p95_ms,
    MAX(CASE WHEN repair_duration_ms IS NULL
      THEN (CAST(now() AS BIGINT) - CAST(repair_time AS BIGINT)) / 1000000000.0
    END) AS oldest_unresolved_repair_age_seconds
  FROM repair_outcomes
)
SELECT
  (CAST(now() AS BIGINT) - CAST(latest.time AS BIGINT)) / 1000000000.0 AS age_seconds,
  latest.slot AS latest_slot,
  summary.completed_slots,
  summary.repair_started_slots,
  summary.repaired_completed_slots,
  summary.unresolved_repair_slots,
  summary.repair_duration_p95_ms,
  summary.oldest_unresolved_repair_age_seconds
FROM window_summary AS summary
LEFT JOIN latest_completion AS latest ON TRUE`

const lightbringerMemorySQL = `WITH samples AS (
  SELECT
    time,
    rss_bytes,
    virtual_bytes,
    ROW_NUMBER() OVER (ORDER BY time) AS oldest_rank,
    ROW_NUMBER() OVER (ORDER BY time DESC) AS newest_rank
  FROM memory
  WHERE kind = 'process' AND time >= now() - INTERVAL '15 minutes'
),
summary AS (
  SELECT
    COUNT(*) AS sample_count,
    MAX(CASE WHEN newest_rank = 1 THEN (CAST(now() AS BIGINT) - CAST(time AS BIGINT)) / 1000000000.0 END) AS latest_sample_age_seconds,
    MAX(CASE WHEN newest_rank = 1 THEN rss_bytes END) AS current_rss_bytes,
    MAX(CASE WHEN newest_rank = 1 THEN virtual_bytes END) AS current_virtual_bytes,
    MAX(rss_bytes) AS peak_rss_bytes,
    MAX(CASE WHEN newest_rank = 1 THEN rss_bytes END) - MAX(CASE WHEN oldest_rank = 1 THEN rss_bytes END) AS rss_change_bytes,
    (CAST(MAX(time) AS BIGINT) - CAST(MIN(time) AS BIGINT)) / 1000000000.0 AS observed_span_seconds
  FROM samples
)
SELECT
  sample_count,
  latest_sample_age_seconds,
  current_rss_bytes,
  current_virtual_bytes,
  peak_rss_bytes,
  rss_change_bytes,
  observed_span_seconds,
  CASE WHEN observed_span_seconds > 0
    THEN CAST(rss_change_bytes AS DOUBLE) / observed_span_seconds
  END AS rss_growth_bytes_per_second
FROM summary`

// queryInflux runs a guarded, size-capped InfluxDB SQL query.
func queryInflux(ctx context.Context, baseURL, database, token, sql string) ([]map[string]json.RawMessage, error) {
	u, err := validateURL(baseURL)
	if err != nil {
		return nil, err
	}
	u.Path = "/api/v3/query_sql"
	body, _ := json.Marshal(map[string]string{"db": database, "q": sql, "format": "json"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, sanitizeHTTPError(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := doPinnedRequest(ctx, req, outboundHTTPTimeout)
	if err != nil {
		return nil, sanitizeHTTPError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("InfluxDB query returned HTTP %d", resp.StatusCode)
	}
	data, err := readCappedBody(resp, maxInfluxSummaryBytes)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil, fmt.Errorf("InfluxDB response must be a JSON row array, got null")
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("failed to parse InfluxDB response")
	}
	return rows, nil
}

func nullableFloatField(row map[string]json.RawMessage, name string) (*float64, error) {
	raw, ok := row[name]
	if !ok || len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("InfluxDB row is missing %s", name)
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return nil, fmt.Errorf("InfluxDB row has invalid %s", name)
	}
	return &value, nil
}

func nullableUintField(row map[string]json.RawMessage, name string) (*uint64, error) {
	raw, ok := row[name]
	if !ok || len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("InfluxDB row is missing %s", name)
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var value uint64
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("InfluxDB row has invalid %s", name)
	}
	return &value, nil
}

func requiredUintField(row map[string]json.RawMessage, name string) (uint64, error) {
	value, err := nullableUintField(row, name)
	if err != nil {
		return 0, err
	}
	if value == nil {
		return 0, fmt.Errorf("InfluxDB row has null %s", name)
	}
	return *value, nil
}

func nullableInt64Field(row map[string]json.RawMessage, name string) (*int64, error) {
	raw, ok := row[name]
	if !ok || len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("InfluxDB row is missing %s", name)
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("InfluxDB row has invalid %s", name)
	}
	return &value, nil
}

func lightbringerIngestHealth(ctx context.Context, cfg Config, influxURL, database string) (ingestHealthOutput, error) {
	out := ingestHealthOutput{
		InfluxURL:        sanitizeEndpointForDisplay(influxURL),
		ObservationState: "no_completion_data",
		WindowSeconds:    int(lightbringerIngestWindow / time.Second),
	}
	token, err := credentialForTarget(cfg.LightbringerInfluxURL, influxURL, cfg.LightbringerInfluxTok)
	if err != nil {
		return out, err
	}
	rows, err := queryInflux(ctx, influxURL, database, token, lightbringerCompletionSQL)
	if err != nil {
		return out, err
	}
	if len(rows) == 0 {
		out.Note = "no completed-slot events in InfluxDB; Lightbringer may not emit metrics or may not have completed a slot"
		return out, nil
	}
	if len(rows) != 1 {
		return out, fmt.Errorf("InfluxDB completion query returned %d rows, want at most 1", len(rows))
	}
	row := rows[0]
	age, err := nullableFloatField(row, "age_seconds")
	if err != nil {
		return out, err
	}
	slot, err := nullableUintField(row, "latest_slot")
	if err != nil {
		return out, err
	}
	if (age == nil) != (slot == nil) {
		return out, fmt.Errorf("InfluxDB completion row must provide age_seconds and latest_slot together")
	}
	if age != nil && *age < -lightbringerClockSkewTolerance.Seconds() {
		return out, fmt.Errorf("InfluxDB completion row has invalid age_seconds")
	}
	if age != nil && *age < 0 {
		*age = 0
		out.ClockSkewAdjusted = true
		out.Note = "completion timestamp was up to 1 second ahead of the query clock; age was clamped to zero"
	}
	completed, err := requiredUintField(row, "completed_slots")
	if err != nil {
		return out, err
	}
	repairStarted, err := requiredUintField(row, "repair_started_slots")
	if err != nil {
		return out, err
	}
	repairedCompleted, err := requiredUintField(row, "repaired_completed_slots")
	if err != nil {
		return out, err
	}
	unresolved, err := requiredUintField(row, "unresolved_repair_slots")
	if err != nil {
		return out, err
	}
	p95, err := nullableFloatField(row, "repair_duration_p95_ms")
	if err != nil {
		return out, err
	}
	oldestUnresolved, err := nullableFloatField(row, "oldest_unresolved_repair_age_seconds")
	if err != nil {
		return out, err
	}
	if repairedCompleted > completed || repairedCompleted > repairStarted || unresolved != repairStarted-repairedCompleted {
		return out, fmt.Errorf("InfluxDB repair summary is inconsistent")
	}
	if (repairedCompleted == 0) != (p95 == nil) || p95 != nil && *p95 < 0 {
		return out, fmt.Errorf("InfluxDB row has inconsistent repair_duration_p95_ms")
	}
	if (unresolved == 0) != (oldestUnresolved == nil) {
		return out, fmt.Errorf("InfluxDB row has inconsistent oldest_unresolved_repair_age_seconds")
	}
	if oldestUnresolved != nil && *oldestUnresolved < -lightbringerClockSkewTolerance.Seconds() {
		return out, fmt.Errorf("InfluxDB row has invalid oldest_unresolved_repair_age_seconds")
	}
	if oldestUnresolved != nil && *oldestUnresolved < 0 {
		*oldestUnresolved = 0
		out.ClockSkewAdjusted = true
		appendObservationNote(&out.Note, "repair timestamp was up to 1 second ahead of the query clock; age was clamped to zero")
	}
	out.CompletedSlots = completed
	out.CompletionRatePerSecond = float64(completed) / lightbringerIngestWindow.Seconds()
	out.WindowRepairStartedSlots = repairStarted
	out.WindowRepairedCompletedSlots = repairedCompleted
	out.WindowUnresolvedRepairSlots = unresolved
	out.WindowCompletedRepairDurationP95Ms = p95
	out.WindowOldestUnresolvedRepairAgeSec = oldestUnresolved
	if completed > 0 {
		share := float64(repairedCompleted) * 100 / float64(completed)
		out.WindowRepairSharePercent = &share
	}
	if age == nil {
		appendObservationNote(&out.Note, "no completed-slot events in InfluxDB; Lightbringer may not emit metrics or may not have completed a slot")
		return out, nil
	}
	out.LastCompletionAgeSec = age
	out.LatestCompletedSlot = slot
	if completed == 0 || *age > lightbringerIngestWindow.Seconds() {
		out.ObservationState = "stale"
		appendObservationNote(&out.Note, "completion telemetry does not meet the five-minute freshness requirement")
		return out, nil
	}
	out.ObservationState = "observed"
	return out, nil
}

type lightbringerMemoryInput struct{}

type lightbringerMemoryOutput struct {
	InfluxURL            string   `json:"influxdb_url"`
	ObservationState     string   `json:"observation_state"` // observed | stale | no_memory_data
	ClockSkewAdjusted    bool     `json:"clock_skew_adjusted,omitempty"`
	WindowSeconds        int      `json:"window_seconds"`
	SampleCount          uint64   `json:"sample_count"`
	LatestSampleAgeSec   *float64 `json:"latest_sample_age_seconds,omitempty"`
	CurrentRSSBytes      *uint64  `json:"current_rss_bytes,omitempty"`
	CurrentVirtualBytes  *uint64  `json:"current_virtual_bytes,omitempty"`
	PeakRSSBytes         *uint64  `json:"peak_rss_bytes,omitempty"`
	RSSChangeBytes       *int64   `json:"rss_change_bytes,omitempty"`
	ObservedSpanSec      *float64 `json:"observed_span_seconds,omitempty"`
	RSSGrowthBytesPerSec *float64 `json:"rss_growth_bytes_per_second,omitempty"`
	Note                 string   `json:"note,omitempty"`
}

func lightbringerMemory(ctx context.Context, cfg Config, influxURL, database string) (lightbringerMemoryOutput, error) {
	out := lightbringerMemoryOutput{
		InfluxURL:        sanitizeEndpointForDisplay(influxURL),
		ObservationState: "no_memory_data",
		WindowSeconds:    int(lightbringerMemoryWindow / time.Second),
	}
	token, err := credentialForTarget(cfg.LightbringerInfluxURL, influxURL, cfg.LightbringerInfluxTok)
	if err != nil {
		return out, err
	}
	rows, err := queryInflux(ctx, influxURL, database, token, lightbringerMemorySQL)
	if err != nil {
		return out, err
	}
	if len(rows) == 0 {
		out.Note = "no process-memory samples in the 15-minute window"
		return out, nil
	}
	if len(rows) != 1 {
		return out, fmt.Errorf("InfluxDB memory query returned %d rows, want at most 1", len(rows))
	}
	row := rows[0]
	sampleCount, err := requiredUintField(row, "sample_count")
	if err != nil {
		return out, err
	}
	age, err := nullableFloatField(row, "latest_sample_age_seconds")
	if err != nil {
		return out, err
	}
	currentRSS, err := nullableUintField(row, "current_rss_bytes")
	if err != nil {
		return out, err
	}
	currentVirtual, err := nullableUintField(row, "current_virtual_bytes")
	if err != nil {
		return out, err
	}
	peakRSS, err := nullableUintField(row, "peak_rss_bytes")
	if err != nil {
		return out, err
	}
	change, err := nullableInt64Field(row, "rss_change_bytes")
	if err != nil {
		return out, err
	}
	span, err := nullableFloatField(row, "observed_span_seconds")
	if err != nil {
		return out, err
	}
	growth, err := nullableFloatField(row, "rss_growth_bytes_per_second")
	if err != nil {
		return out, err
	}
	fieldsPresent := age != nil && currentRSS != nil && currentVirtual != nil && peakRSS != nil && change != nil && span != nil
	anyFieldPresent := age != nil || currentRSS != nil || currentVirtual != nil || peakRSS != nil || change != nil || span != nil || growth != nil
	if sampleCount == 0 {
		if anyFieldPresent {
			return out, fmt.Errorf("InfluxDB memory summary contains values with zero samples")
		}
		out.Note = "no process-memory samples in the 15-minute window"
		return out, nil
	}
	if !fieldsPresent || *age < -lightbringerClockSkewTolerance.Seconds() || *span < 0 || *peakRSS < *currentRSS {
		return out, fmt.Errorf("InfluxDB memory summary is inconsistent")
	}
	if (*span == 0) != (growth == nil) {
		return out, fmt.Errorf("InfluxDB memory summary has inconsistent growth rate")
	}
	if *age < 0 {
		*age = 0
		out.ClockSkewAdjusted = true
		out.Note = "memory timestamp was up to 1 second ahead of the query clock; age was clamped to zero"
	}
	out.ObservationState = "observed"
	out.SampleCount = sampleCount
	out.LatestSampleAgeSec = age
	out.CurrentRSSBytes = currentRSS
	out.CurrentVirtualBytes = currentVirtual
	out.PeakRSSBytes = peakRSS
	out.RSSChangeBytes = change
	out.ObservedSpanSec = span
	out.RSSGrowthBytesPerSec = growth
	if *age > lightbringerMemoryFreshness.Seconds() {
		out.ObservationState = "stale"
		appendObservationNote(&out.Note, "latest process-memory sample is older than 30 seconds")
	}
	return out, nil
}

func registerLightbringerTools(server *mcpsdk.Server, cfg Config) {
	addTool(server, cfg, &mcpsdk.Tool{
		Name:        "mithril_lightbringer_stream_probe",
		Annotations: annReadOnlyNetwork,
		Description: "Sample three Lightbringer gRPC messages for stream activity. Slots are counted without treating completion-order delivery as chain continuity. This tests the sidecar stream, not Mithril consumption, and does not require InfluxDB.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ streamProbeInput) (*mcpsdk.CallToolResult, streamProbeOutput, error) {
		addr := cfg.LightbringerGRPCAddr
		out, err := lightbringerStreamProbe(ctx, addr)
		return nil, out, err
	})

	addTool(server, cfg, &mcpsdk.Tool{
		Name:        "mithril_lightbringer_ingest_health",
		Annotations: annReadOnlyNetwork,
		Description: "Read the newest Lightbringer completion plus a fixed five-minute completion and repair cohort from InfluxDB. Window repair counts cover repairs initiated in that window, not the current backlog; duration p95 covers completed repairs only.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ ingestHealthInput) (*mcpsdk.CallToolResult, ingestHealthOutput, error) {
		influxURL := cfg.LightbringerInfluxURL
		if influxURL == "" {
			return nil, ingestHealthOutput{}, fmt.Errorf("MITHRIL_LIGHTBRINGER_INFLUXDB_URL is not configured")
		}
		database := cfg.LightbringerInfluxDB
		res, err := lightbringerIngestHealth(ctx, cfg, influxURL, database)
		if err != nil {
			return nil, ingestHealthOutput{}, err
		}
		return nil, res, nil
	})

	addTool(server, cfg, &mcpsdk.Tool{
		Name:        "mithril_lightbringer_memory",
		Annotations: annReadOnlyNetwork,
		Description: "Summarize Lightbringer process RSS and virtual address space over a fixed 15-minute InfluxDB window. Growth is observational and does not by itself prove a memory leak.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ lightbringerMemoryInput) (*mcpsdk.CallToolResult, lightbringerMemoryOutput, error) {
		influxURL := cfg.LightbringerInfluxURL
		if influxURL == "" {
			return nil, lightbringerMemoryOutput{}, fmt.Errorf("MITHRIL_LIGHTBRINGER_INFLUXDB_URL is not configured")
		}
		res, err := lightbringerMemory(ctx, cfg, influxURL, cfg.LightbringerInfluxDB)
		if err != nil {
			return nil, lightbringerMemoryOutput{}, err
		}
		return nil, res, nil
	})
}
