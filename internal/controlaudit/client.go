package controlaudit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	maxReceiptBytes       = 2048
	maxSummaryBytes       = 1024
	defaultRequestTimeout = 15 * time.Second
)

var ErrPermanentRejection = errors.New("audit receiver permanently rejected the event")

// ClientConfig fixes both halves of the node-host transport identity.
type ClientConfig struct {
	Endpoint       string
	Certificate    tls.Certificate
	ServerRoots    *x509.CertPool
	ServerName     string
	ServerKeyPin   PublicKeyPin
	ConnectTimeout time.Duration
	RequestTimeout time.Duration
}

// Client sends canonical events to one pinned off-host receiver.
type Client struct {
	eventEndpoint   string
	summaryEndpoint string
	http            *http.Client
}

// NewClient constructs a proxy-free, redirect-free mutual-TLS client.
func NewClient(config ClientConfig) (*Client, error) {
	endpoint, err := auditEndpoint(config.Endpoint)
	if err != nil {
		return nil, err
	}
	tlsConfig, err := clientTLSConfig(config, endpoint)
	if err != nil {
		return nil, err
	}
	connectTimeout := config.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = 5 * time.Second
	}
	requestTimeout := config.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = defaultRequestTimeout
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          2,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		TLSClientConfig:       tlsConfig,
	}
	return &Client{
		eventEndpoint: endpoint.String(),
		summaryEndpoint: (&url.URL{
			Scheme: endpoint.Scheme,
			Host:   endpoint.Host,
			Path:   summaryPath,
		}).String(),
		http: &http.Client{
			Transport: transport,
			Timeout:   requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func auditEndpoint(raw string) (*url.URL, error) {
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery ||
		endpoint.Fragment != "" || endpoint.RawPath != "" {
		return nil, errors.New("audit endpoint must be one HTTPS origin")
	}
	if endpoint.Path != "" && endpoint.Path != "/" {
		return nil, errors.New("audit endpoint must not contain a path")
	}
	endpoint.Path = eventPath
	return endpoint, nil
}

func clientTLSConfig(config ClientConfig, endpoint *url.URL) (*tls.Config, error) {
	if len(config.Certificate.Certificate) == 0 || config.Certificate.PrivateKey == nil {
		return nil, errors.New("audit client TLS certificate is incomplete")
	}
	if config.ServerRoots == nil {
		return nil, errors.New("audit server CA pool is required")
	}
	if config.ServerKeyPin == (PublicKeyPin{}) {
		return nil, errors.New("audit server public-key pin is required")
	}
	serverName := config.ServerName
	if serverName == "" {
		serverName = endpoint.Hostname()
	}
	expectedPin := config.ServerKeyPin
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{config.Certificate},
		RootCAs:      config.ServerRoots,
		ServerName:   serverName,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
				return errors.New("server certificate was not verified")
			}
			pin, err := PinCertificate(state.PeerCertificates[0])
			if err != nil {
				return err
			}
			if pin != expectedPin {
				return errors.New("server public-key identity does not match")
			}
			return nil
		},
	}, nil
}

// Append performs one delivery attempt.
func (client *Client) Append(ctx context.Context, event Event) (Receipt, error) {
	if ctx == nil {
		return Receipt{}, errors.New("append audit event: nil context")
	}
	encoded, err := MarshalEvent(event)
	if err != nil {
		return Receipt{}, err
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		client.eventEndpoint,
		bytes.NewReader(encoded),
	)
	if err != nil {
		return Receipt{}, errors.New("create audit request")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")

	response, err := client.http.Do(request)
	if err != nil {
		return Receipt{}, errors.New("audit receiver is unavailable")
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxReceiptBytes+1))
	if readErr != nil || len(body) > maxReceiptBytes {
		return Receipt{}, errors.New("audit receiver response is invalid")
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		if response.StatusCode == http.StatusInsufficientStorage {
			return Receipt{}, ErrPermanentRejection
		}
		if response.StatusCode == http.StatusRequestTimeout ||
			response.StatusCode == http.StatusTooEarly ||
			response.StatusCode == http.StatusTooManyRequests ||
			response.StatusCode >= 500 {
			return Receipt{}, errors.New("audit receiver is unavailable")
		}
		return Receipt{}, ErrPermanentRejection
	}
	receipt, err := parseReceipt(body)
	if err != nil || receipt.validateFor(event) != nil {
		return Receipt{}, errors.New("audit receiver response is invalid")
	}
	return receipt, nil
}

// Summary retrieves only the authenticated durable prefix identity. The
// receiver never exposes audit events through this protocol.
func (client *Client) Summary(ctx context.Context) (Summary, error) {
	if ctx == nil {
		return Summary{}, errors.New("read audit summary: nil context")
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		client.summaryEndpoint,
		nil,
	)
	if err != nil {
		return Summary{}, errors.New("create audit summary request")
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.http.Do(request)
	if err != nil {
		return Summary{}, errors.New("audit receiver is unavailable")
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxSummaryBytes+1))
	if readErr != nil || len(body) > maxSummaryBytes || response.StatusCode != http.StatusOK {
		return Summary{}, errors.New("audit receiver summary is unavailable")
	}
	summary, err := parseSummary(body)
	if err != nil {
		return Summary{}, errors.New("audit receiver summary is invalid")
	}
	return summary, nil
}

// Close releases idle transport connections.
func (client *Client) Close() {
	client.http.CloseIdleConnections()
}

func parseReceipt(encoded []byte) (Receipt, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var receipt Receipt
	if err := decoder.Decode(&receipt); err != nil {
		return Receipt{}, err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Receipt{}, errors.New("receipt contains trailing data")
	}
	if receipt.Version != ProtocolVersion || receipt.EventID == "" ||
		receipt.EventHash == "" || receipt.Sequence == 0 {
		return Receipt{}, errors.New("receipt fields are invalid")
	}
	return receipt, nil
}

type summaryResponse struct {
	Version      uint16 `json:"version"`
	Records      uint64 `json:"records"`
	LastSequence uint64 `json:"last_sequence"`
	TipHash      string `json:"tip_hash"`
	Bytes        uint64 `json:"bytes"`
}

func marshalSummary(summary Summary) ([]byte, error) {
	if err := validateSummary(summary); err != nil {
		return nil, err
	}
	return json.Marshal(summaryResponse{
		Version:      ProtocolVersion,
		Records:      summary.Records,
		LastSequence: summary.LastSequence,
		TipHash:      summary.TipHash,
		Bytes:        summary.Bytes,
	})
}

func parseSummary(encoded []byte) (Summary, error) {
	if len(encoded) == 0 || len(encoded) > maxSummaryBytes {
		return Summary{}, errors.New("summary size is invalid")
	}
	var response summaryResponse
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return Summary{}, err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Summary{}, errors.New("summary contains trailing data")
	}
	if response.Version != ProtocolVersion {
		return Summary{}, errors.New("summary version is unsupported")
	}
	summary := Summary{
		Records:      response.Records,
		LastSequence: response.LastSequence,
		TipHash:      response.TipHash,
		Bytes:        response.Bytes,
	}
	if err := validateSummary(summary); err != nil {
		return Summary{}, err
	}
	canonical, err := marshalSummary(summary)
	if err != nil || !bytes.Equal(encoded, canonical) {
		return Summary{}, errors.New("summary encoding is not canonical")
	}
	return summary, nil
}

func validateSummary(summary Summary) error {
	if summary.Records == 0 {
		if summary.LastSequence != 0 || summary.TipHash != "" || summary.Bytes != 0 {
			return errors.New("empty summary fields are inconsistent")
		}
		return nil
	}
	if summary.LastSequence != summary.Records ||
		summary.Bytes == 0 ||
		len(summary.TipHash) != sha256.Size*2 ||
		strings.ToLower(summary.TipHash) != summary.TipHash {
		return errors.New("summary fields are inconsistent")
	}
	if _, err := hex.DecodeString(summary.TipHash); err != nil {
		return errors.New("summary tip hash is invalid")
	}
	return nil
}
