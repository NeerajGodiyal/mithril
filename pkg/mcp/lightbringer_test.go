package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/overcast"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeSlotServer struct {
	overcast.UnimplementedSlotStreamServer
	slots       []uint64
	responses   []overcast.SlotResponse
	terminalErr error
	waitForStop bool
}

func (f *fakeSlotServer) StreamSlots(_ *overcast.SlotStreamRequest, stream grpc.ServerStreamingServer[overcast.SlotResponse]) error {
	if f.waitForStop && len(f.responses) == 0 {
		<-stream.Context().Done()
		return stream.Context().Err()
	}
	if len(f.responses) > 0 {
		for i := range f.responses {
			if err := stream.Send(&f.responses[i]); err != nil {
				return err
			}
		}
		if f.waitForStop {
			<-stream.Context().Done()
			return stream.Context().Err()
		}
		return f.terminalErr
	}
	for i, s := range f.slots {
		var parent uint64
		if i > 0 {
			parent = f.slots[i-1]
		}
		if err := stream.Send(&overcast.SlotResponse{Slot: s, ParentSlot: parent}); err != nil {
			return err
		}
	}
	return f.terminalErr
}

func startFakeLightbringerStream(t *testing.T, fake *fakeSlotServer) string {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for fake Lightbringer stream: %v", err)
	}
	srv := grpc.NewServer()
	overcast.RegisterSlotStreamServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		_ = lis.Close()
	})
	return lis.Addr().String()
}

func probeFakeLightbringerStream(t *testing.T, ctx context.Context, fake *fakeSlotServer) streamProbeOutput {
	t.Helper()

	out, err := lightbringerStreamProbe(ctx, startFakeLightbringerStream(t, fake))
	if err != nil {
		t.Fatalf("probe fake Lightbringer stream: %v", err)
	}
	return out
}

func TestLightbringerStreamProbe(t *testing.T) {
	out := probeFakeLightbringerStream(t, context.Background(), &fakeSlotServer{slots: []uint64{100, 101, 102}})
	if !out.Reachable || !out.ActivityObserved || out.SlotsSeen != 3 || out.DistinctSlots != 3 || !out.CompleteSample {
		t.Errorf("probe = %+v (want reachable activity across 3 distinct slots)", out)
	}
	if out.State != "active" || len(out.Samples) != 3 {
		t.Errorf("probe state = %+v", out)
	}
}

func TestLightbringerStreamProbeUnreachable(t *testing.T) {
	// Port 1 has nothing listening.
	out, err := lightbringerStreamProbe(context.Background(), "127.0.0.1:1")
	if err != nil {
		t.Fatalf("unreachable is a soft answer, not an error: %v", err)
	}
	if out.Reachable || out.TerminalError == "" || out.State != "unreachable" {
		t.Errorf("unreachable probe should report reachable=false + error, got %+v", out)
	}
}

func TestLightbringerStreamProbePropagatesCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := lightbringerStreamProbe(ctx, startFakeLightbringerStream(t, &fakeSlotServer{waitForStop: true}))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("idle stream caller deadline error = %v, want context deadline exceeded", err)
	}
}

func TestLightbringerStreamProbePropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := lightbringerStreamProbe(ctx, "127.0.0.1:1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled probe error = %v, want context canceled", err)
	}

	ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := lightbringerStreamProbe(ctx, startFakeLightbringerStream(t, &fakeSlotServer{
		responses:   []overcast.SlotResponse{{Slot: 100, ParentSlot: 99}},
		waitForStop: true,
	}))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("mid-stream caller deadline error = %v, want context deadline exceeded", err)
	}
}

func TestLightbringerStreamProbeBackendErrorProvesReachability(t *testing.T) {
	out := probeFakeLightbringerStream(t, context.Background(), &fakeSlotServer{
		terminalErr: status.Error(codes.Internal, "backend failed"),
	})
	if out.State != "backend_error" || !out.Reachable || out.TerminalError == "" {
		t.Fatalf("backend failure probe = %+v, want reachable backend_error", out)
	}
}

func TestEmptyStreamStateClassifiesGRPCTermination(t *testing.T) {
	tests := []struct {
		code codes.Code
		want string
	}{
		{codes.Canceled, "inconclusive"},
		{codes.DeadlineExceeded, "inconclusive"},
		{codes.ResourceExhausted, "inconclusive"},
		{codes.Unavailable, "unreachable"},
		{codes.Internal, "backend_error"},
	}
	for _, tt := range tests {
		t.Run(tt.code.String(), func(t *testing.T) {
			got := emptyStreamState(context.Background(), status.Error(tt.code, "backend termination"))
			if got != tt.want {
				t.Fatalf("emptyStreamState(%s) = %q, want %q", tt.code, got, tt.want)
			}
		})
	}
}

func TestValidateDialAddr(t *testing.T) {
	validate := func(ctx context.Context, addr string) error {
		_, err := resolveSafeDialTargets(ctx, addr, net.DefaultResolver)
		return err
	}
	if err := validate(context.Background(), "127.0.0.1:3001"); err != nil {
		t.Errorf("loopback should be allowed: %v", err)
	}
	if err := validate(context.Background(), "10.0.0.1:3001"); err == nil {
		t.Error("private IP should be rejected")
	}
	if err := validate(context.Background(), "169.254.169.254:3001"); err == nil {
		t.Error("metadata IP should be rejected")
	}
	if err := validate(context.Background(), "not-a-host-port"); err == nil {
		t.Error("malformed addr should be rejected")
	}
	if err := validate(context.Background(), ":3001"); err == nil {
		t.Error("empty host should be rejected")
	}
	if err := validate(context.Background(), "127.0.0.1:"); err == nil {
		t.Error("empty port should be rejected")
	}
	if err := validate(context.Background(), "127.0.0.1:65536"); err == nil {
		t.Error("out-of-range port should be rejected")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := validate(ctx, "resolution-must-fail.invalid:3001"); err == nil {
		t.Error("DNS resolution failure should be rejected")
	}
}

func TestResolveSafeDialTargetsFailsClosedAndPins(t *testing.T) {
	ctx := context.Background()
	if _, err := resolveSafeDialTargets(ctx, "example.test:3001", fixedResolver{err: context.DeadlineExceeded}); err == nil {
		t.Fatal("DNS failure must be rejected")
	}
	if _, err := resolveSafeDialTargets(ctx, "example.test:3001", fixedResolver{}); err == nil {
		t.Fatal("empty DNS result must be rejected")
	}
	if _, err := resolveSafeDialTargets(ctx, "example.test:3001", fixedResolver{addresses: []net.IPAddr{
		{IP: net.ParseIP("8.8.8.8")},
		{IP: net.ParseIP("169.254.169.254")},
	}}); err == nil {
		t.Fatal("one blocked address must reject the whole mixed answer")
	}
	if _, err := resolveSafeDialTargets(ctx, "example.test:3001", fixedResolver{addresses: []net.IPAddr{
		{IP: net.ParseIP("8.8.8.8")},
	}}); err == nil || !strings.Contains(err.Error(), "only to loopback") {
		t.Fatalf("public insecure gRPC target error = %v", err)
	}

	targets, err := resolveSafeDialTargets(ctx, "example.test:3001", fixedResolver{addresses: []net.IPAddr{
		{IP: net.ParseIP("127.0.0.1")},
	}})
	if err != nil {
		t.Fatalf("resolve safe target: %v", err)
	}
	if len(targets) != 1 || targets[0] != "127.0.0.1:3001" {
		t.Fatalf("targets = %v, want pinned loopback target", targets)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			conn.Close()
		}
		close(accepted)
	}()
	conn, err := pinnedGRPCDialer([]string{listener.Addr().String()})(ctx, "rebound.example:65535")
	if err != nil {
		t.Fatalf("dial pinned target: %v", err)
	}
	conn.Close()
	<-accepted
}

func TestLightbringerIngestHealth(t *testing.T) {
	var gotQuery, gotDB, gotFormat, gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/query_sql" {
			http.Error(w, "wrong path", 404)
			return
		}
		var body struct {
			DB     string `json:"db"`
			Query  string `json:"q"`
			Format string `json:"format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode Influx request: %v", err)
		}
		gotQuery, gotDB, gotFormat = body.Query, body.DB, body.Format
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"age_seconds":1.5,"latest_slot":285000042,"completed_slots":100,"repair_started_slots":10,"repaired_completed_slots":8,"unresolved_repair_slots":2,"repair_duration_p95_ms":42.5,"oldest_unresolved_repair_age_seconds":12}]`))
	}))
	defer ts.Close()

	cfg := Config{LightbringerInfluxURL: ts.URL + "/configured/path?api-key=secret", LightbringerInfluxDB: "lightbringer", LightbringerInfluxTok: "configured-token"}
	out, err := lightbringerIngestHealth(context.Background(), cfg, ts.URL+"/caller/path", "lightbringer")
	if err != nil {
		t.Fatalf("ingest health: %v", err)
	}
	if gotQuery != lightbringerCompletionSQL ||
		!strings.Contains(gotQuery, "CAST(now() AS BIGINT) - CAST(latest.time AS BIGINT)") ||
		!strings.Contains(gotQuery, "kind = 'repair_initiate'") ||
		!strings.Contains(gotQuery, "INTERVAL '5 minutes'") ||
		strings.Contains(gotQuery, "CAST((now() - time) AS DOUBLE)") ||
		strings.Contains(gotQuery, "serve_repair") {
		t.Errorf("Influx query = %q", gotQuery)
	}
	if gotDB != "lightbringer" || gotFormat != "json" {
		t.Errorf("Influx request db/format = %q/%q", gotDB, gotFormat)
	}
	if gotAuth != "Bearer configured-token" {
		t.Errorf("same-origin request Authorization = %q", gotAuth)
	}
	if out.ObservationState != "observed" {
		t.Errorf("observation state = %q", out.ObservationState)
	}
	if out.LastCompletionAgeSec == nil || *out.LastCompletionAgeSec != 1.5 {
		t.Errorf("completion age = %v, want 1.5", out.LastCompletionAgeSec)
	}
	if out.LatestCompletedSlot == nil || *out.LatestCompletedSlot != 285000042 {
		t.Errorf("latest_completed_slot = %v, want 285000042", out.LatestCompletedSlot)
	}
	if out.WindowSeconds != 300 || out.CompletedSlots != 100 || out.CompletionRatePerSecond != 1.0/3.0 {
		t.Errorf("completion window = %+v", out)
	}
	if out.WindowRepairStartedSlots != 10 || out.WindowRepairedCompletedSlots != 8 || out.WindowUnresolvedRepairSlots != 2 ||
		out.WindowRepairSharePercent == nil || *out.WindowRepairSharePercent != 8 ||
		out.WindowCompletedRepairDurationP95Ms == nil || *out.WindowCompletedRepairDurationP95Ms != 42.5 ||
		out.WindowOldestUnresolvedRepairAgeSec == nil || *out.WindowOldestUnresolvedRepairAgeSec != 12 {
		t.Errorf("repair window = %+v", out)
	}
	wantOrigin, _ := canonicalOrigin(ts.URL)
	if out.InfluxURL != wantOrigin {
		t.Errorf("displayed Influx URL = %q, want origin %q", out.InfluxURL, wantOrigin)
	}
}

func TestLightbringerInfluxCredentialIsOriginBound(t *testing.T) {
	configured := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer configured.Close()

	var gotAuth string
	override := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`[{"age_seconds":2,"latest_slot":9,"completed_slots":1,"repair_started_slots":0,"repaired_completed_slots":0,"unresolved_repair_slots":0,"repair_duration_p95_ms":null,"oldest_unresolved_repair_age_seconds":null}]`))
	}))
	defer override.Close()

	cfg := Config{LightbringerInfluxURL: configured.URL, LightbringerInfluxTok: "must-not-cross-origins"}
	if _, err := lightbringerIngestHealth(context.Background(), cfg, override.URL, "lightbringer"); err != nil {
		t.Fatalf("cross-origin query: %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("configured credential leaked to override origin: %q", gotAuth)
	}
}

func TestLightbringerIngestHealthStrictResponseParsing(t *testing.T) {
	const zeroWindow = `,"completed_slots":0,"repair_started_slots":0,"repaired_completed_slots":0,"unresolved_repair_slots":0,"repair_duration_p95_ms":null,"oldest_unresolved_repair_age_seconds":null`
	tests := []struct {
		name      string
		response  string
		wantState string
		wantErr   bool
	}{
		{"no rows", `[]`, "no_completion_data", false},
		{"null latest completion", `[{"age_seconds":null,"latest_slot":null` + zeroWindow + `}]`, "no_completion_data", false},
		{"missing age", `[{"latest_slot":1` + zeroWindow + `}]`, "", true},
		{"missing slot", `[{"age_seconds":1` + zeroWindow + `}]`, "", true},
		{"age beyond clock-skew tolerance", `[{"age_seconds":-1.000001,"latest_slot":1` + zeroWindow + `}]`, "", true},
		{"repair age beyond clock-skew tolerance", `[{"age_seconds":1,"latest_slot":1,"completed_slots":1,"repair_started_slots":1,"repaired_completed_slots":0,"unresolved_repair_slots":1,"repair_duration_p95_ms":null,"oldest_unresolved_repair_age_seconds":-1.000001}]`, "", true},
		{"string age", `[{"age_seconds":"1","latest_slot":1` + zeroWindow + `}]`, "", true},
		{"negative slot", `[{"age_seconds":1,"latest_slot":-1` + zeroWindow + `}]`, "", true},
		{"fractional slot", `[{"age_seconds":1,"latest_slot":1.5` + zeroWindow + `}]`, "", true},
		{"missing window", `[{"age_seconds":1,"latest_slot":1}]`, "", true},
		{"inconsistent repair counts", `[{"age_seconds":1,"latest_slot":1,"completed_slots":1,"repair_started_slots":2,"repaired_completed_slots":1,"unresolved_repair_slots":0,"repair_duration_p95_ms":2,"oldest_unresolved_repair_age_seconds":null}]`, "", true},
		{"multiple rows", `[{"age_seconds":1,"latest_slot":1` + zeroWindow + `},{"age_seconds":2,"latest_slot":2` + zeroWindow + `}]`, "", true},
		{"top-level null", `null`, "", true},
		{"malformed JSON", `[`, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Write([]byte(tc.response))
			}))
			defer ts.Close()
			out, err := lightbringerIngestHealth(context.Background(), Config{}, ts.URL, "lightbringer")
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v; output=%+v", err, tc.wantErr, out)
			}
			if !tc.wantErr && out.ObservationState != tc.wantState {
				t.Fatalf("state = %q, want %q", out.ObservationState, tc.wantState)
			}
		})
	}
}

func TestLightbringerIngestHealthToleratesSmallClockSkew(t *testing.T) {
	for _, age := range []string{"-0.000001", "-1"} {
		t.Run(age, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `[{"age_seconds":%s,"latest_slot":1,"completed_slots":1,"repair_started_slots":0,"repaired_completed_slots":0,"unresolved_repair_slots":0,"repair_duration_p95_ms":null,"oldest_unresolved_repair_age_seconds":null}]`, age)
			}))
			defer ts.Close()

			out, err := lightbringerIngestHealth(context.Background(), Config{}, ts.URL, "lightbringer")
			if err != nil {
				t.Fatalf("small clock skew was rejected: %v", err)
			}
			if !out.ClockSkewAdjusted || out.LastCompletionAgeSec == nil || *out.LastCompletionAgeSec != 0 || !strings.Contains(out.Note, "clamped") {
				t.Fatalf("clock-skew adjustment was not reported honestly: %+v", out)
			}
		})
	}
}

func TestLightbringerIngestHealthToleratesSmallRepairClockSkew(t *testing.T) {
	for _, age := range []string{"-0.000001", "-1"} {
		t.Run(age, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `[{"age_seconds":1,"latest_slot":1,"completed_slots":1,"repair_started_slots":1,"repaired_completed_slots":0,"unresolved_repair_slots":1,"repair_duration_p95_ms":null,"oldest_unresolved_repair_age_seconds":%s}]`, age)
			}))
			defer ts.Close()

			out, err := lightbringerIngestHealth(context.Background(), Config{}, ts.URL, "lightbringer")
			if err != nil {
				t.Fatalf("small repair clock skew was rejected: %v", err)
			}
			if !out.ClockSkewAdjusted || out.WindowOldestUnresolvedRepairAgeSec == nil || *out.WindowOldestUnresolvedRepairAgeSec != 0 || !strings.Contains(out.Note, "repair timestamp") {
				t.Fatalf("repair clock-skew adjustment was not reported honestly: %+v", out)
			}
		})
	}
}

func TestLightbringerIngestFreshnessBoundaries(t *testing.T) {
	tests := []struct {
		name      string
		age       string
		completed uint64
		want      string
	}{
		{name: "fresh at five minutes", age: "300", completed: 1, want: "observed"},
		{name: "stale beyond five minutes", age: "300.000001", completed: 1, want: "stale"},
		{name: "stale without a window completion", age: "1", completed: 0, want: "stale"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `[{"age_seconds":%s,"latest_slot":1,"completed_slots":%d,"repair_started_slots":0,"repaired_completed_slots":0,"unresolved_repair_slots":0,"repair_duration_p95_ms":null,"oldest_unresolved_repair_age_seconds":null}]`, tc.age, tc.completed)
			}))
			defer ts.Close()

			out, err := lightbringerIngestHealth(context.Background(), Config{}, ts.URL, "lightbringer")
			if err != nil {
				t.Fatal(err)
			}
			if out.ObservationState != tc.want {
				t.Fatalf("state = %q, want %q", out.ObservationState, tc.want)
			}
		})
	}
}

func TestLightbringerMemory(t *testing.T) {
	var gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"q"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode memory query: %v", err)
			return
		}
		gotQuery = body.Query
		w.Write([]byte(`[{"sample_count":90,"latest_sample_age_seconds":2,"current_rss_bytes":2048,"current_virtual_bytes":8192,"peak_rss_bytes":4096,"rss_change_bytes":512,"observed_span_seconds":890,"rss_growth_bytes_per_second":0.5752808988764045}]`))
	}))
	defer ts.Close()

	out, err := lightbringerMemory(context.Background(), Config{}, ts.URL, "lightbringer")
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery != lightbringerMemorySQL || !strings.Contains(gotQuery, "INTERVAL '15 minutes'") || !strings.Contains(gotQuery, "kind = 'process'") {
		t.Errorf("memory query = %q", gotQuery)
	}
	if out.ObservationState != "observed" || out.WindowSeconds != 900 || out.SampleCount != 90 ||
		out.LatestSampleAgeSec == nil || *out.LatestSampleAgeSec != 2 ||
		out.CurrentRSSBytes == nil || *out.CurrentRSSBytes != 2048 ||
		out.CurrentVirtualBytes == nil || *out.CurrentVirtualBytes != 8192 ||
		out.PeakRSSBytes == nil || *out.PeakRSSBytes != 4096 ||
		out.RSSChangeBytes == nil || *out.RSSChangeBytes != 512 ||
		out.ObservedSpanSec == nil || *out.ObservedSpanSec != 890 || out.RSSGrowthBytesPerSec == nil {
		t.Fatalf("memory output = %+v", out)
	}
}

func TestLightbringerMemoryFreshnessBoundary(t *testing.T) {
	for _, tc := range []struct {
		name string
		age  string
		want string
	}{
		{name: "fresh at thirty seconds", age: "30", want: "observed"},
		{name: "stale beyond thirty seconds", age: "30.000001", want: "stale"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `[{"sample_count":1,"latest_sample_age_seconds":%s,"current_rss_bytes":2,"current_virtual_bytes":3,"peak_rss_bytes":2,"rss_change_bytes":0,"observed_span_seconds":0,"rss_growth_bytes_per_second":null}]`, tc.age)
			}))
			defer ts.Close()

			out, err := lightbringerMemory(context.Background(), Config{}, ts.URL, "lightbringer")
			if err != nil {
				t.Fatal(err)
			}
			if out.ObservationState != tc.want {
				t.Fatalf("state = %q, want %q", out.ObservationState, tc.want)
			}
		})
	}
}

func TestLightbringerMemoryNoDataAndStrictParsing(t *testing.T) {
	tests := []struct {
		name     string
		response string
		wantErr  bool
	}{
		{"no rows", `[]`, false},
		{"aggregate with no samples", `[{"sample_count":0,"latest_sample_age_seconds":null,"current_rss_bytes":null,"current_virtual_bytes":null,"peak_rss_bytes":null,"rss_change_bytes":null,"observed_span_seconds":null,"rss_growth_bytes_per_second":null}]`, false},
		{"missing count", `[{"latest_sample_age_seconds":null}]`, true},
		{"values with zero samples", `[{"sample_count":0,"latest_sample_age_seconds":1,"current_rss_bytes":1,"current_virtual_bytes":1,"peak_rss_bytes":1,"rss_change_bytes":0,"observed_span_seconds":0,"rss_growth_bytes_per_second":null}]`, true},
		{"peak below current", `[{"sample_count":1,"latest_sample_age_seconds":1,"current_rss_bytes":2,"current_virtual_bytes":3,"peak_rss_bytes":1,"rss_change_bytes":0,"observed_span_seconds":0,"rss_growth_bytes_per_second":null}]`, true},
		{"growth without span", `[{"sample_count":1,"latest_sample_age_seconds":1,"current_rss_bytes":2,"current_virtual_bytes":3,"peak_rss_bytes":2,"rss_change_bytes":0,"observed_span_seconds":0,"rss_growth_bytes_per_second":1}]`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Write([]byte(tc.response))
			}))
			defer ts.Close()
			out, err := lightbringerMemory(context.Background(), Config{}, ts.URL, "lightbringer")
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v; output=%+v", err, tc.wantErr, out)
			}
			if !tc.wantErr && out.ObservationState != "no_memory_data" {
				t.Fatalf("state = %q", out.ObservationState)
			}
		})
	}
}

func TestStreamProbePartialSampleIsIncomplete(t *testing.T) {
	out := probeFakeLightbringerStream(t, context.Background(), &fakeSlotServer{slots: []uint64{500}})
	if !out.Reachable || !out.ActivityObserved || out.SlotsSeen != 1 || out.DistinctSlots != 1 {
		t.Fatalf("want reachable with 1 slot, got %+v", out)
	}
	if out.CompleteSample || out.State != "incomplete" || out.TerminalError == "" {
		t.Error("a partial sample must remain incomplete")
	}
}

func TestStreamProbeAllowsSkippedSlotWhenParentConnects(t *testing.T) {
	out := probeFakeLightbringerStream(t, context.Background(), &fakeSlotServer{responses: []overcast.SlotResponse{
		{Slot: 100, ParentSlot: 99},
		{Slot: 102, ParentSlot: 100}, // slot 101 was skipped; parent still connects
		{Slot: 103, ParentSlot: 102},
	}})
	if !out.ActivityObserved || out.State != "active" || out.DistinctSlots != 3 {
		t.Fatalf("skipped-slot activity = %+v", out)
	}
}

func TestStreamProbeDoesNotInferContinuityFromCompletionOrder(t *testing.T) {
	out := probeFakeLightbringerStream(t, context.Background(), &fakeSlotServer{responses: []overcast.SlotResponse{
		{Slot: 102, ParentSlot: 100},
		{Slot: 100, ParentSlot: 99}, // completed after its child was delivered
		{Slot: 103, ParentSlot: 102},
	}})
	if !out.ActivityObserved || !out.CompleteSample || out.State != "active" || out.DistinctSlots != 3 {
		t.Fatalf("out-of-order completion activity = %+v", out)
	}
}

func TestStreamProbeDuplicateSlotsAreInconclusive(t *testing.T) {
	out := probeFakeLightbringerStream(t, context.Background(), &fakeSlotServer{responses: []overcast.SlotResponse{
		{Slot: 100, ParentSlot: 99},
		{Slot: 100, ParentSlot: 99},
		{Slot: 100, ParentSlot: 99},
	}})
	if !out.ActivityObserved || !out.CompleteSample || out.State != "no_progress" || out.DistinctSlots != 1 {
		t.Fatalf("duplicate-slot sample = %+v", out)
	}
}

func TestStreamProbeReportsTerminalErrorAfterPartialSample(t *testing.T) {
	out := probeFakeLightbringerStream(t, context.Background(), &fakeSlotServer{
		responses: []overcast.SlotResponse{{Slot: 100, ParentSlot: 99}, {Slot: 101, ParentSlot: 100}},
		terminalErr: status.Error(codes.Unavailable,
			"remote failure\ntoken=TERMINAL_SECRET "+strings.Repeat("x", maxProbeErrorRunes*2)),
	})
	if !out.Reachable || !out.ActivityObserved || out.CompleteSample || out.State != "incomplete" {
		t.Fatalf("partial terminal failure = %+v", out)
	}
	if out.TerminalError == "" {
		t.Fatalf("terminal error missing: %+v", out)
	}
	if strings.ContainsAny(out.TerminalError, "\r\n") || len([]rune(out.TerminalError)) > maxProbeErrorRunes {
		t.Fatalf("terminal error is not bounded/sanitized: %q", out.TerminalError)
	}
	if strings.Contains(out.TerminalError, "TERMINAL_SECRET") {
		t.Fatalf("terminal error leaked a secret: %q", out.TerminalError)
	}
}

func TestStreamProbeResourceExhaustedIsInconclusive(t *testing.T) {
	out := probeFakeLightbringerStream(t, context.Background(), &fakeSlotServer{
		terminalErr: status.Error(codes.ResourceExhausted, "slot message exceeds receive limit"),
	})
	if out.State != "inconclusive" || out.Reachable || out.ActivityObserved || out.TerminalError == "" {
		t.Fatalf("resource exhausted probe = %+v", out)
	}
}

func TestStreamProbeRejectsBlockedAddr(t *testing.T) {
	_, err := lightbringerStreamProbe(context.Background(), "169.254.169.254:3001")
	if err == nil {
		t.Error("a blocked dial target must be a hard error, not a soft reachable=false")
	}
}
