package rpcclient

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Overclock-Validator/mithril/internal/safedisplay"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/jsonrpc"
)

const defaultRequestTimeout = 30 * time.Second

type RpcClient struct {
	client   *rpc.Client
	endpoint string
}

type displaySafeError struct {
	err error
}

func (e displaySafeError) Error() string { return SanitizeErrorForDisplay(e.err) }
func (e displaySafeError) Unwrap() error { return e.err }

func NewRpcClient(endpoint string) *RpcClient {
	return newRpcClient(endpoint, defaultRequestTimeout)
}

func newRpcClient(endpoint string, timeout time.Duration) *RpcClient {
	client := rpc.NewWithCustomRPCClient(jsonrpc.NewClientWithOpts(endpoint, &jsonrpc.RPCClientOpts{
		HTTPClient: &http.Client{Timeout: timeout},
	}))
	return &RpcClient{client: client, endpoint: endpoint}
}

// Endpoint returns the RPC endpoint URL
func (c *RpcClient) Endpoint() string {
	return c.endpoint
}

// EndpointForDisplay returns an origin-only form suitable for diagnostics.
func (c *RpcClient) EndpointForDisplay() string {
	if c == nil {
		return "[configured endpoint]"
	}
	return SanitizeEndpointForDisplay(c.endpoint)
}

// SanitizeEndpointForDisplay removes userinfo, paths, queries, and fragments.
func SanitizeEndpointForDisplay(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Host == "" {
		return "[configured endpoint]"
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "[configured endpoint]"
	}
	return (&url.URL{Scheme: scheme, Host: u.Host}).String()
}

// SanitizeTextForDisplay removes common credentials from diagnostic text.
func SanitizeTextForDisplay(value string) string {
	return safedisplay.Text(value, SanitizeEndpointForDisplay)
}

// SanitizeErrorForDisplay removes credentials from an error before display or persistence.
func SanitizeErrorForDisplay(err error) string {
	if err == nil {
		return ""
	}
	return SanitizeTextForDisplay(err.Error())
}

// WrapErrorForDisplay preserves errors.Is/errors.As behavior while ensuring
// ordinary formatting cannot print secret-bearing endpoint text.
func WrapErrorForDisplay(err error) error {
	if err == nil {
		return nil
	}
	return displaySafeError{err: err}
}
