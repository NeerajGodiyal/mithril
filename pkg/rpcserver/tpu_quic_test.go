package rpcserver

import (
	"context"
	"crypto/tls"
	"io"
	"net/netip"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTPUQUICSenderSendsOneTransactionPerUniStream(t *testing.T) {
	serverCert, err := newTPUQUICClientCertificate()
	require.NoError(t, err)

	listener, err := quic.ListenAddr(
		"127.0.0.1:0",
		&tls.Config{
			Certificates: []tls.Certificate{serverCert},
			ClientAuth:   tls.RequireAnyClientCert,
			NextProtos:   []string{tpuQUICALPN},
			MinVersion:   tls.VersionTLS13,
		},
		nil,
	)
	require.NoError(t, err)
	defer listener.Close()

	received := make(chan []byte, 1)
	go func() {
		conn, err := listener.Accept(context.Background())
		if err != nil {
			received <- nil
			return
		}
		stream, err := conn.AcceptUniStream(context.Background())
		if err != nil {
			received <- nil
			return
		}
		payload, err := io.ReadAll(stream)
		if err != nil {
			received <- nil
			return
		}
		received <- payload
	}()

	addr := netip.MustParseAddrPort(listener.Addr().String())
	payload := []byte("wire transaction")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	require.NoError(t, newTPUQUICSender().Send(ctx, payload, addr))
	assert.Equal(t, payload, <-received)
}
