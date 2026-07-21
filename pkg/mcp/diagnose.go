package mcp

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	corestate "github.com/Overclock-Validator/mithril/pkg/state"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Finding is one diagnostic observation.
type Finding struct {
	Severity string `json:"severity"` // critical | high | medium | low | info
	Category string `json:"category"`
	Message  string `json:"message"`
}

const (
	diagnosticHealthy  = "healthy"
	diagnosticDegraded = "degraded"
	diagnosticCritical = "critical"
	diagnosticUnknown  = "unknown"

	checkOK       = "ok"
	checkDegraded = "degraded"
	checkCritical = "critical"
	checkUnknown  = "unknown"
	checkSkipped  = "skipped"

	diagnoseReplayCaveat = "phase sum is not wall-clock latency; complete fields do not prove each phase ran, and asynchronous timing can be assigned to another slot"
)

// DiagnosticCheck records one health source and its evidence.
type DiagnosticCheck struct {
	Name     string `json:"name"`
	Status   string `json:"status"` // ok | degraded | critical | unknown | skipped
	Evidence string `json:"evidence"`
}

type diagnoseInput struct {
	IncludeLogs        *bool `json:"include_logs,omitempty" jsonschema:"include recent error-log scan (default true)"`
	IncludeReplayTrend *bool `json:"include_replay_trend,omitempty" jsonschema:"include replay-timing trend (default true)"`
}

type diagnoseOutput struct {
	Status              string                      `json:"status"` // healthy | degraded | critical | unknown
	AssessmentScope     string                      `json:"assessment_scope"`
	ObservedAt          string                      `json:"observed_at"`
	SafeForAutomation   bool                        `json:"safe_for_automation"`
	EvidenceComplete    bool                        `json:"evidence_complete"`
	Checks              []DiagnosticCheck           `json:"checks"`
	Findings            []Finding                   `json:"findings"`
	MetricsSnapshot     *MetricsSummary             `json:"metrics_snapshot"`
	RPCSnapshot         *SlotInfo                   `json:"rpc_snapshot"`
	ShutdownState       *ShutdownStateSummary       `json:"shutdown_state"`
	RecentErrors        []LogLine                   `json:"recent_errors"`
	ReplayStats         *ReplayStats                `json:"replay_stats"`
	ReplayMeta          *ReplayMeta                 `json:"replay_meta"`
	SlotsBehind         *SlotComparison             `json:"slots_behind"`
	DivergenceArtifacts []DivergenceArtifactSummary `json:"divergence_artifacts"`
	HostHealth          *hostHealthOutput           `json:"host_health"`
	LightbringerStream  *streamProbeOutput          `json:"lightbringer_stream,omitempty"`
	LightbringerIngest  *ingestHealthOutput         `json:"lightbringer_ingest,omitempty"`
	LightbringerMemory  *lightbringerMemoryOutput   `json:"lightbringer_memory,omitempty"`
}

// mergeDiagnosticStatus applies the fail-closed overall precedence:
// critical > unknown > degraded > healthy. A missing configured signal therefore
// cannot be hidden by otherwise healthy checks.
func mergeDiagnosticStatus(current, checkStatus string) string {
	switch checkStatus {
	case checkCritical:
		return diagnosticCritical
	case checkUnknown:
		if current != diagnosticCritical {
			return diagnosticUnknown
		}
	case checkDegraded:
		if current == diagnosticHealthy {
			return diagnosticDegraded
		}
	}
	return current
}

func findingSeverity(checkStatus string) string {
	switch checkStatus {
	case checkCritical:
		return "critical"
	case checkDegraded:
		return "high"
	case checkUnknown:
		return "medium"
	default:
		return "info"
	}
}

func diagnoseReplayEvidence(stats ReplayStats, meta ReplayMeta, warnMs float64) (string, string) {
	switch {
	case meta.SourceChangedDuringScan:
		return checkUnknown, "replay timing source changed while it was read; phase-sum trend cannot be assessed safely"
	case meta.IncompleteTail:
		return checkUnknown, "newest replay JSONL record is incomplete; phase-sum trend cannot be assessed safely"
	case !stats.FieldFound || stats.Count == 0:
		return checkUnknown, "block_total replay timing data is absent"
	case stats.ShapeIncompleteCount > 0:
		return checkUnknown, fmt.Sprintf("%d replay samples lack the complete block_total field shape", stats.ShapeIncompleteCount)
	case stats.Count < minReplayHealthSamples:
		return checkUnknown, fmt.Sprintf("only %d block_total samples with complete field shape are available; need at least %d", stats.Count, minReplayHealthSamples)
	case warnMs <= 0 || math.IsNaN(warnMs) || math.IsInf(warnMs, 0):
		return checkUnknown, fmt.Sprintf("replay p99 threshold is invalid: %v", warnMs)
	case stats.P99Ms > warnMs:
		evidence := fmt.Sprintf("replay p99 %.1fms across %d complete records exceeds %.1fms; %s", stats.P99Ms, stats.Count, warnMs, diagnoseReplayCaveat)
		if meta.ParseErrors > 0 {
			evidence += fmt.Sprintf("; %d lines were unparseable", meta.ParseErrors)
		}
		return checkDegraded, evidence
	case meta.ParseErrors > 0:
		return checkDegraded, fmt.Sprintf("replay p99 %.1fms; %d lines were unparseable; %s", stats.P99Ms, meta.ParseErrors, diagnoseReplayCaveat)
	default:
		return checkOK, fmt.Sprintf("replay p99 %.1fms across %d complete records; %s", stats.P99Ms, stats.Count, diagnoseReplayCaveat)
	}
}

func incompleteLogScanEvidence(scan LogScanMeta) string {
	return fmt.Sprintf(
		"scan incomplete (omitted_prefix_bytes=%d oversized_lines=%d unparsed_lines=%d incomparable_since_lines=%d incomplete_tail=%v source_changed_during_scan=%v)",
		scan.OmittedPrefixBytes, scan.OversizedLines, scan.UnparsedLines, scan.IncomparableSinceLines, scan.IncompleteTail, scan.SourceChangedDuringScan,
	)
}

func diagnoseLightbringerStreamEvidence(stream streamProbeOutput) (string, string) {
	switch stream.State {
	case "active":
		return checkOK, fmt.Sprintf("Lightbringer stream returned %d messages across %d distinct slots; completion-order delivery is not chain-continuity evidence", stream.SlotsSeen, stream.DistinctSlots)
	case "unreachable", "backend_error", "incomplete", "no_progress":
		return checkDegraded, fmt.Sprintf("Lightbringer stream state is %s", stream.State)
	default:
		return checkUnknown, fmt.Sprintf("Lightbringer stream state is %s", stream.State)
	}
}

func diagnoseLightbringerIngestEvidence(ingest ingestHealthOutput) (string, string) {
	if ingest.LastCompletionAgeSec == nil || ingest.LatestCompletedSlot == nil {
		return checkUnknown, "Lightbringer has no completed-slot telemetry"
	}
	if ingest.ObservationState == "stale" {
		return checkDegraded, fmt.Sprintf("latest Lightbringer completion is %.1fs old with %d completed slots in the five-minute window", *ingest.LastCompletionAgeSec, ingest.CompletedSlots)
	}
	if ingest.ObservationState != "observed" {
		return checkUnknown, fmt.Sprintf("Lightbringer ingest telemetry state is %s", ingest.ObservationState)
	}
	if ingest.CompletedSlots == 0 || *ingest.LastCompletionAgeSec > lightbringerIngestWindow.Seconds() {
		return checkDegraded, fmt.Sprintf("latest Lightbringer completion is %.1fs old with %d completed slots in the five-minute window", *ingest.LastCompletionAgeSec, ingest.CompletedSlots)
	}
	return checkOK, fmt.Sprintf(
		"latest Lightbringer completion is %.1fs old; %d completed and %d repair-started slots in the five-minute window",
		*ingest.LastCompletionAgeSec, ingest.CompletedSlots, ingest.WindowRepairStartedSlots,
	)
}

func diagnoseLightbringerMemoryEvidence(memory lightbringerMemoryOutput) (string, string) {
	if memory.LatestSampleAgeSec == nil || memory.CurrentRSSBytes == nil {
		return checkUnknown, "Lightbringer has no process-memory telemetry in the 15-minute window"
	}
	if memory.ObservationState == "stale" {
		return checkDegraded, fmt.Sprintf("latest Lightbringer process-memory sample is %.1fs old", *memory.LatestSampleAgeSec)
	}
	if memory.ObservationState != "observed" {
		return checkUnknown, fmt.Sprintf("Lightbringer memory telemetry state is %s", memory.ObservationState)
	}
	if *memory.LatestSampleAgeSec > lightbringerMemoryFreshness.Seconds() {
		return checkDegraded, fmt.Sprintf("latest Lightbringer process-memory sample is %.1fs old", *memory.LatestSampleAgeSec)
	}
	return checkOK, fmt.Sprintf(
		"Lightbringer RSS is %d bytes from a %.1fs-old sample across %d sample(s); no host-limit health threshold is inferred",
		*memory.CurrentRSSBytes, *memory.LatestSampleAgeSec, memory.SampleCount,
	)
}

type diagnoseHostCollector func(context.Context, Config, *MetricsSummary, *ShutdownStateSummary) hostHealthOutput

func runDiagnosisWithHostCollector(ctx context.Context, cfg Config, in diagnoseInput, collectHost diagnoseHostCollector) diagnoseOutput {
	includeLogs := in.IncludeLogs == nil || *in.IncludeLogs
	includeReplay := in.IncludeReplayTrend == nil || *in.IncludeReplayTrend

	out := diagnoseOutput{
		Status:              diagnosticHealthy,
		AssessmentScope:     "point_in_time_snapshot",
		ObservedAt:          time.Now().UTC().Format(time.RFC3339Nano),
		SafeForAutomation:   false,
		EvidenceComplete:    true,
		Checks:              []DiagnosticCheck{},
		Findings:            []Finding{},
		RecentErrors:        []LogLine{},
		DivergenceArtifacts: []DivergenceArtifactSummary{},
	}
	addCheck := func(name, status, evidence string) {
		out.Checks = append(out.Checks, DiagnosticCheck{Name: name, Status: status, Evidence: evidence})
		out.Status = mergeDiagnosticStatus(out.Status, status)
		if status == checkUnknown {
			out.EvidenceComplete = false
		}
		if status != checkOK && status != checkSkipped {
			out.Findings = append(out.Findings, Finding{
				Severity: findingSeverity(status),
				Category: name,
				Message:  evidence,
			})
		}
	}

	// Metrics provide a point-in-time node snapshot. The bounded replay JSONL
	// window below supplies the recent latency trend.
	if cfg.MetricsURL == "" {
		addCheck("metrics", checkUnknown, "metrics endpoint is not configured")
	} else if body, err := fetchMetricsText(ctx, cfg.MetricsURL); err != nil {
		addCheck("metrics", checkUnknown, fmt.Sprintf("cannot reach configured metrics endpoint: %v", err))
	} else if summary, err := parseMetrics(body); err != nil {
		addCheck("metrics", checkUnknown, fmt.Sprintf("failed to parse configured metrics endpoint: %v", err))
	} else {
		out.MetricsSnapshot = summary
		if summary.Slot == nil {
			addCheck("metrics", checkUnknown, "slot metric is absent")
		} else {
			addCheck("metrics", checkOK, fmt.Sprintf("metrics reachable at slot %d", *summary.Slot))
		}
	}

	// Probe Mithril RPC directly even when no external reference RPC is configured.
	if cfg.RPCURL == "" {
		addCheck("rpc", checkUnknown, "Mithril RPC endpoint is not configured")
	} else if client, err := newMithrilRPCClient(cfg.RPCURL); err != nil {
		addCheck("rpc", checkUnknown, fmt.Sprintf("Mithril RPC client initialization failed: %v", err))
	} else if info, err := client.getSlotInfo(ctx); err != nil {
		addCheck("rpc", checkUnknown, fmt.Sprintf("Mithril RPC getEpochInfo failed: %v", err))
	} else {
		out.RPCSnapshot = &info
		addCheck("rpc", checkOK, fmt.Sprintf("Mithril RPC reachable at slot %d, epoch %d, block height %d", info.AbsoluteSlot, info.Epoch, info.BlockHeight))
	}

	// State stage and the previous shutdown reason are separate checks: a ready
	// database can still carry an abnormal shutdown that requires review.
	if cfg.StatePath == "" {
		addCheck("state", checkSkipped, "state path is not configured")
		addCheck("shutdown", checkSkipped, "shutdown history unavailable because state is not configured")
	} else if state, err := readShutdownStateContext(ctx, cfg.StatePath); err != nil {
		addCheck("state", checkUnknown, fmt.Sprintf("configured state file is unavailable: %v", err))
		addCheck("shutdown", checkUnknown, "shutdown history cannot be evaluated without readable state")
	} else if state == nil {
		addCheck("state", checkUnknown, "configured state file does not exist")
		addCheck("shutdown", checkUnknown, "shutdown history cannot be evaluated because state is missing")
	} else {
		out.ShutdownState = summarizeShutdownState(state)
		if state.StateSchemaVersion == nil || *state.StateSchemaVersion != corestate.CurrentStateSchemaVersion {
			got := "missing"
			if state.StateSchemaVersion != nil {
				got = strconv.FormatUint(uint64(*state.StateSchemaVersion), 10)
			}
			addCheck("state", checkCritical, fmt.Sprintf("state schema version is %s; this Mithril build requires v%d", got, corestate.CurrentStateSchemaVersion))
		} else {
			stage := ""
			if out.ShutdownState.Stage != nil {
				stage = strings.ToLower(strings.TrimSpace(*out.ShutdownState.Stage))
			}
			switch stage {
			case "ready":
				addCheck("state", checkOK, "state stage is ready")
			case "building", "downloading":
				addCheck("state", checkDegraded, fmt.Sprintf("state stage is %s; node is not ready", stage))
			case "corrupted":
				evidence := "state stage is corrupted"
				if out.ShutdownState.CorruptionReason != nil && strings.TrimSpace(*out.ShutdownState.CorruptionReason) != "" {
					evidence += ": " + *out.ShutdownState.CorruptionReason
				}
				addCheck("state", checkCritical, evidence)
			case "":
				addCheck("state", checkUnknown, "state stage is missing")
			default:
				addCheck("state", checkUnknown, fmt.Sprintf("state stage %q is unrecognized", stage))
			}
		}

		safeShutdownReason := ""
		if out.ShutdownState.LastShutdownReason != nil {
			safeShutdownReason = *out.ShutdownState.LastShutdownReason
		}
		if state.LastShutdownReason == nil || strings.TrimSpace(*state.LastShutdownReason) == "" {
			addCheck("shutdown", checkOK, "no previous shutdown reason is recorded")
		} else if reason, ok := state.parsedShutdownReason(); !ok {
			addCheck("shutdown", checkUnknown, fmt.Sprintf("last shutdown reason is unrecognized: %q", safeShutdownReason))
		} else {
			switch reason {
			case shutdownNormal:
				addCheck("shutdown", checkOK, "last shutdown was graceful")
			case shutdownCompleted:
				addCheck("shutdown", checkOK, "last run completed its configured replay range")
			case shutdownStall:
				addCheck("shutdown", checkDegraded, fmt.Sprintf("last shutdown followed a block-fetch stall: %s", safeShutdownReason))
			case shutdownLeaderSchedule:
				addCheck("shutdown", checkDegraded, fmt.Sprintf("last shutdown followed a leader-schedule failure: %s", safeShutdownReason))
			case shutdownError:
				addCheck("shutdown", checkCritical, fmt.Sprintf("last shutdown was a replay error: %s", safeShutdownReason))
			}
		}
	}

	errorLevel := LevelError
	if !includeLogs {
		addCheck("logs", checkSkipped, "recent error-log scan disabled by request")
	} else if cfg.LogDir == "" {
		addCheck("logs", checkSkipped, "log directory is not configured")
	} else if lines, total, scan, err := tailLogContextWithMeta(ctx, cfg.LogDir, 20, &errorLevel, ""); err != nil {
		addCheck("logs", checkUnknown, fmt.Sprintf("configured log source is unavailable: %v", err))
	} else {
		out.RecentErrors = lines
		if !scan.Complete {
			out.EvidenceComplete = false
		}
		evidence := fmt.Sprintf("scanned log window contains %d error-level entries; returning the newest %d; no degradation is inferred without a reliable wall-clock recency window", total, len(lines))
		if !scan.Complete {
			evidence += "; " + incompleteLogScanEvidence(scan)
		}
		addCheck("logs", checkOK, evidence)
	}

	if cfg.LogDir == "" {
		addCheck("divergence_artifacts", checkSkipped, "log directory is not configured")
	} else if info, err := os.Stat(cfg.LogDir); err != nil {
		addCheck("divergence_artifacts", checkUnknown, fmt.Sprintf("configured divergence source is unavailable: %v", err))
	} else if !info.IsDir() {
		addCheck("divergence_artifacts", checkUnknown, "configured log path is not a directory")
	} else if artifacts, meta, err := readDivergenceArtifactsContext(ctx, cfg.LogDir); err != nil {
		addCheck("divergence_artifacts", checkUnknown, fmt.Sprintf("failed to read divergence artifacts: %v", err))
	} else {
		out.DivergenceArtifacts = summarizeDivergenceArtifacts(artifacts)
		incomplete := meta.Invalid + meta.Oversized + meta.Unreadable
		if incomplete > 0 || meta.Truncated {
			out.EvidenceComplete = false
		}
		if len(artifacts) == 0 && (incomplete > 0 || meta.Truncated) {
			addCheck("divergence_artifacts", checkUnknown, fmt.Sprintf("divergence artifact scan is incomplete: invalid=%d oversized=%d unreadable=%d truncated=%v", meta.Invalid, meta.Oversized, meta.Unreadable, meta.Truncated))
		} else if len(artifacts) == 0 {
			addCheck("divergence_artifacts", checkOK, "no bank-hash mismatch artifacts found; this does not prove consensus verification ran or the node is healthy")
		} else {
			var slots []string
			for _, artifact := range artifacts {
				if artifact.CheckedSlot != nil {
					slots = append(slots, strconv.FormatUint(*artifact.CheckedSlot, 10))
				}
			}
			slotText := "unknown"
			if len(slots) > 0 {
				slotText = strings.Join(slots, ", ")
			}
			addCheck("divergence_artifacts", checkCritical, fmt.Sprintf("%d bank-hash divergence artifact(s) present (slots: %s); halt and re-bootstrap", len(artifacts), slotText))
		}
	}

	if !includeReplay {
		addCheck("replay", checkSkipped, "replay timing check disabled by request")
	} else if cfg.ReplayPath == "" {
		addCheck("replay", checkSkipped, "replay timing path is not configured")
	} else {
		n := 100
		_, stats, meta, err := readReplayTimingsContext(ctx, cfg.ReplayPath, nil, nil, &n, timingBlockTotal)
		if err != nil {
			addCheck("replay", checkUnknown, fmt.Sprintf("configured replay timing source is unavailable: %v", err))
		} else {
			out.ReplayStats = &stats
			out.ReplayMeta = &meta
			if meta.ParseErrors > 0 || meta.IncompleteTail || meta.SourceChangedDuringScan {
				out.EvidenceComplete = false
			}
			status, evidence := diagnoseReplayEvidence(stats, meta, cfg.ReplayP99WarnMs)
			addCheck("replay", status, evidence)
		}
	}

	if cfg.ReferenceRPCURL == "" {
		addCheck("cross_check", checkSkipped, "reference RPC is not configured")
	} else if comparison, err := slotsBehindCheck(ctx, cfg, cfg.ReferenceRPCURL, defaultCommitment); err != nil {
		out.EvidenceComplete = false
		addCheck("cross_check", checkUnknown, fmt.Sprintf("configured reference RPC cross-check is unavailable: %v", err))
	} else {
		out.SlotsBehind = &comparison
		slotsAhead := uint64(0)
		if comparison.MithrilSlot > comparison.ReferenceSlot {
			slotsAhead = comparison.MithrilSlot - comparison.ReferenceSlot
		}
		switch {
		case comparison.Status == "behind":
			addCheck("cross_check", checkDegraded, fmt.Sprintf("node is %d slots behind reference (threshold %d)", comparison.SlotsBehind, comparison.Threshold))
		case comparison.Status == "ahead" && slotsAhead >= 10:
			addCheck("cross_check", checkDegraded, fmt.Sprintf("node is %d slots ahead of reference; reference may be stale or commitments differ", slotsAhead))
		case comparison.Status == "in_sync", comparison.Status == "ahead":
			addCheck("cross_check", checkOK, fmt.Sprintf("slot cross-check status is %s (%d slots behind)", comparison.Status, comparison.SlotsBehind))
		default:
			addCheck("cross_check", checkUnknown, fmt.Sprintf("slot cross-check returned unrecognized status %q", comparison.Status))
		}
	}

	host := collectHost(ctx, cfg, out.MetricsSnapshot, out.ShutdownState)
	out.HostHealth = &host
	if !host.EvidenceComplete {
		out.EvidenceComplete = false
	}
	hostStatus := checkUnknown
	switch host.Status {
	case diagnosticHealthy:
		hostStatus = checkOK
	case diagnosticDegraded:
		hostStatus = checkDegraded
	case diagnosticCritical:
		hostStatus = checkCritical
	}
	addCheck("host", hostStatus, fmt.Sprintf("host health is %s across %d disk, memory, cgroup, and bootstrap checks; inspect host_health for evidence", host.Status, len(host.Checks)))

	if strings.ToLower(strings.TrimSpace(cfg.BlockSource)) != "lightbringer" {
		addCheck("lightbringer_stream", checkSkipped, "configured block source is not Lightbringer")
		addCheck("lightbringer_ingest", checkSkipped, "configured block source is not Lightbringer")
		addCheck("lightbringer_memory", checkSkipped, "configured block source is not Lightbringer")
	} else {
		stream, err := lightbringerStreamProbe(ctx, cfg.LightbringerGRPCAddr)
		if err != nil {
			addCheck("lightbringer_stream", checkUnknown, fmt.Sprintf("Lightbringer stream probe failed: %v", err))
		} else {
			out.LightbringerStream = &stream
			status, evidence := diagnoseLightbringerStreamEvidence(stream)
			addCheck("lightbringer_stream", status, evidence)
		}

		if cfg.LightbringerInfluxURL == "" {
			addCheck("lightbringer_ingest", checkSkipped, "Lightbringer InfluxDB is not configured")
			addCheck("lightbringer_memory", checkSkipped, "Lightbringer InfluxDB is not configured")
		} else {
			ingest, err := lightbringerIngestHealth(ctx, cfg, cfg.LightbringerInfluxURL, cfg.LightbringerInfluxDB)
			if err != nil {
				addCheck("lightbringer_ingest", checkUnknown, fmt.Sprintf("Lightbringer ingest telemetry is unavailable: %v", err))
			} else {
				out.LightbringerIngest = &ingest
				status, evidence := diagnoseLightbringerIngestEvidence(ingest)
				addCheck("lightbringer_ingest", status, evidence)
			}

			memory, err := lightbringerMemory(ctx, cfg, cfg.LightbringerInfluxURL, cfg.LightbringerInfluxDB)
			if err != nil {
				addCheck("lightbringer_memory", checkUnknown, fmt.Sprintf("Lightbringer memory telemetry is unavailable: %v", err))
			} else {
				out.LightbringerMemory = &memory
				status, evidence := diagnoseLightbringerMemoryEvidence(memory)
				addCheck("lightbringer_memory", status, evidence)
			}
		}
	}

	if len(out.Findings) == 0 {
		out.Findings = append(out.Findings, Finding{
			Severity: "info",
			Category: "overall",
			Message:  "available checks passed; this snapshot does not prove sustained consensus activity, slot progress, voting, rewards, or Lightbringer health",
		})
	}
	return out
}

func registerDiagnoseTool(server *mcpsdk.Server, cfg Config) {
	registerDiagnoseToolWithHostCollector(server, cfg, collectHostHealth)
}

func registerDiagnoseToolWithHostCollector(server *mcpsdk.Server, cfg Config, collectHost diagnoseHostCollector) {
	addTool(server, cfg, &mcpsdk.Tool{
		Name:        "mithril_diagnose",
		Annotations: annReadOnlyNetwork,
		Description: "Assess configured health sources and return evidence plus healthy, degraded, critical, or unknown. safe_for_automation is false because this snapshot does not prove consensus activity, progress, voting, rewards, or Lightbringer use.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in diagnoseInput) (*mcpsdk.CallToolResult, diagnoseOutput, error) {
		return nil, runDiagnosisWithHostCollector(ctx, cfg, in, collectHost), nil
	})
}
