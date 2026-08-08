package monitor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Detection bounds are fixed here so a slow fault fails the test.
const (
	// A collection cycle must finish within this bound even when every endpoint
	// is unreachable: the cycle that reports an outage must not be stalled by
	// that same outage.
	sloDetectionCycle = 3 * time.Second
	// Recovery must show in the next cycle. An alert that fires but never
	// resolves is an incomplete signal.
	sloRecoveryCycles = 1
)

// injectable is one fault this harness can produce honestly.
type injectable struct {
	name string
	// apply produces the fault and returns a function that clears it, so
	// recovery is exercised by the same case.
	apply func(f *injectFixture) func()
	// wantProbeZero names roles whose probe must read 0 while injected.
	wantProbeZero []string
	// wantCollectorZero names collectors that must report failure.
	wantCollectorZero []string
}

type injectFixture struct {
	nodeUp, primaryUp, fallbackUp bool
	nodeBody                      string
	collector                     *Collector
	registry                      *prometheus.Registry
	read                          func(string) map[string]float64
}

// newInjectFixture wires a collector against three controllable endpoints.
func newInjectFixture(t *testing.T) *injectFixture {
	t.Helper()
	f := &injectFixture{nodeUp: true, primaryUp: true, fallbackUp: true, nodeBody: epochInfo("1000")}

	mk := func(up *bool, body func() string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if !*up {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(body()))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	node := mk(&f.nodeUp, func() string { return f.nodeBody })
	primary := mk(&f.primaryUp, func() string { return slotResult("1150") })
	fallback := mk(&f.fallbackUp, func() string { return slotResult("1150") })
	inventory := newInventoryFixture(t)
	nodeExporter := nodeExporterServer(t, validNodeExporterMetrics())

	reg := prometheus.NewRegistry()
	f.registry = reg
	cfg := stateConfig(inventory, nodeExporter.URL)
	cfg.NodeRPCURL = node.URL
	cfg.ReferencePrimaryURL = primary.URL
	cfg.ReferenceFallbackURL = fallback.URL
	cfg.ProbeTimeoutSeconds = 1
	f.collector = New(cfg, NewMetrics(reg))

	f.read = func(family string) map[string]float64 {
		gathered, err := reg.Gather()
		if err != nil {
			t.Fatalf("gather: %v", err)
		}
		out := map[string]float64{}
		for _, mf := range gathered {
			if mf.GetName() != family {
				continue
			}
			for _, m := range mf.GetMetric() {
				key := ""
				for _, l := range m.GetLabel() {
					if key != "" {
						key += ","
					}
					key += l.GetName() + "=" + l.GetValue()
				}
				out[key] = m.GetGauge().GetValue()
			}
		}
		return out
	}
	return f
}

func TestFailureInjectionMatrix(t *testing.T) {
	faults := []injectable{
		{
			name:              "node process loss",
			apply:             func(f *injectFixture) func() { f.nodeUp = false; return func() { f.nodeUp = true } },
			wantProbeZero:     []string{"node"},
			wantCollectorZero: []string{CollectorNodeRPC},
		},
		{
			name: "node RPC returns malformed evidence",
			apply: func(f *injectFixture) func() {
				prev := f.nodeBody
				f.nodeBody = `{"jsonrpc":"2.0","id":1,"result":{"epoch":1}}`
				return func() { f.nodeBody = prev }
			},
			wantProbeZero:     []string{"node"},
			wantCollectorZero: []string{CollectorNodeRPC},
		},
		{
			name: "node RPC returns a JSON-RPC error",
			apply: func(f *injectFixture) func() {
				prev := f.nodeBody
				f.nodeBody = `{"jsonrpc":"2.0","id":1,"error":{"code":-32601}}`
				return func() { f.nodeBody = prev }
			},
			wantProbeZero:     []string{"node"},
			wantCollectorZero: []string{CollectorNodeRPC},
		},
		{
			name:          "primary reference loss",
			apply:         func(f *injectFixture) func() { f.primaryUp = false; return func() { f.primaryUp = true } },
			wantProbeZero: []string{"reference_primary"},
		},
		{
			name: "both references lost",
			apply: func(f *injectFixture) func() {
				f.primaryUp, f.fallbackUp = false, false
				return func() { f.primaryUp, f.fallbackUp = true, true }
			},
			wantProbeZero:     []string{"reference_primary", "reference_fallback"},
			wantCollectorZero: []string{CollectorReferenceRPC},
		},
		{
			name: "total outage: node and both references",
			apply: func(f *injectFixture) func() {
				f.nodeUp, f.primaryUp, f.fallbackUp = false, false, false
				return func() { f.nodeUp, f.primaryUp, f.fallbackUp = true, true, true }
			},
			wantProbeZero:     []string{"node", "reference_primary", "reference_fallback"},
			wantCollectorZero: []string{CollectorNodeRPC, CollectorReferenceRPC},
		},
	}

	for _, fault := range faults {
		t.Run(fault.name, func(t *testing.T) {
			f := newInjectFixture(t)
			f.collector.Collect(context.Background())
			for _, role := range fault.wantProbeZero {
				if got := f.read("mithril_rpc_probe_success")["role="+role]; got != 1 {
					t.Fatalf("baseline: role %s not healthy before injection", role)
				}
			}

			clear := fault.apply(f)

			start := time.Now()
			f.collector.Collect(context.Background())
			if elapsed := time.Since(start); elapsed > sloDetectionCycle {
				t.Errorf("detection took %v, exceeding the %v SLO", elapsed, sloDetectionCycle)
			}

			probes := f.read("mithril_rpc_probe_success")
			for _, role := range fault.wantProbeZero {
				if got := probes["role="+role]; got != 0 {
					t.Errorf("role %s reads %v under %q, want 0", role, got, fault.name)
				}
			}
			collectors := f.read("mithril_monitor_collect_success")
			for _, c := range fault.wantCollectorZero {
				if got := collectors["collector="+c]; got != 0 {
					t.Errorf("collector %s reads %v under %q, want 0", c, got, fault.name)
				}
			}
			// A collector that vanishes is indistinguishable from a scrape
			// failure, so the inventory must stay complete under every fault.
			if len(collectors) != 3 {
				t.Errorf("collector inventory is %d under %q, want 3", len(collectors), fault.name)
			}

			clear()
			for i := 0; i < sloRecoveryCycles; i++ {
				f.collector.Collect(context.Background())
			}
			probes = f.read("mithril_rpc_probe_success")
			for _, role := range fault.wantProbeZero {
				if got := probes["role="+role]; got != 1 {
					t.Errorf("role %s did not recover within %d cycle(s): %v", role, sloRecoveryCycles, got)
				}
			}
		})
	}
}

// Keep the monitor-owned metric inventory aligned with its rule consumers.
func TestMonitorMetricNamesAppearInAlertRules(t *testing.T) {
	rules, err := os.ReadFile(filepath.Join("..", "..", "prometheus", "rules", "mithril.yml"))
	if err != nil {
		t.Skipf("alert rules not present: %v", err)
	}

	// Families the rules reference that this monitor is responsible for.
	monitorFamilies := map[string]bool{
		"mithril_rpc_probe_success":                         true,
		"mithril_rpc_probe_last_success_timestamp_seconds":  true,
		"mithril_node_replay_slot":                          true,
		"mithril_reference_slot":                            true,
		"mithril_node_slot_delta":                           true,
		"mithril_reference_slot_disagreement_slots":         true,
		"mithril_monitor_collect_success":                   true,
		"mithril_monitor_last_collection_timestamp_seconds": true,
		"mithril_monitor_identity_info":                     true,
		"mithril_expected_target":                           true,
		"mithril_expected_filesystem_role":                  true,
		"mithril_filesystem_avail_bytes":                    true,
		"mithril_filesystem_size_bytes":                     true,
	}

	// What the monitor actually emits. The isolated registry contains only
	// monitor-owned families, so this also discovers future additions.
	f := newInjectFixture(t)
	f.collector.Collect(context.Background())
	emitted := map[string]bool{}
	gathered, err := f.registry.Gather()
	if err != nil {
		t.Fatalf("gather monitor metrics: %v", err)
	}
	for _, family := range gathered {
		emitted[family.GetName()] = true
	}

	var missing []string
	for family := range monitorFamilies {
		if !emitted[family] || !strings.Contains(string(rules), family) {
			missing = append(missing, family)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("alert rules consume monitor families that the monitor never emits: %v", missing)
	}

	// And the reverse: every monitor family must have a rule consumer. Otherwise
	// the producer can look healthy while its evidence is ignored.
	var unused []string
	for family := range emitted {
		if !strings.Contains(string(rules), family) {
			unused = append(unused, family)
		}
	}
	sort.Strings(unused)
	if len(unused) > 0 {
		t.Fatalf("monitor emits families that no alert rule consumes: %v", unused)
	}
}

func TestMonitorEmitsExpectedAlertLabelValues(t *testing.T) {
	f := newInjectFixture(t)
	f.collector.Collect(context.Background())

	// role= values the rules select on must be values the monitor really emits.
	emittedRoles := map[string]bool{}
	for key := range f.read("mithril_rpc_probe_success") {
		emittedRoles[strings.TrimPrefix(key, "role=")] = true
	}
	for _, role := range []string{RoleNode, RoleReferencePrimary, RoleReferenceFallback} {
		if !emittedRoles[role] {
			t.Errorf("rules select role=%q but the monitor never emits it", role)
		}
	}

	// The delta series must carry BOTH the local view and the provider
	// commitment, or a rule cannot express what it is comparing.
	for key := range f.read("mithril_node_slot_delta") {
		if !strings.Contains(key, "node_view="+NodeViewLocalReplay) {
			t.Errorf("delta series %q lacks the node_view label the rules rely on", key)
		}
		if !strings.Contains(key, "commitment="+CommitmentConfirmed) {
			t.Errorf("delta series %q lacks the commitment label the rules rely on", key)
		}
	}
}
