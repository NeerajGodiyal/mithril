package alpenglow

import (
	"context"
	"crypto/tls"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/stretchr/testify/require"
)

func TestReceiverDecodesVotorVoteFromQUICUniStream(t *testing.T) {
	observer := NewObserver()
	receiver, err := NewReceiver(ReceiverConfig{
		BindAddr:     "127.0.0.1:0",
		ShredVersion: 0x1234,
		LogInterval:  -1,
	}, observer)
	require.NoError(t, err)
	defer receiver.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)
	go func() {
		runErr <- receiver.Run(ctx)
	}()

	msg := NewVoteMessage(NewFinalizationVote(42), testSignatureSeq(0x55), 7)
	msg.ShredVersion = 0x1234
	payload, err := EncodeMessage(msg)
	require.NoError(t, err)

	conn := sendVotorQUICPayload(t, receiver.Addr().String(), payload)
	defer conn.CloseWithError(0, "test done")

	require.Eventually(t, func() bool {
		return observer.Snapshot().VotesObserved == 1
	}, 2*time.Second, 10*time.Millisecond)

	stats := receiver.Stats()
	require.EqualValues(t, 1, stats.ConnectionsAccepted)
	require.EqualValues(t, 1, stats.StreamsReceived)
	require.EqualValues(t, 1, stats.MessagesDecoded)
	require.EqualValues(t, 1, stats.VotesDecoded)
	require.EqualValues(t, 42, stats.LatestVoteSlot)

	cancel()
	require.NoError(t, receiver.Close())
	require.NoError(t, <-runErr)
}

func TestReceiverRejectsMismatchedShredVersionBeforeObservation(t *testing.T) {
	observer := NewObserver()
	receiver, err := NewReceiver(ReceiverConfig{
		BindAddr:     "127.0.0.1:0",
		ShredVersion: 0x1234,
		LogInterval:  -1,
	}, observer)
	require.NoError(t, err)
	defer receiver.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- receiver.Run(ctx) }()

	msg := NewVoteMessage(NewFinalizationVote(42), testSignatureSeq(0x55), 7)
	msg.ShredVersion = 0x5678
	payload, err := EncodeMessage(msg)
	require.NoError(t, err)
	conn := sendVotorQUICPayload(t, receiver.Addr().String(), payload)
	defer conn.CloseWithError(0, "test done")

	require.Eventually(t, func() bool {
		return receiver.Stats().ShredVersionMismatches == 1
	}, 2*time.Second, 10*time.Millisecond)
	require.Zero(t, observer.Snapshot().VotesObserved)
	require.Zero(t, receiver.Stats().DecodeErrors)

	cancel()
	require.NoError(t, receiver.Close())
	require.NoError(t, <-runErr)
}

func TestReceiverAdmissionRejectsBeforeTrustedObservation(t *testing.T) {
	observer := NewObserver()
	admissions := make(chan Message, 1)
	receiver, err := NewReceiver(ReceiverConfig{
		BindAddr:    "127.0.0.1:0",
		LogInterval: -1,
		AdmitMessage: func(msg Message) (Message, bool) {
			admissions <- msg
			return Message{}, false
		},
	}, observer)
	require.NoError(t, err)
	defer receiver.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- receiver.Run(ctx) }()

	msg := NewCertificateMessage(Certificate{
		Type:      CertificateSkip,
		Slot:      42,
		Signature: testSignatureSeq(0x42),
		Bitmap:    []byte{0, 0, 0},
	})
	payload, err := EncodeMessage(msg)
	require.NoError(t, err)
	conn := sendVotorQUICPayload(t, receiver.Addr().String(), payload)
	defer conn.CloseWithError(0, "test done")

	select {
	case <-admissions:
	case <-time.After(2 * time.Second):
		t.Fatal("admission callback was not invoked")
	}
	require.Eventually(t, func() bool {
		return receiver.Stats().MessagesDecoded == 1
	}, 2*time.Second, 10*time.Millisecond)
	require.Zero(t, observer.Snapshot().CertificatesObserved)

	cancel()
	require.NoError(t, receiver.Close())
	require.NoError(t, <-runErr)
}

func TestReceiverCountsMalformedVotorPayload(t *testing.T) {
	observer := NewObserver()
	receiver, err := NewReceiver(ReceiverConfig{
		BindAddr:    "127.0.0.1:0",
		LogInterval: -1,
	}, observer)
	require.NoError(t, err)
	defer receiver.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)
	go func() {
		runErr <- receiver.Run(ctx)
	}()

	conn := sendVotorQUICPayload(t, receiver.Addr().String(), []byte{0xff})
	defer conn.CloseWithError(0, "test done")

	require.Eventually(t, func() bool {
		return receiver.Stats().DecodeErrors == 1
	}, 2*time.Second, 10*time.Millisecond)
	require.Zero(t, observer.Snapshot().VotesObserved)

	cancel()
	require.NoError(t, receiver.Close())
	require.NoError(t, <-runErr)
}

func sendVotorQUICPayload(t *testing.T, addr string, payload []byte) *quic.Conn {
	t.Helper()

	cert, err := newVotorQUICCertificate(nil)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, err := quic.DialAddr(ctx, addr, &tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{VotorQUICALPN},
	}, nil)
	require.NoError(t, err)

	stream, err := conn.OpenUniStreamSync(ctx)
	require.NoError(t, err)
	_, err = stream.Write(payload)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	return conn
}
