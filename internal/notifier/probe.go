package notifier

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/smtp"
	"time"
)

// SESProbe checks the independent email fallback route.
//
// It records success only on SMTP ACCEPTANCE — the server taking responsibility
// for the message. It does not claim inbox delivery: acceptance is
// what this process can observe, and reporting it as delivery would overstate
// the evidence. Bounce, spam filing and mailbox rules are all invisible here.
type SESProbe struct {
	metrics *Metrics
	send    func(context.Context, string, smtp.Auth, string, []string, []byte) error
	now     func() time.Time

	Addr     string
	Username string
	Password string
	From     string
	CanaryTo string
	Interval time.Duration
	Timeout  time.Duration
}

// NewSESProbe builds a probe. The send function is injectable so tests never
// contact a real SMTP server.
func NewSESProbe(metrics *Metrics) *SESProbe {
	return &SESProbe{
		metrics:  metrics,
		send:     sendSMTP,
		now:      time.Now,
		Interval: DefaultProbeInterval,
		Timeout:  DefaultSendTimeout,
	}
}

// ProbeOnce sends one tagged canary and records the outcome.
func (p *SESProbe) ProbeOnce(ctx context.Context) error {
	if p.Addr == "" ||
		!validSESUsername(p.Username) ||
		p.Password == "" ||
		!validMailbox(p.From) ||
		!validMailbox(p.CanaryTo) {
		return errors.New("SES probe is not configured")
	}
	p.metrics.ProbeAttempts.WithLabelValues(RouteSES).Inc()
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultSendTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// The canary is tagged so a human can recognise it, and carries no alert
	// content: this proves the ROUTE works, not what is flowing through it.
	msg := []byte("From: " + p.From + "\r\n" +
		"To: " + p.CanaryTo + "\r\n" +
		"Subject: [mithril-canary] alert delivery route probe\r\n\r\n" +
		"Automated canary confirming the SES fallback route accepts mail.\r\n" +
		"SMTP acceptance only; this does not assert inbox delivery.\r\n")

	auth := smtp.PlainAuth("", p.Username, p.Password, hostOnly(p.Addr))
	if err := p.send(ctx, p.Addr, auth, p.From, []string{p.CanaryTo}, msg); err != nil {
		p.metrics.ProbeFailures.WithLabelValues(RouteSES).Inc()
		p.metrics.ProbeSuccess.WithLabelValues(RouteSES).Set(0)
		// The SMTP error can echo credentials supplied during AUTH.
		return errors.New("SES canary was not accepted")
	}
	p.metrics.ProbeSuccess.WithLabelValues(RouteSES).Set(1)
	p.metrics.ProbeLastSuccessAt.WithLabelValues(RouteSES).Set(float64(p.now().Unix()))
	return nil
}

// Run probes on an interval until ctx is cancelled.
func (p *SESProbe) Run(ctx context.Context) {
	// time.NewTicker panics on a non-positive interval, and this runs in a
	// goroutine where a panic is unrecoverable and takes the whole notifier
	// with it — turning "probing is off" into "no alert is ever delivered".
	// Guard here too because probes can be run outside the command startup path.
	if p.Interval <= 0 {
		return
	}
	ticker := time.NewTicker(p.Interval)
	defer ticker.Stop()
	if ctx.Err() == nil {
		_ = p.ProbeOnce(ctx)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = p.ProbeOnce(ctx)
		}
	}
}

// TelegramProbe verifies the primary route without emitting a real alert.
type TelegramProbe struct {
	telegram *Telegram
	Interval time.Duration
	now      func() time.Time
	posted   bool
}

// NewTelegramProbe builds a probe over an existing deliverer.
func NewTelegramProbe(t *Telegram) *TelegramProbe {
	return &TelegramProbe{telegram: t, Interval: DefaultProbeInterval, now: time.Now}
}

// ProbeOnce records Telegram route health. The first probe of a process posts a
// real message so the whole delivery path is proved end to end; later probes
// only verify the token, chat, and network so an hourly health check does not
// fill the operator's chat. Both paths drive the same metrics, so the
// staleness and health rules are unaffected.
func (p *TelegramProbe) ProbeOnce(ctx context.Context) error {
	now := p.now()
	p.telegram.metrics.ProbeAttempts.WithLabelValues(RouteTelegram).Inc()

	var err error
	if p.posted {
		err = p.checkRoutes(ctx)
	} else {
		err = p.telegram.probe(ctx, Alert{
			Status:      "firing",
			Fingerprint: "canary-" + now.UTC().Format(time.RFC3339Nano),
			StartsAt:    now.UTC().Format(time.RFC3339Nano),
			Labels: map[string]string{
				"alertname": "MithrilNotifierCanary",
				"severity":  "info",
			},
			Annotations: map[string]string{
				"summary": "Automatic health check. No action needed.",
			},
		})
	}
	if err != nil {
		p.telegram.metrics.ProbeFailures.WithLabelValues(RouteTelegram).Inc()
		p.telegram.metrics.ProbeSuccess.WithLabelValues(RouteTelegram).Set(0)
		return err
	}
	p.posted = true
	p.telegram.metrics.ProbeSuccess.WithLabelValues(RouteTelegram).Set(1)
	p.telegram.metrics.ProbeLastSuccessAt.WithLabelValues(RouteTelegram).Set(float64(now.Unix()))
	return nil
}

// checkRoutes verifies every allowlisted chat without posting. One reachable
// chat is not enough: a route that has silently lost a destination is exactly
// what this probe exists to catch.
func (p *TelegramProbe) checkRoutes(ctx context.Context) error {
	for _, chatID := range p.telegram.cfg.AllowedChatIDs {
		if err := p.telegram.checkRoute(ctx, chatID); err != nil {
			return err
		}
	}
	return nil
}

// Run probes until ctx is cancelled. Each canary carries a distinct
// fingerprint so it never collides with a real alert's dedup entry.
func (p *TelegramProbe) Run(ctx context.Context) {
	// Same guard as SESProbe.Run: the disabled interval is zero and NewTicker
	// panics on a non-positive interval.
	if p.Interval <= 0 {
		return
	}
	ticker := time.NewTicker(p.Interval)
	defer ticker.Stop()
	if ctx.Err() == nil {
		_ = p.ProbeOnce(ctx)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = p.ProbeOnce(ctx)
		}
	}
}

func hostOnly(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err == nil {
		return host
	}
	return addr
}

func sendSMTP(
	ctx context.Context,
	addr string,
	auth smtp.Auth,
	from string,
	to []string,
	msg []byte,
) error {
	return sendSMTPWithRootCAs(ctx, addr, auth, from, to, msg, nil)
}

func sendSMTPWithRootCAs(
	ctx context.Context,
	addr string,
	auth smtp.Auth,
	from string,
	to []string,
	msg []byte,
	rootCAs *x509.CertPool,
) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return err
		}
	}
	stop := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Now())
	})
	defer stop()

	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer client.Close()
	if ok, _ := client.Extension("STARTTLS"); !ok {
		return errors.New("SMTP server does not offer STARTTLS")
	}
	if err := client.StartTLS(&tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: host,
		RootCAs:    rootCAs,
	}); err != nil {
		return err
	}
	if auth != nil {
		if err := client.Auth(auth); err != nil {
			return err
		}
	}
	if err := client.Mail(from); err != nil {
		return err
	}
	for _, recipient := range to {
		if err := client.Rcpt(recipient); err != nil {
			return err
		}
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := writer.Write(msg); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return client.Quit()
}
