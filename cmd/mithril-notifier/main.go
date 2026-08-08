// Command mithril-notifier delivers Alertmanager alerts to Telegram.
//
// It is DELIVERY, not authorization: it decides nothing about whether an alert
// is real, and a delivery failure never resolves or deletes the Alertmanager
// record — Alertmanager owns that state and will retry.
//
// It runs on the operations host alongside Alertmanager. Its bot token, chat
// allowlist and TLS material come from permission-restricted runtime files
// readable by its service account; none of it is in this repository.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/Overclock-Validator/mithril/internal/notifier"
	"github.com/Overclock-Validator/mithril/internal/safefile"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:9094", "address to receive Alertmanager webhooks on")
	metricsAddr := flag.String("metrics-listen", "127.0.0.1:9096", "address to expose /metrics on")
	configPath := flag.String("config", notifier.DefaultConfigPath, "absolute path to config.toml")
	verifyDeploy := flag.String("verify-deploy-config", "",
		"directory of Alertmanager/Prometheus config to check for unreplaced placeholders, then exit")
	flag.Parse()

	// Intended as an ExecStartPre gate: an unreplaced deadman URL turns into a
	// permanently firing delivery-failure alert rather than a visible error.
	if *verifyDeploy != "" {
		if err := notifier.VerifyDeployConfig(*verifyDeploy); err != nil {
			fmt.Fprintf(os.Stderr, "mithril-notifier: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stdout, "monitoring config contains no unreplaced placeholders")
		return
	}

	cfg, err := notifier.LoadConfig(*configPath)
	if err != nil {
		// Never contains the token; see notifier.LoadConfig.
		fmt.Fprintf(os.Stderr, "mithril-notifier: %v\n", err)
		os.Exit(1)
	}

	registry := prometheus.NewRegistry()
	metrics := notifier.NewMetrics(registry)
	metrics.SetSESConfigured(cfg.SESConfigured())
	handler := notifier.New(cfg, metrics)

	var sesProbe *notifier.SESProbe
	if cfg.SESConfigured() {
		password, err := notifier.LoadSecretFile(cfg.SESPasswordFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mithril-notifier: SES password file is unusable")
			os.Exit(1)
		}
		sesProbe = notifier.NewSESProbe(metrics)
		sesProbe.Addr = cfg.SESAddr
		sesProbe.Username = cfg.SESUsername
		sesProbe.Password = password
		sesProbe.From = cfg.SESFrom
		sesProbe.CanaryTo = cfg.SESCanaryTo
		sesProbe.Interval = cfg.ProbeInterval()
		sesProbe.Timeout = cfg.SendTimeout()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Metrics are served on a separate plain listener: Prometheus scrapes it,
	// and it must not require the Alertmanager client certificate.
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	metricsServer := &http.Server{
		Addr:              *metricsAddr,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    16 << 10,
	}

	mux := http.NewServeMux()
	mux.Handle("/notify", handler)
	server := newWebhookServer(*listen, mux, cfg.WebhookTimeout())

	tlsCfg, err := clientAuthTLS(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mithril-notifier: %v\n", err)
		os.Exit(1)
	}
	server.TLSConfig = tlsCfg

	metricsListener, err := net.Listen("tcp", metricsServer.Addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mithril-notifier: metrics listener could not start")
		os.Exit(1)
	}
	webhookListener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		_ = metricsListener.Close()
		fmt.Fprintln(os.Stderr, "mithril-notifier: webhook listener could not start")
		os.Exit(1)
	}

	var probes sync.WaitGroup
	if !cfg.ProbeDisabled() {
		telegramProbe := handler.TelegramProbe()
		telegramProbe.Interval = cfg.ProbeInterval()
		probes.Add(1)
		go func() {
			defer probes.Done()
			telegramProbe.Run(ctx)
		}()
	}

	// ProbeDisabled applies to both Telegram and SES probes.
	if sesProbe != nil && !cfg.ProbeDisabled() {
		probes.Add(1)
		go func() {
			defer probes.Done()
			sesProbe.Run(ctx)
		}()
	}

	serverErr := runServers(ctx, server, metricsServer, webhookListener, metricsListener)
	stop()
	probes.Wait()
	if serverErr != nil {
		fmt.Fprintf(os.Stderr, "mithril-notifier: %v\n", serverErr)
		os.Exit(1)
	}
}

func newWebhookServer(addr string, handler http.Handler, webhookTimeout time.Duration) *http.Server {
	if webhookTimeout <= 0 {
		webhookTimeout = notifier.DefaultWebhookTimeout
	}
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      webhookTimeout + 5*time.Second,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
}

func runServers(
	ctx context.Context,
	webhookServer, metricsServer *http.Server,
	webhookListener, metricsListener net.Listener,
) error {
	type serverResult struct {
		name string
		err  error
	}
	results := make(chan serverResult, 2)
	go func() {
		results <- serverResult{name: "metrics", err: metricsServer.Serve(metricsListener)}
	}()
	go func() {
		results <- serverResult{name: "webhook", err: webhookServer.ServeTLS(webhookListener, "", "")}
	}()

	var unexpected error
	select {
	case <-ctx.Done():
	case result := <-results:
		if result.err == nil || errors.Is(result.err, http.ErrServerClosed) {
			unexpected = fmt.Errorf("%s server stopped unexpectedly", result.name)
		} else {
			unexpected = fmt.Errorf("%s server stopped unexpectedly: %w", result.name, result.err)
		}
	}

	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return errors.Join(unexpected, shutdownServers(shutdown, webhookServer, metricsServer))
}

func shutdownServers(ctx context.Context, servers ...*http.Server) error {
	results := make(chan error, len(servers))
	for _, server := range servers {
		go func(server *http.Server) {
			results <- server.Shutdown(ctx)
		}(server)
	}

	var shutdownErr error
	for range servers {
		if err := <-results; err != nil {
			shutdownErr = errors.Join(shutdownErr, err)
		}
	}
	if shutdownErr == nil {
		return nil
	}

	for _, server := range servers {
		if err := server.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			shutdownErr = errors.Join(shutdownErr, err)
		}
	}
	return fmt.Errorf("server shutdown failed: %w", shutdownErr)
}

// clientAuthTLS requires a certificate signed by the dedicated, single-purpose
// client CA. Alertmanager's generic webhook computes no body HMAC, so this CA
// is the producer-verifiable boundary.
func clientAuthTLS(cfg notifier.Config) (*tls.Config, error) {
	caPEM, err := readTLSFile(cfg.ClientCAFile, false)
	if err != nil {
		return nil, fmt.Errorf("client CA file is unreadable")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("client CA file contains no usable certificate")
	}
	certPEM, err := readTLSFile(cfg.ServerCertFile, false)
	if err != nil {
		return nil, fmt.Errorf("server certificate or key is unusable")
	}
	keyPEM, err := readTLSFile(cfg.ServerKeyFile, true)
	if err != nil {
		return nil, fmt.Errorf("server certificate or key is unusable")
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("server certificate or key is unusable")
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}, nil
}

const maxTLSFileBytes = 1 << 20

func readTLSFile(path string, private bool) ([]byte, error) {
	forbidden := os.FileMode(0o022)
	if private {
		forbidden = 0o077
	}
	data, err := safefile.ReadTrustedRegular(path, safefile.ReadOptions{
		MaxBytes:               maxTLSFileBytes,
		ForbiddenPerm:          forbidden,
		RejectAncestorSymlinks: true,
	})
	if err != nil {
		return nil, fmt.Errorf("TLS file: %w", err)
	}
	return data, nil
}
