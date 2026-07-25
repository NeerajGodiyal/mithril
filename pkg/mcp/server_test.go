package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestInteractiveServeHint(t *testing.T) {
	for _, test := range []struct {
		name string
		mode os.FileMode
		want string
	}{
		{name: "terminal", mode: os.ModeCharDevice, want: interactiveServeMessage + "\n"},
		{name: "pipe"},
		{name: "regular file", mode: 0o600},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			writeInteractiveServeHint(&stderr, test.mode)
			if got := stderr.String(); got != test.want {
				t.Fatalf("stderr = %q, want %q", got, test.want)
			}
		})
	}
}

func TestIsCleanShutdown(t *testing.T) {
	clean := []error{
		nil,
		io.EOF,
		fmt.Errorf("wrapped: %w", io.EOF),
		context.Canceled,
		mcpsdk.ErrConnectionClosed,
		fmt.Errorf("wrapped: %w", mcpsdk.ErrConnectionClosed),
		errors.New("server is closing"),      // exact internal jsonrpc2 sentinel
		errors.New("server is closing: EOF"), // exact observed wrapped rendering
	}
	for _, err := range clean {
		if !isCleanShutdown(err) {
			t.Errorf("isCleanShutdown(%v) = false, want true (clean shutdown)", err)
		}
	}
	dirty := []error{
		errors.New("some genuine failure"),
		errors.New("bind: address already in use"),
		errors.New("connection closed"),
		errors.New("server is closing because the disk failed"),
		errors.New("prefix: server is closing: EOF"),
	}
	for _, err := range dirty {
		if isCleanShutdown(err) {
			t.Errorf("isCleanShutdown(%v) = true, want false (real error)", err)
		}
	}
}

func TestBoundedFrameReadCloserEnforcesEachNDJSONFrame(t *testing.T) {
	const limit = 4
	valid := "\n \t\r\nnull\n1234\r\ntrue"
	reader := newBoundedFrameReadCloser(io.NopCloser(strings.NewReader(valid)), limit)
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read valid boundary-sized frames: %v", err)
	}
	const normalized = "null\n1234\ntrue\n"
	if string(got) != normalized {
		t.Fatalf("valid frames = %q, want normalized %q", got, normalized)
	}

	reader = newBoundedFrameReadCloser(io.NopCloser(strings.NewReader("12345\n")), limit)
	got, err = io.ReadAll(reader)
	if !errors.Is(err, errStdioInputFrameTooLarge) {
		t.Fatalf("oversized frame error = %v, want %v", err, errStdioInputFrameTooLarge)
	}
	if len(got) != 0 {
		t.Fatalf("bytes delivered before oversized error = %q, want none", got)
	}

	for _, invalid := range []string{"{}\r{}\n", "{} {}\n", "null\rx", string([]byte{'"', 0xff, '"', '\n'})} {
		reader = newBoundedFrameReadCloser(io.NopCloser(strings.NewReader(invalid)), 64)
		if got, err := io.ReadAll(reader); !errors.Is(err, errStdioInputFrameInvalid) || len(got) != 0 {
			t.Errorf("invalid frame %q = bytes %q, error %v; want no bytes and %v", invalid, got, err, errStdioInputFrameInvalid)
		}
	}
}

func TestStdioFrameLimitsCoverConfiguredToolAdmission(t *testing.T) {
	rate, burst := stdioFrameLimits((Config{RatePerSecond: 750.5, RateBurst: 200}).normalized())
	if rate != 750.5 || burst != 200 {
		t.Fatalf("frame limits = %v/%d, want 750.5/200", rate, burst)
	}
	rate, burst = stdioFrameLimits((Config{RatePerSecond: 5, RateBurst: 10}).normalized())
	if rate != stdioInputFramesPerSec || burst != stdioInputFrameBurst {
		t.Fatalf("default frame limits = %v/%d", rate, burst)
	}
}

func TestBoundedFrameReadCloserRejectsFrameBurstBeforeSDKDecode(t *testing.T) {
	reader := newBoundedFrameReadCloser(io.NopCloser(strings.NewReader("{}\n{}\n{}\n")), 64)
	reader.frameAdmission = newTokenBucket(0, 2, time.Now())
	got, err := io.ReadAll(reader)
	if !errors.Is(err, errStdioInputFrameRateLimited) {
		t.Fatalf("frame burst error = %v, want %v", err, errStdioInputFrameRateLimited)
	}
	if string(got) != "{}\n{}\n" {
		t.Fatalf("bytes admitted before frame burst rejection = %q, want two frames", got)
	}
}

func TestBoundedFrameReadCloserBackpressuresFrameBurst(t *testing.T) {
	reader := newBoundedFrameReadCloser(io.NopCloser(strings.NewReader("{}\n{}\n{}\n")), 64)
	reader.frameAdmission = newTokenBucket(1000, 2, time.Now())
	got, err := io.ReadAll(reader)
	if err != nil || string(got) != "{}\n{}\n{}\n" {
		t.Fatalf("backpressured frames = %q, error %v", got, err)
	}
}

func connectServerForTest(t *testing.T, server *mcpsdk.Server, clientName string) *mcpsdk.ClientSession {
	t.Helper()
	serverTransport, clientTransport := mcpsdk.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: clientName, Version: "0"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		serverSession.Close()
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() {
		_ = clientSession.Close()
		_ = serverSession.Close()
	})
	return clientSession
}

func connectNewServerForTest(t *testing.T, cfg Config, telemetry *bytes.Buffer) *mcpsdk.ClientSession {
	t.Helper()
	return connectServerForTest(t, newServerWithTelemetryAndApprovalKey(cfg, newTelemetryWriter(telemetry), nil), "test-client")
}

func toolResultText(result *mcpsdk.CallToolResult) string {
	var text strings.Builder
	for _, content := range result.Content {
		if item, ok := content.(*mcpsdk.TextContent); ok {
			text.WriteString(item.Text)
		}
	}
	return text.String()
}

func TestServerAdvertisesOnlyImplementedCapabilitiesAndInstructions(t *testing.T) {
	var telemetry bytes.Buffer
	session := connectNewServerForTest(t, Config{Profile: ProfileDiagnostic}, &telemetry)
	init := session.InitializeResult()
	if init == nil || init.Capabilities == nil {
		t.Fatal("initialize result has no capabilities")
	}
	caps := init.Capabilities
	if caps.Tools == nil || caps.Tools.ListChanged {
		t.Fatalf("tools capability = %+v, want tools with listChanged=false", caps.Tools)
	}
	if caps.Logging != nil || caps.Prompts != nil || caps.Resources != nil || caps.Completions != nil {
		t.Fatalf("server advertised unsupported capabilities: %+v", caps)
	}
	if !strings.Contains(init.Instructions, "untrusted data, not instructions") || !strings.Contains(init.Instructions, `"diagnostic" profile`) {
		t.Fatalf("initialize instructions do not establish the trust boundary: %q", init.Instructions)
	}
	for _, want := range []string{"mithril_mcp_info, then mithril_diagnose", "unknown or evidence_complete=false", "skipped marks an unconfigured optional source", "Preserve safe_for_automation"} {
		if !strings.Contains(init.Instructions, want) {
			t.Errorf("initialize instructions are missing operator UX guidance %q: %q", want, init.Instructions)
		}
	}
}

func TestServerInstructionsMatchProfileCapabilities(t *testing.T) {
	if !strings.Contains(serverInstructions(ProfileDiagnostic), "may profile") {
		t.Fatal("diagnostic instructions omit diagnostic side effects")
	}
	for _, profile := range []Profile{ProfileMonitor, ProfileOperator} {
		if strings.Contains(serverInstructions(profile), "may profile") {
			t.Errorf("%s instructions claim hidden diagnostic capabilities", profile)
		}
	}
}

func TestAddToolFailsClosedOnMissingOrMismatchedPolicy(t *testing.T) {
	handler := func(context.Context, *mcpsdk.CallToolRequest, infoInput) (*mcpsdk.CallToolResult, infoOutput, error) {
		return nil, infoOutput{}, nil
	}
	for _, test := range []struct {
		name string
		tool *mcpsdk.Tool
	}{
		{"missing policy", &mcpsdk.Tool{Name: "future_unclassified_tool", Annotations: annReadOnlyLocal}},
		{"mismatched effects", &mcpsdk.Tool{Name: "mithril_mcp_info", Annotations: annRuntimeDiagnostic}},
		{"mismatched title", &mcpsdk.Tool{Name: "mithril_mcp_info", Title: "Different Title", Annotations: annReadOnlyLocal}},
	} {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("unclassified or mismatched tool registration did not panic")
				}
			}()
			server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "policy-test", Version: "0"}, nil)
			addTool(server, Config{Profile: ProfileDiagnostic}, test.tool, handler)
		})
	}
}

func TestUnknownAndProfileHiddenToolsUseProtocolErrors(t *testing.T) {
	var telemetry bytes.Buffer
	session := connectNewServerForTest(t, Config{Profile: ProfileMonitor}, &telemetry)
	for _, call := range []*mcpsdk.CallToolParams{
		{Name: "not_a_real_tool", Arguments: map[string]any{"log_dir": "/etc"}},
		{Name: "mithril_pprof_heap"},
	} {
		result, err := session.CallTool(context.Background(), call)
		if err == nil || result != nil || !strings.Contains(err.Error(), "unknown tool") {
			t.Fatalf("CallTool(%q) = result:%+v error:%v, want protocol unknown-tool error", call.Name, result, err)
		}
	}
}

func TestProfileToolCatalogsAndMetadata(t *testing.T) {
	monitorNames := []string{
		"mithril_cross_check_slot",
		"mithril_diagnose",
		"mithril_get_bank_hash",
		"mithril_get_block_height",
		"mithril_get_latest_blockhash",
		"mithril_get_slot_info",
		"mithril_grep_log",
		"mithril_host_health",
		"mithril_mcp_info",
		"mithril_metric",
		"mithril_read_divergence",
		"mithril_read_replay_timings",
		"mithril_read_rewards",
		"mithril_read_shutdown_state",
		"mithril_scrape_metrics",
		"mithril_tail_log",
	}
	diagnosticNames := append(append([]string(nil), monitorNames...),
		"mithril_get_account_info",
		"mithril_pprof_heap",
		"mithril_pprof_profile",
		"mithril_simulate_transaction",
	)
	operatorNames := append(append([]string(nil), monitorNames...),
		"mithril_execute_service_action",
		"mithril_prepare_service_action",
		"mithril_service_status",
	)
	allNames := append(append([]string(nil), diagnosticNames...), operatorNames[len(monitorNames):]...)
	expectedPolicies := make(map[string]struct{}, len(allNames))
	for _, name := range allNames {
		expectedPolicies[name] = struct{}{}
		if _, ok := toolPolicies[name]; !ok {
			t.Errorf("required tool %q has no policy", name)
		}
	}
	for name := range toolPolicies {
		if _, ok := expectedPolicies[name]; !ok {
			t.Errorf("unexpected tool policy %q is not in the required catalog", name)
		}
	}
	for _, test := range []struct {
		profile Profile
		want    []string
	}{
		{ProfileMonitor, monitorNames},
		{ProfileDiagnostic, diagnosticNames},
		{ProfileOperator, operatorNames},
	} {
		t.Run(string(test.profile), func(t *testing.T) {
			var telemetry bytes.Buffer
			session := connectNewServerForTest(t, Config{Profile: test.profile}, &telemetry)
			listed, err := session.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatalf("list tools: %v", err)
			}
			got := make([]string, 0, len(listed.Tools))
			titles := make(map[string]string, len(listed.Tools))
			for _, tool := range listed.Tools {
				got = append(got, tool.Name)
				if tool.Title == "" || tool.Description == "" || tool.InputSchema == nil || tool.OutputSchema == nil {
					t.Errorf("tool %q has incomplete description/schema metadata: %+v", tool.Name, tool)
				}
				if want := toolPolicies[tool.Name].title; tool.Title != want {
					t.Errorf("tool %q title = %q, want policy title %q", tool.Name, tool.Title, want)
				}
				if previous, exists := titles[tool.Title]; exists {
					t.Errorf("tools %q and %q share title %q", previous, tool.Name, tool.Title)
				}
				titles[tool.Title] = tool.Name
				if tool.Name == "mithril_metric" {
					output, ok := tool.OutputSchema.(map[string]any)
					if !ok || output["type"] != "object" || len(output) != 1 {
						t.Errorf("metric tool must advertise its dynamic value through a permissive object schema, got %#v", tool.OutputSchema)
					}
				}
				if tool.Name == "mithril_read_divergence" || tool.Name == "mithril_diagnose" {
					output, ok := tool.OutputSchema.(map[string]any)
					properties, propertiesOK := output["properties"].(map[string]any)
					if !ok || !propertiesOK || len(properties) == 0 {
						t.Errorf("tool %q must advertise its typed output fields, got %#v", tool.Name, tool.OutputSchema)
					} else {
						want := []string{"diverged", "artifacts"}
						if tool.Name == "mithril_diagnose" {
							want = []string{"status", "checks", "findings"}
						}
						for _, name := range want {
							if _, exists := properties[name]; !exists {
								t.Errorf("tool %q output schema is missing %q", tool.Name, name)
							}
						}
					}
				}
				assertNoUnconstrainedPropertySchemas(t, tool.Name+".inputSchema", tool.InputSchema)
				assertNoUnconstrainedPropertySchemas(t, tool.Name+".outputSchema", tool.OutputSchema)
				if tool.Annotations == nil || tool.Annotations.OpenWorldHint == nil {
					t.Errorf("tool %q has incomplete annotations: %+v", tool.Name, tool.Annotations)
				} else if toolPolicies[tool.Name].annotations == annRuntimeDiagnostic || toolPolicies[tool.Name].annotations == annControlPrepare {
					if tool.Annotations.ReadOnlyHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint || tool.Annotations.IdempotentHint {
						t.Errorf("effectful non-destructive tool %q has incorrect annotations: %+v", tool.Name, tool.Annotations)
					}
				} else if toolPolicies[tool.Name].annotations == annControlExecute {
					if tool.Annotations.ReadOnlyHint || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint || tool.Annotations.IdempotentHint {
						t.Errorf("control execution tool %q has incorrect annotations: %+v", tool.Name, tool.Annotations)
					}
				} else if !tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint {
					t.Errorf("tool %q has incomplete read-only annotations: %+v", tool.Name, tool.Annotations)
				}
				schema, ok := tool.InputSchema.(map[string]any)
				if !ok {
					t.Errorf("tool %q input schema has unexpected type %T", tool.Name, tool.InputSchema)
					continue
				}
				properties, _ := schema["properties"].(map[string]any)
				if tool.Name == "mithril_simulate_transaction" {
					if _, exposed := properties["commitment"]; exposed {
						t.Errorf("simulation input schema advertises unsupported commitment semantics")
					}
				}
				for _, forbidden := range []string{"endpoint", "reference_rpc_url", "log_dir", "path", "state_path"} {
					if _, exposed := properties[forbidden]; exposed {
						t.Errorf("tool %q exposes process-configuration argument %q", tool.Name, forbidden)
					}
				}
				_, exposesRaw := properties["include_raw"]
				if tool.Name == "mithril_scrape_metrics" && exposesRaw != (test.profile == ProfileDiagnostic) {
					t.Errorf("scrape include_raw exposure under %q = %v", test.profile, exposesRaw)
				}
			}
			sort.Strings(got)
			want := append([]string(nil), test.want...)
			sort.Strings(want)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("tool names = %v, want %v", got, want)
			}
		})
	}
}

// Empty schemas are legal JSON Schema, but common MCP hosts reject an
// unconstrained `{}` when it appears as an object property. The Go SDK infers
// that shape for `any`, so check the complete advertised catalog rather than
// relying only on the SDK client accepting its own schema output.
func assertNoUnconstrainedPropertySchemas(t *testing.T, path string, raw any) {
	t.Helper()
	schema, ok := raw.(map[string]any)
	if !ok {
		return
	}
	for _, keyword := range []string{"properties", "patternProperties", "$defs", "definitions"} {
		children, _ := schema[keyword].(map[string]any)
		for name, childRaw := range children {
			child, ok := childRaw.(map[string]any)
			if !ok {
				continue
			}
			childPath := path + "." + keyword + "." + name
			if len(child) == 0 {
				t.Errorf("%s is an unconstrained empty schema rejected by common MCP hosts", childPath)
				continue
			}
			assertNoUnconstrainedPropertySchemas(t, childPath, child)
		}
	}
	for _, keyword := range []string{"items", "contains", "not", "if", "then", "else", "additionalProperties"} {
		if child, ok := schema[keyword].(map[string]any); ok {
			assertNoUnconstrainedPropertySchemas(t, path+"."+keyword, child)
		}
	}
	for _, keyword := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
		children, _ := schema[keyword].([]any)
		for i, child := range children {
			assertNoUnconstrainedPropertySchemas(t, fmt.Sprintf("%s.%s[%d]", path, keyword, i), child)
		}
	}
}

func TestMCPInfoSanitizesOriginsAndReportsPolicyLimits(t *testing.T) {
	cfg := Config{
		Profile:           ProfileDiagnostic,
		MetricsURL:        "https://user:password@metrics.example.com:8443/secret/path?api_key=METRICS_SECRET",
		RPCURL:            "http://rpc.example.com/private/RPC_SECRET?token=QUERY_SECRET",
		PprofURL:          "http://127.0.0.1:6060/debug/pprof?token=PPROF_SECRET",
		LogDir:            "/var/lib/mithril/logs",
		StatePath:         "/var/lib/mithril/state.json",
		ReplayPath:        "/var/lib/mithril/replay_timings.jsonl",
		ReferenceRPCURL:   "https://reference.example.com/REFERENCE_SECRET",
		ReplayP99WarnMs:   321.5,
		SlotsBehindWarn:   42,
		AccountsDir:       "/var/lib/mithril/accounts",
		SnapshotsDir:      "/var/lib/mithril/snapshots",
		ShredstoreDir:     "/var/lib/mithril/shredstore",
		NodeCgroupPath:    "/sys/fs/cgroup/mithril.service",
		BlockSource:       "lightbringer",
		MaxConcurrent:     2,
		RatePerSecond:     3.5,
		RateBurst:         6,
		OutputBudgetBytes: 16 * 1024,
	}
	var telemetry bytes.Buffer
	session := connectNewServerForTest(t, cfg, &telemetry)
	result, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "mithril_mcp_info"})
	if err != nil {
		t.Fatalf("call mcp_info: %v", err)
	}
	if result.IsError {
		t.Fatalf("mcp_info returned tool error: %+v", result.Content)
	}
	if len(result.Content) != 1 {
		t.Fatalf("structured result is missing its compatibility fallback: %+v", result.Content)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured info: %v", err)
	}
	text, ok := result.Content[0].(*mcpsdk.TextContent)
	if !ok || text.Text != string(encoded) {
		t.Fatalf("compatibility fallback does not match structured info: %+v", result.Content)
	}
	for _, secret := range []string{"password", "METRICS_SECRET", "RPC_SECRET", "QUERY_SECRET", "PPROF_SECRET", "REFERENCE_SECRET"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Errorf("mcp_info leaked %q: %s", secret, encoded)
		}
	}
	var info infoOutput
	if err := json.Unmarshal(encoded, &info); err != nil {
		t.Fatalf("decode mcp_info: %v", err)
	}
	if info.MetricsOrigin != "https://metrics.example.com:8443" ||
		info.RPCOrigin != "http://rpc.example.com:80" ||
		info.PprofOrigin != "http://127.0.0.1:6060" {
		t.Fatalf("origins were not canonicalized: %+v", info)
	}
	if !info.MetricsConfigured || !info.RPCConfigured {
		t.Fatal("configured metrics/RPC endpoint was reported as unavailable")
	}
	if info.Profile != ProfileDiagnostic || !info.DiagnosticToolsExposed || info.Limits.MaxConcurrent != 2 || info.Limits.RatePerSecond != 3.5 || info.Limits.RateBurst != 6 ||
		info.Limits.OutputBudgetBytes != 16*1024 || info.Limits.OutputBudgetScope != outputBudgetScope ||
		info.Limits.MaxInputFrameBytes != maxStdioInputFrameBytes || info.Limits.InputFramesPerSec != stdioInputFramesPerSec || info.Limits.InputFrameBurst != stdioInputFrameBurst {
		t.Fatalf("profile/limits mismatch: %+v", info)
	}
	if !info.ReferenceRPC {
		t.Fatal("reference RPC configured flag = false, want true")
	}
	if info.LogDir != cfg.LogDir || info.StatePath != cfg.StatePath || info.ReplayPath != cfg.ReplayPath ||
		info.AccountsDir != cfg.AccountsDir || info.SnapshotsDir != cfg.SnapshotsDir || info.ShredstoreDir != cfg.ShredstoreDir ||
		info.Thresholds.ReplayP99WarnMS != 321.5 || info.Thresholds.SlotsBehindWarn != 42 ||
		info.Thresholds.DiskWarnPercent != DefaultDiskWarnPercent || info.Thresholds.DiskCriticalPercent != DefaultDiskCriticalPercent || !info.NodeCgroupConfigured || info.BlockSource != "lightbringer" {
		t.Fatalf("configured paths/thresholds/block-source metadata mismatch: %+v", info)
	}
}

func TestMCPInfoOmitsInactiveOptionalDetails(t *testing.T) {
	var telemetry bytes.Buffer
	session := connectNewServerForTest(t, Config{Profile: ProfileMonitor}, &telemetry)
	result, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "mithril_mcp_info"})
	if err != nil || result.IsError {
		t.Fatalf("mcp_info = result:%+v error:%v", result, err)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"metrics_origin", "rpc_origin", "pprof_origin"} {
		if _, present := fields[name]; present {
			t.Errorf("inactive optional detail %q should be omitted: %s", name, encoded)
		}
	}
	for name, want := range map[string]string{
		"diagnostic_tools_exposed": "false",
		"operator_tools_exposed":   "false",
		"control_configured":       "false",
		"node_cgroup_configured":   "false",
		"metrics_configured":       "false",
		"rpc_configured":           "false",
	} {
		if got := string(fields[name]); got != want {
			t.Errorf("%s = %s, want %s", name, got, want)
		}
	}
}
