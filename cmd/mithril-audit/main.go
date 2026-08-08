// Command mithril-audit stores an independent copy of service-control events.
//
// It belongs on an independent audit host, not the node or control host.
// It has one append endpoint and no action, shell, query or administration
// endpoint. Runtime TLS material, peer pins, destinations and audit data stay
// outside Git.
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
	"syscall"
	"time"

	"github.com/Overclock-Validator/mithril/internal/controlaudit"
	"github.com/Overclock-Validator/mithril/internal/safefile"
	"github.com/Overclock-Validator/mithril/pkg/mcp"
)

const maxTLSFileBytes = 1 << 20

type pinList []controlaudit.PublicKeyPin

var newApprovalVerifier = mcp.NewControlAuditApprovalVerifier

func (pins *pinList) String() string {
	return fmt.Sprintf("%d configured", len(*pins))
}

func (pins *pinList) Set(value string) error {
	pin, err := controlaudit.ParsePublicKeyPin(value)
	if err != nil {
		return err
	}
	*pins = append(*pins, pin)
	return nil
}

func main() {
	listenAddress := flag.String("listen", "", "explicit address for the mTLS audit receiver")
	storePath := flag.String("store", "", "absolute path to the private audit store")
	serverCertPath := flag.String("server-cert", "", "absolute path to the receiver certificate")
	serverKeyPath := flag.String("server-key", "", "absolute path to the receiver private key")
	clientCAPath := flag.String("client-ca", "", "absolute path to the dedicated client CA")
	approverKeysDir := flag.String(
		"approver-keys-dir",
		"",
		"absolute directory of active approver public keys",
	)
	approverHistoryKeysDir := flag.String(
		"approver-history-keys-dir",
		"",
		"absolute directory of retained historical approver public keys; defaults to --approver-keys-dir",
	)
	targetID := flag.String("target-id", "", "fixed control target ID for this store")
	systemdUnit := flag.String("systemd-unit", "", "fixed systemd service unit for this store")
	systemdScope := flag.String("systemd-scope", "", "fixed systemd scope for this store")
	maxStoreBytes := flag.Uint64(
		"max-store-bytes",
		controlaudit.DefaultMaxStoreBytes,
		"maximum durable audit-store bytes",
	)
	maxStoreRecords := flag.Uint64(
		"max-store-records",
		controlaudit.DefaultMaxStoreRecords,
		"maximum durable audit-store records",
	)
	var clientPins pinList
	flag.Var(&clientPins, "client-pin", "allowed client SPKI SHA-256 pin; repeat for rotation")
	flag.Parse()

	if *listenAddress == "" || *storePath == "" || *serverCertPath == "" ||
		*serverKeyPath == "" || *clientCAPath == "" || *approverKeysDir == "" ||
		*targetID == "" || *systemdUnit == "" || *systemdScope == "" ||
		len(clientPins) == 0 || *maxStoreBytes == 0 || *maxStoreRecords == 0 {
		fmt.Fprintln(os.Stderr, "mithril-audit: listen, store, TLS files, approver keys, target identity, client pin and nonzero store limits are required")
		os.Exit(1)
	}
	historyDir := *approverHistoryKeysDir
	if historyDir == "" {
		historyDir = *approverKeysDir
	}
	verifier, err := configuredApprovalVerifier(
		*approverKeysDir,
		historyDir,
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mithril-audit: approval verification is not configured")
		os.Exit(1)
	}

	certificate, err := loadCertificate(*serverCertPath, *serverKeyPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mithril-audit: server certificate or key is unusable")
		os.Exit(1)
	}
	clientCAs, err := loadCertPool(*clientCAPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mithril-audit: client CA is unusable")
		os.Exit(1)
	}
	receiver, err := controlaudit.NewReceiver(controlaudit.ReceiverConfig{
		StorePath:         *storePath,
		Certificate:       certificate,
		ClientCAs:         clientCAs,
		AllowedClientPins: clientPins,
		ExpectedTargetID:  *targetID,
		ExpectedUnit:      *systemdUnit,
		ExpectedScope:     *systemdScope,
		StoreLimits: controlaudit.StoreLimits{
			MaxBytes:   *maxStoreBytes,
			MaxRecords: *maxStoreRecords,
		},
	}, verifier)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mithril-audit: receiver configuration is unusable")
		os.Exit(1)
	}

	listener, err := net.Listen("tcp", *listenAddress)
	if err != nil {
		_ = receiver.Close()
		fmt.Fprintln(os.Stderr, "mithril-audit: listener could not start")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runReceiver(ctx, receiver, listener); err != nil {
		fmt.Fprintln(os.Stderr, "mithril-audit: receiver stopped unexpectedly")
		os.Exit(1)
	}
}

func configuredApprovalVerifier(
	activePath string,
	historyPath string,
) (controlaudit.ApprovalVerifier, error) {
	return newApprovalVerifier(activePath, historyPath)
}

type receiverRuntime interface {
	Serve(net.Listener) error
	Shutdown(context.Context) error
}

func runReceiver(ctx context.Context, receiver receiverRuntime, listener net.Listener) error {
	result := make(chan error, 1)
	go func() {
		result <- receiver.Serve(listener)
	}()
	select {
	case err := <-result:
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return errors.New("receiver stopped before shutdown")
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return receiver.Shutdown(shutdown)
	}
}

func loadCertificate(certPath, keyPath string) (tls.Certificate, error) {
	certPEM, err := readTLSFile(certPath, false)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM, err := readTLSFile(keyPath, true)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

func loadCertPool(path string) (*x509.CertPool, error) {
	caPEM, err := readTLSFile(path, false)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("CA file contains no certificate")
	}
	return pool, nil
}

func readTLSFile(path string, private bool) ([]byte, error) {
	forbidden := os.FileMode(0o022)
	if private {
		forbidden = 0o077
	}
	return safefile.ReadTrustedRegular(path, safefile.ReadOptions{
		MaxBytes:               maxTLSFileBytes,
		ForbiddenPerm:          forbidden,
		RejectAncestorSymlinks: true,
	})
}
