package notifier

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/Overclock-Validator/mithril/internal/safedisplay"
)

const (
	maxWebhookBodyBytes    = 1 << 20 // 1 MiB
	maxAlertsPerRequest    = 256
	maxLabelsPerAlert      = 64
	maxAnnotationsPerAlert = 64
	maxFingerprintBytes    = 128
	maxStartsAtBytes       = 64
	maxMessageRunes        = 3500
	maxConcurrentWebhooks  = 4
	// maxFieldRunes bounds any single rendered field, so a hostile or broken
	// label cannot produce an unbounded Telegram message.
	maxFieldRunes = 256
)

// Alert is one alert from an Alertmanager webhook payload.
type Alert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	Fingerprint string            `json:"fingerprint"`
	StartsAt    string            `json:"startsAt"`
}

// webhookPayload is Alertmanager's webhook schema. Only version 4 is accepted:
// silently accepting an unknown version would mean guessing at field meanings.
type webhookPayload struct {
	Version         string  `json:"version"`
	Status          string  `json:"status"`
	Alerts          []Alert `json:"alerts"`
	TruncatedAlerts int     `json:"truncatedAlerts"`
}

// Message renders a bounded, plain-text notification for an operator.
func (a Alert) Message() string {
	var b strings.Builder
	b.WriteString(a.title())
	b.WriteByte('\n')

	summary := a.summary()
	fmt.Fprintln(&b, summary)

	fields := []struct {
		key   string
		label string
	}{
		{"deployment_id", "Node"},
		{"target_job", "Component"},
	}
	for _, field := range fields {
		value := safeField(a.Labels[field.key])
		if value != "" {
			fmt.Fprintf(&b, "%s: %s\n", field.label, value)
		}
	}
	if action := a.nextAction(); action != "" {
		fmt.Fprintf(&b, "Next: %s\n", action)
	}
	if started, err := time.Parse(time.RFC3339Nano, a.StartsAt); err == nil {
		fmt.Fprintf(&b, "Time: %s\n", started.UTC().Format("2006-01-02 15:04:05 UTC"))
	}
	return boundMessage(strings.TrimRight(b.String(), "\n"))
}

func (a Alert) title() string {
	name := strings.TrimSpace(a.Labels["alertname"])
	if title := knownAlertTitle(name, a.Status); title != "" {
		return title
	}
	switch a.Status {
	case "firing":
		switch strings.ToLower(strings.TrimSpace(a.Labels["severity"])) {
		case "critical":
			return "🚨 Mithril needs attention"
		case "warning":
			return "⚠️ Mithril warning"
		default:
			return "ℹ️ Mithril update"
		}
	case "resolved":
		if a.neutralResolution() {
			return "ℹ️ Mithril state update"
		}
		if a.informational() {
			return "ℹ️ Mithril update ended"
		}
		return "ℹ️ Mithril alert ended"
	default:
		return "[UNKNOWN] Mithril status update"
	}
}

func knownAlertTitle(name, status string) string {
	resolved := status == "resolved"
	switch name {
	case "MithrilAgentActionCompleted":
		if !resolved {
			return "✅ Devnet trade complete"
		}
	case "MithrilNotifierCanary":
		if !resolved {
			return "✅ Telegram alerts working"
		}
	case "MithrilAgentActionSubmitted":
		if !resolved {
			return "⏳ Devnet trade submitted"
		}
	case "MithrilAgentPriceTargetReached":
		if !resolved {
			return "🎯 SOL price target reached"
		}
	case "MithrilAgentActionTerminal":
		if !resolved {
			return "🚨 Devnet trade stopped"
		}
		return "ℹ️ Trade-stop alert ended"
	case "MithrilAgentExecutionEnabled":
		if !resolved {
			return "🟡 Devnet trading enabled"
		}
		return "ℹ️ Execution-enabled alert ended"
	case "MithrilAgentDown":
		if !resolved {
			return "🚨 Mithril agent offline"
		}
		return "ℹ️ Agent-offline alert ended"
	case "MithrilAgentRestarted":
		if !resolved {
			return "ℹ️ Mithril agent restarted"
		}
	case "MithrilNodeProbeDown":
		if !resolved {
			return "🚨 Mithril node unreachable"
		}
		return "ℹ️ Node-reachability alert ended"
	case "MithrilNodeBehindReference":
		if !resolved {
			return "⚠️ Mithril node behind"
		}
		return "ℹ️ Node-lag alert ended"
	case "MithrilReplayStalled":
		if !resolved {
			return "🚨 Mithril replay stalled"
		}
		return "ℹ️ Replay-stall alert ended"
	}
	return ""
}

func (a Alert) nextAction() string {
	if a.Status == "resolved" {
		return ""
	}
	switch strings.TrimSpace(a.Labels["alertname"]) {
	case "MithrilAgentActionCompleted":
		return "Check agent status before enabling another action."
	case "MithrilAgentActionSubmitted":
		return "Wait for confirmation. Do not enable another action."
	case "MithrilAgentPriceTargetReached":
		return "Review the price status before enabling a trade."
	case "MithrilAgentExecutionEnabled":
		return "One bounded Devnet action may run."
	case "MithrilAgentActionTerminal":
		return "Check agent status before enabling another action."
	}
	if !a.informational() {
		return "Check Mithril status."
	}
	return ""
}

func (a Alert) summary() string {
	if a.Status == "resolved" {
		// Alert expressions can become false because prerequisite telemetry
		// disappeared. Do not turn resolution into an unsupported health claim
		// or repeat the firing annotation as though it were still current.
		return "Alert condition ended. Check current status."
	}
	summary := safeField(a.Annotations["summary"])
	if summary == "" {
		summary = humanAlertName(a.Labels["alertname"])
	}
	return summary
}

func (a Alert) informational() bool {
	return strings.EqualFold(strings.TrimSpace(a.Labels["severity"]), "info")
}

func (a Alert) neutralResolution() bool {
	if a.Status != "resolved" {
		return false
	}
	lifecycle := strings.TrimSpace(a.Labels["lifecycle"])
	return lifecycle == "state" || lifecycle == "event"
}

func humanAlertName(value string) string {
	value = strings.TrimPrefix(safeField(value), "Mithril")
	if value == "" {
		return "Node status changed"
	}
	runes := []rune(value)
	var b strings.Builder
	for index, current := range runes {
		if index > 0 && unicode.IsUpper(current) &&
			(unicode.IsLower(runes[index-1]) ||
				(index+1 < len(runes) && unicode.IsLower(runes[index+1]))) {
			b.WriteByte(' ')
		}
		b.WriteRune(current)
	}
	return b.String()
}

func safeField(value string) string {
	return bound(safedisplay.Text(value, nil))
}

func bound(v string) string {
	// Keep every rendered field on one visual line and remove format controls
	// such as bidi overrides. Alert labels are untrusted input; allowing them
	// to shape later lines could make one alert look like another.
	v = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return ' '
		}
		return r
	}, v)
	v = strings.Join(strings.Fields(v), " ")
	runes := []rune(v)
	if len(runes) <= maxFieldRunes {
		return v
	}
	return string(runes[:maxFieldRunes]) + "…"
}

func boundMessage(v string) string {
	runes := []rune(v)
	if len(runes) <= maxMessageRunes {
		return v
	}
	return string(runes[:maxMessageRunes-1]) + "…"
}

// Handler receives Alertmanager webhooks.
type Handler struct {
	telegram       *Telegram
	metrics        *Metrics
	slots          chan struct{}
	webhookTimeout time.Duration
}

// New builds a handler.
func New(cfg Config, metrics *Metrics) *Handler {
	return &Handler{
		telegram:       NewTelegram(cfg, metrics),
		metrics:        metrics,
		slots:          make(chan struct{}, maxConcurrentWebhooks),
		webhookTimeout: cfg.WebhookTimeout(),
	}
}

// TelegramProbe returns a canary probe over the handler's delivery path.
func (h *Handler) TelegramProbe() *TelegramProbe {
	return NewTelegramProbe(h.telegram)
}

// ServeHTTP accepts a bounded Alertmanager v4 payload and delivers each alert.
//
// A delivery failure returns 500 so Alertmanager retries. It never returns 200
// on failure: a 200 would tell Alertmanager the alert was handled, and the
// operator would never learn the message did not arrive.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.reject(w, RejectBadMethod, http.StatusMethodNotAllowed)
		return
	}
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
		// The TLS stack verifies the chain against the dedicated client CA.
		// Requiring that evidence here keeps the handler safe if it is remounted.
		h.reject(w, RejectNoClientCert, http.StatusUnauthorized)
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodyBytes+1))
	if err != nil {
		h.reject(w, RejectBadSchema, http.StatusBadRequest)
		return
	}
	if len(raw) > maxWebhookBodyBytes {
		h.reject(w, RejectTooLarge, http.StatusRequestEntityTooLarge)
		return
	}

	var payload webhookPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		h.reject(w, RejectBadSchema, http.StatusBadRequest)
		return
	}
	if payload.Version != "4" ||
		!validStatus(payload.Status) ||
		len(payload.Alerts) == 0 ||
		payload.TruncatedAlerts != 0 {
		h.reject(w, RejectBadSchema, http.StatusBadRequest)
		return
	}
	if len(payload.Alerts) > maxAlertsPerRequest {
		h.reject(w, RejectTooManyAlerts, http.StatusRequestEntityTooLarge)
		return
	}
	for _, alert := range payload.Alerts {
		if err := validateAlert(alert); err != nil {
			h.reject(w, RejectBadSchema, http.StatusBadRequest)
			return
		}
	}

	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		h.reject(w, RejectOverloaded, http.StatusServiceUnavailable)
		return
	}

	timeout := h.webhookTimeout
	if timeout <= 0 {
		timeout = DefaultWebhookTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	if err := h.telegram.DeliverGroup(ctx, payload.Alerts); err != nil {
		// Alertmanager keeps its own record and will retry. Its state is
		// unaffected by this response.
		http.Error(w, "delivery failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func validateAlert(alert Alert) error {
	if !validStatus(alert.Status) {
		return fmt.Errorf("invalid alert status")
	}
	if alert.Fingerprint == "" || len(alert.Fingerprint) > maxFingerprintBytes {
		return fmt.Errorf("invalid alert fingerprint")
	}
	if alert.StartsAt == "" || len(alert.StartsAt) > maxStartsAtBytes {
		return fmt.Errorf("invalid alert start time")
	}
	if _, err := time.Parse(time.RFC3339Nano, alert.StartsAt); err != nil {
		return fmt.Errorf("invalid alert start time")
	}
	if len(alert.Labels) > maxLabelsPerAlert || len(alert.Annotations) > maxAnnotationsPerAlert {
		return fmt.Errorf("alert fields exceed limits")
	}
	if bound(alert.Labels["alertname"]) == "" {
		return fmt.Errorf("alert name is missing")
	}
	return nil
}

func validStatus(status string) bool {
	return status == "firing" || status == "resolved"
}

func (h *Handler) reject(w http.ResponseWriter, reason string, status int) {
	h.metrics.Rejected.WithLabelValues(reason).Inc()
	// The response carries the bounded reason only; never request content.
	http.Error(w, reason, status)
}
