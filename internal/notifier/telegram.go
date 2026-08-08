package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	maxTelegramResponseBytes = 32 << 10
	defaultMaxDedupEntries   = 8192
	maxGroupEntries          = 5
	maxGroupEntryRunes       = 600
)

// deliveryKey identifies one alert lifecycle and delivery state. Alertmanager
// keeps fingerprints stable when an alert resolves and can reuse one when the
// same condition fires again. startsAt distinguishes that new incident from a
// retry of the old one; status lets firing and recovery both be delivered.
type deliveryKey struct {
	fingerprint string
	startsAt    string
	status      string
	chatID      int64
}

type deliveryEntry struct {
	inFlight    chan struct{}
	deliveredAt time.Time
}

type telegramDeliveryError struct {
	reason string
	err    error
}

func (e *telegramDeliveryError) Error() string { return e.err.Error() }
func (e *telegramDeliveryError) Unwrap() error { return e.err }

func telegramError(reason, message string) error {
	return &telegramDeliveryError{reason: reason, err: errors.New(message)}
}

func telegramFailureReason(err error) string {
	var deliveryErr *telegramDeliveryError
	if errors.As(err, &deliveryErr) {
		return deliveryErr.reason
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return FailureTimeout
	}
	return FailureInternal
}

// Telegram delivers alerts to allowlisted numeric chat IDs.
type Telegram struct {
	cfg     Config
	metrics *Metrics
	client  *http.Client
	now     func() time.Time

	mu              sync.Mutex
	deliveries      map[deliveryKey]*deliveryEntry
	dedupTTL        time.Duration
	maxDedupEntries int
}

// NewTelegram builds a deliverer with a bounded HTTP client.
func NewTelegram(cfg Config, metrics *Metrics) *Telegram {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{}
	}
	transport := base.Clone()
	transport.Proxy = nil
	return &Telegram{
		cfg:     cfg,
		metrics: metrics,
		client: &http.Client{
			Timeout:   cfg.SendTimeout(),
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		now:             time.Now,
		deliveries:      make(map[deliveryKey]*deliveryEntry),
		dedupTTL:        DefaultDedupTTL,
		maxDedupEntries: defaultMaxDedupEntries,
	}
}

// Deliver sends one alert to every allowlisted chat. Recent per-chat deliveries
// are deduplicated, partial failures retry only failed chats, and Alertmanager
// remains the durable owner of alert state.
func (t *Telegram) Deliver(ctx context.Context, alert Alert) error {
	return t.deliver(ctx, telegramAlerts([]Alert{alert}), true, false)
}

// DeliverGroup sends one bounded summary per chat for an Alertmanager group.
// Deduplication remains per alert, so retries include only lifecycle events
// that were not already acknowledged for that chat.
func (t *Telegram) DeliverGroup(ctx context.Context, alerts []Alert) error {
	if len(alerts) == 0 {
		return nil
	}
	return t.deliver(ctx, telegramAlerts(alerts), true, false)
}

func telegramAlerts(alerts []Alert) []Alert {
	deliverable := make([]Alert, 0, len(alerts))
	for _, alert := range alerts {
		if !Notifiable(alert) {
			continue
		}
		deliverable = append(deliverable, alert)
	}
	return deliverable
}

// Notifiable reports whether an alert is worth interrupting a human for.
//
// An unprompted message must mean "something is wrong and you must act".
// Informational alerts describe state rather than a fault, and every
// lifecycle-event alert (restarted, action completed, action submitted, price
// target reached) is already informational, so this one rule covers them: they
// confirm something the operator deliberately did and are retrievable on
// demand instead. They remain visible in Alertmanager and in the metrics.
//
// A resolved lifecycle event is dropped separately, because the firing message
// already told the operator what happened.
//
// No severity is lowered anywhere to achieve quiet: a real failure still
// carries critical or warning severity and is still delivered.
func Notifiable(alert Alert) bool {
	if alert.Status == "resolved" && strings.TrimSpace(alert.Labels["lifecycle"]) == "event" {
		return false
	}
	switch strings.TrimSpace(alert.Labels["severity"]) {
	case "info", "none":
		return false
	}
	return true
}

// probe delivers a synthetic canary without recording it as a real
// Alertmanager webhook alert.
func (t *Telegram) probe(ctx context.Context, alert Alert) error {
	return t.deliver(ctx, []Alert{alert}, false, true)
}

func (t *Telegram) deliver(
	ctx context.Context,
	alerts []Alert,
	recordAlertMetrics bool,
	silent bool,
) error {
	var failed []int64
	var lastErr error

	for _, chatID := range t.cfg.AllowedChatIDs {
		type reservation struct {
			key   deliveryKey
			entry *deliveryEntry
			alert Alert
		}
		reservations := make([]reservation, 0, len(alerts))
		var reserveErr error
		for _, alert := range alerts {
			key := deliveryKey{
				fingerprint: alert.Fingerprint,
				startsAt:    alert.StartsAt,
				status:      alert.Status,
				chatID:      chatID,
			}
			entry, reserved, err := t.reserve(ctx, key)
			if err != nil {
				reserveErr = err
				break
			}
			if reserved {
				reservations = append(reservations, reservation{key: key, entry: entry, alert: alert})
			}
		}
		if reserveErr != nil {
			for _, reserved := range reservations {
				t.finish(reserved.key, reserved.entry, false)
			}
			if recordAlertMetrics {
				t.metrics.Failed.WithLabelValues(ChannelTelegram).Add(float64(len(alerts)))
				t.metrics.DeliveryFailures.WithLabelValues(
					ChannelTelegram,
					telegramFailureReason(reserveErr),
				).Add(float64(len(alerts)))
			}
			failed = append(failed, chatID)
			lastErr = reserveErr
			continue
		}
		if len(reservations) == 0 {
			continue
		}

		if recordAlertMetrics {
			t.metrics.LastAttemptAt.WithLabelValues(ChannelTelegram).Set(float64(t.now().Unix()))
		}
		pending := make([]Alert, len(reservations))
		for i := range reservations {
			pending[i] = reservations[i].alert
		}
		if err := t.send(ctx, chatID, groupedMessage(pending), silent); err != nil {
			for _, reserved := range reservations {
				t.finish(reserved.key, reserved.entry, false)
			}
			if recordAlertMetrics {
				t.metrics.Failed.WithLabelValues(ChannelTelegram).Add(float64(len(reservations)))
				t.metrics.DeliveryFailures.WithLabelValues(
					ChannelTelegram,
					telegramFailureReason(err),
				).Add(float64(len(reservations)))
			}
			failed = append(failed, chatID)
			lastErr = err
			continue
		}

		for _, reserved := range reservations {
			t.finish(reserved.key, reserved.entry, true)
		}
		if recordAlertMetrics {
			t.metrics.Delivered.WithLabelValues(ChannelTelegram).Add(float64(len(reservations)))
			t.metrics.LastSuccessAt.WithLabelValues(ChannelTelegram).Set(float64(t.now().Unix()))
		}
	}

	if len(failed) > 0 {
		// The count is reported, never the chat IDs: those are private
		// destinations and this error may be logged.
		return fmt.Errorf("telegram delivery failed for %d of %d chats: %w",
			len(failed), len(t.cfg.AllowedChatIDs), lastErr)
	}
	return nil
}

func groupedMessage(alerts []Alert) string {
	if len(alerts) == 1 {
		return alerts[0].Message()
	}

	var b strings.Builder
	switch {
	case allNeutralResolutions(alerts):
		fmt.Fprintf(&b, "ℹ️ %d Mithril state updates", len(alerts))
	case allStatus(alerts, "resolved") && allInformational(alerts):
		fmt.Fprintf(&b, "ℹ️ %d Mithril updates ended", len(alerts))
	case allStatus(alerts, "resolved"):
		fmt.Fprintf(&b, "ℹ️ %d Mithril alerts ended", len(alerts))
	case anyStatus(alerts, "resolved") && allInformational(alerts):
		fmt.Fprintf(&b, "ℹ️ %d Mithril updates", len(alerts))
	case anyStatus(alerts, "resolved"):
		fmt.Fprintf(&b, "⚠️ %d Mithril alert updates", len(alerts))
	case allInformational(alerts):
		fmt.Fprintf(&b, "ℹ️ %d Mithril updates", len(alerts))
	case anyInformational(alerts):
		fmt.Fprintf(&b, "⚠️ %d Mithril alerts and updates", len(alerts))
	default:
		fmt.Fprintf(&b, "🚨 %d Mithril alerts", len(alerts))
	}
	shown := len(alerts)
	if shown > maxGroupEntries {
		shown = maxGroupEntries
	}
	for _, alert := range alerts[:shown] {
		b.WriteString("\n• ")
		b.WriteString(boundRunes(compactAlert(alert), maxGroupEntryRunes))
	}
	if omitted := len(alerts) - shown; omitted > 0 {
		fmt.Fprintf(&b, "\n• +%d more", omitted)
	}
	return boundMessage(b.String())
}

func compactAlert(alert Alert) string {
	switch alert.Status {
	case "resolved":
		return "Ended: " + humanAlertName(alert.Labels["alertname"])
	case "firing":
		return alert.summary()
	default:
		return "Status changed: " + alert.summary()
	}
}

func allInformational(alerts []Alert) bool {
	for _, alert := range alerts {
		if !alert.informational() {
			return false
		}
	}
	return len(alerts) > 0
}

func anyInformational(alerts []Alert) bool {
	for _, alert := range alerts {
		if alert.informational() {
			return true
		}
	}
	return false
}

func allStatus(alerts []Alert, status string) bool {
	for _, alert := range alerts {
		if alert.Status != status {
			return false
		}
	}
	return len(alerts) > 0
}

func anyStatus(alerts []Alert, status string) bool {
	for _, alert := range alerts {
		if alert.Status == status {
			return true
		}
	}
	return false
}

func allNeutralResolutions(alerts []Alert) bool {
	for _, alert := range alerts {
		if !alert.neutralResolution() {
			return false
		}
	}
	return len(alerts) > 0
}

func boundRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
}

func (t *Telegram) reserve(ctx context.Context, key deliveryKey) (*deliveryEntry, bool, error) {
	for {
		now := t.now()
		t.mu.Lock()
		t.pruneLocked(now)
		entry := t.deliveries[key]
		if entry == nil {
			if !t.makeRoomLocked() {
				t.mu.Unlock()
				return nil, false, telegramError(
					FailureOverloaded,
					"telegram delivery deduplication capacity reached",
				)
			}
			entry = &deliveryEntry{inFlight: make(chan struct{})}
			t.deliveries[key] = entry
			t.mu.Unlock()
			return entry, true, nil
		}
		if entry.inFlight == nil {
			t.mu.Unlock()
			return nil, false, nil
		}
		done := entry.inFlight
		t.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-done:
		}
	}
}

func (t *Telegram) finish(key deliveryKey, entry *deliveryEntry, delivered bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.deliveries[key] != entry {
		return
	}
	done := entry.inFlight
	if delivered {
		entry.deliveredAt = t.now()
		entry.inFlight = nil
	} else {
		delete(t.deliveries, key)
	}
	close(done)
}

func (t *Telegram) pruneLocked(now time.Time) {
	for key, entry := range t.deliveries {
		if entry.inFlight == nil && now.Sub(entry.deliveredAt) >= t.dedupTTL {
			delete(t.deliveries, key)
		}
	}
}

func (t *Telegram) makeRoomLocked() bool {
	if len(t.deliveries) < t.maxDedupEntries {
		return true
	}
	var (
		oldestKey deliveryKey
		oldest    *deliveryEntry
	)
	for key, entry := range t.deliveries {
		if entry.inFlight != nil {
			continue
		}
		if oldest == nil || entry.deliveredAt.Before(oldest.deliveredAt) {
			oldestKey, oldest = key, entry
		}
	}
	if oldest == nil {
		return false
	}
	delete(t.deliveries, oldestKey)
	t.metrics.DedupEvictions.Inc()
	return true
}

// checkRoute proves the token, chat, and network path are usable without
// posting anything. The routine probe uses this so route health stays
// observable without filling the operator's chat with health notices.
func (t *Telegram) checkRoute(ctx context.Context, chatID int64) error {
	ctx, cancel := context.WithTimeout(ctx, t.cfg.SendTimeout())
	defer cancel()

	body, err := json.Marshal(map[string]any{"chat_id": chatID})
	if err != nil {
		return telegramError(FailureInternal, "telegram request could not be encoded")
	}

	// The token is in the path, so a URL must never reach an error or a log.
	url := fmt.Sprintf("%s/bot%s/getChat", t.cfg.APIBase(), t.cfg.BotToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return telegramError(FailureInternal, "telegram request could not be created")
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return telegramError(FailureTimeout, "telegram request timed out")
		}
		return telegramError(FailureNetwork, "telegram request failed")
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTelegramResponseBytes+1))
	if err != nil {
		return telegramError(FailureNetwork, "telegram response could not be read")
	}
	if len(raw) > maxTelegramResponseBytes {
		return telegramError(FailureMalformedAck, "telegram response was too large")
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return telegramError(FailureRateLimited, "telegram rate limited the request")
	}
	if resp.StatusCode != http.StatusOK {
		return telegramError(FailureRejected, fmt.Sprintf("telegram returned HTTP %d", resp.StatusCode))
	}
	var ack struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(raw, &ack); err != nil {
		return telegramError(FailureMalformedAck, "telegram response was not valid JSON")
	}
	if !ack.OK {
		return telegramError(FailureRejected, "telegram did not acknowledge the route check")
	}
	return nil
}

// send performs one bounded sendMessage call.
func (t *Telegram) send(ctx context.Context, chatID int64, text string, silent bool) error {
	ctx, cancel := context.WithTimeout(ctx, t.cfg.SendTimeout())
	defer cancel()

	body, err := json.Marshal(map[string]any{
		"chat_id":                  chatID,
		"text":                     text,
		"disable_web_page_preview": true,
		"disable_notification":     silent,
	})
	if err != nil {
		return telegramError(FailureInternal, "telegram request could not be encoded")
	}

	// The token is in the path, so a URL must never reach an error or a log.
	url := fmt.Sprintf("%s/bot%s/sendMessage", t.cfg.APIBase(), t.cfg.BotToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return telegramError(FailureInternal, "telegram request could not be created")
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		// net/http errors embed the full URL, which contains the bot token.
		if ctx.Err() != nil {
			return telegramError(FailureTimeout, "telegram request timed out")
		}
		return telegramError(FailureNetwork, "telegram request failed")
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTelegramResponseBytes+1))
	if err != nil {
		return telegramError(FailureNetwork, "telegram response could not be read")
	}
	if len(raw) > maxTelegramResponseBytes {
		return telegramError(FailureMalformedAck, "telegram response was too large")
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return telegramError(FailureRateLimited, "telegram rate limited the request")
	}
	if resp.StatusCode != http.StatusOK {
		return telegramError(FailureRejected, fmt.Sprintf("telegram returned HTTP %d", resp.StatusCode))
	}

	// Telegram signals application-level failure inside a 200 response, so the
	// status code alone is not acknowledgement.
	var ack struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(raw, &ack); err != nil {
		return telegramError(FailureMalformedAck, "telegram response was not valid JSON")
	}
	if !ack.OK {
		return telegramError(FailureRejected, "telegram did not acknowledge the message")
	}
	return nil
}
