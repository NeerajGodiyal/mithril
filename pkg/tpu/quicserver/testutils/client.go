package testutils

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/tpu/quicserver"
	"github.com/quic-go/quic-go"
)

// Client sends TPU QUIC transactions for bench/fuzz runs.
type Client struct {
	tlsCert  tls.Certificate
	tlsConf  *tls.Config
	quicConf *quic.Config
}

// NewClient creates a QUIC client suitable for hammering a TPU server.
func NewClient() (*Client, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate client identity: %w", err)
	}

	cert, err := newClientCertificate(priv)
	if err != nil {
		return nil, err
	}

	return &Client{
		tlsCert: cert,
		tlsConf: &tls.Config{
			Certificates:       []tls.Certificate{cert},
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS13,
			NextProtos:         []string{quicserver.ALPNTPUProtocolID},
		},
		quicConf: &quic.Config{
			HandshakeIdleTimeout: quicserver.DefaultHandshakeTimeout,
			MaxIdleTimeout:       quicserver.QuicMaxTimeout,
			KeepAlivePeriod:      time.Second,
		},
	}, nil
}

func (c *Client) Dial(ctx context.Context, addr string) (*quic.Conn, error) {
	conf := c.tlsConf.Clone()
	conf.ServerName = quicServerName(addr)
	return quic.DialAddr(ctx, addr, conf, c.quicConf)
}

func (c *Client) Send(ctx context.Context, conn *quic.Conn, payload []byte) error {
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
			return fmt.Errorf("short quic stream write: wrote 0 of %d bytes", len(remaining))
		}
		remaining = remaining[written:]
	}
	return stream.Close()
}

func quicServerName(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "localhost.sol"
	}
	if strings.Contains(host, ":") {
		host = strings.ReplaceAll(host, ":", "-")
	}
	return host + "." + port + ".sol"
}

func newClientCertificate(priv ed25519.PrivateKey) (tls.Certificate, error) {
	pub := priv.Public().(ed25519.PublicKey)

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
	leaf, err := x509.ParseCertificate(certDER)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  priv,
		Leaf:        leaf,
	}, nil
}
