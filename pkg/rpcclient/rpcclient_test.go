package rpcclient

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// credentialEndpoint is a documentation-only URL shaped like the private RPC
// providers this node is pointed at in practice, where the key rides in the
// query string. No real host or key appears here.
const credentialEndpoint = "https://rpc.example.com/v1/?api-key=SUPERSECRETVALUE1234"

const endpointSecret = "SUPERSECRETVALUE1234"

func TestEndpointAccessorsSeparateRawFromDisplay(t *testing.T) {
	client := NewRpcClient(credentialEndpoint)

	if got := client.Endpoint(); got != credentialEndpoint {
		t.Errorf("Endpoint() = %q, want the configured URL verbatim; requests need the key intact", got)
	}

	display := client.EndpointForDisplay()
	if strings.Contains(display, endpointSecret) {
		t.Errorf("EndpointForDisplay() leaked the API key: %q", display)
	}
	if want := "https://rpc.example.com"; display != want {
		t.Errorf("EndpointForDisplay() = %q, want %q", display, want)
	}
}

func TestEndpointForDisplayHandlesNilClient(t *testing.T) {
	var client *RpcClient
	if got := client.EndpointForDisplay(); got != "[configured endpoint]" {
		t.Fatalf("nil client EndpointForDisplay() = %q, want the placeholder", got)
	}
}

func TestRpcClientRequestTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	client := newRpcClient(server.URL, 20*time.Millisecond)
	started := time.Now()
	if _, err := client.GetSlot(); err == nil {
		t.Fatal("GetSlot() succeeded against a server that never responds")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("GetSlot() took %v, want at most 1s", elapsed)
	}
}

func TestWrapErrorForDisplayHidesCredentialsButKeepsMatching(t *testing.T) {
	sentinel := errors.New("dial " + credentialEndpoint + ": connection refused")
	wrapped := WrapErrorForDisplay(sentinel)

	if wrapped == nil {
		t.Fatal("WrapErrorForDisplay returned nil for a non-nil error")
	}
	if strings.Contains(wrapped.Error(), endpointSecret) {
		t.Errorf("wrapped error leaked the API key: %q", wrapped.Error())
	}
	if !errors.Is(wrapped, sentinel) {
		t.Error("errors.Is no longer matches the wrapped error; sanitizing broke identity")
	}
	if !errors.Is(errors.Join(wrapped, errors.New("other")), sentinel) {
		t.Error("errors.Is fails through a join; the wrapper must stay transparent")
	}
	if unwrapped := errors.Unwrap(wrapped); unwrapped != sentinel {
		t.Errorf("Unwrap() = %v, want the original error", unwrapped)
	}

	// A typed error must remain recoverable with errors.As.
	typed := &net0Error{msg: "reaching " + credentialEndpoint}
	wrappedTyped := WrapErrorForDisplay(typed)
	var target *net0Error
	if !errors.As(wrappedTyped, &target) {
		t.Error("errors.As cannot recover the concrete type through the wrapper")
	}
	if strings.Contains(wrappedTyped.Error(), endpointSecret) {
		t.Errorf("wrapped typed error leaked the API key: %q", wrappedTyped.Error())
	}

	if WrapErrorForDisplay(nil) != nil {
		t.Error("WrapErrorForDisplay(nil) must stay nil so callers can compare against nil")
	}
	if SanitizeErrorForDisplay(nil) != "" {
		t.Error("SanitizeErrorForDisplay(nil) must be empty")
	}
}

type net0Error struct{ msg string }

func (e *net0Error) Error() string { return e.msg }
