package notifier

import "github.com/prometheus/client_golang/prometheus"

// Channels are a bounded label set. A chat ID is a private destination and a
// token is a credential, so neither may ever become a label value.
const (
	ChannelTelegram = "telegram"

	RouteTelegram = "primary_telegram"
	RouteSES      = "secondary_ses"

	FailureNetwork      = "network"
	FailureTimeout      = "timeout"
	FailureRateLimited  = "rate_limited"
	FailureRejected     = "rejected"
	FailureMalformedAck = "malformed_ack"
	FailureOverloaded   = "overloaded"
	FailureInternal     = "internal"
)

var allChannels = []string{ChannelTelegram}
var allRoutes = []string{RouteTelegram, RouteSES}
var allFailureReasons = []string{
	FailureNetwork,
	FailureTimeout,
	FailureRateLimited,
	FailureRejected,
	FailureMalformedAck,
	FailureOverloaded,
	FailureInternal,
}

// Metrics separates real Alertmanager webhook delivery from synthetic route
// probes. In particular, SES probe success means SMTP acceptance only; it must
// never look like a fallback alert was delivered.
type Metrics struct {
	LastAttemptAt      *prometheus.GaugeVec
	LastSuccessAt      *prometheus.GaugeVec
	Delivered          *prometheus.CounterVec
	Failed             *prometheus.CounterVec
	DeliveryFailures   *prometheus.CounterVec
	Rejected           *prometheus.CounterVec
	DedupEvictions     prometheus.Counter
	RouteConfigured    *prometheus.GaugeVec
	ProbeAttempts      *prometheus.CounterVec
	ProbeFailures      *prometheus.CounterVec
	ProbeSuccess       *prometheus.GaugeVec
	ProbeLastSuccessAt *prometheus.GaugeVec
}

// NewMetrics registers and initializes every series, so a scrape taken before
// the first alert returns a complete set rather than nothing.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		LastAttemptAt: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mithril_notifier_last_attempt_timestamp_seconds",
			Help: "Unix seconds of the last real Alertmanager alert delivery attempt.",
		}, []string{"channel"}),
		LastSuccessAt: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mithril_notifier_last_success_timestamp_seconds",
			Help: "Unix seconds of the last successful real Alertmanager alert delivery. " +
				"Divergence from the attempt timestamp is what shows delivery failing.",
		}, []string{"channel"}),
		Delivered: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mithril_notifier_delivered_total",
			Help: "Real Alertmanager webhook alerts delivered successfully, by channel.",
		}, []string{"channel"}),
		Failed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mithril_notifier_failed_total",
			Help: "Real Alertmanager webhook alert delivery attempts that failed, by channel.",
		}, []string{"channel"}),
		DeliveryFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mithril_notifier_delivery_failures_total",
			Help: "Real alert delivery failures by channel and bounded reason.",
		}, []string{"channel", "reason"}),
		Rejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mithril_notifier_rejected_total",
			Help: "Inbound webhook requests rejected, by bounded reason. " +
				"Reasons are a fixed enum; no request content becomes a label.",
		}, []string{"reason"}),
		DedupEvictions: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mithril_notifier_dedup_evictions_total",
			Help: "Unexpired completed delivery keys evicted to preserve the deduplication memory bound.",
		}),
		RouteConfigured: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mithril_notification_route_configured",
			Help: "1 when this bounded notification route is configured, 0 otherwise.",
		}, []string{"route"}),
		ProbeAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mithril_notification_probe_attempts_total",
			Help: "Synthetic notification-route canary attempts, by route.",
		}, []string{"route"}),
		ProbeFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mithril_notification_probe_failures_total",
			Help: "Synthetic notification-route canary failures, by route.",
		}, []string{"route"}),
		ProbeSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mithril_notification_probe_success",
			Help: "1 when the last route canary succeeded, 0 otherwise. " +
				"SES success means SMTP acceptance, not inbox or alert delivery.",
		}, []string{"route"}),
		ProbeLastSuccessAt: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mithril_notification_probe_last_success_timestamp_seconds",
			Help: "Unix seconds of the last successful delivery-route canary.",
		}, []string{"route"}),
	}
	for _, c := range []prometheus.Collector{
		m.LastAttemptAt, m.LastSuccessAt, m.Delivered, m.Failed, m.DeliveryFailures, m.Rejected,
		m.DedupEvictions, m.RouteConfigured, m.ProbeAttempts, m.ProbeFailures,
		m.ProbeSuccess, m.ProbeLastSuccessAt,
	} {
		reg.MustRegister(c)
	}
	for _, ch := range allChannels {
		m.LastAttemptAt.WithLabelValues(ch).Set(0)
		m.LastSuccessAt.WithLabelValues(ch).Set(0)
		m.Delivered.WithLabelValues(ch).Add(0)
		m.Failed.WithLabelValues(ch).Add(0)
		for _, reason := range allFailureReasons {
			m.DeliveryFailures.WithLabelValues(ch, reason).Add(0)
		}
	}
	for _, reason := range allRejectReasons {
		m.Rejected.WithLabelValues(reason).Add(0)
	}
	for _, route := range allRoutes {
		m.RouteConfigured.WithLabelValues(route).Set(0)
		m.ProbeAttempts.WithLabelValues(route).Add(0)
		m.ProbeFailures.WithLabelValues(route).Add(0)
		m.ProbeSuccess.WithLabelValues(route).Set(0)
		m.ProbeLastSuccessAt.WithLabelValues(route).Set(0)
	}
	m.RouteConfigured.WithLabelValues(RouteTelegram).Set(1)
	return m
}

// SetSESConfigured records whether the complete optional SES canary route is
// present. The fixed method avoids introducing configuration values as labels.
func (m *Metrics) SetSESConfigured(configured bool) {
	value := 0.0
	if configured {
		value = 1
	}
	m.RouteConfigured.WithLabelValues(RouteSES).Set(value)
}

// Reject reasons are a bounded enum. Echoing a parse error or request body into
// a label would put attacker-controlled text into the metric namespace.
const (
	RejectBadMethod     = "bad_method"
	RejectBadSchema     = "bad_schema"
	RejectTooLarge      = "too_large"
	RejectTooManyAlerts = "too_many_alerts"
	RejectNoClientCert  = "no_client_cert"
	RejectOverloaded    = "overloaded"
)

var allRejectReasons = []string{
	RejectBadMethod, RejectBadSchema, RejectTooLarge,
	RejectTooManyAlerts, RejectNoClientCert, RejectOverloaded,
}
