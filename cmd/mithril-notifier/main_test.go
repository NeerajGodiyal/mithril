package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/internal/notifier"
	"github.com/prometheus/client_golang/prometheus"
)

type testIssuer struct {
	cert *x509.Certificate
	key  ed25519.PrivateKey
	pem  []byte
}

func makeIssuer(t *testing.T, name string, serial int64) testIssuer {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return testIssuer{
		cert: cert,
		key:  private,
		pem:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
}

func makeLeaf(
	t *testing.T,
	issuer testIssuer,
	name string,
	serial int64,
	usage x509.ExtKeyUsage,
) ([]byte, []byte, tls.Certificate) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}
	if usage == x509.ExtKeyUsageServerAuth {
		template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		template.DNSNames = []string{"localhost"}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, issuer.cert, public, issuer.key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return certPEM, keyPEM, pair
}

func tlsFixture(
	t *testing.T,
) (notifier.Config, *x509.CertPool, tls.Certificate, tls.Certificate) {
	t.Helper()
	dir := trustedTempDir(t)
	ca := makeIssuer(t, "notifier-ca", 1)
	serverPEM, serverKey, _ := makeLeaf(t, ca, "notifier", 2, x509.ExtKeyUsageServerAuth)
	_, _, validClient := makeLeaf(t, ca, "alertmanager", 3, x509.ExtKeyUsageClientAuth)
	otherCA := makeIssuer(t, "other-ca", 4)
	_, _, invalidClient := makeLeaf(t, otherCA, "other-client", 5, x509.ExtKeyUsageClientAuth)

	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cfg := notifier.Config{
		ClientCAFile:   write("ca.pem", ca.pem),
		ServerCertFile: write("server.pem", serverPEM),
		ServerKeyFile:  write("server-key.pem", serverKey),
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca.pem) {
		t.Fatal("test CA could not be loaded")
	}
	return cfg, roots, validClient, invalidClient
}

func TestClientAuthTLSRequiresTrustedClient(t *testing.T) {
	cfg, roots, validClient, invalidClient := tlsFixture(t)
	tlsConfig, err := clientAuthTLS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	handler := notifier.New(
		notifier.Config{BotToken: "1:test", AllowedChatIDs: []int64{1}},
		notifier.NewMetrics(prometheus.NewRegistry()),
	)
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = tlsConfig
	server.StartTLS()
	t.Cleanup(server.Close)

	request := func(certificates []tls.Certificate) (int, error) {
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			RootCAs:      roots,
			Certificates: certificates,
		}}}
		response, err := client.Get(server.URL)
		if err != nil {
			return 0, err
		}
		_ = response.Body.Close()
		return response.StatusCode, nil
	}
	status, err := request([]tls.Certificate{validClient})
	if err != nil {
		t.Fatalf("trusted Alertmanager client was rejected: %v", err)
	}
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("verified client reached the handler with status %d, want 405", status)
	}
	if _, err := request(nil); err == nil {
		t.Fatal("client without a certificate was accepted")
	}
	if _, err := request([]tls.Certificate{invalidClient}); err == nil {
		t.Fatal("client signed by another CA was accepted")
	}
}

func TestClientAuthTLSRejectsUnsafePrivateKeyAndSymlink(t *testing.T) {
	cfg, _, _, _ := tlsFixture(t)
	if err := os.Chmod(cfg.ServerKeyFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := clientAuthTLS(cfg); err == nil {
		t.Fatal("group-readable server key was accepted")
	}

	if err := os.Chmod(cfg.ServerKeyFile, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(trustedTempDir(t), "server-cert-link.pem")
	if err := os.Symlink(cfg.ServerCertFile, link); err != nil {
		t.Fatal(err)
	}
	cfg.ServerCertFile = link
	if _, err := clientAuthTLS(cfg); err == nil {
		t.Fatal("symlinked server certificate was accepted")
	}

	cfg, _, _, _ = tlsFixture(t)
	if err := os.Chmod(cfg.ClientCAFile, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := clientAuthTLS(cfg); err == nil {
		t.Fatal("group-writable client CA was accepted")
	}
}

type failingListener struct{}

func (failingListener) Accept() (net.Conn, error) { return nil, errors.New("accept failed") }
func (failingListener) Close() error              { return nil }
func (failingListener) Addr() net.Addr            { return testAddr("failing") }

type testAddr string

func (a testAddr) Network() string { return "test" }
func (a testAddr) String() string  { return string(a) }

func TestRunServersReturnsUnexpectedListenerFailure(t *testing.T) {
	cfg, _, _, _ := tlsFixture(t)
	tlsConfig, err := clientAuthTLS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	webhookListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	webhook := &http.Server{Handler: http.NewServeMux(), TLSConfig: tlsConfig}
	metrics := &http.Server{Handler: http.NewServeMux()}
	err = runServers(context.Background(), webhook, metrics, webhookListener, failingListener{})
	if err == nil || !strings.Contains(err.Error(), "metrics") {
		t.Fatalf("listener failure returned %v", err)
	}
	if !strings.Contains(err.Error(), "accept failed") {
		t.Fatalf("listener failure lost its cause: %v", err)
	}
}

func TestRunServersStopsCleanlyOnCancellation(t *testing.T) {
	cfg, _, _, _ := tlsFixture(t)
	tlsConfig, err := clientAuthTLS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	webhookListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	metricsListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = webhookListener.Close()
		t.Fatal(err)
	}
	webhook := &http.Server{Handler: http.NewServeMux(), TLSConfig: tlsConfig}
	metrics := &http.Server{Handler: http.NewServeMux()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runServers(ctx, webhook, metrics, webhookListener, metricsListener); err != nil {
		t.Fatalf("clean cancellation returned %v", err)
	}
}

func TestShutdownServersClosesConnectionsAfterDeadline(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	server := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()

	requestDone := make(chan error, 1)
	go func() {
		response, err := http.Get("http://" + listener.Addr().String())
		if response != nil {
			_ = response.Body.Close()
		}
		requestDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("test handler did not start")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = shutdownServers(ctx, server)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expired shutdown returned %v", err)
	}
	select {
	case err := <-requestDone:
		if err == nil {
			t.Fatal("forced close left the active request successful")
		}
	case <-time.After(time.Second):
		t.Fatal("forced close did not release the client")
	}
	select {
	case err := <-serveDone:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("forced close did not stop Serve")
	}
}

func TestWebhookServerHasBoundedHTTPTimeouts(t *testing.T) {
	server := newWebhookServer("127.0.0.1:0", http.NewServeMux(), 30*time.Second)
	if server.ReadHeaderTimeout != 5*time.Second {
		t.Fatalf("ReadHeaderTimeout = %v", server.ReadHeaderTimeout)
	}
	if server.ReadTimeout != 10*time.Second {
		t.Fatalf("ReadTimeout = %v", server.ReadTimeout)
	}
	if server.WriteTimeout != 35*time.Second {
		t.Fatalf("WriteTimeout = %v", server.WriteTimeout)
	}
	if server.IdleTimeout != time.Minute {
		t.Fatalf("IdleTimeout = %v", server.IdleTimeout)
	}
	if server.MaxHeaderBytes != 16<<10 {
		t.Fatalf("MaxHeaderBytes = %d", server.MaxHeaderBytes)
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
