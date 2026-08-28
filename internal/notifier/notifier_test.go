package notifier

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	testToken     = "123456:SUPERSECRETBOTTOKEN12345"
	testTLSFields = "client_ca_file = \"/tmp/notifier-ca.pem\"\n" +
		"server_cert_file = \"/tmp/notifier-cert.pem\"\n" +
		"server_key_file = \"/tmp/notifier-key.pem\"\n"
)

type notifierRoundTripper func(*http.Request) (*http.Response, error)

func (fn notifierRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestNewTelegramHandlesCustomDefaultTransport(t *testing.T) {
	original := http.DefaultTransport
	http.DefaultTransport = notifierRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unused")
	})
	t.Cleanup(func() { http.DefaultTransport = original })

	telegram := NewTelegram(testConfig("https://telegram.invalid", 111), NewMetrics(prometheus.NewRegistry()))
	transport, ok := telegram.client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("Telegram did not install a direct HTTP transport")
	}
}

// fakeTelegram records every sendMessage it receives and answers per script.
type fakeTelegram struct {
	mu       sync.Mutex
	requests []map[string]any
	paths    []string
	// failFor makes specific chat IDs fail, so partial-failure retry is testable.
	failFor map[int64]bool
	status  int
	ackOK   *bool
}

func newFakeTelegram(t *testing.T) (*httptest.Server, *fakeTelegram) {
	t.Helper()
	f := &fakeTelegram{failFor: map[int64]bool{}, status: http.StatusOK}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)

		f.mu.Lock()
		f.requests = append(f.requests, body)
		f.paths = append(f.paths, r.URL.Path)
		status, failFor, ackOK := f.status, f.failFor, f.ackOK
		f.mu.Unlock()

		var chatID int64
		if v, ok := body["chat_id"].(float64); ok {
			chatID = int64(v)
		}
		if failFor[chatID] {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		ok := true
		if ackOK != nil {
			ok = *ackOK
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": ok})
	}))
	t.Cleanup(srv.Close)
	return srv, f
}

func (f *fakeTelegram) sentTo() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int64
	for _, r := range f.requests {
		if v, ok := r["chat_id"].(float64); ok {
			out = append(out, int64(v))
		}
	}
	return out
}

func testConfig(apiURL string, chats ...int64) Config {
	return Config{BotToken: testToken, AllowedChatIDs: chats, TelegramAPIURL: apiURL, SendTimeoutSec: 2}
}

func gaugeValue(t *testing.T, reg *prometheus.Registry, name, labelName, labelValue string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == labelName && label.GetValue() == labelValue {
					return metric.GetGauge().GetValue()
				}
			}
		}
	}
	t.Fatalf("%s{%s=%q} is missing", name, labelName, labelValue)
	return 0
}

func counterValue(t *testing.T, reg *prometheus.Registry, name, labelName, labelValue string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == labelName && label.GetValue() == labelValue {
					return metric.GetCounter().GetValue()
				}
			}
		}
	}
	t.Fatalf("%s{%s=%q} is missing", name, labelName, labelValue)
	return 0
}

func counterValueWithLabels(
	t *testing.T,
	reg *prometheus.Registry,
	name string,
	want map[string]string,
) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			got := make(map[string]string, len(metric.GetLabel()))
			for _, label := range metric.GetLabel() {
				got[label.GetName()] = label.GetValue()
			}
			match := true
			for key, value := range want {
				if got[key] != value {
					match = false
					break
				}
			}
			if match {
				return metric.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("%s%v is missing", name, want)
	return 0
}

func hasMetricLabel(
	t *testing.T,
	reg *prometheus.Registry,
	name, labelName, labelValue string,
) bool {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == labelName && label.GetValue() == labelValue {
					return true
				}
			}
		}
	}
	return false
}

func scalarCounterValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == name && len(family.GetMetric()) == 1 {
			return family.GetMetric()[0].GetCounter().GetValue()
		}
	}
	t.Fatalf("%s is missing", name)
	return 0
}

func verifiedClientTLS() *tls.ConnectionState {
	cert := &x509.Certificate{}
	return &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{cert},
		VerifiedChains:   [][]*x509.Certificate{{cert}},
	}
}

func firingAlert(fingerprint string) Alert {
	return Alert{
		Status: "firing", Fingerprint: fingerprint, StartsAt: "2026-07-29T00:00:00Z",
		Labels:      map[string]string{"alertname": "MithrilNodeProbeDown", "severity": "critical"},
		Annotations: map[string]string{"summary": "Node RPC is unreachable"},
	}
}

func TestDeliverDeduplicatesButAllowsRecovery(t *testing.T) {
	srv, fake := newFakeTelegram(t)
	tg := NewTelegram(testConfig(srv.URL, 111), NewMetrics(prometheus.NewRegistry()))

	firing := firingAlert("fp-1")
	if err := tg.Deliver(context.Background(), firing); err != nil {
		t.Fatalf("first firing delivery: %v", err)
	}
	// A redelivery of the SAME state must be suppressed.
	if err := tg.Deliver(context.Background(), firing); err != nil {
		t.Fatalf("duplicate firing delivery returned an error: %v", err)
	}
	if got := len(fake.sentTo()); got != 1 {
		t.Fatalf("duplicate firing produced %d sends, want 1", got)
	}

	resolved := firing
	resolved.Status = "resolved"
	if err := tg.Deliver(context.Background(), resolved); err != nil {
		t.Fatalf("resolved delivery: %v", err)
	}
	if got := len(fake.sentTo()); got != 2 {
		t.Fatalf("recovery for the same fingerprint produced %d total sends, want 2", got)
	}

	refired := firing
	refired.StartsAt = "2026-07-29T01:00:00Z"
	if err := tg.Deliver(context.Background(), refired); err != nil {
		t.Fatalf("new lifecycle delivery: %v", err)
	}
	if got := len(fake.sentTo()); got != 3 {
		t.Fatalf("new lifecycle for the same fingerprint produced %d total sends, want 3", got)
	}
}

func TestDeliverSuppressesResolvedEvents(t *testing.T) {
	srv, fake := newFakeTelegram(t)
	tg := NewTelegram(testConfig(srv.URL, 111), NewMetrics(prometheus.NewRegistry()))

	event := firingAlert("event")
	event.Labels["lifecycle"] = "event"
	if err := tg.Deliver(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	event.Status = "resolved"
	if err := tg.Deliver(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if got := len(fake.sentTo()); got != 1 {
		t.Fatalf("resolved event produced %d sends, want only its firing message", got)
	}

	state := firingAlert("state")
	state.Labels["lifecycle"] = "state"
	state.Status = "resolved"
	if err := tg.DeliverGroup(t.Context(), []Alert{event, state}); err != nil {
		t.Fatal(err)
	}
	if got := len(fake.sentTo()); got != 2 {
		t.Fatalf("mixed group produced %d sends, want firing event and state resolution", got)
	}
}

func TestDeliverGroupDeduplicatesIdenticalKeysBeforeReserve(t *testing.T) {
	srv, fake := newFakeTelegram(t)
	reg := prometheus.NewRegistry()
	tg := NewTelegram(testConfig(srv.URL, 111), NewMetrics(reg))
	alert := firingAlert("duplicate-in-group")

	if err := tg.DeliverGroup(t.Context(), []Alert{alert, alert}); err != nil {
		t.Fatal(err)
	}
	if got := len(fake.sentTo()); got != 1 {
		t.Fatalf("identical group keys produced %d sends, want 1", got)
	}
	if got := counterValue(t, reg, "mithril_notifier_delivered_total", "channel", ChannelTelegram); got != 1 {
		t.Fatalf("identical group keys counted %v deliveries, want 1", got)
	}
}

func TestReserveFailureCountsOnlyAffectedKeys(t *testing.T) {
	srv, fake := newFakeTelegram(t)
	reg := prometheus.NewRegistry()
	tg := NewTelegram(testConfig(srv.URL, 111), NewMetrics(reg))
	tg.maxDedupEntries = 1
	busy := firingAlert("busy")
	tg.deliveries[deliveryKey{
		fingerprint: busy.Fingerprint,
		startsAt:    busy.StartsAt,
		status:      busy.Status,
		chatID:      111,
	}] = &deliveryEntry{inFlight: make(chan struct{})}
	alert := firingAlert("overloaded")

	if err := tg.DeliverGroup(t.Context(), []Alert{alert, alert}); err == nil {
		t.Fatal("deduplication capacity failure reported success")
	}
	if got := len(fake.sentTo()); got != 0 {
		t.Fatalf("capacity failure produced %d sends", got)
	}
	if got := counterValue(t, reg, "mithril_notifier_failed_total", "channel", ChannelTelegram); got != 1 {
		t.Fatalf("failed reservations = %v, want 1", got)
	}
	if got := counterValueWithLabels(t, reg, "mithril_notifier_delivery_failures_total", map[string]string{
		"channel": ChannelTelegram,
		"reason":  FailureOverloaded,
	}); got != 1 {
		t.Fatalf("overloaded reservations = %v, want 1", got)
	}
}

func TestPartialFailureRetriesOnlyFailedChats(t *testing.T) {
	srv, fake := newFakeTelegram(t)
	fake.failFor[222] = true

	tg := NewTelegram(testConfig(srv.URL, 111, 222, 333), NewMetrics(prometheus.NewRegistry()))
	alert := firingAlert("fp-partial")

	if err := tg.Deliver(context.Background(), alert); err == nil {
		t.Fatal("a partial failure reported success")
	}
	first := fake.sentTo()
	if len(first) != 3 {
		t.Fatalf("first attempt contacted %d chats, want 3", len(first))
	}

	// Second attempt: only the previously failed chat may be contacted.
	fake.mu.Lock()
	fake.requests = nil
	fake.failFor = map[int64]bool{}
	fake.mu.Unlock()

	if err := tg.Deliver(context.Background(), alert); err != nil {
		t.Fatalf("retry after the failure returned: %v", err)
	}
	retried := fake.sentTo()
	if len(retried) != 1 || retried[0] != 222 {
		t.Fatalf("retry contacted %v, want only the failed chat 222", retried)
	}
}

func TestSlowChatDoesNotStarveLaterAllowlistedChats(t *testing.T) {
	firstEntered := make(chan struct{})
	laterEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ChatID int64 `json:"chat_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch body.ChatID {
		case 111:
			close(firstEntered)
			select {
			case <-releaseFirst:
			case <-r.Context().Done():
				return
			}
		case 222:
			close(laterEntered)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(srv.Close)
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(releaseFirst) }) })

	tg := NewTelegram(testConfig(srv.URL, 111, 222), NewMetrics(prometheus.NewRegistry()))
	done := make(chan error, 1)
	go func() { done <- tg.Deliver(t.Context(), firingAlert("parallel-chats")) }()
	select {
	case <-firstEntered:
	case <-time.After(time.Second):
		t.Fatal("first chat was not attempted")
	}
	select {
	case <-laterEntered:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("later chat was starved by the slow first chat")
	}
	release.Do(func() { close(releaseFirst) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestNonAllowlistedChatIsNeverContacted(t *testing.T) {
	srv, fake := newFakeTelegram(t)
	cfg := testConfig(srv.URL, 111)
	tg := NewTelegram(cfg, NewMetrics(prometheus.NewRegistry()))

	alert := firingAlert("fp-allow")
	// A hostile label must not redirect delivery.
	alert.Labels["chat_id"] = "999"
	if err := tg.Deliver(context.Background(), alert); err != nil {
		t.Fatal(err)
	}
	for _, id := range fake.sentTo() {
		if id != 111 {
			t.Fatalf("delivered to non-allowlisted chat %d", id)
		}
	}
}

func TestNoAckIsFailureNotSuccess(t *testing.T) {
	srv, fake := newFakeTelegram(t)
	no := false
	fake.ackOK = &no

	tg := NewTelegram(testConfig(srv.URL, 111), NewMetrics(prometheus.NewRegistry()))
	if err := tg.Deliver(context.Background(), firingAlert("fp-ack")); err == nil {
		t.Fatal("an unacknowledged send was treated as delivered")
	}

	// It must not be marked delivered, so a retry still attempts it.
	fake.ackOK = nil
	if err := tg.Deliver(context.Background(), firingAlert("fp-ack")); err != nil {
		t.Fatalf("retry after a no-ack failed: %v", err)
	}
	if got := len(fake.sentTo()); got != 2 {
		t.Fatalf("got %d sends, want 2 (the failure must not have been recorded as delivered)", got)
	}
}

func TestRateLimitIsFailure(t *testing.T) {
	srv, fake := newFakeTelegram(t)
	fake.status = http.StatusTooManyRequests
	reg := prometheus.NewRegistry()
	tg := NewTelegram(testConfig(srv.URL, 111), NewMetrics(reg))
	if err := tg.Deliver(context.Background(), firingAlert("fp-429")); err == nil {
		t.Fatal("a rate-limited send was treated as delivered")
	}
	if got := counterValueWithLabels(t, reg, "mithril_notifier_delivery_failures_total", map[string]string{
		"channel": ChannelTelegram,
		"reason":  FailureRateLimited,
	}); got != 1 {
		t.Fatalf("rate-limited failure metric = %v, want 1", got)
	}
}

func TestErrorsNeverLeakTokenOrChatIDs(t *testing.T) {
	srv, fake := newFakeTelegram(t)
	fake.failFor[222] = true
	tg := NewTelegram(testConfig(srv.URL, 111, 222), NewMetrics(prometheus.NewRegistry()))

	err := tg.Deliver(context.Background(), firingAlert("fp-redact"))
	if err == nil {
		t.Fatal("expected a delivery failure")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("error leaked the bot token: %v", err)
	}
	if strings.Contains(err.Error(), "222") {
		t.Fatalf("error leaked a chat ID: %v", err)
	}

	// Unreachable host: the URL embeds the token, so this error path matters most.
	dead := NewTelegram(testConfig("http://127.0.0.1:1", 111), NewMetrics(prometheus.NewRegistry()))
	err = dead.Deliver(context.Background(), firingAlert("fp-dead"))
	if err == nil {
		t.Fatal("expected an unreachable-host failure")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("unreachable-host error leaked the bot token: %v", err)
	}
	// The token really is in the path we would have called, so this is not vacuous.
	if len(fake.paths) > 0 && !strings.Contains(fake.paths[0], testToken) {
		t.Fatal("test setup is wrong: the token is not in the request path")
	}
}

// postWebhook drives the handler with a v4 payload.
func postWebhook(t *testing.T, h *Handler, body string, withCert bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/notify", strings.NewReader(body))
	if withCert {
		// Chain validation is exercised by the command's TLS integration test.
		req.TLS = verifiedClientTLS()
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func webhookBody(version, fingerprint string) string {
	return `{"version":"` + version + `","status":"firing","alerts":[{"status":"firing",` +
		`"fingerprint":"` + fingerprint + `","startsAt":"2026-07-29T00:00:00Z",` +
		`"labels":{"alertname":"X"},"annotations":{"summary":"s"}}]}`
}

func TestHandlerRequiresClientCertificate(t *testing.T) {
	srv, _ := newFakeTelegram(t)
	h := New(testConfig(srv.URL, 111), NewMetrics(prometheus.NewRegistry()))

	if rec := postWebhook(t, h, webhookBody("4", "fp-nocert"), false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a request with no client certificate returned %d, want 401", rec.Code)
	}
	unverified := httptest.NewRequest(
		http.MethodPost,
		"/notify",
		strings.NewReader(webhookBody("4", "fp-unverified")),
	)
	unverified.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{}}}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, unverified)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("an unverified client certificate returned %d, want 401", rec.Code)
	}
	if rec := postWebhook(t, h, webhookBody("4", "fp-cert"), true); rec.Code != http.StatusOK {
		t.Fatalf("a verified client certificate returned %d, want 200", rec.Code)
	}
}

func TestHandlerRejectsBadInput(t *testing.T) {
	srv, fake := newFakeTelegram(t)
	h := New(testConfig(srv.URL, 111), NewMetrics(prometheus.NewRegistry()))

	cases := map[string]struct {
		body string
		want int
	}{
		"wrong schema version": {webhookBody("3", "fp-v3"), http.StatusBadRequest},
		"not json":             {`{`, http.StatusBadRequest},
		"oversized body":       {`{"version":"4","alerts":[],"pad":"` + strings.Repeat("x", 1<<20) + `"}`, http.StatusRequestEntityTooLarge},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if rec := postWebhook(t, h, tc.body, true); rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
	if got := len(fake.sentTo()); got != 0 {
		t.Errorf("rejected requests still produced %d sends", got)
	}
}

func TestDeliveryFailureReturns500SoAlertmanagerRetries(t *testing.T) {
	srv, fake := newFakeTelegram(t)
	fake.failFor[111] = true
	h := New(testConfig(srv.URL, 111), NewMetrics(prometheus.NewRegistry()))

	rec := postWebhook(t, h, webhookBody("4", "fp-fail"), true)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed delivery returned %d, want 500 so Alertmanager retries", rec.Code)
	}
}

func TestAlertMessageIsBoundedAndDeterministic(t *testing.T) {
	alert := Alert{
		Status: "firing", Fingerprint: "fp",
		Labels: map[string]string{
			"alertname": "X", "zeta": "z", "alpha": "a",
			"huge": strings.Repeat("A", 10_000),
		},
	}
	first := alert.Message()
	for i := 0; i < 20; i++ {
		if alert.Message() != first {
			t.Fatal("message rendering is not deterministic across map iterations")
		}
	}
	if len([]rune(first)) > 4_000 {
		t.Fatalf("message is %d runes; an unbounded label produced an unbounded message", len([]rune(first)))
	}
	if strings.Contains(first, "alpha") || strings.Contains(first, "zeta") {
		t.Error("internal labels were included in the operator message")
	}
}

func TestAlertMessageUsesOperatorLanguage(t *testing.T) {
	alert := firingAlert("readable")
	alert.Labels["deployment_id"] = "rpc-1"
	message := alert.Message()
	for _, expected := range []string{
		"Mithril node unreachable",
		"Node RPC is unreachable",
		"Node: rpc-1",
		"Next: Check Mithril status.",
		"Time: 2026-07-29 00:00:00 UTC",
	} {
		if !strings.Contains(message, expected) {
			t.Fatalf("message %q does not contain %q", message, expected)
		}
	}
	if strings.Contains(message, "FIRING") || strings.Contains(message, "alertname=") {
		t.Fatalf("message contains Alertmanager terminology: %q", message)
	}

	alert.Status = "resolved"
	message = alert.Message()
	if !strings.Contains(message, "Node-reachability alert ended") ||
		!strings.Contains(message, "Alert condition ended. Check current status.") ||
		strings.Contains(message, "Node RPC is unreachable") ||
		strings.Contains(message, "Next:") {
		t.Fatalf("resolved message is unclear: %q", message)
	}
}

func TestInformationalAlertUsesNeutralLanguage(t *testing.T) {
	alert := firingAlert("completed")
	alert.Labels["severity"] = "info"
	alert.Labels["alertname"] = "MithrilAgentActionCompleted"
	alert.Annotations["summary"] = "A Mithril Devnet swap completed"

	message := alert.Message()
	if !strings.Contains(message, "Devnet trade complete") ||
		!strings.Contains(message, "Check agent status before enabling another action") ||
		strings.Contains(message, "No action needed") ||
		strings.Contains(message, "needs attention") ||
		strings.Contains(message, "recovered") {
		t.Fatalf("informational firing message is misleading: %q", message)
	}

	alert.Status = "resolved"
	message = alert.Message()
	if !strings.Contains(message, "Mithril update ended") ||
		!strings.Contains(message, "Alert condition ended. Check current status.") ||
		strings.Contains(message, "A Mithril Devnet swap completed") ||
		strings.Contains(message, "recovered") {
		t.Fatalf("informational resolved message is misleading: %q", message)
	}
}

func TestSubmittedActionGuidanceDoesNotClaimControlState(t *testing.T) {
	alert := firingAlert("submitted")
	alert.Labels["severity"] = "info"
	alert.Labels["alertname"] = "MithrilAgentActionSubmitted"
	alert.Annotations["summary"] = "Devnet swap submitted; confirmation is pending"

	message := alert.Message()
	if !strings.Contains(message, "Wait for confirmation. Do not enable another action.") ||
		strings.Contains(message, "Trading stays stopped") ||
		strings.Contains(message, "Execution stays stopped") {
		t.Fatalf("submitted-action guidance is misleading: %q", message)
	}
}

func TestResolvedAlertsDoNotInferRecoveryFromResolution(t *testing.T) {
	tests := []struct {
		name      string
		alertName string
		title     string
	}{
		{"agent terminal", "MithrilAgentActionTerminal", "Trade-stop alert ended"},
		{"execution enabled", "MithrilAgentExecutionEnabled", "Execution-enabled alert ended"},
		{"agent down", "MithrilAgentDown", "Agent-offline alert ended"},
		{"node probe", "MithrilNodeProbeDown", "Node-reachability alert ended"},
		{"node lag", "MithrilNodeBehindReference", "Node-lag alert ended"},
		{"replay stalled", "MithrilReplayStalled", "Replay-stall alert ended"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			alert := firingAlert(test.name)
			alert.Labels["alertname"] = test.alertName
			alert.Status = "resolved"
			message := alert.Message()
			if !strings.Contains(message, test.title) ||
				!strings.Contains(message, "Alert condition ended. Check current status.") {
				t.Fatalf("resolved message lacks neutral guidance: %q", message)
			}
			for _, unsupported := range []string{
				"Mithril recovered", "Mithril agent online", "Mithril node reachable",
				"Mithril node caught up", "Mithril replay moving", "Devnet trading stopped",
			} {
				if strings.Contains(message, unsupported) {
					t.Fatalf("resolved message infers unsupported state %q: %q", unsupported, message)
				}
			}
		})
	}
}

func TestPriceTargetAlertGivesClearStoppedNextStep(t *testing.T) {
	alert := firingAlert("price-ready")
	alert.Labels["severity"] = "info"
	alert.Labels["lifecycle"] = "event"
	alert.Labels["alertname"] = "MithrilAgentPriceTargetReached"
	alert.Annotations["summary"] = "SOL price target reached; execution is stopped"

	message := alert.Message()
	for _, expected := range []string{
		"SOL price target reached",
		"execution is stopped",
		"Review the price status before enabling a trade",
	} {
		if !strings.Contains(message, expected) {
			t.Fatalf("price target message %q does not contain %q", message, expected)
		}
	}
	if strings.Contains(message, "needs attention") || strings.Contains(message, "FIRING") {
		t.Fatalf("price target message is misleading: %q", message)
	}
}

func TestGroupedInformationalAlertsUseNeutralLanguage(t *testing.T) {
	first := firingAlert("first")
	first.Labels["severity"] = "info"
	second := firingAlert("second")
	second.Labels["severity"] = "info"

	message := groupedMessage([]Alert{first, second})
	if !strings.Contains(message, "2 Mithril updates") ||
		strings.Contains(message, "needs attention") ||
		strings.Contains(message, "new alerts") {
		t.Fatalf("informational group is misleading: %q", message)
	}

	second.Labels["severity"] = "critical"
	message = groupedMessage([]Alert{first, second})
	if !strings.Contains(message, "2 Mithril alerts and updates") {
		t.Fatalf("mixed group is unclear: %q", message)
	}
}

func TestGroupedResolvedAlertsDescribeLifecycle(t *testing.T) {
	first := firingAlert("first")
	first.Status = "resolved"
	second := firingAlert("second")
	second.Status = "resolved"

	message := groupedMessage([]Alert{first, second})
	if !strings.Contains(message, "2 Mithril alerts ended") ||
		strings.Contains(message, "new alerts") {
		t.Fatalf("resolved group is misleading: %q", message)
	}

	first.Labels["severity"] = "info"
	second.Labels["severity"] = "info"
	message = groupedMessage([]Alert{first, second})
	if !strings.Contains(message, "2 Mithril updates ended") {
		t.Fatalf("resolved informational group is unclear: %q", message)
	}

	second.Status = "firing"
	message = groupedMessage([]Alert{first, second})
	if !strings.Contains(message, "2 Mithril updates") ||
		strings.Contains(message, "⚠️") ||
		strings.Contains(message, "new alerts") {
		t.Fatalf("mixed lifecycle group is misleading: %q", message)
	}

	second.Labels["severity"] = "critical"
	message = groupedMessage([]Alert{first, second})
	if !strings.Contains(message, "2 Mithril alert updates") {
		t.Fatalf("actionable mixed lifecycle group is unclear: %q", message)
	}
}

func TestStateTransitionResolutionUsesNeutralLanguage(t *testing.T) {
	for _, lifecycle := range []string{"state", "event"} {
		t.Run(lifecycle, func(t *testing.T) {
			alert := firingAlert(lifecycle)
			alert.Status = "resolved"
			alert.Labels["alertname"] = "MithrilControlTransition"
			alert.Labels["lifecycle"] = lifecycle

			message := alert.Message()
			if !strings.Contains(message, "Mithril state update") ||
				strings.Contains(message, "recovered") {
				t.Fatalf("state-transition resolution is misleading: %q", message)
			}
		})
	}

	first := firingAlert("first")
	first.Status = "resolved"
	first.Labels["alertname"] = "MithrilControlTransition"
	first.Labels["lifecycle"] = "state"
	second := firingAlert("second")
	second.Status = "resolved"
	second.Labels["alertname"] = "MithrilControlTransition"
	second.Labels["lifecycle"] = "event"
	message := groupedMessage([]Alert{first, second})
	if !strings.Contains(message, "2 Mithril state updates") ||
		strings.Contains(message, "recovered") || strings.Contains(message, "🔒") {
		t.Fatalf("state-transition group is misleading: %q", message)
	}
}

func TestAlertMessageHidesDiagnosticLabels(t *testing.T) {
	alert := firingAlert("labels")
	alert.Labels["outcome"] = "halted"
	alert.Labels["mode"] = "no_new_actions"
	alert.Labels["decision"] = "pending"
	alert.Labels["verdict"] = "unresolved"
	alert.Labels["category"] = "quote_unavailable"
	alert.Labels["service"] = "mithril-agent"

	message := alert.Message()
	for _, hidden := range []string{
		"Outcome:", "Mode:", "Decision:", "Verdict:", "Category:", "Service:",
	} {
		if strings.Contains(message, hidden) {
			t.Fatalf("message %q contains diagnostic field %q", message, hidden)
		}
	}
}

func TestAlertMessageCannotInjectLinesOrDirectionControls(t *testing.T) {
	for _, key := range []string{"deployment_id", "target_job"} {
		t.Run(key, func(t *testing.T) {
			message := (Alert{
				Status: "firing",
				Labels: map[string]string{
					"alertname": "Real\n[RESOLVED] Fake",
					key:         "before\u202Eafter\r\ncredential=https://secret.invalid",
				},
				Annotations: map[string]string{"summary": "one\nMithrilNodeHealthy"},
			}).Message()

			for _, forbidden := range []string{
				"\n[RESOLVED]", "\nMithrilNodeHealthy", "\u202E", "\r",
				"\ncredential=", "secret.invalid", "credential=https",
			} {
				if strings.Contains(message, forbidden) {
					t.Fatalf("rendered message retained control sequence %q: %q", forbidden, message)
				}
			}
			if !strings.Contains(message, "[REDACTED]") {
				t.Fatalf("rendered message did not mark redacted content: %q", message)
			}
		})
	}
}

func TestSESProbeRecordsAcceptanceOnly(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	p := NewSESProbe(metrics)
	p.Addr, p.From, p.CanaryTo = "smtp.example:587", "from@example", "canary@example"
	p.Username, p.Password = "AKIAEXAMPLE", "SUPERSECRETSMTPPASSWORD"

	var sent []byte
	p.send = func(_ context.Context, addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		sent = msg
		return nil
	}
	if err := p.ProbeOnce(context.Background()); err != nil {
		t.Fatalf("accepted canary reported failure: %v", err)
	}
	if !strings.Contains(string(sent), "mithril-canary") {
		t.Error("canary is not tagged recognisably")
	}
	if !strings.Contains(string(sent), "does not assert inbox delivery") {
		t.Error("canary does not state the limit of what acceptance proves")
	}
	if got := gaugeValue(t, reg, "mithril_notification_probe_success", "route", RouteSES); got != 1 {
		t.Fatalf("SES probe success = %v, want 1", got)
	}
	if got := counterValue(
		t, reg, "mithril_notification_probe_attempts_total", "route", RouteSES,
	); got != 1 {
		t.Fatalf("SES probe attempts = %v, want 1", got)
	}
	if hasMetricLabel(t, reg, "mithril_notifier_delivered_total", "channel", "ses") {
		t.Fatal("an SES canary was exposed as a real alert-delivery channel")
	}

	// A rejected canary must fail, and must not echo the SMTP password.
	p.send = func(context.Context, string, smtp.Auth, string, []string, []byte) error {
		return errors.New("535 auth failed for user AKIAEXAMPLE with SUPERSECRETSMTPPASSWORD")
	}
	err := p.ProbeOnce(context.Background())
	if err == nil {
		t.Fatal("a rejected canary reported success")
	}
	if strings.Contains(err.Error(), "SUPERSECRETSMTPPASSWORD") {
		t.Fatalf("SES error leaked the SMTP password: %v", err)
	}
	if got := gaugeValue(t, reg, "mithril_notification_probe_success", "route", RouteSES); got != 0 {
		t.Fatalf("SES probe failure left success at %v", got)
	}
	if got := counterValue(
		t, reg, "mithril_notification_probe_attempts_total", "route", RouteSES,
	); got != 2 {
		t.Fatalf("SES probe attempts = %v, want 2", got)
	}
	if got := counterValue(
		t, reg, "mithril_notification_probe_failures_total", "route", RouteSES,
	); got != 1 {
		t.Fatalf("SES probe failures = %v, want 1", got)
	}
}

func TestAlertMessageRedactsCredentialsBeforeDelivery(t *testing.T) {
	alert := firingAlert("fp-redaction")
	alert.Labels["api_key"] = "LABELSECRET123"
	alert.Labels[strings.Repeat("x", maxFieldRunes)+"_api_key"] = "LONGKEYSECRET234"
	alert.Labels["endpoint"] = "https://operator:URLSECRET456@example.invalid/path"
	alert.Annotations["summary"] = "Authorization: Bearer SUMMARYSECRET789"

	message := alert.Message()
	for _, secret := range []string{"LABELSECRET123", "LONGKEYSECRET234", "URLSECRET456", "SUMMARYSECRET789"} {
		if strings.Contains(message, secret) {
			t.Fatalf("rendered alert leaked %q: %q", secret, message)
		}
	}
	if !strings.Contains(message, "[REDACTED]") {
		t.Fatalf("rendered alert did not explain that content was withheld: %q", message)
	}

	srv, fake := newFakeTelegram(t)
	tg := NewTelegram(testConfig(srv.URL, 111), NewMetrics(prometheus.NewRegistry()))
	if err := tg.Deliver(context.Background(), alert); err != nil {
		t.Fatalf("deliver redacted alert: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 1 {
		t.Fatalf("Telegram requests = %d, want 1", len(fake.requests))
	}
	delivered, _ := fake.requests[0]["text"].(string)
	for _, secret := range []string{"LABELSECRET123", "LONGKEYSECRET234", "URLSECRET456", "SUMMARYSECRET789"} {
		if strings.Contains(delivered, secret) {
			t.Fatalf("Telegram request leaked %q: %q", secret, delivered)
		}
	}
}

func TestConfigRejectsUnsafePermissionsAndLeaks(t *testing.T) {
	// Resolved because a trusted read now rejects a symlinked ancestor, and on
	// macOS t.TempDir() sits under /var, which is a symlink to /private/var.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	content := "bot_token = \"" + testToken + "\"\nallowed_chat_ids = [111]\n" + testTLSFields
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("0600 config rejected: %v", err)
	}
	if cfg.BotToken != testToken || len(cfg.AllowedChatIDs) != 1 || cfg.AllowedChatIDs[0] != 111 {
		t.Fatal("config did not load")
	}

	for _, mode := range []os.FileMode{0o640, 0o644, 0o666} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		_, err := LoadConfig(path)
		if err == nil {
			t.Errorf("mode %o accepted for a token-bearing file", mode)
			continue
		}
		if strings.Contains(err.Error(), testToken) {
			t.Errorf("config error leaked the bot token: %v", err)
		}
	}

	// An empty allowlist delivers nowhere, which looks identical to a broken
	// notifier at the exact moment an alert matters.
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		path,
		[]byte("bot_token = \""+testToken+"\"\nallowed_chat_ids = []\n"+testTLSFields),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Error("an empty chat allowlist was accepted")
	}
}

func TestTelegramFailureDoesNotAffectIndependentSMTPAcceptanceProbe(t *testing.T) {
	srv, fake := newFakeTelegram(t)
	fake.failFor[111] = true // the only Telegram destination is broken

	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	h := New(testConfig(srv.URL, 111), metrics)

	rec := postWebhook(t, h, webhookBody("4", "fp-fallback"), true)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("notifier returned %d; a 200 would tell Alertmanager the alert was "+
			"handled and it would stop retrying", rec.Code)
	}

	ses := NewSESProbe(metrics)
	ses.Addr, ses.From, ses.CanaryTo = "smtp.example:587", "from@example", "canary@example"
	ses.Username, ses.Password = "user", "password"
	var accepted bool
	ses.send = func(context.Context, string, smtp.Auth, string, []string, []byte) error {
		accepted = true
		return nil
	}
	if err := ses.ProbeOnce(context.Background()); err != nil {
		t.Fatalf("SES SMTP acceptance probe failed while Telegram was down: %v", err)
	}
	if !accepted {
		t.Fatal("SES SMTP acceptance probe did not attempt delivery")
	}

	series := map[string]float64{}
	gathered, _ := reg.Gather()
	for _, mf := range gathered {
		for _, m := range mf.GetMetric() {
			key := mf.GetName()
			for _, l := range m.GetLabel() {
				key += "," + l.GetName() + "=" + l.GetValue()
			}
			if m.Counter != nil {
				series[key] = m.GetCounter().GetValue()
			}
		}
	}
	if series["mithril_notifier_failed_total,channel=telegram"] < 1 {
		t.Errorf("telegram failure was not recorded: %v", series)
	}
	if series["mithril_notifier_delivered_total,channel=telegram"] != 0 {
		t.Error("a failed telegram delivery was counted as delivered")
	}
	if got := counterValue(
		t, reg, "mithril_notification_probe_attempts_total", "route", RouteSES,
	); got != 1 {
		t.Errorf("SES canary attempt was not recorded: %v", got)
	}
	if got := gaugeValue(t, reg, "mithril_notification_probe_success", "route", RouteSES); got != 1 {
		t.Errorf("SES SMTP acceptance was not recorded as probe success: %v", got)
	}
}

func TestProbeRunStopsOnContextCancel(t *testing.T) {
	srv, _ := newFakeTelegram(t)
	metrics := NewMetrics(prometheus.NewRegistry())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: Run must return promptly

	done := make(chan struct{}, 2)
	ses := NewSESProbe(metrics)
	ses.Addr, ses.From, ses.CanaryTo = "smtp.example:587", "f@example", "c@example"
	ses.Username, ses.Password = "user", "password"
	ses.send = func(context.Context, string, smtp.Auth, string, []string, []byte) error { return nil }
	ses.Interval = time.Hour
	go func() { ses.Run(ctx); done <- struct{}{} }()

	tp := NewTelegramProbe(NewTelegram(testConfig(srv.URL, 111), metrics))
	tp.Interval = time.Hour
	go func() { tp.Run(ctx); done <- struct{}{} }()

	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("a probe loop did not stop on context cancellation")
		}
	}
}

func TestIncidentBurstStaysBounded(t *testing.T) {
	srv, fake := newFakeTelegram(t)
	metrics := NewMetrics(prometheus.NewRegistry())
	h := New(testConfig(srv.URL, 111), metrics)

	// A burst of distinct alerts, as a cascading failure produces.
	var alerts []string
	for i := 0; i < 50; i++ {
		alerts = append(alerts, fmt.Sprintf(
			`{"status":"firing","fingerprint":"burst-%d","startsAt":"2026-07-29T00:00:00Z","labels":{"alertname":"A%d"},"annotations":{"summary":"s"}}`, i, i))
	}
	body := `{"version":"4","status":"firing","alerts":[` + strings.Join(alerts, ",") + `]}`

	rec := postWebhook(t, h, body, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("burst of 50 alerts returned %d", rec.Code)
	}
	if got := len(fake.sentTo()); got != 1 {
		t.Fatalf("burst produced %d Telegram messages, want one grouped summary", got)
	}
	fake.mu.Lock()
	groupText, _ := fake.requests[0]["text"].(string)
	fake.mu.Unlock()
	if !strings.Contains(groupText, "50 Mithril alerts") ||
		!strings.Contains(groupText, "+45 more") {
		t.Fatalf("group summary did not report its bounded coverage: %q", groupText)
	}

	// Alertmanager retries the whole group. Every alert is already delivered,
	// so a retry must produce NO additional sends — otherwise an incident
	// multiplies its own notification volume on every retry.
	before := len(fake.sentTo())
	for i := 0; i < 5; i++ {
		if rec := postWebhook(t, h, body, true); rec.Code != http.StatusOK {
			t.Fatalf("retry %d returned %d", i, rec.Code)
		}
	}
	if after := len(fake.sentTo()); after != before {
		t.Fatalf("5 retries of an already-delivered burst produced %d extra sends", after-before)
	}

	// Beyond the per-request cap the request is refused outright rather than
	// being partially processed, so a hostile or runaway sender cannot make the
	// notifier do unbounded work.
	var huge []string
	for i := 0; i < maxAlertsPerRequest+1; i++ {
		huge = append(huge, fmt.Sprintf(`{"status":"firing","fingerprint":"h-%d","startsAt":"2026-07-29T00:00:00Z","labels":{"alertname":"H"}}`, i))
	}
	oversized := `{"version":"4","status":"firing","alerts":[` + strings.Join(huge, ",") + `]}`
	sendsBefore := len(fake.sentTo())
	if rec := postWebhook(t, h, oversized, true); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-cap burst returned %d, want 413", rec.Code)
	}
	if len(fake.sentTo()) != sendsBefore {
		t.Error("an over-cap request was partially delivered instead of refused")
	}

	// The cap must also be a MEANINGFUL number. The check above references the
	// constant, so it would still pass if the cap were raised to something
	// effectively unbounded — the limit would exist on paper while doing
	// nothing. A real incident cascade is tens of alerts, not thousands.
	if maxAlertsPerRequest < 16 {
		t.Errorf("maxAlertsPerRequest = %d is too low to carry a real incident cascade", maxAlertsPerRequest)
	}
	if maxAlertsPerRequest > 1024 {
		t.Errorf("maxAlertsPerRequest = %d is effectively unbounded; the cap no longer limits anything",
			maxAlertsPerRequest)
	}
}

func TestConfigDefaultsAndRejections(t *testing.T) {
	// Defaults apply when the field is unset or nonsensical.
	if got := (Config{}).SendTimeout(); got != DefaultSendTimeout {
		t.Errorf("unset send timeout = %v, want the default %v", got, DefaultSendTimeout)
	}
	if got := (Config{SendTimeoutSec: -5}).SendTimeout(); got != DefaultSendTimeout {
		t.Errorf("negative send timeout = %v, want the default", got)
	}
	if got := (Config{SendTimeoutSec: 7}).SendTimeout(); got != 7*time.Second {
		t.Errorf("explicit send timeout = %v, want 7s", got)
	}
	if got := (Config{}).ProbeInterval(); got != DefaultProbeInterval {
		t.Errorf("unset probe interval = %v, want %v", got, DefaultProbeInterval)
	}
	if got := (Config{ProbeIntervalSec: 7}).ProbeInterval(); got != 7*time.Second {
		t.Errorf("explicit probe interval = %v, want 7s", got)
	}
	if got := (Config{}).WebhookTimeout(); got != DefaultWebhookTimeout {
		t.Errorf("unset webhook timeout = %v, want %v", got, DefaultWebhookTimeout)
	}
	if got := (Config{}).APIBase(); got != "https://api.telegram.org" {
		t.Errorf("unset API base = %q, want the real Telegram host", got)
	}
	if got := (Config{TelegramAPIURL: "http://127.0.0.1:1"}).APIBase(); got != "http://127.0.0.1:1" {
		t.Errorf("explicit API base was ignored: %q", got)
	}

	// Resolved because a trusted read now rejects a symlinked ancestor, and on
	// macOS t.TempDir() sits under /var, which is a symlink to /private/var.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tokenLine := "bot_token = \"" + testToken + "\"\n"
	good := tokenLine + "allowed_chat_ids = [1]\n" + testTLSFields

	t.Run("loopback test endpoint", func(t *testing.T) {
		path := filepath.Join(dir, "loopback.toml")
		content := good + "telegram_api_url = \"http://127.0.0.1:19090\"\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err != nil {
			t.Fatalf("safe loopback endpoint was rejected: %v", err)
		}
	})

	t.Run("valid SES route", func(t *testing.T) {
		path := filepath.Join(dir, "ses.toml")
		content := good +
			"send_timeout_seconds = 30\n" +
			"webhook_timeout_seconds = 30\n" +
			"probe_interval_seconds = 60\n" +
			"ses_addr = \"email-smtp.us-east-1.amazonaws.com:587\"\n" +
			"ses_username = \"user\"\n" +
			"ses_password_file = \"/run/credentials/mithril-notifier/ses-password\"\n" +
			"ses_from = \"from@example\"\n" +
			"ses_canary_to = \"canary@example\"\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err != nil {
			t.Fatalf("valid SES route was rejected: %v", err)
		}
	})

	t.Run("path rejections", func(t *testing.T) {
		valid := filepath.Join(dir, "ok.toml")
		if err := os.WriteFile(valid, []byte(good), 0o600); err != nil {
			t.Fatal(err)
		}
		for name, path := range map[string]string{
			"empty":     "",
			"relative":  "config.toml",
			"unclean":   dir + "/./ok.toml",
			"dotdot":    dir + "/sub/../ok.toml",
			"missing":   filepath.Join(dir, "absent.toml"),
			"directory": dir,
		} {
			if _, err := LoadConfig(path); err == nil {
				t.Errorf("%s path was accepted", name)
			}
		}

		// A symlink is refused even when it points at a valid file: the target
		// can be repointed after the permissions were checked.
		link := filepath.Join(dir, "link.toml")
		_ = os.Symlink(valid, link)
		if _, err := LoadConfig(link); err == nil {
			t.Error("a symlinked config was accepted")
		} else if !strings.Contains(err.Error(), "symlink") {
			t.Errorf("symlink error = %q, want it to name the symlink", err)
		}

		// The leaf check above passes with a symlinked ANCESTOR, because
		// O_NOFOLLOW only protects the final component. Only the leaf was ever
		// tested, so the read of the bot token and the SES password ran without
		// ancestor protection while every comparable read elsewhere had it.
		//
		// Redirecting a parent directory substitutes the whole config: the
		// service then starts with an attacker's bot token and an attacker's
		// chat allowlist, and nothing about that looks like a failure.
		real := t.TempDir()
		hidden := filepath.Join(real, "config.toml")
		if err := os.WriteFile(hidden, []byte(good), 0o600); err != nil {
			t.Fatal(err)
		}
		parent := filepath.Join(dir, "viaparent")
		if err := os.Symlink(real, parent); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(filepath.Join(parent, "config.toml")); err == nil {
			t.Error("a config reached through a symlinked ancestor directory was accepted")
		}
	})

	t.Run("content rejections", func(t *testing.T) {
		path := filepath.Join(dir, "bad.toml")
		base := tokenLine + "allowed_chat_ids = [1]\n"
		cases := map[string]string{
			"no bot token":     "allowed_chat_ids = [1]\n" + testTLSFields,
			"unsafe bot token": "bot_token = \"token/with/path\"\nallowed_chat_ids = [1]\n" + testTLSFields,
			"bot token without id": "bot_token = \"secret\"\nallowed_chat_ids = [1]\n" +
				testTLSFields,
			"bot token with nonnumeric id": "bot_token = \"bot:secret\"\nallowed_chat_ids = [1]\n" +
				testTLSFields,
			"bot token with empty secret": "bot_token = \"123:\"\nallowed_chat_ids = [1]\n" +
				testTLSFields,
			"bot token with extra colon": "bot_token = \"123:a:b\"\nallowed_chat_ids = [1]\n" +
				testTLSFields,
			"alternate API host": base + "telegram_api_url = \"https://example.invalid\"\n" +
				testTLSFields,
			"loopback API path": base + "telegram_api_url = \"http://127.0.0.1:19090/not-api\"\n" +
				testTLSFields,
			"empty allowlist":   tokenLine + "allowed_chat_ids = []\n" + testTLSFields,
			"chat id zero":      tokenLine + "allowed_chat_ids = [0]\n" + testTLSFields,
			"duplicate chat id": tokenLine + "allowed_chat_ids = [1, 1]\n" + testTLSFields,
			"too many chats": tokenLine + "allowed_chat_ids = [1,2,3,4,5,6,7,8,9]\n" +
				testTLSFields,
			"timeout too large": base + "send_timeout_seconds = 999\n" + testTLSFields,
			"timeout negative":  base + "send_timeout_seconds = -1\n" + testTLSFields,
			"send exceeds webhook timeout": base +
				"send_timeout_seconds = 20\nwebhook_timeout_seconds = 10\n" + testTLSFields,
			"probe too short": base + "probe_interval_seconds = 59\n" + testTLSFields,
			"probe too large": base + "probe_interval_seconds = 999999\n" + testTLSFields,
			"webhook timeout too large": base + "webhook_timeout_seconds = 31\n" +
				testTLSFields,
			"unknown field": base + "send_timout_seconds = 10\n" + testTLSFields,
			"missing TLS":   base,
			"missing CA": base +
				"server_cert_file = \"/tmp/c.pem\"\nserver_key_file = \"/tmp/k.pem\"\n",
			"missing certificate": base +
				"client_ca_file = \"/tmp/ca.pem\"\nserver_key_file = \"/tmp/k.pem\"\n",
			"missing key": base +
				"client_ca_file = \"/tmp/ca.pem\"\nserver_cert_file = \"/tmp/c.pem\"\n",
			"relative ca path": base +
				"client_ca_file = \"ca.pem\"\nserver_cert_file = \"/tmp/c.pem\"\nserver_key_file = \"/tmp/k.pem\"\n",
			"relative cert path": base +
				"client_ca_file = \"/tmp/ca.pem\"\nserver_cert_file = \"c.pem\"\nserver_key_file = \"/tmp/k.pem\"\n",
			"relative key path": base +
				"client_ca_file = \"/tmp/ca.pem\"\nserver_cert_file = \"/tmp/c.pem\"\nserver_key_file = \"k.pem\"\n",
			"partial SES": base + testTLSFields + "ses_addr = \"smtp.example:587\"\n",
			"non-SES credential destination": base + testTLSFields +
				"ses_addr = \"smtp.example:587\"\n" +
				"ses_username = \"user\"\n" +
				"ses_password_file = \"/run/credentials/ses-password\"\n" +
				"ses_from = \"from@example\"\n" +
				"ses_canary_to = \"canary@example\"\n",
			"loopback SES outside test mode": base + testTLSFields +
				"ses_addr = \"127.0.0.1:2525\"\n" +
				"ses_username = \"user\"\n" +
				"ses_password_file = \"/run/credentials/ses-password\"\n" +
				"ses_from = \"from@example\"\n" +
				"ses_canary_to = \"canary@example\"\n",
			"relative SES password file": base + testTLSFields +
				"ses_addr = \"smtp.example:587\"\n" +
				"ses_username = \"user\"\n" +
				"ses_password_file = \"password\"\n" +
				"ses_from = \"from@example\"\n" +
				"ses_canary_to = \"canary@example\"\n",
			"SES username with whitespace": base + testTLSFields +
				"ses_addr = \"email-smtp.us-east-1.amazonaws.com:587\"\n" +
				"ses_username = \"user name\"\n" +
				"ses_password_file = \"/run/credentials/ses-password\"\n" +
				"ses_from = \"from@example\"\n" +
				"ses_canary_to = \"canary@example\"\n",
			"SES sender header injection": base + testTLSFields +
				"ses_addr = \"email-smtp.us-east-1.amazonaws.com:587\"\n" +
				"ses_username = \"user\"\n" +
				"ses_password_file = \"/run/credentials/ses-password\"\n" +
				"ses_from = \"from@example\\r\\nBcc: outsider@example\"\n" +
				"ses_canary_to = \"canary@example\"\n",
			"SES recipient display name": base + testTLSFields +
				"ses_addr = \"email-smtp.us-east-1.amazonaws.com:587\"\n" +
				"ses_username = \"user\"\n" +
				"ses_password_file = \"/run/credentials/ses-password\"\n" +
				"ses_from = \"from@example\"\n" +
				"ses_canary_to = \"Canary <canary@example>\"\n",
		}
		for name, content := range cases {
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(path); err == nil {
				t.Errorf("%s was accepted", name)
			}
		}
	})

	t.Run("oversized file", func(t *testing.T) {
		path := filepath.Join(dir, "huge.toml")
		big := good + "# " + strings.Repeat("x", 70<<10) + "\n"
		if err := os.WriteFile(path, []byte(big), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Error("an oversized config was accepted")
		}
	})
}

func TestLoadSecretFileUsesPrivateRegularFile(t *testing.T) {
	// Resolved because a trusted read now rejects a symlinked ancestor, and on
	// macOS t.TempDir() sits under /var, which is a symlink to /private/var.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte("smtp-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret, err := LoadSecretFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if secret != "smtp-secret" {
		t.Fatalf("secret = %q", secret)
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSecretFile(path); err == nil {
		t.Fatal("group-readable secret was accepted")
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "secret-link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSecretFile(link); err == nil {
		t.Fatal("symlinked secret was accepted")
	}
}

func TestSESAddressValidationRestrictsCredentialDestination(t *testing.T) {
	cases := []struct {
		addr          string
		allowLoopback bool
		want          bool
	}{
		{"email-smtp.us-east-1.amazonaws.com:25", false, true},
		{"email-smtp.eu-west-2.amazonaws.com:587", false, true},
		{"email-smtp.ap-south-1.amazonaws.com:2587", false, true},
		{"email-smtp.cn-north-1.amazonaws.com.cn:587", false, true},
		{"email-smtp-fips.us-gov-west-1.api.aws:587", false, true},
		{"email-smtp.us-east-1.amazonaws.com:465", false, false},
		{"email-smtp.us-east-1.amazonaws.com:2465", false, false},
		{"smtp.example:587", false, false},
		{"email-smtp.bad.region.amazonaws.com:587", false, false},
		{"127.0.0.1:2525", false, false},
		{"127.0.0.1:2525", true, true},
		{"[::1]:2525", true, true},
		{"localhost:0", true, false},
	}
	for _, tc := range cases {
		if got := validSESAddr(tc.addr, tc.allowLoopback); got != tc.want {
			t.Errorf("validSESAddr(%q, %v) = %v, want %v",
				tc.addr, tc.allowLoopback, got, tc.want)
		}
	}
}

func TestSESIdentityValidation(t *testing.T) {
	for value, want := range map[string]bool{
		"user":                     true,
		"AKIAEXAMPLE_123-xyz":      true,
		"":                         false,
		" user":                    false,
		"user ":                    false,
		"user name":                false,
		"user\r\nother":            false,
		strings.Repeat("x", 255):   false,
		"contains\u007fcontrol":    false,
		"contains\u00a0whitespace": false,
	} {
		if got := validSESUsername(value); got != want {
			t.Errorf("validSESUsername(%q) = %v, want %v", value, got, want)
		}
	}

	for value, want := range map[string]bool{
		"from@example":                        true,
		"alerts+canary@example.com":           true,
		"":                                    false,
		" from@example":                       false,
		"From <from@example>":                 false,
		"from@example, other@example":         false,
		"from@example\r\nBcc: other@example":  false,
		"from@example\nBcc: other@example":    false,
		strings.Repeat("x", 250) + "@example": false,
	} {
		if got := validMailbox(value); got != want {
			t.Errorf("validMailbox(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestHostOnlyStripsPort(t *testing.T) {
	for in, want := range map[string]string{
		"email-smtp.us-east-1.amazonaws.com:587": "email-smtp.us-east-1.amazonaws.com",
		"smtp.example:25":                        "smtp.example",
		"no-port":                                "no-port",
		"":                                       "",
		"[::1]:587":                              "::1",
	} {
		if got := hostOnly(in); got != want {
			t.Errorf("hostOnly(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSESProbeRefusesUnconfigured(t *testing.T) {
	for name, p := range map[string]*SESProbe{
		"no address":      {Addr: "", Username: "user", Password: "secret", From: "a@example", CanaryTo: "c@example"},
		"no recipient":    {Addr: "smtp.example:587", Username: "user", Password: "secret", From: "a@example", CanaryTo: ""},
		"injected sender": {Addr: "smtp.example:587", Username: "user", Password: "secret", From: "a@example\r\nBcc: b@example", CanaryTo: "c@example"},
	} {
		p.metrics = NewMetrics(prometheus.NewRegistry())
		p.now = time.Now
		if err := p.ProbeOnce(context.Background()); err == nil {
			t.Errorf("%s: an unconfigured probe reported success", name)
		}
	}
}

func TestHandlerRemainingRejectionPaths(t *testing.T) {
	srv, fake := newFakeTelegram(t)
	h := New(testConfig(srv.URL, 111), NewMetrics(prometheus.NewRegistry()))

	// A non-POST request must be refused: Alertmanager only ever POSTs, so
	// anything else is a misconfiguration or a probe.
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/notify", strings.NewReader("{}")))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s returned %d, want 405", method, rec.Code)
		}
	}

	// An alert with no fingerprint has no stable dedup key.
	body := `{"version":"4","status":"firing","alerts":[{"status":"firing","fingerprint":"","startsAt":"2026-07-29T00:00:00Z","labels":{"alertname":"NoFP"}}]}`
	before := len(fake.sentTo())
	if rec := postWebhook(t, h, body, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("fingerprint-less alert returned %d", rec.Code)
	}
	if len(fake.sentTo()) != before {
		t.Error("an alert with no fingerprint was delivered; retries would resend it forever")
	}
}

func TestMessageHandlesMissingStatus(t *testing.T) {
	msg := Alert{Fingerprint: "fp", Labels: map[string]string{"alertname": "X"}}.Message()
	if !strings.HasPrefix(msg, "[UNKNOWN]") {
		t.Errorf("message with no status = %q, want it to lead with [UNKNOWN]", msg)
	}
}

func TestMalformedTelegramResponseIsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>gateway error</html>"))
	}))
	t.Cleanup(srv.Close)

	tg := NewTelegram(testConfig(srv.URL, 111), NewMetrics(prometheus.NewRegistry()))
	if err := tg.Deliver(context.Background(), firingAlert("fp-html")); err == nil {
		t.Fatal("a non-JSON 200 response was treated as delivered")
	}
}

func TestOversizedTelegramResponseIsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}` + strings.Repeat(" ", maxTelegramResponseBytes)))
	}))
	t.Cleanup(srv.Close)

	tg := NewTelegram(testConfig(srv.URL, 111), NewMetrics(prometheus.NewRegistry()))
	if err := tg.Deliver(context.Background(), firingAlert("fp-large-response")); err == nil {
		t.Fatal("an oversized Telegram response was treated as delivered")
	}
}

func TestMalformedConfigFileIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	content := "bot_token = \"" + testToken + "\"\nthis is not valid toml at all\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("malformed TOML was accepted")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("parse error leaked the token: %v", err)
	}
}

func TestProbeLoopsActuallyTick(t *testing.T) {
	srv, fake := newFakeTelegram(t)
	metrics := NewMetrics(prometheus.NewRegistry())

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	sent := make(chan struct{}, 8)
	ses := NewSESProbe(metrics)
	ses.Addr, ses.From, ses.CanaryTo = "smtp.example:587", "f@example", "c@example"
	ses.Username, ses.Password = "user", "password"
	ses.Interval = 50 * time.Millisecond
	ses.send = func(context.Context, string, smtp.Auth, string, []string, []byte) error {
		select {
		case sent <- struct{}{}:
		default:
		}
		return nil
	}
	go ses.Run(ctx)

	tp := NewTelegramProbe(NewTelegram(testConfig(srv.URL, 111), metrics))
	tp.Interval = 50 * time.Millisecond
	go tp.Run(ctx)

	<-ctx.Done()
	if len(sent) < 2 {
		t.Errorf("SES probe ticked %d times in 500ms at a 50ms interval", len(sent))
	}
	if len(fake.sentTo()) == 0 {
		t.Error("telegram probe never delivered a canary on tick")
	}
}

func TestConcurrentDeliveryUsesOneInFlightSend(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{})
	secondRequest := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
		} else {
			secondRequest <- struct{}{}
		}
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(srv.Close)

	tg := NewTelegram(testConfig(srv.URL, 111), NewMetrics(prometheus.NewRegistry()))
	errs := make(chan error, 2)
	alert := firingAlert("fp-concurrent")
	go func() { errs <- tg.Deliver(context.Background(), alert) }()
	<-entered
	go func() { errs <- tg.Deliver(context.Background(), alert) }()

	duplicated := false
	select {
	case <-secondRequest:
		duplicated = true
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent delivery returned %v", err)
		}
	}
	if duplicated || calls.Load() != 1 {
		t.Fatalf("concurrent duplicate produced %d sends", calls.Load())
	}
}

func TestDedupEntriesExpireAndStayBounded(t *testing.T) {
	srv, fake := newFakeTelegram(t)
	reg := prometheus.NewRegistry()
	tg := NewTelegram(testConfig(srv.URL, 111), NewMetrics(reg))
	tg.maxDedupEntries = 2
	tg.dedupTTL = time.Hour
	now := time.Unix(1_000, 0)
	tg.now = func() time.Time { return now }

	for _, fingerprint := range []string{"a", "b", "c"} {
		if err := tg.Deliver(context.Background(), firingAlert(fingerprint)); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
	}
	if len(tg.deliveries) != 2 {
		t.Fatalf("dedup map contains %d entries, want 2", len(tg.deliveries))
	}
	if _, exists := tg.deliveries[deliveryKey{
		fingerprint: "a",
		startsAt:    "2026-07-29T00:00:00Z",
		status:      "firing",
		chatID:      111,
	}]; exists {
		t.Fatal("oldest completed dedup entry was not evicted")
	}
	if got := scalarCounterValue(t, reg, "mithril_notifier_dedup_evictions_total"); got != 1 {
		t.Fatalf("dedup evictions = %v, want 1", got)
	}

	now = now.Add(2 * time.Hour)
	if err := tg.Deliver(context.Background(), firingAlert("c")); err != nil {
		t.Fatal(err)
	}
	if got := len(fake.sentTo()); got != 4 {
		t.Fatalf("expired dedup entry produced %d total sends, want 4", got)
	}
}

func TestScheduledFourHourReminderIsNotDeduplicated(t *testing.T) {
	srv, fake := newFakeTelegram(t)
	tg := NewTelegram(testConfig(srv.URL, 111), NewMetrics(prometheus.NewRegistry()))
	now := time.Unix(1_000, 0)
	tg.now = func() time.Time { return now }
	alert := firingAlert("still-firing")

	if err := tg.Deliver(context.Background(), alert); err != nil {
		t.Fatal(err)
	}
	now = now.Add(4 * time.Hour)
	if err := tg.Deliver(context.Background(), alert); err != nil {
		t.Fatal(err)
	}
	if got := len(fake.sentTo()); got != 2 {
		t.Fatalf("four-hour repeat produced %d sends, want 2", got)
	}
}

func TestPartialFailureDoesNotDuplicateSuccessfulChatBeforeReminder(t *testing.T) {
	srv, fake := newFakeTelegram(t)
	fake.failFor[222] = true
	tg := NewTelegram(testConfig(srv.URL, 111, 222), NewMetrics(prometheus.NewRegistry()))
	start := time.Unix(1_000, 0)
	now := start
	tg.now = func() time.Time { return now }
	alert := firingAlert("long-partial-outage")

	if err := tg.Deliver(context.Background(), alert); err == nil {
		t.Fatal("initial partial failure reported success")
	}

	now = start.Add(2 * time.Hour)
	fake.mu.Lock()
	fake.requests = nil
	fake.mu.Unlock()
	if err := tg.Deliver(context.Background(), alert); err == nil {
		t.Fatal("continued partial failure reported success")
	}
	if retried := fake.sentTo(); len(retried) != 1 || retried[0] != 222 {
		t.Fatalf("two-hour retry contacted %v, want only failed chat 222", retried)
	}

	now = start.Add(alertRepeatInterval)
	fake.mu.Lock()
	fake.requests = nil
	fake.failFor = map[int64]bool{}
	fake.mu.Unlock()
	if err := tg.Deliver(context.Background(), alert); err != nil {
		t.Fatalf("scheduled reminder delivery failed: %v", err)
	}
	if reminded := fake.sentTo(); len(reminded) != 2 {
		t.Fatalf("four-hour reminder contacted %v, want both chats", reminded)
	}
}

func TestHandlerRejectsConcurrentOverload(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(srv.Close)

	h := New(testConfig(srv.URL, 111), NewMetrics(prometheus.NewRegistry()))
	h.slots = make(chan struct{}, 1)

	firstStatus := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/notify", strings.NewReader(webhookBody("4", "first")))
		req.TLS = verifiedClientTLS()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		firstStatus <- rec.Code
	}()
	<-started

	if rec := postWebhook(t, h, webhookBody("4", "second"), true); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("overloaded handler returned %d, want 503", rec.Code)
	}
	close(release)
	if status := <-firstStatus; status != http.StatusOK {
		t.Fatalf("first request returned %d", status)
	}
}

func TestHandlerBoundsTotalDeliveryTime(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)

	h := New(testConfig(srv.URL, 111), NewMetrics(prometheus.NewRegistry()))
	h.webhookTimeout = 20 * time.Millisecond
	started := time.Now()
	rec := postWebhook(t, h, webhookBody("4", "slow"), true)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("timed-out delivery returned %d, want 500", rec.Code)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded request took %v", elapsed)
	}
}

func TestTelegramDoesNotFollowRedirects(t *testing.T) {
	var redirectedHits atomic.Int32
	redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectedHits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(redirected.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirected.URL, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	tg := NewTelegram(testConfig(redirector.URL, 111), NewMetrics(prometheus.NewRegistry()))
	err := tg.Deliver(context.Background(), firingAlert("fp-redirect"))
	if err == nil {
		t.Fatal("redirected Telegram request was treated as delivered")
	}
	if redirectedHits.Load() != 0 {
		t.Fatal("Telegram client followed a redirect and exposed the token-bearing referrer")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("redirect failure leaked the token: %v", err)
	}
}

func TestTelegramDisablesAmbientProxy(t *testing.T) {
	tg := NewTelegram(testConfig("http://127.0.0.1:1", 111), NewMetrics(prometheus.NewRegistry()))
	transport, ok := tg.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Telegram transport type = %T, want *http.Transport", tg.client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("Telegram client inherited an ambient proxy")
	}
}

func TestWebhookPrevalidationIsAtomicAndBounded(t *testing.T) {
	srv, fake := newFakeTelegram(t)
	h := New(testConfig(srv.URL, 111), NewMetrics(prometheus.NewRegistry()))
	valid := firingAlert("valid")
	tooManyLabels := map[string]string{}
	for i := 0; i <= maxLabelsPerAlert; i++ {
		tooManyLabels[fmt.Sprintf("label_%d", i)] = "value"
	}
	cases := map[string]webhookPayload{
		"empty alert list": {
			Version: "4", Status: "firing",
		},
		"truncated alert list": {
			Version: "4", Status: "firing", Alerts: []Alert{valid}, TruncatedAlerts: 1,
		},
		"invalid group status": {
			Version: "4", Status: "unknown", Alerts: []Alert{valid},
		},
		"invalid alert status": {
			Version: "4", Status: "firing",
			Alerts: []Alert{{Status: "unknown", Fingerprint: "fp", StartsAt: "2026-07-29T00:00:00Z"}},
		},
		"oversized fingerprint": {
			Version: "4", Status: "firing",
			Alerts: []Alert{{
				Status: "firing", Fingerprint: strings.Repeat("x", maxFingerprintBytes+1),
				StartsAt: "2026-07-29T00:00:00Z",
			}},
		},
		"missing lifecycle start": {
			Version: "4", Status: "firing",
			Alerts: []Alert{{Status: "firing", Fingerprint: "fp"}},
		},
		"missing alert name": {
			Version: "4", Status: "firing",
			Alerts: []Alert{{
				Status: "firing", Fingerprint: "fp",
				StartsAt: "2026-07-29T00:00:00Z",
			}},
		},
		"visually blank alert name": {
			Version: "4", Status: "firing",
			Alerts: []Alert{{
				Status: "firing", Fingerprint: "fp",
				StartsAt: "2026-07-29T00:00:00Z",
				Labels:   map[string]string{"alertname": " \n\u202e "},
			}},
		},
		"malformed lifecycle start": {
			Version: "4", Status: "firing",
			Alerts: []Alert{{
				Status: "firing", Fingerprint: "fp", StartsAt: "not-a-time",
			}},
		},
		"too many labels": {
			Version: "4", Status: "firing",
			Alerts: []Alert{{
				Status: "firing", Fingerprint: "fp",
				StartsAt: "2026-07-29T00:00:00Z", Labels: tooManyLabels,
			}},
		},
		"valid before invalid": {
			Version: "4", Status: "firing",
			Alerts: []Alert{valid, {
				Status: "firing", Fingerprint: "", StartsAt: "2026-07-29T00:00:00Z",
			}},
		},
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			if rec := postWebhook(t, h, string(raw), true); rec.Code != http.StatusBadRequest {
				t.Fatalf("invalid payload returned %d", rec.Code)
			}
		})
	}
	if got := len(fake.sentTo()); got != 0 {
		t.Fatalf("prevalidation delivered %d alerts before rejecting the payload", got)
	}
}

func TestAlertMessageHasTotalBound(t *testing.T) {
	message := (Alert{
		Status: "firing",
		Labels: map[string]string{
			"alertname":     "ManyLabels",
			"severity":      strings.Repeat("x", maxFieldRunes),
			"deployment_id": strings.Repeat("x", maxFieldRunes),
			"target_job":    strings.Repeat("x", maxFieldRunes),
			"job":           strings.Repeat("x", maxFieldRunes),
			"instance":      strings.Repeat("x", maxFieldRunes),
			"role":          strings.Repeat("x", maxFieldRunes),
			"collector":     strings.Repeat("x", maxFieldRunes),
			"route":         strings.Repeat("x", maxFieldRunes),
			"state":         strings.Repeat("x", maxFieldRunes),
		},
		Annotations: map[string]string{"summary": strings.Repeat("x", maxFieldRunes)},
	}).Message()
	if got := len([]rune(message)); got > maxMessageRunes {
		t.Fatalf("message contains %d runes, max is %d", got, maxMessageRunes)
	}
	if strings.Contains(message, "label_") {
		t.Fatal("message exposed an unrecognized label")
	}
}

func TestBoundMessageMarksTruncation(t *testing.T) {
	message := boundMessage(strings.Repeat("x", maxMessageRunes+1))
	if len([]rune(message)) != maxMessageRunes || !strings.HasSuffix(message, "…") {
		t.Fatal("bounded message does not visibly mark truncation")
	}
}

func TestRouteConfiguredMetricIsBounded(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	if got := gaugeValue(
		t, reg, "mithril_notification_route_configured", "route", RouteTelegram,
	); got != 1 {
		t.Fatalf("Telegram configured = %v, want 1", got)
	}
	if got := gaugeValue(
		t, reg, "mithril_notification_route_configured", "route", RouteSES,
	); got != 0 {
		t.Fatalf("unset SES configured = %v, want 0", got)
	}
	metrics.SetSESConfigured(true)
	if got := gaugeValue(
		t, reg, "mithril_notification_route_configured", "route", RouteSES,
	); got != 1 {
		t.Fatalf("configured SES = %v, want 1", got)
	}
	metrics.SetSESConfigured(false)
	if got := gaugeValue(
		t, reg, "mithril_notification_route_configured", "route", RouteSES,
	); got != 0 {
		t.Fatalf("cleared SES configured = %v, want 0", got)
	}
	for _, route := range allRoutes {
		if got := gaugeValue(t, reg, "mithril_notification_probe_enabled", "route", route); got != 0 {
			t.Fatalf("initial %s probe enabled = %v, want 0", route, got)
		}
	}
	metrics.SetProbesEnabled(true, false)
	if got := gaugeValue(
		t, reg, "mithril_notification_probe_enabled", "route", RouteTelegram,
	); got != 1 {
		t.Fatalf("Telegram probe enabled = %v, want 1", got)
	}
	if got := gaugeValue(
		t, reg, "mithril_notification_probe_enabled", "route", RouteSES,
	); got != 0 {
		t.Fatalf("SES probe enabled without configuration = %v, want 0", got)
	}
	metrics.SetProbesEnabled(true, true)
	if got := gaugeValue(
		t, reg, "mithril_notification_probe_enabled", "route", RouteTelegram,
	); got != 1 {
		t.Fatalf("enabled Telegram probe = %v, want 1", got)
	}
	if got := gaugeValue(
		t, reg, "mithril_notification_probe_enabled", "route", RouteSES,
	); got != 1 {
		t.Fatalf("enabled SES probe = %v, want 1", got)
	}
	metrics.SetProbesEnabled(false, true)
	for _, route := range allRoutes {
		if got := gaugeValue(t, reg, "mithril_notification_probe_enabled", "route", route); got != 0 {
			t.Fatalf("disabled %s probe = %v, want 0", route, got)
		}
	}
}

func TestRouteProbeMetricsRecordSuccessAndFailure(t *testing.T) {
	srv, fake := newFakeTelegram(t)
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	tg := NewTelegram(testConfig(srv.URL, 111), metrics)
	probe := NewTelegramProbe(tg)
	now := time.Unix(1_234, 0)
	probe.now = func() time.Time { return now }

	if err := probe.ProbeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := gaugeValue(t, reg, "mithril_notification_probe_success", "route", RouteTelegram); got != 1 {
		t.Fatalf("Telegram probe success = %v, want 1", got)
	}
	if got := gaugeValue(
		t,
		reg,
		"mithril_notification_probe_last_success_timestamp_seconds",
		"route",
		RouteTelegram,
	); got != 1234 {
		t.Fatalf("Telegram probe timestamp = %v, want 1234", got)
	}
	if got := counterValue(
		t, reg, "mithril_notification_probe_attempts_total", "route", RouteTelegram,
	); got != 1 {
		t.Fatalf("Telegram probe attempts = %v, want 1", got)
	}
	if got := counterValue(
		t, reg, "mithril_notifier_delivered_total", "channel", ChannelTelegram,
	); got != 0 {
		t.Fatalf("Telegram canary counted as %v real alert deliveries", got)
	}
	fake.mu.Lock()
	silent, _ := fake.requests[0]["disable_notification"].(bool)
	text, _ := fake.requests[0]["text"].(string)
	fake.mu.Unlock()
	if !silent {
		t.Fatal("Telegram canary was not sent silently")
	}
	if !strings.Contains(text, "Telegram alerts working") ||
		!strings.Contains(text, "Automatic health check. No action needed.") ||
		strings.Contains(text, "Severity:") ||
		strings.Contains(text, "needs attention") {
		t.Fatalf("Telegram canary text is misleading: %q", text)
	}

	fake.mu.Lock()
	fake.status = http.StatusInternalServerError
	fake.mu.Unlock()
	now = now.Add(time.Second)
	if err := probe.ProbeOnce(context.Background()); err == nil {
		t.Fatal("failed Telegram probe reported success")
	}
	if got := gaugeValue(t, reg, "mithril_notification_probe_success", "route", RouteTelegram); got != 0 {
		t.Fatalf("Telegram probe failure left success at %v", got)
	}
	if got := counterValue(
		t, reg, "mithril_notification_probe_attempts_total", "route", RouteTelegram,
	); got != 2 {
		t.Fatalf("Telegram probe attempts = %v, want 2", got)
	}
	if got := counterValue(
		t, reg, "mithril_notification_probe_failures_total", "route", RouteTelegram,
	); got != 1 {
		t.Fatalf("Telegram probe failures = %v, want 1", got)
	}
}

func TestSESProbeHonorsTimeout(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	probe := NewSESProbe(metrics)
	probe.Addr = "smtp.example:587"
	probe.Username = "user"
	probe.Password = "password"
	probe.From = "from@example"
	probe.CanaryTo = "canary@example"
	probe.Timeout = 20 * time.Millisecond
	probe.send = func(ctx context.Context, _ string, _ smtp.Auth, _ string, _ []string, _ []byte) error {
		<-ctx.Done()
		return ctx.Err()
	}

	started := time.Now()
	if err := probe.ProbeOnce(context.Background()); err == nil {
		t.Fatal("timed-out SES probe reported success")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("SES timeout took %v", elapsed)
	}
	if got := gaugeValue(t, reg, "mithril_notification_probe_success", "route", RouteSES); got != 0 {
		t.Fatalf("SES timeout left success at %v", got)
	}
	if got := counterValue(
		t, reg, "mithril_notification_probe_failures_total", "route", RouteSES,
	); got != 1 {
		t.Fatalf("SES timeout failures = %v, want 1", got)
	}
}

func TestSendSMTPHonorsContextDeadline(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = sendSMTP(ctx, listener.Addr().String(), nil, "from@example", []string{"to@example"}, nil)
	if err == nil {
		t.Fatal("SMTP server without a greeting did not time out")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("SMTP context deadline took %v", elapsed)
	}

	select {
	case conn := <-accepted:
		_ = conn.Close()
	case <-time.After(time.Second):
		t.Fatal("SMTP test server did not accept the connection")
	}
}

func TestSendSMTPRequiresSTARTTLS(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("220 smtp.example ESMTP\r\n"))
		reader := bufio.NewReader(conn)
		if _, err := reader.ReadString('\n'); err != nil {
			return
		}
		_, _ = conn.Write([]byte("250-smtp.example\r\n250 SIZE 1024\r\n"))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = sendSMTP(ctx, listener.Addr().String(), nil, "from@example", []string{"to@example"}, nil)
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("server without STARTTLS returned %v", err)
	}
	<-serverDone
}

func TestSendSMTPVerifiesTLSRootAndServerName(t *testing.T) {
	fixture := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	certificate := fixture.TLS.Certificates[0]
	transport := fixture.Client().Transport.(*http.Transport)
	roots := transport.TLSClientConfig.RootCAs
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := leaf.VerifyHostname("127.0.0.1"); err != nil {
		t.Fatalf("test certificate does not cover loopback IP: %v", err)
	}
	if err := leaf.VerifyHostname("localhost"); err == nil {
		t.Fatal("test certificate unexpectedly covers localhost")
	}
	fixture.Close()

	send := func(addr string, rootCAs *x509.CertPool) error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return sendSMTPWithRootCAs(
			ctx,
			addr,
			nil,
			"from@example",
			[]string{"to@example"},
			[]byte("Subject: probe\r\n\r\nbody\r\n"),
			rootCAs,
		)
	}

	addr, done := startTestSMTPServer(t, certificate)
	if err := send(addr, roots); err != nil {
		t.Fatalf("trusted matching STARTTLS server was rejected: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("trusted SMTP server failed: %v", err)
	}

	addr, done = startTestSMTPServer(t, certificate)
	if err := send(addr, x509.NewCertPool()); err == nil {
		t.Fatal("server signed by an untrusted root was accepted")
	}
	if err := <-done; err == nil {
		t.Fatal("untrusted-root server unexpectedly completed TLS")
	}

	addr, done = startTestSMTPServer(t, certificate)
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := send(net.JoinHostPort("localhost", port), roots); err == nil {
		t.Fatal("certificate for another hostname was accepted")
	}
	if err := <-done; err == nil {
		t.Fatal("wrong-name server unexpectedly completed TLS")
	}
}

func startTestSMTPServer(t *testing.T, certificate tls.Certificate) (string, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		done <- serveTestSMTP(conn, certificate)
	}()
	return listener.Addr().String(), done
}

func serveTestSMTP(conn net.Conn, certificate tls.Certificate) error {
	reader := bufio.NewReader(conn)
	if _, err := io.WriteString(conn, "220 test ESMTP\r\n"); err != nil {
		return err
	}
	if err := expectSMTPCommand(reader, "EHLO "); err != nil {
		return err
	}
	if _, err := io.WriteString(conn, "250-test\r\n250 STARTTLS\r\n"); err != nil {
		return err
	}
	if err := expectSMTPCommand(reader, "STARTTLS"); err != nil {
		return err
	}
	if _, err := io.WriteString(conn, "220 ready\r\n"); err != nil {
		return err
	}

	tlsConn := tls.Server(conn, &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{certificate},
	})
	if err := tlsConn.Handshake(); err != nil {
		return err
	}
	reader = bufio.NewReader(tlsConn)
	for _, exchange := range []struct {
		command string
		reply   string
	}{
		{"EHLO ", "250 test\r\n"},
		{"MAIL FROM:", "250 ok\r\n"},
		{"RCPT TO:", "250 ok\r\n"},
		{"DATA", "354 continue\r\n"},
	} {
		if err := expectSMTPCommand(reader, exchange.command); err != nil {
			return err
		}
		if _, err := io.WriteString(tlsConn, exchange.reply); err != nil {
			return err
		}
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		if line == ".\r\n" {
			break
		}
	}
	if _, err := io.WriteString(tlsConn, "250 accepted\r\n"); err != nil {
		return err
	}
	if err := expectSMTPCommand(reader, "QUIT"); err != nil {
		return err
	}
	_, err := io.WriteString(tlsConn, "221 bye\r\n")
	return err
}

func expectSMTPCommand(reader *bufio.Reader, prefix string) error {
	line, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.HasPrefix(line, prefix) {
		return fmt.Errorf("SMTP command %q does not start with %q", strings.TrimSpace(line), prefix)
	}
	return nil
}

// The routine health probe must not keep posting to the operator's chat. It
// posts once so the whole path is proved, then verifies the route silently.
func TestTelegramProbePostsOnceThenChecksRouteSilently(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			calls = append(calls, "sendMessage")
		case strings.HasSuffix(r.URL.Path, "/getChat"):
			calls = append(calls, "getChat")
		default:
			calls = append(calls, "other")
		}
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	metrics := NewMetrics(prometheus.NewRegistry())
	probe := NewTelegramProbe(NewTelegram(testConfig(srv.URL, 111), metrics))

	for range 4 {
		if err := probe.ProbeOnce(context.Background()); err != nil {
			t.Fatalf("probe failed: %v", err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	posts := 0
	for _, c := range calls {
		if c == "sendMessage" {
			posts++
		}
	}
	if posts != 1 {
		t.Fatalf("probe posted %d messages across 4 cycles, want exactly 1 (calls: %v)", posts, calls)
	}
	if len(calls) != 4 {
		t.Fatalf("probe made %d calls across 4 cycles, want 4 (calls: %v)", len(calls), calls)
	}
}

// Suppressing the message must not suppress the failure: a broken route still
// has to fail, or the probe stops being evidence.
func TestTelegramProbeFailsWhenSilentRouteCheckFails(t *testing.T) {
	var mu sync.Mutex
	posted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		first := !posted
		posted = true
		mu.Unlock()
		if first && strings.HasSuffix(r.URL.Path, "/sendMessage") {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	probe := NewTelegramProbe(NewTelegram(testConfig(srv.URL, 111), NewMetrics(prometheus.NewRegistry())))
	if err := probe.ProbeOnce(context.Background()); err != nil {
		t.Fatalf("first probe should post and succeed: %v", err)
	}
	if err := probe.ProbeOnce(context.Background()); err == nil {
		t.Fatal("a failing route check was reported as healthy")
	}
}

// Only faults may interrupt a human. State and confirmations are retrievable.
func TestNotifiableOnlyAllowsActionableAlerts(t *testing.T) {
	tests := []struct {
		name     string
		severity string
		status   string
		extra    map[string]string
		want     bool
	}{
		{name: "critical fault", severity: "critical", status: "firing", want: true},
		{name: "warning fault", severity: "warning", status: "firing", want: true},
		{name: "informational state", severity: "info", status: "firing", want: false},
		{name: "deadman heartbeat", severity: "none", status: "firing", want: false},
		{name: "action completed", severity: "info", status: "firing",
			extra: map[string]string{"lifecycle": "event"}, want: false},
		{name: "resolved lifecycle event", severity: "critical", status: "resolved",
			extra: map[string]string{"lifecycle": "event"}, want: false},
		{name: "resolved fault", severity: "critical", status: "resolved", want: true},
		{name: "unlabelled alert defaults to notifying", severity: "", status: "firing", want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			alert := Alert{
				Status:      test.status,
				Fingerprint: "fp",
				Labels:      map[string]string{"alertname": "X", "severity": test.severity},
			}
			for k, v := range test.extra {
				alert.Labels[k] = v
			}
			if got := Notifiable(alert); got != test.want {
				t.Fatalf("Notifiable = %v, want %v", got, test.want)
			}
		})
	}
}

// Quieting must never silence a genuine failure.
func TestInformationalAlertsAreNotPostedButFaultsAre(t *testing.T) {
	srv, fake := newFakeTelegram(t)
	tg := NewTelegram(testConfig(srv.URL, 111), NewMetrics(prometheus.NewRegistry()))

	restarted := firingAlert("restarted")
	restarted.Labels["severity"] = "info"
	restarted.Labels["lifecycle"] = "event"
	if err := tg.DeliverGroup(t.Context(), []Alert{restarted}); err != nil {
		t.Fatal(err)
	}
	if got := len(fake.sentTo()); got != 0 {
		t.Fatalf("an informational lifecycle alert produced %d sends, want 0", got)
	}

	diverged := firingAlert("diverged")
	diverged.Labels["severity"] = "critical"
	if err := tg.DeliverGroup(t.Context(), []Alert{diverged}); err != nil {
		t.Fatal(err)
	}
	if got := len(fake.sentTo()); got != 1 {
		t.Fatalf("a critical fault produced %d sends, want 1", got)
	}
}

func TestProbeIntervalSupportsDisableAndAlertCompatibleGaps(t *testing.T) {
	tests := []struct {
		seconds  int
		want     time.Duration
		disabled bool
	}{
		{seconds: -1, want: 0, disabled: true},
		{seconds: 0, want: DefaultProbeInterval},
		{seconds: 60, want: time.Minute},
		{seconds: 3_600, want: time.Hour},
	}
	for _, test := range tests {
		cfg := Config{ProbeIntervalSec: test.seconds}
		if got := cfg.ProbeInterval(); got != test.want {
			t.Errorf("ProbeInterval(%d) = %v, want %v", test.seconds, got, test.want)
		}
		if got := cfg.ProbeDisabled(); got != test.disabled {
			t.Errorf("ProbeDisabled(%d) = %v, want %v", test.seconds, got, test.disabled)
		}
	}
}

func TestProbeIntervalValidationBounds(t *testing.T) {
	// Other required fields are absent here, so assert on the specific message
	// rather than overall validity.
	const want = "probe_interval_seconds"
	check := func(seconds int, shouldReject bool) {
		t.Helper()
		err := Config{
			BotToken:         testToken,
			AllowedChatIDs:   []int64{111},
			ProbeIntervalSec: seconds,
		}.validate()
		rejected := err != nil && strings.Contains(err.Error(), want)
		if rejected != shouldReject {
			t.Errorf("probe_interval_seconds=%d rejected=%v, want %v (err: %v)",
				seconds, rejected, shouldReject, err)
		}
	}
	for _, ok := range []int{-1, 0, 60, 3_600} {
		check(ok, false)
	}
	for _, bad := range []int{-2, 59, 3_601, 86_400} {
		check(bad, true)
	}
}

// A disabled interval is zero, so each Run method must return before calling
// time.NewTicker. Recover the panic here so the test observes it deterministically.
func TestProbesSurviveTheDisabledInterval(t *testing.T) {
	for name, run := range map[string]func(context.Context){
		"ses":      (&SESProbe{Interval: 0}).Run,
		"telegram": (&TelegramProbe{Interval: 0}).Run,
	} {
		t.Run(name, func(t *testing.T) {
			var recovered any
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer func() { recovered = recover() }()
				run(t.Context())
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not return on a disabled interval")
			}
			if recovered != nil {
				t.Fatalf("Run panicked on the disabled interval: %v", recovered)
			}
		})
	}
}
