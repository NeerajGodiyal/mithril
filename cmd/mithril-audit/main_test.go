package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/internal/controlaudit"
)

type receiverStub struct {
	serveErr    error
	serveWait   chan struct{}
	shutdownErr error
	shutdown    chan struct{}
}

func (stub *receiverStub) Serve(net.Listener) error {
	if stub.serveWait != nil {
		<-stub.serveWait
	}
	return stub.serveErr
}

func (stub *receiverStub) Shutdown(context.Context) error {
	if stub.serveWait != nil {
		close(stub.serveWait)
	}
	if stub.shutdown != nil {
		close(stub.shutdown)
	}
	return stub.shutdownErr
}

func TestPinListParsesOnlyCanonicalPins(t *testing.T) {
	var pins pinList
	value := strings.Repeat("ab", 32)
	if err := pins.Set(value); err != nil {
		t.Fatal(err)
	}
	if len(pins) != 1 || hex.EncodeToString(pins[0][:]) != value {
		t.Fatalf("parsed pins = %x", pins)
	}
	if got := pins.String(); got != "1 configured" {
		t.Fatalf("pin list string = %q", got)
	}
	for _, invalid := range []string{strings.ToUpper(value), value[:63], strings.Repeat("z", 64)} {
		if err := pins.Set(invalid); err == nil {
			t.Errorf("invalid pin %q was accepted", invalid)
		}
	}
}

func TestRunReceiverRequiresContextDrivenShutdown(t *testing.T) {
	t.Run("unexpected clean exit", func(t *testing.T) {
		err := runReceiver(context.Background(), &receiverStub{}, nil)
		if err == nil || !strings.Contains(err.Error(), "before shutdown") {
			t.Fatalf("runReceiver error = %v", err)
		}
	})

	t.Run("unexpected server error", func(t *testing.T) {
		want := errors.New("serve failed")
		if err := runReceiver(context.Background(), &receiverStub{serveErr: want}, nil); !errors.Is(err, want) {
			t.Fatalf("runReceiver error = %v, want %v", err, want)
		}
	})

	t.Run("http shutdown is still premature", func(t *testing.T) {
		err := runReceiver(context.Background(), &receiverStub{serveErr: http.ErrServerClosed}, nil)
		if err == nil || !strings.Contains(err.Error(), "before shutdown") {
			t.Fatalf("runReceiver error = %v", err)
		}
	})

	t.Run("cancel shuts down receiver", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		shutdown := make(chan struct{})
		stub := &receiverStub{
			serveErr:  errors.New("serve failed"),
			serveWait: make(chan struct{}),
			shutdown:  shutdown,
		}
		if err := runReceiver(ctx, stub, nil); err != nil {
			t.Fatalf("runReceiver error = %v", err)
		}
		select {
		case <-shutdown:
		default:
			t.Fatal("receiver was not shut down")
		}
	})

	t.Run("shutdown error propagates", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		want := errors.New("shutdown failed")
		err := runReceiver(ctx, &receiverStub{
			serveErr:    errors.New("serve failed"),
			serveWait:   make(chan struct{}),
			shutdownErr: want,
		}, nil)
		if !errors.Is(err, want) {
			t.Fatalf("runReceiver error = %v, want %v", err, want)
		}
	})
}

func TestConfiguredApprovalVerifierDelegatesBothKeySets(t *testing.T) {
	original := newApprovalVerifier
	t.Cleanup(func() { newApprovalVerifier = original })
	want := &approvalVerifierStub{}
	newApprovalVerifier = func(active, history string) (controlaudit.ApprovalVerifier, error) {
		if active != "/active" || history != "/history" {
			t.Fatalf("approval verifier paths = %q, %q", active, history)
		}
		return want, nil
	}
	verifier, err := configuredApprovalVerifier("/active", "/history")
	if err != nil || verifier != want {
		t.Fatalf("configuredApprovalVerifier = %v, %v", verifier, err)
	}
}

type approvalVerifierStub struct{}

func (*approvalVerifierStub) VerifyApproval(context.Context, controlaudit.Event) (controlaudit.ApprovalBinding, error) {
	return controlaudit.ApprovalBinding{}, nil
}

func (*approvalVerifierStub) VerifyStateTransition(context.Context, controlaudit.Event, controlaudit.Event) error {
	return nil
}

func TestTLSLoadersUseTrustedFiles(t *testing.T) {
	certPEM, keyPEM := testCertificatePEM(t)
	dir := trustedTempDir(t)
	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if certificate, err := loadCertificate(certPath, keyPath); err != nil || len(certificate.Certificate) != 1 {
		t.Fatalf("loadCertificate = %+v, %v", certificate, err)
	}
	if pool, err := loadCertPool(certPath); err != nil || pool == nil {
		t.Fatalf("loadCertPool = %v, %v", pool, err)
	}

	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCertificate(certPath, keyPath); err == nil {
		t.Fatal("group-readable private key was accepted")
	}
	if err := os.WriteFile(certPath, []byte("not a certificate"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCertPool(certPath); err == nil {
		t.Fatal("invalid CA file was accepted")
	}
}

func trustedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func testCertificatePEM(t *testing.T) ([]byte, []byte) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "audit.example"},
		DNSNames:              []string{"audit.example"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}
