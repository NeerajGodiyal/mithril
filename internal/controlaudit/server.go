package controlaudit

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	eventPath                     = "/v1/events"
	summaryPath                   = "/v1/summary"
	receiverMaxConnections        = 16
	receiverMaxConcurrentRequests = 4
	receiverRequestsPerSecond     = 8
	receiverRequestBurst          = 16
	receiverOverloadRetrySeconds  = "1"
	receiverOverloadError         = "receiver_overloaded"
)

// PublicKeyPin is the SHA-256 digest of a certificate's DER
// SubjectPublicKeyInfo. Pinning the key permits deliberate certificate renewal
// without silently changing the peer identity.
type PublicKeyPin [sha256.Size]byte

// ParsePublicKeyPin parses a lowercase hexadecimal SPKI pin.
func ParsePublicKeyPin(value string) (PublicKeyPin, error) {
	var pin PublicKeyPin
	if len(value) != hex.EncodedLen(len(pin)) || strings.ToLower(value) != value {
		return pin, errors.New("public-key pin must be 64 lowercase hexadecimal characters")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return pin, errors.New("public-key pin is invalid")
	}
	copy(pin[:], decoded)
	return pin, nil
}

// PinCertificate derives the pin used by receiver and client configurations.
func PinCertificate(cert *x509.Certificate) (PublicKeyPin, error) {
	if cert == nil || len(cert.RawSubjectPublicKeyInfo) == 0 {
		return PublicKeyPin{}, errors.New("certificate has no public-key identity")
	}
	return sha256.Sum256(cert.RawSubjectPublicKeyInfo), nil
}

// ReceiverConfig binds one private store and every allowed client key to one
// control target. Certificate private keys remain in tls.Certificate and are
// never serialized by this package.
type ReceiverConfig struct {
	StorePath         string
	Certificate       tls.Certificate
	ClientCAs         *x509.CertPool
	AllowedClientPins []PublicKeyPin
	ExpectedTargetID  string
	ExpectedUnit      string
	ExpectedScope     string
	StoreLimits       StoreLimits
}

// Receiver is an mTLS-only append endpoint. It exposes no action or shell
// endpoint.
type Receiver struct {
	server        *http.Server
	store         *Store
	policy        receiverPolicy
	requestSlots  chan struct{}
	requestTokens *requestTokenBucket
}

// NewReceiver constructs the production receiver and is also the test seam for
// injecting an independent approval verifier and pinned TLS identities.
func NewReceiver(config ReceiverConfig, verifier ApprovalVerifier) (*Receiver, error) {
	tlsConfig, err := receiverTLSConfig(config)
	if err != nil {
		return nil, err
	}
	policy, err := newReceiverPolicy(config)
	if err != nil {
		return nil, err
	}
	if verifier == nil {
		return nil, ErrApprovalVerifierRequired
	}
	store, err := OpenStoreWithLimits(
		config.StorePath,
		policyApprovalVerifier{
			policy:   policy,
			delegate: verifier,
		},
		config.StoreLimits,
	)
	if err != nil {
		return nil, err
	}
	receiver := &Receiver{
		store:        store,
		policy:       policy,
		requestSlots: make(chan struct{}, receiverMaxConcurrentRequests),
		requestTokens: newRequestTokenBucket(
			receiverRequestsPerSecond,
			receiverRequestBurst,
			time.Now,
		),
	}
	receiver.server = &http.Server{
		Handler:           receiver,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	return receiver, nil
}

type receiverPolicy struct {
	targetID string
	unit     string
	scope    string
}

func newReceiverPolicy(config ReceiverConfig) (receiverPolicy, error) {
	if !identifierPattern.MatchString(config.ExpectedTargetID) {
		return receiverPolicy{}, errors.New("receiver target ID is invalid")
	}
	if !unitPattern.MatchString(config.ExpectedUnit) {
		return receiverPolicy{}, errors.New("receiver systemd unit is invalid")
	}
	if config.ExpectedScope != "system" && config.ExpectedScope != "user" {
		return receiverPolicy{}, errors.New("receiver systemd scope is invalid")
	}
	return receiverPolicy{
		targetID: config.ExpectedTargetID,
		unit:     config.ExpectedUnit,
		scope:    config.ExpectedScope,
	}, nil
}

func (policy receiverPolicy) matches(event Event) bool {
	return event.TargetID == policy.targetID &&
		event.Unit == policy.unit &&
		event.Scope == policy.scope
}

type policyApprovalVerifier struct {
	policy   receiverPolicy
	delegate ApprovalVerifier
}

func (verifier policyApprovalVerifier) VerifyApproval(
	ctx context.Context,
	event Event,
) (ApprovalBinding, error) {
	if !verifier.policy.matches(event) {
		return ApprovalBinding{}, errors.New("event does not match receiver identity policy")
	}
	return verifier.delegate.VerifyApproval(ctx, event)
}

func (verifier policyApprovalVerifier) VerifyStateTransition(
	ctx context.Context,
	previous Event,
	next Event,
) error {
	return verifier.delegate.VerifyStateTransition(ctx, previous, next)
}

func receiverTLSConfig(config ReceiverConfig) (*tls.Config, error) {
	if len(config.Certificate.Certificate) == 0 || config.Certificate.PrivateKey == nil {
		return nil, errors.New("receiver TLS certificate is incomplete")
	}
	if config.ClientCAs == nil {
		return nil, errors.New("receiver client CA pool is required")
	}
	allowed := make(map[PublicKeyPin]struct{}, len(config.AllowedClientPins))
	for _, pin := range config.AllowedClientPins {
		if pin == (PublicKeyPin{}) {
			return nil, errors.New("receiver client public-key pin is empty")
		}
		allowed[pin] = struct{}{}
	}
	if len(allowed) == 0 {
		return nil, errors.New("receiver requires at least one client public-key pin")
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{config.Certificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    config.ClientCAs,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
				return errors.New("client certificate was not verified")
			}
			pin, err := PinCertificate(state.PeerCertificates[0])
			if err != nil {
				return err
			}
			if _, ok := allowed[pin]; !ok {
				return errors.New("client public-key identity is not allowed")
			}
			return nil
		},
	}, nil
}

// Serve runs the receiver over the configured mutual-TLS boundary.
func (receiver *Receiver) Serve(listener net.Listener) error {
	if listener == nil {
		return errors.New("audit receiver listener is nil")
	}
	return receiver.server.ServeTLS(
		newConnectionLimitListener(listener, receiverMaxConnections),
		"",
		"",
	)
}

// Shutdown stops new requests, waits for active requests, then closes the
// durable store.
func (receiver *Receiver) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("audit receiver shutdown context is nil")
	}
	if err := receiver.server.Shutdown(ctx); err != nil {
		return err
	}
	return receiver.store.Close()
}

// Close immediately closes the listener/connections and durable store.
func (receiver *Receiver) Close() error {
	return errors.Join(receiver.server.Close(), receiver.store.Close())
}

// Summary verifies and returns the receiver's durable chain prefix.
func (receiver *Receiver) Summary() (Summary, error) {
	return receiver.store.Summary()
}

func (receiver *Receiver) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	if request.TLS == nil || len(request.TLS.VerifiedChains) == 0 {
		writeServerError(writer, http.StatusUnauthorized, "mutual_tls_required")
		return
	}
	if !receiver.admitRequest() {
		writer.Header().Set("Retry-After", receiverOverloadRetrySeconds)
		writeServerError(writer, http.StatusTooManyRequests, receiverOverloadError)
		return
	}
	defer receiver.releaseRequest()
	if request.URL.RawPath != "" || request.URL.RawQuery != "" ||
		request.URL.ForceQuery {
		writeServerError(writer, http.StatusNotFound, "not_found")
		return
	}
	switch request.URL.Path {
	case summaryPath:
		receiver.serveSummary(writer, request)
		return
	case eventPath:
	default:
		writeServerError(writer, http.StatusNotFound, "not_found")
		return
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeServerError(writer, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || request.Header.Get("Content-Encoding") != "" {
		writeServerError(writer, http.StatusUnsupportedMediaType, "unsupported_content")
		return
	}
	if request.ContentLength > MaxEventBytes {
		writeServerError(writer, http.StatusRequestEntityTooLarge, "event_too_large")
		return
	}
	body := http.MaxBytesReader(writer, request.Body, MaxEventBytes)
	encoded, err := io.ReadAll(body)
	if err != nil {
		writeServerError(writer, http.StatusRequestEntityTooLarge, "event_too_large")
		return
	}
	event, err := ParseEvent(encoded)
	if err != nil {
		writeServerError(writer, http.StatusUnprocessableEntity, "invalid_event")
		return
	}
	if !receiver.policy.matches(event) {
		writeServerError(writer, http.StatusUnprocessableEntity, "identity_policy_mismatch")
		return
	}
	receipt, duplicate, err := receiver.store.Append(request.Context(), event)
	if err != nil {
		switch {
		case errors.Is(err, ErrConflictingDuplicate),
			errors.Is(err, ErrChainMismatch),
			errors.Is(err, ErrSequenceMismatch):
			writeServerError(writer, http.StatusConflict, "audit_conflict")
		case errors.Is(err, ErrApprovalRejected):
			writeServerError(writer, http.StatusUnprocessableEntity, "approval_rejected")
		case errors.Is(err, ErrStoreUncertain), errors.Is(err, ErrStoreClosed):
			writeServerError(writer, http.StatusServiceUnavailable, "store_unavailable")
		case errors.Is(err, ErrStoreLimit):
			writeServerError(writer, http.StatusInsufficientStorage, "store_full")
		default:
			writeServerError(writer, http.StatusInternalServerError, "internal_error")
		}
		return
	}
	status := http.StatusCreated
	if duplicate {
		status = http.StatusOK
	}
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(receipt)
}

func (receiver *Receiver) admitRequest() bool {
	select {
	case receiver.requestSlots <- struct{}{}:
	default:
		return false
	}
	if receiver.requestTokens.allow() {
		return true
	}
	receiver.releaseRequest()
	return false
}

func (receiver *Receiver) releaseRequest() {
	<-receiver.requestSlots
}

type requestTokenBucket struct {
	mu       sync.Mutex
	now      func() time.Time
	last     time.Time
	tokens   float64
	rate     float64
	capacity float64
}

func newRequestTokenBucket(rate, capacity int, now func() time.Time) *requestTokenBucket {
	current := now()
	return &requestTokenBucket{
		now:      now,
		last:     current,
		tokens:   float64(capacity),
		rate:     float64(rate),
		capacity: float64(capacity),
	}
}

func (bucket *requestTokenBucket) allow() bool {
	bucket.mu.Lock()
	defer bucket.mu.Unlock()

	current := bucket.now()
	if elapsed := current.Sub(bucket.last).Seconds(); elapsed > 0 {
		bucket.tokens += elapsed * bucket.rate
		if bucket.tokens > bucket.capacity {
			bucket.tokens = bucket.capacity
		}
		bucket.last = current
	}
	if bucket.tokens < 1 {
		return false
	}
	bucket.tokens--
	return true
}

type connectionLimitListener struct {
	net.Listener
	slots     chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func newConnectionLimitListener(listener net.Listener, limit int) *connectionLimitListener {
	return &connectionLimitListener{
		Listener: listener,
		slots:    make(chan struct{}, limit),
		done:     make(chan struct{}),
	}
}

func (listener *connectionLimitListener) Accept() (net.Conn, error) {
	select {
	case listener.slots <- struct{}{}:
	case <-listener.done:
		return nil, net.ErrClosed
	}
	connection, err := listener.Listener.Accept()
	if err != nil {
		<-listener.slots
		return nil, err
	}
	return &connectionLimitConnection{
		Conn:    connection,
		release: func() { <-listener.slots },
	}, nil
}

func (listener *connectionLimitListener) Close() error {
	listener.closeOnce.Do(func() {
		close(listener.done)
		listener.closeErr = listener.Listener.Close()
	})
	return listener.closeErr
}

type connectionLimitConnection struct {
	net.Conn
	releaseOnce sync.Once
	release     func()
}

func (connection *connectionLimitConnection) Close() error {
	err := connection.Conn.Close()
	connection.releaseOnce.Do(connection.release)
	return err
}

func (receiver *Receiver) serveSummary(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writeServerError(writer, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if request.ContentLength > 0 || len(request.TransferEncoding) != 0 {
		writeServerError(writer, http.StatusBadRequest, "request_body_not_allowed")
		return
	}
	summary, err := receiver.store.Summary()
	if err != nil {
		writeServerError(writer, http.StatusServiceUnavailable, "store_unavailable")
		return
	}
	encoded, err := marshalSummary(summary)
	if err != nil {
		writeServerError(writer, http.StatusInternalServerError, "internal_error")
		return
	}
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(encoded)
}

func writeServerError(writer http.ResponseWriter, status int, code string) {
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(struct {
		Error string `json:"error"`
	}{Error: code})
}
