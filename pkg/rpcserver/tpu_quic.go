package rpcserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

const (
	tpuQUICALPN             = "solana-tpu"
	tpuQUICMaxIdleTimeout   = 30 * time.Second
	tpuQUICKeepAlive        = time.Second
	tpuQUICHandshakeTimeout = 3 * time.Second
)

var defaultTPUQUICSender = newTPUQUICSender()

type tpuQUICSender struct {
	mu       sync.Mutex
	conns    map[netip.AddrPort]*quic.Conn
	tlsConf  *tls.Config
	quicConf *quic.Config
}

func newTPUQUICSender() *tpuQUICSender {
	cert, err := newTPUQUICClientCertificate()
	if err != nil {
		panic(fmt.Sprintf("create TPU QUIC client certificate: %v", err))
	}

	return &tpuQUICSender{
		conns: make(map[netip.AddrPort]*quic.Conn),
		tlsConf: &tls.Config{
			Certificates:       []tls.Certificate{cert},
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS13,
			NextProtos:         []string{tpuQUICALPN},
			ClientSessionCache: tls.NewLRUClientSessionCache(128),
		},
		quicConf: &quic.Config{
			HandshakeIdleTimeout: tpuQUICHandshakeTimeout,
			MaxIdleTimeout:       tpuQUICMaxIdleTimeout,
			KeepAlivePeriod:      tpuQUICKeepAlive,
			TokenStore:           quic.NewLRUTokenStore(128, 4),
		},
	}
}

func (sender *tpuQUICSender) Send(ctx context.Context, payload []byte, addr netip.AddrPort) error {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		conn, err := sender.connection(ctx, addr)
		if err != nil {
			lastErr = err
			continue
		}

		if err := sender.sendOnConnection(ctx, conn, payload); err != nil {
			lastErr = err
			sender.dropConnection(addr, conn)
			continue
		}
		return nil
	}
	return lastErr
}

func (sender *tpuQUICSender) connection(ctx context.Context, addr netip.AddrPort) (*quic.Conn, error) {
	sender.mu.Lock()
	if conn := sender.conns[addr]; conn != nil {
		if conn.Context().Err() == nil {
			sender.mu.Unlock()
			return conn, nil
		}
		delete(sender.conns, addr)
	}
	sender.mu.Unlock()

	conn, err := quic.DialAddr(ctx, addr.String(), sender.tlsConfigFor(addr), sender.quicConf)
	if err != nil {
		return nil, err
	}

	sender.mu.Lock()
	defer sender.mu.Unlock()
	if existing := sender.conns[addr]; existing != nil && existing.Context().Err() == nil {
		_ = conn.CloseWithError(0, "superseded")
		return existing, nil
	}
	sender.conns[addr] = conn
	return conn, nil
}

func (sender *tpuQUICSender) tlsConfigFor(addr netip.AddrPort) *tls.Config {
	conf := sender.tlsConf.Clone()
	conf.ServerName = tpuQUICServerName(addr)
	return conf
}

func (sender *tpuQUICSender) sendOnConnection(ctx context.Context, conn *quic.Conn, payload []byte) error {
	stream, err := conn.OpenUniStreamSync(ctx)
	if err != nil {
		return err
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = stream.SetWriteDeadline(deadline)
	}

	remaining := payload
	for len(remaining) > 0 {
		written, err := stream.Write(remaining)
		if err != nil {
			stream.CancelWrite(0)
			return err
		}
		if written == 0 {
			stream.CancelWrite(0)
			return fmt.Errorf("short QUIC stream write: wrote 0 of %d bytes", len(remaining))
		}
		remaining = remaining[written:]
	}

	if err := stream.Close(); err != nil {
		return err
	}
	return nil
}

func (sender *tpuQUICSender) dropConnection(addr netip.AddrPort, conn *quic.Conn) {
	sender.mu.Lock()
	if sender.conns[addr] == conn {
		delete(sender.conns, addr)
	}
	sender.mu.Unlock()
	_ = conn.CloseWithError(0, "dropped")
}

func tpuQUICServerName(addr netip.AddrPort) string {
	host := addr.Addr().String()
	if addr.Addr().Is6() {
		host = strings.ReplaceAll(host, ":", "-")
	}
	return host + "." + strconv.Itoa(int(addr.Port())) + ".sol"
}

func newTPUQUICClientCertificate() (tls.Certificate, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "Solana node",
		},
		NotBefore:             time.Unix(0, 0),
		NotAfter:              time.Date(4096, 1, 1, 0, 0, 0, 0, time.UTC),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		DNSNames:              []string{"localhost"},
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return tls.Certificate{}, err
	}

	return tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  priv,
		Leaf:        cert,
	}, nil
}
