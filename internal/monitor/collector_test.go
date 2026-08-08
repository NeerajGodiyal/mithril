package monitor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type monitorRoundTripper func(*http.Request) (*http.Response, error)

func (fn monitorRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestNewHandlesCustomDefaultTransport(t *testing.T) {
	original := http.DefaultTransport
	http.DefaultTransport = monitorRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unused")
	})
	t.Cleanup(func() { http.DefaultTransport = original })

	collector := New(Config{}, NewMetrics(prometheus.NewRegistry()))
	transport, ok := collector.client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("monitor did not install a direct HTTP transport")
	}
}

// fakeRPC serves one canned JSON-RPC response.
func fakeRPC(t *testing.T, body string, status int, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if delay > 0 {
			time.Sleep(delay)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func epochInfo(slot string) string {
	return `{"jsonrpc":"2.0","id":1,"result":{"absoluteSlot":` + slot + `,"epoch":1,"slotIndex":1,"slotsInEpoch":432000}}`
}

func slotResult(slot string) string {
	return `{"jsonrpc":"2.0","id":1,"result":` + slot + `}`
}

func TestNewCollectorDisablesAmbientHTTPProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")

	collector := New(Config{}, NewMetrics(prometheus.NewRegistry()))
	transport, ok := collector.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", collector.client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("collector inherited an ambient proxy function")
	}
}

// harness wires a collector against three fake endpoints and returns a reader
// for the resulting series.
func harness(t *testing.T, nodeBody, primaryBody, fallbackBody string) (*Collector, func(string) map[string]float64) {
	t.Helper()
	node := fakeRPC(t, nodeBody, http.StatusOK, 0)
	primary := fakeRPC(t, primaryBody, http.StatusOK, 0)
	fallback := fakeRPC(t, fallbackBody, http.StatusOK, 0)

	reg := prometheus.NewRegistry()
	cfg := Config{
		NodeRPCURL:           node.URL,
		ReferencePrimaryURL:  primary.URL,
		ReferenceFallbackURL: fallback.URL,
		ProbeTimeoutSeconds:  2,
	}
	c := New(cfg, NewMetrics(reg))

	read := func(family string) map[string]float64 {
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
	return c, read
}

func TestCollectHappyPathProducesSignedDelta(t *testing.T) {
	c, read := harness(t, epochInfo("1000"), slotResult("1150"), slotResult("1148"))
	c.Collect(context.Background())

	if got := read("mithril_rpc_probe_success"); got["role=node"] != 1 ||
		got["role=reference_primary"] != 1 || got["role=reference_fallback"] != 1 {
		t.Fatalf("probe success = %v, want all 1", got)
	}
	if got := read("mithril_node_replay_slot")["node_view=local_replay"]; got != 1000 {
		t.Errorf("node replay slot = %v, want 1000", got)
	}
	delta := read("mithril_node_slot_delta")
	if got := delta["commitment=confirmed,node_view=local_replay,role=primary"]; got != 150 {
		t.Errorf("primary delta = %v, want 150 (provider minus node)", got)
	}
	if got := read("mithril_reference_slot_disagreement_slots")["commitment=confirmed"]; got != 2 {
		t.Errorf("disagreement = %v, want 2", got)
	}
	if got := read("mithril_monitor_collect_success"); len(got) != 3 {
		t.Fatalf("collect_success has %d series, want exactly 3: %v", len(got), got)
	}
	if got := read("mithril_monitor_last_collection_timestamp_seconds")[""]; got == 0 {
		t.Fatal("collection completion timestamp was not published")
	}
}

func TestLocalAheadDeltaIsNegativeNotClamped(t *testing.T) {
	c, read := harness(t, epochInfo("2000"), slotResult("1950"), slotResult("1950"))
	c.Collect(context.Background())

	got := read("mithril_node_slot_delta")["commitment=confirmed,node_view=local_replay,role=primary"]
	if got != -50 {
		t.Fatalf("delta = %v, want -50; a local-ahead result must not be clamped", got)
	}
}

func TestNodeOutageIsVisibleWithoutMCP(t *testing.T) {
	primary := fakeRPC(t, slotResult("1150"), http.StatusOK, 0)
	fallback := fakeRPC(t, slotResult("1150"), http.StatusOK, 0)
	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	down.Close() // refuse connections, as a dead host would

	reg := prometheus.NewRegistry()
	c := New(Config{
		NodeRPCURL: down.URL, ReferencePrimaryURL: primary.URL,
		ReferenceFallbackURL: fallback.URL, ProbeTimeoutSeconds: 2,
	}, NewMetrics(reg))
	c.Collect(context.Background())

	gathered, _ := reg.Gather()
	values := map[string]float64{}
	for _, mf := range gathered {
		for _, m := range mf.GetMetric() {
			key := mf.GetName()
			for _, l := range m.GetLabel() {
				key += "," + l.GetName() + "=" + l.GetValue()
			}
			values[key] = m.GetGauge().GetValue()
		}
	}

	if values["mithril_rpc_probe_success,role=node"] != 0 {
		t.Error("a dead node reported probe success")
	}
	if values["mithril_monitor_collect_success,collector=node_rpc"] != 0 {
		t.Error("node_rpc collector reported success against a dead node")
	}
	// The references still work, so their evidence must survive the node outage.
	if values["mithril_rpc_probe_success,role=reference_primary"] != 1 {
		t.Error("a node outage suppressed reference evidence")
	}
	// No delta may be published against an absent node slot.
	// Prometheus sorts labels by NAME, so the key order is commitment,
	// node_view, role. Getting this wrong makes the lookup miss and the
	// assertion pass vacuously.
	const deltaKey = "mithril_node_slot_delta,commitment=confirmed,node_view=local_replay,role=primary"
	if _, present := values[deltaKey]; !present {
		t.Fatalf("delta series missing entirely; keys were %v", keysOf(values))
	}
	if values[deltaKey] != 0 {
		t.Errorf("a delta of %v was computed against an unreachable node", values[deltaKey])
	}
}

func TestMalformedAndStaleResponsesFailClosed(t *testing.T) {
	bodies := map[string]string{
		"not json":             `{`,
		"jsonrpc error":        `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found"}}`,
		"no result":            `{"jsonrpc":"2.0","id":1}`,
		"result wrong type":    `{"jsonrpc":"2.0","id":1,"result":"1000"}`,
		"missing absoluteSlot": `{"jsonrpc":"2.0","id":1,"result":{"epoch":1}}`,
		"null result":          `{"jsonrpc":"2.0","id":1,"result":null}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			c, read := harness(t, body, slotResult("1150"), slotResult("1150"))
			c.Collect(context.Background())
			if got := read("mithril_rpc_probe_success")["role=node"]; got != 0 {
				t.Fatalf("malformed node response reported success")
			}
			if got := read("mithril_node_replay_slot")["node_view=local_replay"]; got != 0 {
				t.Errorf("a malformed response published slot %v", got)
			}
		})
	}
}

func TestGetSlotNullFailsClosed(t *testing.T) {
	c, read := harness(t, epochInfo("1000"), `{"jsonrpc":"2.0","id":1,"result":null}`, slotResult("1150"))
	c.Collect(context.Background())

	if got := read("mithril_rpc_probe_success")["role=reference_primary"]; got != 0 {
		t.Fatalf("null getSlot reported success: %v", got)
	}
	if got := read("mithril_monitor_collect_success")["collector=reference_rpc"]; got != 1 {
		t.Fatalf("healthy fallback was suppressed: %v", got)
	}
}

func TestSlowEndpointIsBounded(t *testing.T) {
	slow := fakeRPC(t, slotResult("1150"), http.StatusOK, 3*time.Second)
	node := fakeRPC(t, epochInfo("1000"), http.StatusOK, 0)
	fallback := fakeRPC(t, slotResult("1150"), http.StatusOK, 0)

	reg := prometheus.NewRegistry()
	c := New(Config{
		NodeRPCURL: node.URL, ReferencePrimaryURL: slow.URL,
		ReferenceFallbackURL: fallback.URL, ProbeTimeoutSeconds: 1,
	}, NewMetrics(reg))

	start := time.Now()
	c.Collect(context.Background())
	// The endpoint sleeps 3s and the probe timeout is 1s, so a bounded cycle
	// finishes near 1s. Allowing 3s would pass even if the bound were ignored.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("collection took %v with a 1s probe timeout; the bound was not applied", elapsed)
	}
}

func TestAllSlowEndpointsShareOneTimeoutWindow(t *testing.T) {
	slow := func(body string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(3 * time.Second):
				_, _ = w.Write([]byte(body))
			}
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	node := slow(epochInfo("1000"))
	primary := slow(slotResult("1150"))
	fallback := slow(slotResult("1150"))
	c := New(Config{
		NodeRPCURL: node.URL, ReferencePrimaryURL: primary.URL,
		ReferenceFallbackURL: fallback.URL, ProbeTimeoutSeconds: 1,
	}, NewMetrics(prometheus.NewRegistry()))

	start := time.Now()
	c.Collect(context.Background())
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("three timed-out probes took %v; want one shared timeout window", elapsed)
	}
}

func TestErrorsNeverContainConfiguredValues(t *testing.T) {
	const secret = "SUPERSECRETAPIKEY123"
	url := "http://127.0.0.1:1/rpc?api-key=" + secret

	reg := prometheus.NewRegistry()
	c := New(Config{
		NodeRPCURL: url, ReferencePrimaryURL: url, ReferenceFallbackURL: url,
		ProbeTimeoutSeconds: 1,
	}, NewMetrics(reg))

	_, err := c.ProbeOnce(context.Background(), url, "getSlot")
	if err == nil {
		t.Fatal("probe of an unreachable endpoint succeeded")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("probe error leaked the API key: %v", err)
	}
	if strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("probe error leaked the configured host: %v", err)
	}
}

func TestNodeIsNeverLabelledConfirmed(t *testing.T) {
	c, read := harness(t, epochInfo("1000"), slotResult("1150"), slotResult("1150"))
	c.Collect(context.Background())

	for key := range read("mithril_node_replay_slot") {
		if strings.Contains(key, "confirmed") {
			t.Fatalf("node replay slot carries a confirmed label: %q", key)
		}
		if !strings.Contains(key, NodeViewLocalReplay) {
			t.Errorf("node replay slot is not labelled local_replay: %q", key)
		}
	}
	// The delta names both the local view and the provider commitment, so the
	// asymmetry of the comparison stays visible in the series itself.
	for key := range read("mithril_node_slot_delta") {
		if !strings.Contains(key, "node_view=local_replay") || !strings.Contains(key, "commitment=confirmed") {
			t.Errorf("delta series does not record both sides of the comparison: %q", key)
		}
	}
}

func TestConfigRejectsUnsafePermissions(t *testing.T) {
	dir := trustedTempDir(t)
	path := filepath.Join(dir, "providers.toml")
	content := `node_rpc_url = "http://127.0.0.1:8899"
reference_primary_url = "https://primary.example/rpc?api-key=SECRET"
reference_fallback_url = "https://fallback.example/rpc?api-key=SECRET"
` + validStateConfigFields(dir)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("0600 config was rejected: %v", err)
	}

	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o666} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		_, err := LoadConfig(path)
		if err == nil {
			t.Errorf("mode %o was accepted for a credentials file", mode)
			continue
		}
		if !strings.Contains(err.Error(), "permissions") {
			t.Errorf("mode %o error = %q, want it to name permissions", mode, err)
		}
		if strings.Contains(err.Error(), "SECRET") {
			t.Errorf("config error leaked file content: %v", err)
		}
	}
}

func TestConfigErrorsNameFieldsNotValues(t *testing.T) {
	dir := trustedTempDir(t)
	path := filepath.Join(dir, "providers.toml")
	const secret = "SUPERSECRETAPIKEY123"

	cases := map[string]string{
		"missing node": `reference_primary_url = "https://a.example/?k=` + secret + `"
reference_fallback_url = "https://b.example/?k=` + secret + `"`,
		"non-http scheme": `node_rpc_url = "ftp://evil.example/?k=` + secret + `"
reference_primary_url = "https://a.example"
reference_fallback_url = "https://b.example"`,
		"malformed toml": `node_rpc_url = "https://a.example/?k=` + secret + `"` + "\nthis is not toml",
		"unknown field": `node_rpc_url = "https://a.example/?k=` + secret + `"
reference_primary_url = "https://b.example"
reference_fallback_url = "https://c.example"
reference_primray_url = "https://typo.example"`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(path)
			if err == nil {
				t.Fatal("invalid config was accepted")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("config error leaked a credential: %v", err)
			}
		})
	}
}

func TestConfigRejectsMalformedHTTPURLs(t *testing.T) {
	dir := trustedTempDir(t)
	path := filepath.Join(dir, "providers.toml")
	const secret = "SUPERSECRETAPIKEY123"

	for name, nodeURL := range map[string]string{
		"missing host":      "http:///rpc",
		"missing authority": "https://",
		"wrong scheme":      "httpx://example.test",
		"invalid escape":    "https://example.test/%zz",
		"fragment":          "https://example.test/rpc#secret=" + secret,
		"leading space":     " https://example.test",
	} {
		t.Run(name, func(t *testing.T) {
			content := fmt.Sprintf(
				"node_rpc_url = %q\nreference_primary_url = \"https://a.example\"\nreference_fallback_url = \"https://b.example\"\n",
				nodeURL,
			)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(path)
			if err == nil {
				t.Fatal("malformed URL was accepted")
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), nodeURL) {
				t.Fatalf("validation error leaked the configured value: %v", err)
			}
		})
	}
}

func TestConfigRequiresDistinctSecureReferenceHosts(t *testing.T) {
	dir := trustedTempDir(t)
	path := filepath.Join(dir, "providers.toml")
	for name, endpoints := range map[string][2]string{
		"same host": {
			"https://same.example/primary",
			"https://same.example/fallback",
		},
		"same host with root dot": {
			"https://same.example/primary",
			"https://SAME.EXAMPLE./fallback",
		},
		"plaintext primary": {
			"http://primary.example/rpc",
			"https://fallback.example/rpc",
		},
		"plaintext fallback": {
			"https://primary.example/rpc",
			"http://fallback.example/rpc",
		},
	} {
		t.Run(name, func(t *testing.T) {
			content := fmt.Sprintf(
				"node_rpc_url = \"http://127.0.0.1:8899\"\nreference_primary_url = %q\nreference_fallback_url = %q\n%s",
				endpoints[0],
				endpoints[1],
				validStateConfigFields(dir),
			)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(path); err == nil {
				t.Fatal("unsafe reference configuration was accepted")
			}
		})
	}
}

func TestProbeRejectsOversizedResponse(t *testing.T) {
	body := slotResult("1150") + strings.Repeat(" ", maxProbeResponseBytes)
	server := fakeRPC(t, body, http.StatusOK, 0)
	c := New(Config{ProbeTimeoutSeconds: 1}, NewMetrics(prometheus.NewRegistry()))

	if _, err := c.ProbeOnce(context.Background(), server.URL, "getSlot"); err == nil {
		t.Fatal("oversized response was accepted")
	}
}

func TestProbeDoesNotFollowRedirects(t *testing.T) {
	var followed bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		followed = true
	}))
	t.Cleanup(target.Close)
	redirect := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusTemporaryRedirect))
	t.Cleanup(redirect.Close)

	c := New(Config{ProbeTimeoutSeconds: 1}, NewMetrics(prometheus.NewRegistry()))
	if _, err := c.ProbeOnce(context.Background(), redirect.URL, "getSlot"); err == nil {
		t.Fatal("redirect response was accepted")
	}
	if followed {
		t.Fatal("probe followed a redirect")
	}
}

func TestDecodeSlotRejectsValuesOutsideExactMetricRange(t *testing.T) {
	const (
		maximum  = "9007199254740991"
		overflow = "9007199254740992"
	)
	for _, tc := range []struct {
		name, method, body string
	}{
		{"getSlot", "getSlot", slotResult(overflow)},
		{"getEpochInfo", "getEpochInfo", epochInfo(overflow)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeSlot([]byte(tc.body), tc.method); err == nil {
				t.Fatal("slot outside the exact metric range was accepted")
			}
		})
	}

	for _, tc := range []struct {
		method, body string
	}{
		{"getSlot", slotResult(maximum)},
		{"getEpochInfo", epochInfo(maximum)},
	} {
		if got, err := decodeSlot([]byte(tc.body), tc.method); err != nil || got != maxExactMetricInteger {
			t.Errorf("%s maximum exact metric slot = %d, %v", tc.method, got, err)
		}
	}
}

func TestDecodeSlotRejectsInvalidJSONRPCEnvelopes(t *testing.T) {
	for name, body := range map[string]string{
		"missing version":          `{"id":1,"result":10}`,
		"wrong version":            `{"jsonrpc":"1.0","id":1,"result":10}`,
		"missing id":               `{"jsonrpc":"2.0","result":10}`,
		"wrong numeric id":         `{"jsonrpc":"2.0","id":2,"result":10}`,
		"string id":                `{"jsonrpc":"2.0","id":"1","result":10}`,
		"result and error":         `{"jsonrpc":"2.0","id":1,"result":10,"error":{"code":-1}}`,
		"neither result nor error": `{"jsonrpc":"2.0","id":1}`,
		"null error":               `{"jsonrpc":"2.0","id":1,"error":null}`,
		"duplicate result":         `{"jsonrpc":"2.0","id":1,"result":10,"result":11}`,
		"unknown field":            `{"jsonrpc":"2.0","id":1,"result":10,"extra":true}`,
		"invalid UTF-8":            "{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"absoluteSlot\":10,\"extra\":\"\xff\"}}",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeSlot([]byte(body), "getSlot"); err == nil {
				t.Fatal("invalid JSON-RPC envelope was accepted")
			}
		})
	}
}

// keysOf lists series keys, so a failed lookup reports what was actually there
// instead of silently reading zero.
func keysOf(values map[string]float64) []string {
	out := make([]string, 0, len(values))
	for k := range values {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestProvidersAreProbedAtConfirmed(t *testing.T) {
	var mu sync.Mutex
	bodies := map[string]string{}

	record := func(role string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
			mu.Lock()
			bodies[role] = string(raw)
			mu.Unlock()
			_, _ = w.Write([]byte(slotResult("1150")))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		mu.Lock()
		bodies["node"] = string(raw)
		mu.Unlock()
		_, _ = w.Write([]byte(epochInfo("1000")))
	}))
	t.Cleanup(node.Close)

	c := New(Config{
		NodeRPCURL: node.URL, ReferencePrimaryURL: record("primary").URL,
		ReferenceFallbackURL: record("fallback").URL, ProbeTimeoutSeconds: 2,
	}, NewMetrics(prometheus.NewRegistry()))
	c.Collect(context.Background())

	mu.Lock()
	defer mu.Unlock()
	for _, role := range []string{"primary", "fallback"} {
		body := bodies[role]
		if !strings.Contains(body, `"getSlot"`) {
			t.Errorf("%s was not probed with getSlot: %s", role, body)
		}
		if !strings.Contains(body, `"commitment":"confirmed"`) {
			t.Errorf("%s was probed without confirmed commitment: %s", role, body)
		}
	}
	// The node is asked a different question, and must NOT carry a commitment:
	// its answer is local state with no commitment semantics.
	if !strings.Contains(bodies["node"], `"getEpochInfo"`) {
		t.Errorf("node was not probed with getEpochInfo: %s", bodies["node"])
	}
	if strings.Contains(bodies["node"], "commitment") {
		t.Errorf("node probe requested a commitment it cannot honour: %s", bodies["node"])
	}
}

func TestMonitorConfigDefaultsAndRejections(t *testing.T) {
	if got := (Config{}).ProbeTimeout(); got != DefaultProbeTimeout {
		t.Errorf("unset probe timeout = %v, want the default %v", got, DefaultProbeTimeout)
	}
	if got := (Config{ProbeTimeoutSeconds: -3}).ProbeTimeout(); got != DefaultProbeTimeout {
		t.Errorf("negative probe timeout = %v, want the default", got)
	}
	if got := (Config{ProbeTimeoutSeconds: 9}).ProbeTimeout(); got != 9*time.Second {
		t.Errorf("explicit probe timeout = %v, want 9s", got)
	}

	dir := trustedTempDir(t)
	valid := filepath.Join(dir, "providers.toml")
	good := "node_rpc_url = \"http://127.0.0.1:8899\"\n" +
		"reference_primary_url = \"https://a.example\"\n" +
		"reference_fallback_url = \"https://b.example\"\n" +
		validStateConfigFields(dir)
	if err := os.WriteFile(valid, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, path := range map[string]string{
		"empty":     "",
		"relative":  "providers.toml",
		"unclean":   dir + "/./providers.toml",
		"missing":   filepath.Join(dir, "absent.toml"),
		"directory": dir,
	} {
		if _, err := LoadConfig(path); err == nil {
			t.Errorf("%s path was accepted", name)
		}
	}

	// A symlink is refused even pointing at a valid file: the target can be
	// repointed after the permission check.
	link := filepath.Join(dir, "link.toml")
	_ = os.Symlink(valid, link)
	if _, err := LoadConfig(link); err == nil {
		t.Error("a symlinked config was accepted")
	}

	bad := filepath.Join(dir, "bad.toml")
	for name, content := range map[string]string{
		"missing node":      "reference_primary_url = \"https://a.example\"\nreference_fallback_url = \"https://b.example\"\n",
		"missing primary":   "node_rpc_url = \"http://n\"\nreference_fallback_url = \"https://b.example\"\n",
		"missing fallback":  "node_rpc_url = \"http://n\"\nreference_primary_url = \"https://a.example\"\n",
		"timeout too large": good + "probe_timeout_seconds = 999\n",
		"timeout negative":  good + "probe_timeout_seconds = -1\n",
	} {
		if err := os.WriteFile(bad, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(bad); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	huge := filepath.Join(dir, "huge.toml")
	if err := os.WriteFile(huge, []byte(good+"# "+strings.Repeat("x", 70<<10)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(huge); err == nil {
		t.Error("an oversized config was accepted")
	}
}

func validStateConfigFields(dir string) string {
	return fmt.Sprintf(
		"deployment_id = \"deployment-a\"\n"+
			"systemd_unit = \"mithril-mainnet.service\"\n"+
			"systemd_scope = \"system\"\n"+
			"node_exporter_metrics_url = \"http://127.0.0.1:9100/metrics\"\n"+
			"inventory_manifest_path = %q\n"+
			"inventory_signature_path = %q\n"+
			"inventory_public_key_path = %q\n",
		filepath.Join(dir, "inventory.json"),
		filepath.Join(dir, "inventory.sig"),
		filepath.Join(dir, "inventory.pem"),
	)
}

func TestProbeOnceRejectsUnusableRequests(t *testing.T) {
	const secret = "SUPERSECRETAPIKEY123"
	c := New(Config{ProbeTimeoutSeconds: 1}, NewMetrics(prometheus.NewRegistry()))

	for name, url := range map[string]string{
		"unparseable": "://bad\x7f?k=" + secret,
		"empty":       "",
		"unsupported": "gopher://example/?k=" + secret,
	} {
		_, err := c.ProbeOnce(context.Background(), url, "getSlot")
		if err == nil {
			t.Errorf("%s URL was accepted", name)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s error leaked the key: %v", name, err)
		}
	}
	called := make(chan struct{}, 1)
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called <- struct{}{}
		_, _ = w.Write([]byte(epochInfo("1000")))
	}))
	t.Cleanup(live.Close)
	if _, err := c.ProbeOnce(context.Background(), live.URL, "unknown"); err == nil {
		t.Error("an unsupported probe method was accepted")
	}
	select {
	case <-called:
		t.Error("an unsupported probe method reached the endpoint")
	default:
	}

	// A non-200 response is a failure, and the status is reported without the
	// body — which could contain anything the endpoint chose to return.
	srv := fakeRPC(t, `{"secret":"`+secret+`"}`, http.StatusInternalServerError, 0)
	_, err := c.ProbeOnce(context.Background(), srv.URL, "getSlot")
	if err == nil {
		t.Fatal("an HTTP 500 was treated as success")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error echoed the response body: %v", err)
	}
}

// trustedTempDir is t.TempDir() with every symlink resolved. Trusted reads
// reject a symlinked ancestor, and on macOS t.TempDir() sits under /var, which
// is itself a symlink to /private/var — so an unresolved temp path fails the
// check for a reason that has nothing to do with what the test is asserting.
func trustedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
