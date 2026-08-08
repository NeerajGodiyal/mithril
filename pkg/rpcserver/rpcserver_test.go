package rpcserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	jsonrpc "github.com/filecoin-project/go-jsonrpc"
	solanarpc "github.com/gagliardetto/solana-go/rpc"
	"golang.org/x/time/rate"
)

type stubRPCListener struct {
	addr   net.Addr
	closed bool
}

func (listener *stubRPCListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }
func (listener *stubRPCListener) Addr() net.Addr            { return listener.addr }
func (listener *stubRPCListener) Close() error {
	listener.closed = true
	return nil
}

func rpcHandlerTestServer() *RpcServer {
	return &RpcServer{bindIP: net.ParseIP(DefaultRPCBindAddress)}
}

func rpcHandlerTestRequest(method string, body *strings.Reader) *http.Request {
	req := httptest.NewRequest(method, "http://127.0.0.1/", body)
	req.Header.Set("Content-Type", "application/json")
	return req
}

type deadlineRPCAPI struct {
	deadline chan time.Time
}

func (api *deadlineRPCAPI) GetBlockHeight(ctx context.Context, _ jsonrpc.RawParams) (uint64, error) {
	deadline, _ := ctx.Deadline()
	api.deadline <- deadline
	return 0, nil
}

type notificationRPCAPI struct {
	calls atomic.Int32
}

func (api *notificationRPCAPI) GetBlockHeight(context.Context, jsonrpc.RawParams) (uint64, error) {
	return uint64(api.calls.Add(1)), nil
}

type methodConfusionRPCAPI struct {
	cheap     atomic.Int32
	expensive atomic.Int32
}

func (api *methodConfusionRPCAPI) GetBlockHeight(context.Context, jsonrpc.RawParams) (uint64, error) {
	return uint64(api.cheap.Add(1)), nil
}

func (api *methodConfusionRPCAPI) SendTransaction(context.Context, jsonrpc.RawParams) (string, error) {
	api.expensive.Add(1)
	return "sent", nil
}

type blockingRequestBody struct {
	started chan struct{}
	release <-chan struct{}
}

func (body *blockingRequestBody) Read([]byte) (int, error) {
	select {
	case <-body.started:
	default:
		close(body.started)
	}
	<-body.release
	return 0, io.EOF
}

func (*blockingRequestBody) Close() error { return nil }

func TestServeHTTPQuietlyHandlesUnsupportedMethod(t *testing.T) {
	rpcServer := rpcHandlerTestServer()
	req := rpcHandlerTestRequest(http.MethodPost, strings.NewReader(`{"jsonrpc":"2.0","method":"getSlot","id":7}`))
	rec := httptest.NewRecorder()

	rpcServer.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d", http.StatusInternalServerError, rec.Code)
	}

	var resp struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.JSONRPC != "2.0" || resp.ID != 7 {
		t.Fatalf("unexpected response identity: %+v", resp)
	}
	if resp.Error.Code != -32601 || resp.Error.Message != "method 'getSlot' not found" {
		t.Fatalf("unexpected error response: %+v", resp.Error)
	}
}

func TestServeHTTPQuietlyHandlesInvalidRequest(t *testing.T) {
	rpcServer := rpcHandlerTestServer()
	req := rpcHandlerTestRequest(http.MethodPost, strings.NewReader(""))
	rec := httptest.NewRecorder()

	rpcServer.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rec.Code)
	}

	var resp struct {
		JSONRPC string `json:"jsonrpc"`
		ID      any    `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.JSONRPC != "2.0" || resp.ID != nil {
		t.Fatalf("unexpected response identity: %+v", resp)
	}
	if resp.Error.Code != -32600 || resp.Error.Message != "Invalid request" {
		t.Fatalf("unexpected error response: %+v", resp.Error)
	}
}

func TestServeHTTPSeparatesInvalidRequestsFromParseErrors(t *testing.T) {
	for _, test := range []struct {
		name       string
		body       string
		wantStatus int
		wantCode   int
	}{
		{name: "number", body: `1`, wantStatus: http.StatusBadRequest, wantCode: -32600},
		{name: "wrong method type", body: `{"jsonrpc":"2.0","method":1,"id":7}`, wantStatus: http.StatusBadRequest, wantCode: -32600},
		{name: "missing protocol version", body: `{"method":"getBlockHeight","id":7}`, wantStatus: http.StatusBadRequest, wantCode: -32600},
		{name: "wrong protocol version", body: `{"jsonrpc":"1.0","method":"getBlockHeight","id":7}`, wantStatus: http.StatusBadRequest, wantCode: -32600},
		{name: "non-string protocol version", body: `{"jsonrpc":2,"method":"getBlockHeight","id":7}`, wantStatus: http.StatusBadRequest, wantCode: -32600},
		{name: "malformed JSON", body: `{`, wantStatus: http.StatusInternalServerError, wantCode: -32700},
	} {
		t.Run(test.name, func(t *testing.T) {
			rpcServer := rpcHandlerTestServer()
			req := rpcHandlerTestRequest(http.MethodPost, strings.NewReader(test.body))
			rec := httptest.NewRecorder()

			rpcServer.ServeHTTP(rec, req)

			if rec.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, test.wantStatus, rec.Body.String())
			}
			var response struct {
				ID    any `json:"id"`
				Error struct {
					Code int `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.ID != nil || response.Error.Code != test.wantCode {
				t.Fatalf("response = %+v, want null id and code %d", response, test.wantCode)
			}
		})
	}
}

func TestServeHTTPQuietlyHandlesNonRPCProbe(t *testing.T) {
	rpcServer := rpcHandlerTestServer()
	req := rpcHandlerTestRequest(http.MethodGet, strings.NewReader(""))
	rec := httptest.NewRecorder()

	rpcServer.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d", http.StatusNotFound, rec.Code)
	}
}

func TestRPCAPIMethodSet(t *testing.T) {
	apiType := reflect.TypeOf(&rpcAPI{})
	got := make(map[string]struct{}, apiType.NumMethod())
	for i := 0; i < apiType.NumMethod(); i++ {
		method := apiType.Method(i)
		got[formatRPCMethodName("", method.Name)] = struct{}{}
	}

	if !reflect.DeepEqual(got, supportedRPCMethods) {
		t.Fatalf("registered RPC methods = %v, want %v", got, supportedRPCMethods)
	}
}

func TestRPCServiceRejectsUnregisteredServerMethods(t *testing.T) {
	rpcServer := &RpcServer{}
	rpcServer.rpcService = newRPCService(rpcServer)

	var methodName string
	serverType := reflect.TypeOf(rpcServer)
	for i := 0; i < serverType.NumMethod(); i++ {
		method := serverType.Method(i)
		name := formatRPCMethodName("", method.Name)
		if _, supported := supportedRPCMethods[name]; !supported && method.Type.NumIn() > 1 {
			methodName = name
			break
		}
	}
	if methodName == "" {
		t.Fatal("expected at least one non-RPC server method")
	}

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		fmt.Sprintf(`{"jsonrpc":"2.0","method":%q,"params":[],"id":1}`, methodName),
	))
	rec := httptest.NewRecorder()
	rpcServer.rpcService.ServeHTTP(rec, req)

	var resp struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Error.Code != -32601 {
		t.Fatalf("unexpected error code %d", resp.Error.Code)
	}
}

func TestRPCServiceRequestSizeLimit(t *testing.T) {
	rpcServer := &RpcServer{}
	rpcServer.rpcService = newRPCService(rpcServer)

	body := `{"jsonrpc":"2.0","method":"getBlockHeight","params":[],"id":1,"padding":"` +
		strings.Repeat("x", maxRPCRequestBytes) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	rpcServer.rpcService.ServeHTTP(rec, req)

	var resp struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Error.Code != -32700 {
		t.Fatalf("unexpected error code %d", resp.Error.Code)
	}
}

func TestServeHTTPAcceptsRequestAtOuterLimit(t *testing.T) {
	rpcServer := rpcHandlerTestServer()
	rpcServer.rpcService = newRPCService(rpcServer)

	prefix := `{"jsonrpc":"2.0","method":"getBlockHeight","params":[],"id":1,"padding":"`
	suffix := `"}`
	body := prefix + strings.Repeat("x", maxRPCRequestBytes-len(prefix)-len(suffix)) + suffix
	req := rpcHandlerTestRequest(http.MethodPost, strings.NewReader(body))
	rec := httptest.NewRecorder()
	rpcServer.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
}

func TestServeHTTPRewindsSupportedRequestBody(t *testing.T) {
	for _, test := range []struct {
		name    string
		chunked bool
	}{
		{name: "known length"},
		{name: "chunked", chunked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			rpcServer := rpcHandlerTestServer()
			rpcServer.rpcService = newRPCService(rpcServer)
			req := rpcHandlerTestRequest(
				http.MethodPost,
				strings.NewReader(`{"jsonrpc":"2.0","method":"getBlockHeight","params":[],"id":1}`),
			)
			if test.chunked {
				req.ContentLength = -1
			}
			rec := httptest.NewRecorder()

			rpcServer.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
			}
			var resp struct {
				ID     int    `json:"id"`
				Result uint64 `json:"result"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal response: %v", err)
			}
			if resp.ID != 1 {
				t.Fatalf("response id = %d, want 1", resp.ID)
			}
		})
	}
}

func TestServeHTTPPreservesValidIDsAndRejectsInvalidIDs(t *testing.T) {
	tests := []struct {
		name      string
		id        string
		wantCode  int
		wantID    string
		wantCalls int32
		wantError int
	}{
		{
			name:      "null",
			id:        "null",
			wantCode:  http.StatusOK,
			wantID:    "null",
			wantCalls: 1,
		},
		{
			name:      "large integer",
			id:        "9007199254740993",
			wantCode:  http.StatusOK,
			wantID:    "9007199254740993",
			wantCalls: 1,
		},
		{
			name:      "string",
			id:        `"client-1"`,
			wantCode:  http.StatusOK,
			wantID:    `"client-1"`,
			wantCalls: 1,
		},
		{
			name:      "object",
			id:        `{}`,
			wantCode:  http.StatusBadRequest,
			wantID:    "null",
			wantError: -32600,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rpcServer := rpcHandlerTestServer()
			api := &notificationRPCAPI{}
			service := jsonrpc.NewServer(jsonrpc.WithServerMethodNameFormatter(formatRPCMethodName))
			service.Register("MithrilRpc", api)
			rpcServer.rpcService = service
			body := fmt.Sprintf(
				`{"jsonrpc":"2.0","method":"getBlockHeight","params":[],"id":%s}`,
				test.id,
			)
			req := rpcHandlerTestRequest(http.MethodPost, strings.NewReader(body))
			rec := httptest.NewRecorder()

			rpcServer.ServeHTTP(rec, req)

			if rec.Code != test.wantCode {
				t.Fatalf("status = %d, want %d: %s", rec.Code, test.wantCode, rec.Body.String())
			}
			if calls := api.calls.Load(); calls != test.wantCalls {
				t.Fatalf("method calls = %d, want %d", calls, test.wantCalls)
			}
			var response struct {
				ID    json.RawMessage `json:"id"`
				Error *struct {
					Code int `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if string(response.ID) != test.wantID {
				t.Fatalf("response id = %s, want %s", response.ID, test.wantID)
			}
			if test.wantError == 0 && response.Error != nil {
				t.Fatalf("unexpected error response: %+v", response.Error)
			}
			if test.wantError != 0 && (response.Error == nil || response.Error.Code != test.wantError) {
				t.Fatalf("error response = %+v, want code %d", response.Error, test.wantError)
			}
		})
	}
}

func TestServeHTTPSingleNotificationsAreSilent(t *testing.T) {
	for _, test := range []struct {
		name      string
		method    string
		wantCalls int32
	}{
		{name: "supported", method: "getBlockHeight", wantCalls: 1},
		{name: "unsupported", method: "getSlot"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rpcServer := rpcHandlerTestServer()
			api := &notificationRPCAPI{}
			service := jsonrpc.NewServer(jsonrpc.WithServerMethodNameFormatter(formatRPCMethodName))
			service.Register("MithrilRpc", api)
			rpcServer.rpcService = service
			body := fmt.Sprintf(`{"jsonrpc":"2.0","method":%q,"params":[]}`, test.method)
			req := rpcHandlerTestRequest(http.MethodPost, strings.NewReader(body))
			rec := httptest.NewRecorder()

			rpcServer.ServeHTTP(rec, req)

			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusNoContent, rec.Body.String())
			}
			if rec.Body.Len() != 0 {
				t.Fatalf("notification response = %q, want empty", rec.Body.String())
			}
			if calls := api.calls.Load(); calls != test.wantCalls {
				t.Fatalf("method calls = %d, want %d", calls, test.wantCalls)
			}
		})
	}
}

func TestServeHTTPRejectsOversizedOuterBody(t *testing.T) {
	for _, test := range []struct {
		name    string
		chunked bool
	}{
		{name: "known length"},
		{name: "chunked", chunked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			rpcServer := rpcHandlerTestServer()
			body := strings.Repeat("x", maxRPCRequestBytes+1)
			req := rpcHandlerTestRequest(http.MethodPost, strings.NewReader(body))
			if test.chunked {
				req.ContentLength = -1
			}
			rec := httptest.NewRecorder()

			rpcServer.ServeHTTP(rec, req)

			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
			}
			if rec.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("content type = %q", rec.Header().Get("Content-Type"))
			}
		})
	}
}

func TestServeHTTPCapsAndChargesBatchRequests(t *testing.T) {
	makeBatch := func(count int) string {
		requests := make([]string, count)
		for i := range requests {
			requests[i] = fmt.Sprintf(
				`{"jsonrpc":"2.0","method":"getBlockHeight","params":[],"id":%d}`,
				i+1,
			)
		}
		return "[" + strings.Join(requests, ",") + "]"
	}

	t.Run("cap", func(t *testing.T) {
		rpcServer := rpcHandlerTestServer()
		req := rpcHandlerTestRequest(
			http.MethodPost,
			strings.NewReader(makeBatch(maxRPCBatchRequests+1)),
		)
		rec := httptest.NewRecorder()

		rpcServer.ServeHTTP(rec, req)

		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusRequestEntityTooLarge, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "maximum 32") {
			t.Fatalf("batch-cap response = %s", rec.Body.String())
		}
	})

	t.Run("rate is per call", func(t *testing.T) {
		rpcServer := rpcHandlerTestServer()
		rpcServer.requestRate = rate.NewLimiter(0, 1)
		req := rpcHandlerTestRequest(http.MethodPost, strings.NewReader(makeBatch(2)))
		rec := httptest.NewRecorder()

		rpcServer.ServeHTTP(rec, req)

		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusTooManyRequests, rec.Body.String())
		}
		if rec.Header().Get("Retry-After") != "1" {
			t.Fatal("rate-limited batch is missing Retry-After")
		}
	})
}

func TestScanRPCBatchStopsAtLimitAndRejectsTrailingJSON(t *testing.T) {
	items := make([]string, maxRPCBatchRequests)
	for i := range items {
		items[i] = `{"method":"getBlockHeight"}`
	}

	tooLarge := "[" + strings.Join(append(items, `{"method":`), ",") + "]"
	if _, err := scanRPCBatch([]byte(tooLarge)); !errors.Is(err, errRPCBatchTooLarge) {
		t.Fatalf("33rd item error = %v, want batch-size error before decoding it", err)
	}

	trailing := "[" + strings.Join(items, ",") + `] {}`
	if _, err := scanRPCBatch([]byte(trailing)); err == nil || errors.Is(err, errRPCBatchTooLarge) {
		t.Fatalf("trailing JSON error = %v, want parse error", err)
	}
}

func TestServeHTTPRejectsExpensiveMethodsInMultiCallBatches(t *testing.T) {
	for _, method := range []string{"simulateTransaction", "sendTransaction"} {
		t.Run(method, func(t *testing.T) {
			body := fmt.Sprintf(
				`[{"jsonrpc":"2.0","method":"getBlockHeight","params":[],"id":1},{"jsonrpc":"2.0","method":%q,"id":2}]`,
				method,
			)
			rpcServer := rpcHandlerTestServer()
			rpcServer.SetSlotCtx(&sealevel.SlotCtx{})
			rpcServer.rpcService = newRPCService(rpcServer)
			req := rpcHandlerTestRequest(http.MethodPost, strings.NewReader(body))
			rec := httptest.NewRecorder()

			rpcServer.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
			}
			var responses []struct {
				ID    int `json:"id"`
				Error *struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &responses); err != nil {
				t.Fatal(err)
			}
			if len(responses) != 2 || responses[0].ID != 1 || responses[0].Error != nil {
				t.Fatalf("ordinary batch response = %+v", responses)
			}
			if responses[1].ID != 2 || responses[1].Error == nil ||
				responses[1].Error.Code != -32600 ||
				!strings.Contains(responses[1].Error.Message, "does not support batch requests") {
				t.Fatalf("expensive batch response = %+v", responses[1])
			}
		})
	}
}

func TestServeHTTPDispatchesTheMethodItValidated(t *testing.T) {
	rpcServer := rpcHandlerTestServer()
	api := &methodConfusionRPCAPI{}
	service := jsonrpc.NewServer(jsonrpc.WithServerMethodNameFormatter(formatRPCMethodName))
	service.Register("MithrilRpc", api)
	rpcServer.rpcService = service
	body := `[{"jsonrpc":"2.0","method":"sendTransaction","Method":"getBlockHeight","params":[],"id":1},{"jsonrpc":"2.0","method":"sendTransaction","Method":"getBlockHeight","params":[],"id":2}]`
	req := rpcHandlerTestRequest(http.MethodPost, strings.NewReader(body))
	rec := httptest.NewRecorder()

	rpcServer.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if api.expensive.Load() != 0 || api.cheap.Load() != 2 {
		t.Fatalf("validated cheap=%d expensive=%d, want 2 and 0", api.cheap.Load(), api.expensive.Load())
	}
}

func TestServeHTTPReadsRemoteBodyBeforeTakingAdmissionSlot(t *testing.T) {
	rpcServer := rpcHandlerTestServer()
	rpcServer.rpcService = newRPCService(rpcServer)
	rpcServer.remoteSlots = make(chan struct{}, 1)
	external := rpcListenerHandler{server: rpcServer, bindIP: net.ParseIP("192.0.2.10")}
	release := make(chan struct{})
	first := httptest.NewRequest(http.MethodPost, "http://192.0.2.10/", nil)
	first.Body = &blockingRequestBody{started: make(chan struct{}), release: release}
	first.ContentLength = 1
	first.Header.Set("Content-Type", "application/json")
	first.Host = "192.0.2.10:8899"
	first.RemoteAddr = "198.51.100.20:50000"
	firstRecorder := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		external.ServeHTTP(firstRecorder, first)
		close(firstDone)
	}()
	<-first.Body.(*blockingRequestBody).started

	second := rpcHandlerTestRequest(http.MethodPost, strings.NewReader(`{"jsonrpc":"2.0","method":"getBlockHeight","params":[],"id":1}`))
	second.Host = "192.0.2.10:8899"
	second.RemoteAddr = "198.51.100.21:50000"
	secondRecorder := httptest.NewRecorder()
	external.ServeHTTP(secondRecorder, second)
	close(release)
	<-firstDone

	if secondRecorder.Code != http.StatusOK {
		t.Fatalf("complete remote request status = %d, want %d", secondRecorder.Code, http.StatusOK)
	}
}

func TestCallRPCPreservesSuccessfulResultAfterContextExpiry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := callRPC(ctx, func() (string, error) {
		cancel()
		return "sent", nil
	})
	if err != nil || result != "sent" {
		t.Fatalf("result=%q err=%v, want successful sent result", result, err)
	}
}

func TestServeHTTPHandlesNotificationBatches(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantCode  int
		wantIDs   []string
		wantCalls int32
	}{
		{
			name:      "notification before request",
			body:      `[{"jsonrpc":"2.0","method":"getBlockHeight"},{"jsonrpc":"2.0","method":"getBlockHeight","id":7}]`,
			wantCode:  http.StatusOK,
			wantIDs:   []string{"7"},
			wantCalls: 2,
		},
		{
			name:      "notification after request",
			body:      `[{"jsonrpc":"2.0","method":"getBlockHeight","id":7},{"jsonrpc":"2.0","method":"getBlockHeight"}]`,
			wantCode:  http.StatusOK,
			wantIDs:   []string{"7"},
			wantCalls: 2,
		},
		{
			name:      "notifications only",
			body:      `[{"jsonrpc":"2.0","method":"getBlockHeight"},{"jsonrpc":"2.0","method":"getBlockHeight"}]`,
			wantCode:  http.StatusNoContent,
			wantCalls: 2,
		},
		{
			name:      "explicit null id is a request",
			body:      `[{"jsonrpc":"2.0","method":"getBlockHeight","id":null},{"jsonrpc":"2.0","method":"getBlockHeight"}]`,
			wantCode:  http.StatusOK,
			wantIDs:   []string{"null"},
			wantCalls: 2,
		},
		{
			name:      "unsupported notification is silent",
			body:      `[{"jsonrpc":"2.0","method":"getSlot"},{"jsonrpc":"2.0","method":"getBlockHeight","id":7}]`,
			wantCode:  http.StatusOK,
			wantIDs:   []string{"7"},
			wantCalls: 1,
		},
		{
			name:      "expensive notification is rejected silently",
			body:      `[{"jsonrpc":"2.0","method":"sendTransaction"},{"jsonrpc":"2.0","method":"getBlockHeight","id":7}]`,
			wantCode:  http.StatusOK,
			wantIDs:   []string{"7"},
			wantCalls: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rpcServer := rpcHandlerTestServer()
			api := &notificationRPCAPI{}
			service := jsonrpc.NewServer(jsonrpc.WithServerMethodNameFormatter(formatRPCMethodName))
			service.Register("MithrilRpc", api)
			rpcServer.rpcService = service
			req := rpcHandlerTestRequest(http.MethodPost, strings.NewReader(test.body))
			rec := httptest.NewRecorder()

			rpcServer.ServeHTTP(rec, req)

			if rec.Code != test.wantCode {
				t.Fatalf("status = %d, want %d: %s", rec.Code, test.wantCode, rec.Body.String())
			}
			if calls := api.calls.Load(); calls != test.wantCalls {
				t.Fatalf("method calls = %d, want %d", calls, test.wantCalls)
			}
			if test.wantCode == http.StatusNoContent {
				if rec.Body.Len() != 0 {
					t.Fatalf("notification-only response = %q, want empty", rec.Body.String())
				}
				return
			}
			if !json.Valid(rec.Body.Bytes()) {
				t.Fatalf("response is invalid JSON: %q", rec.Body.String())
			}
			var responses []struct {
				ID json.RawMessage `json:"id"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &responses); err != nil {
				t.Fatal(err)
			}
			if len(responses) != len(test.wantIDs) {
				t.Fatalf("response count = %d, want %d", len(responses), len(test.wantIDs))
			}
			for i, response := range responses {
				if string(response.ID) != test.wantIDs[i] {
					t.Fatalf("response %d id = %s, want %s", i, response.ID, test.wantIDs[i])
				}
			}
		})
	}
}

func TestServeHTTPKeepsValidSiblingsOfInvalidBatchItems(t *testing.T) {
	for _, invalid := range []string{
		`1`,
		`"x"`,
		`[]`,
		`{"jsonrpc":"2.0","method":1,"id":2}`,
		`{"method":"getBlockHeight","params":[],"id":2}`,
		`{"jsonrpc":"1.0","method":"getBlockHeight","params":[],"id":2}`,
		`{"jsonrpc":2,"method":"getBlockHeight","params":[],"id":2}`,
	} {
		t.Run(invalid, func(t *testing.T) {
			rpcServer := rpcHandlerTestServer()
			api := &notificationRPCAPI{}
			service := jsonrpc.NewServer(jsonrpc.WithServerMethodNameFormatter(formatRPCMethodName))
			service.Register("MithrilRpc", api)
			rpcServer.rpcService = service
			body := fmt.Sprintf(
				`[%s,{"jsonrpc":"2.0","method":"getBlockHeight","params":[],"id":7}]`,
				invalid,
			)
			req := rpcHandlerTestRequest(http.MethodPost, strings.NewReader(body))
			rec := httptest.NewRecorder()

			rpcServer.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
			}
			if calls := api.calls.Load(); calls != 1 {
				t.Fatalf("method calls = %d, want 1", calls)
			}
			var responses []struct {
				ID    json.RawMessage `json:"id"`
				Error *struct {
					Code int `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &responses); err != nil {
				t.Fatal(err)
			}
			if len(responses) != 2 {
				t.Fatalf("response count = %d, want 2", len(responses))
			}
			if string(responses[0].ID) != "null" || responses[0].Error == nil ||
				responses[0].Error.Code != -32600 {
				t.Fatalf("invalid-item response = %+v", responses[0])
			}
			if string(responses[1].ID) != "7" || responses[1].Error != nil {
				t.Fatalf("valid-sibling response = %+v", responses[1])
			}
		})
	}
}

func TestServeHTTPAddsJSONResponseTypeAndHandlerDeadline(t *testing.T) {
	rpcServer := rpcHandlerTestServer()
	api := &deadlineRPCAPI{deadline: make(chan time.Time, 1)}
	service := jsonrpc.NewServer(jsonrpc.WithServerMethodNameFormatter(formatRPCMethodName))
	service.Register("MithrilRpc", api)
	rpcServer.rpcService = service
	req := rpcHandlerTestRequest(
		http.MethodPost,
		strings.NewReader(`{"jsonrpc":"2.0","method":"getBlockHeight","params":[],"id":1}`),
	)
	started := time.Now()
	rec := httptest.NewRecorder()

	rpcServer.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q, want application/json", got)
	}
	deadline := <-api.deadline
	remaining := deadline.Sub(started)
	if remaining < rpcRequestContextTimeout-100*time.Millisecond ||
		remaining > rpcRequestContextTimeout+100*time.Millisecond {
		t.Fatalf("request context deadline is %v from start, want approximately %v", remaining, rpcRequestContextTimeout)
	}
}

func TestServeHTTPRejectsBrowserAndUpgradeRequests(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*http.Request)
		wantStatus int
	}{
		{
			name: "foreign host",
			mutate: func(req *http.Request) {
				req.Host = "attacker.example"
			},
			wantStatus: http.StatusMisdirectedRequest,
		},
		{
			name: "origin",
			mutate: func(req *http.Request) {
				req.Header.Set("Origin", "https://attacker.example")
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "websocket upgrade",
			mutate: func(req *http.Request) {
				req.Header.Set("Connection", "Upgrade")
				req.Header.Set("Upgrade", "websocket")
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "connection upgrade",
			mutate: func(req *http.Request) {
				req.Header.Set("Connection", "keep-alive, Upgrade")
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "upgrade header",
			mutate: func(req *http.Request) {
				req.Header.Set("Upgrade", "websocket")
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "missing content type",
			mutate: func(req *http.Request) {
				req.Header.Del("Content-Type")
			},
			wantStatus: http.StatusUnsupportedMediaType,
		},
		{
			name: "simple browser content type",
			mutate: func(req *http.Request) {
				req.Header.Set("Content-Type", "text/plain")
			},
			wantStatus: http.StatusUnsupportedMediaType,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rpcServer := rpcHandlerTestServer()
			rpcServer.rpcService = newRPCService(rpcServer)
			req := rpcHandlerTestRequest(
				http.MethodPost,
				strings.NewReader(`{"jsonrpc":"2.0","method":"getBlockHeight","params":[],"id":1}`),
			)
			test.mutate(req)
			rec := httptest.NewRecorder()

			rpcServer.ServeHTTP(rec, req)

			if rec.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, test.wantStatus)
			}
		})
	}
}

func TestServeHTTPRunsCheapValidationBeforeAdmission(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*http.Request)
		wantStatus int
	}{
		{
			name: "non-POST probe",
			mutate: func(req *http.Request) {
				req.Method = http.MethodGet
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "wrong content type",
			mutate: func(req *http.Request) {
				req.Header.Set("Content-Type", "text/plain")
			},
			wantStatus: http.StatusUnsupportedMediaType,
		},
		{
			name: "browser origin",
			mutate: func(req *http.Request) {
				req.Header.Set("Origin", "https://attacker.example")
			},
			wantStatus: http.StatusForbidden,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rpcServer := rpcHandlerTestServer()
			rpcServer.requestRate = rate.NewLimiter(0, 0)
			req := rpcHandlerTestRequest(http.MethodPost, strings.NewReader(`{}`))
			test.mutate(req)
			rec := httptest.NewRecorder()

			rpcServer.ServeHTTP(rec, req)

			if rec.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, test.wantStatus)
			}
		})
	}

	t.Run("known oversized body", func(t *testing.T) {
		rpcServer := rpcHandlerTestServer()
		rpcServer.requestRate = rate.NewLimiter(0, 0)
		req := rpcHandlerTestRequest(
			http.MethodPost,
			strings.NewReader(strings.Repeat("x", maxRPCRequestBytes+1)),
		)
		rec := httptest.NewRecorder()

		rpcServer.ServeHTTP(rec, req)

		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
		}
	})
}

func TestServeHTTPAcceptsJSONCharset(t *testing.T) {
	rpcServer := rpcHandlerTestServer()
	rpcServer.rpcService = newRPCService(rpcServer)
	req := rpcHandlerTestRequest(
		http.MethodPost,
		strings.NewReader(`{"jsonrpc":"2.0","method":"getBlockHeight","params":[],"id":1}`),
	)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	rec := httptest.NewRecorder()

	rpcServer.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
}

func TestServeHTTPAllowsLocalhostForLoopbackListener(t *testing.T) {
	rpcServer := rpcHandlerTestServer()
	rpcServer.rpcService = newRPCService(rpcServer)
	req := rpcHandlerTestRequest(
		http.MethodPost,
		strings.NewReader(`{"jsonrpc":"2.0","method":"getBlockHeight","params":[],"id":1}`),
	)
	req.Host = "localhost:8899"
	rec := httptest.NewRecorder()

	rpcServer.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
}

func TestListenerHandlersEnforceTheirOwnHost(t *testing.T) {
	rpcServer := rpcHandlerTestServer()
	rpcServer.rpcService = newRPCService(rpcServer)
	external := rpcListenerHandler{server: rpcServer, bindIP: net.ParseIP("192.0.2.10")}
	local := rpcListenerHandler{server: rpcServer, bindIP: net.ParseIP(DefaultRPCBindAddress)}

	tests := []struct {
		name       string
		handler    http.Handler
		host       string
		wantStatus int
	}{
		{
			name:       "external listener accepts external identity",
			handler:    external,
			host:       "192.0.2.10:8899",
			wantStatus: http.StatusOK,
		},
		{
			name:       "external listener rejects loopback identity",
			handler:    external,
			host:       "127.0.0.1:8899",
			wantStatus: http.StatusMisdirectedRequest,
		},
		{
			name:       "local listener accepts loopback identity",
			handler:    local,
			host:       "127.0.0.1:8899",
			wantStatus: http.StatusOK,
		},
		{
			name:       "local listener rejects external identity",
			handler:    local,
			host:       "192.0.2.10:8899",
			wantStatus: http.StatusMisdirectedRequest,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := rpcHandlerTestRequest(
				http.MethodPost,
				strings.NewReader(`{"jsonrpc":"2.0","method":"getBlockHeight","params":[],"id":1}`),
			)
			req.Host = test.host
			rec := httptest.NewRecorder()

			test.handler.ServeHTTP(rec, req)

			if rec.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, test.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestServeHTTPAdmissionLimits(t *testing.T) {
	t.Run("rate", func(t *testing.T) {
		rpcServer := rpcHandlerTestServer()
		rpcServer.requestRate = rate.NewLimiter(0, 0)
		req := rpcHandlerTestRequest(http.MethodPost, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()

		rpcServer.ServeHTTP(rec, req)

		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
		}
		if rec.Header().Get("Retry-After") != "1" {
			t.Fatal("rate-limited response is missing Retry-After")
		}
	})

	t.Run("concurrency", func(t *testing.T) {
		rpcServer := rpcHandlerTestServer()
		rpcServer.requestSlots = make(chan struct{}, 1)
		rpcServer.requestSlots <- struct{}{}
		req := rpcHandlerTestRequest(http.MethodPost, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()

		rpcServer.ServeHTTP(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
		}
	})
}

func TestRemoteAdmissionLeavesLocalCapacity(t *testing.T) {
	rpcServer := rpcHandlerTestServer()
	rpcServer.rpcService = newRPCService(rpcServer)
	rpcServer.remoteRate = rate.NewLimiter(0, 0)
	rpcServer.remoteSlots = make(chan struct{}, 1)
	rpcServer.remoteSlots <- struct{}{}

	body := `{"jsonrpc":"2.0","method":"getBlockHeight","params":[],"id":1}`
	external := rpcListenerHandler{server: rpcServer, bindIP: net.ParseIP("192.0.2.10")}
	req := rpcHandlerTestRequest(http.MethodPost, strings.NewReader(body))
	req.Host = "192.0.2.10:8899"
	req.RemoteAddr = "198.51.100.20:50000"
	rec := httptest.NewRecorder()
	external.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("remote status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}

	local := rpcListenerHandler{server: rpcServer, bindIP: net.ParseIP(DefaultRPCBindAddress)}
	req = rpcHandlerTestRequest(http.MethodPost, strings.NewReader(body))
	rec = httptest.NewRecorder()
	local.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("local status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	rpcServer.remoteRate = nil
	req = rpcHandlerTestRequest(http.MethodPost, strings.NewReader(body))
	req.Host = "192.0.2.10:8899"
	req.RemoteAddr = "198.51.100.20:50000"
	rec = httptest.NewRecorder()
	external.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("remote concurrency status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestLocalRPCRequestClassificationUsesListenerAndTCPPeer(t *testing.T) {
	tests := []struct {
		name       string
		bind       string
		remoteAddr string
		want       bool
	}{
		{name: "loopback listener", bind: "127.0.0.1", remoteAddr: "198.51.100.20:50000", want: true},
		{name: "wildcard loopback peer", bind: "0.0.0.0", remoteAddr: "127.0.0.1:50000", want: true},
		{name: "wildcard remote peer", bind: "0.0.0.0", remoteAddr: "198.51.100.20:50000"},
		{name: "exact external loopback peer", bind: "192.0.2.10", remoteAddr: "127.0.0.1:50000"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isLocalRPCRequest(net.ParseIP(test.bind), test.remoteAddr); got != test.want {
				t.Fatalf("isLocalRPCRequest(%q, %q) = %v, want %v", test.bind, test.remoteAddr, got, test.want)
			}
		})
	}
}

func TestRPCAPIGuardsContextAndReservesExpensiveLocalCapacity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	api := &rpcAPI{}
	if _, err := api.GetBlockHeight(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled call error = %v, want context canceled", err)
	}

	rpcServer := &RpcServer{
		expensiveSlots:       make(chan struct{}, maxRPCConcurrentExpensiveRequests),
		remoteExpensiveSlots: make(chan struct{}, maxRPCRemoteExpensiveRequests),
	}
	for range maxRPCRemoteExpensiveRequests {
		rpcServer.remoteExpensiveSlots <- struct{}{}
	}
	if _, err := callExpensiveRPC(context.Background(), rpcServer, func() (int, error) {
		return 0, errors.New("must not run")
	}); !errors.Is(err, errRPCServerBusy) {
		t.Fatalf("remote expensive call error = %v, want busy", err)
	}

	localCtx := context.WithValue(context.Background(), rpcLocalRequestContextKey{}, true)
	if got, err := callExpensiveRPC(localCtx, rpcServer, func() (int, error) { return 7, nil }); err != nil || got != 7 {
		t.Fatalf("local expensive call = %d, %v", got, err)
	}
}

func TestRPCHTTPServerLimits(t *testing.T) {
	server := newRPCHTTPServer(http.NotFoundHandler())
	if server.ReadHeaderTimeout != rpcReadHeaderTimeout ||
		server.ReadTimeout != rpcReadTimeout ||
		server.WriteTimeout != rpcWriteTimeout ||
		server.IdleTimeout != rpcIdleTimeout ||
		server.MaxHeaderBytes != maxRPCHeaderBytes {
		t.Fatalf("unexpected HTTP server limits: %+v", server)
	}
	if rpcRequestContextTimeout >= rpcWriteTimeout {
		t.Fatalf("request context timeout %v must be shorter than write timeout %v", rpcRequestContextTimeout, rpcWriteTimeout)
	}
}

func TestRequestHostAllowed(t *testing.T) {
	tests := []struct {
		name string
		bind string
		host string
		want bool
	}{
		{name: "specific IPv4", bind: "192.0.2.10", host: "192.0.2.10:8899", want: true},
		{name: "wrong IPv4", bind: "192.0.2.10", host: "192.0.2.11:8899"},
		{name: "wildcard IPv4", bind: "0.0.0.0", host: "192.0.2.10:8899", want: true},
		{name: "specific IPv6", bind: "2001:db8::10", host: "[2001:db8::10]:8899", want: true},
		{name: "wildcard IPv6", bind: "::", host: "[2001:db8::10]:8899", want: true},
		{name: "DNS rejected", bind: "0.0.0.0", host: "node.example:8899"},
		{name: "unspecified host rejected", bind: "0.0.0.0", host: "0.0.0.0:8899"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rpcServer := &RpcServer{bindIP: net.ParseIP(test.bind)}
			if got := requestHostAllowed(rpcServer.bindIP, test.host); got != test.want {
				t.Fatalf("requestHostAllowed(%q) = %v, want %v", test.host, got, test.want)
			}
		})
	}
}

func TestNewRPCServerDefaultsToLoopback(t *testing.T) {
	rpcServer := NewRpcServer(nil, 0, &sealevel.SysvarEpochSchedule{})
	t.Cleanup(func() {
		_ = rpcServer.Shutdown(context.Background())
	})

	if len(rpcServer.listeners) != 1 {
		t.Fatalf("default listener count = %d, want 1", len(rpcServer.listeners))
	}
	host, _, err := net.SplitHostPort(rpcServer.listeners[0].listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		t.Fatalf("listener address %q is not loopback", host)
	}
	if cap(rpcServer.requestSlots) != maxRPCConcurrentRequests ||
		cap(rpcServer.remoteSlots) != maxRPCRemoteConcurrentRequests ||
		cap(rpcServer.expensiveSlots) != maxRPCConcurrentExpensiveRequests ||
		cap(rpcServer.remoteExpensiveSlots) != maxRPCRemoteExpensiveRequests {
		t.Fatal("RPC admission capacities do not match their declared limits")
	}
}

func TestNewRPCServerRejectsInvalidBindAddress(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected invalid bind address to panic")
		}
	}()
	NewRpcServerWithBindAddress(nil, "", 0, &sealevel.SysvarEpochSchedule{})
}

func TestNewRPCServerErrorConstructorRejectsInvalidBindAddress(t *testing.T) {
	server, err := NewRpcServerWithBindAddressE(nil, "invalid", 0, &sealevel.SysvarEpochSchedule{})
	if err == nil || server != nil {
		t.Fatalf("NewRpcServerWithBindAddressE() = (%v, %v), want nil server and error", server, err)
	}
}

func TestNewRPCServerUsesResolvedClusterRPCEndpoints(t *testing.T) {
	resolved := []string{"https://rpc.example.invalid"}
	server, err := NewRpcServerWithClusterRPCEndpointsE(
		nil,
		"127.0.0.1",
		0,
		&sealevel.SysvarEpochSchedule{},
		resolved,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = server.Shutdown(context.Background())
	})

	resolved[0] = "https://changed.example.invalid"
	if got, want := server.clusterRPCEndpoints, []string{"https://rpc.example.invalid"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cluster RPC endpoints = %v, want %v", got, want)
	}
}

func TestNewRPCServerErrorConstructorReportsPortCollision(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := uint16(listener.Addr().(*net.TCPAddr).Port)

	server, err := NewRpcServerWithBindAddressE(nil, "127.0.0.1", port, &sealevel.SysvarEpochSchedule{})
	if err == nil || server != nil {
		t.Fatalf("NewRpcServerWithBindAddressE() = (%v, %v), want nil server and error", server, err)
	}
}

func TestOpenRPCListenersAddsOnlyRequiredCompanion(t *testing.T) {
	tests := []struct {
		name          string
		bind          string
		wantAddresses []string
	}{
		{
			name:          "loopback IPv4",
			bind:          "127.0.0.1",
			wantAddresses: []string{"127.0.0.1:8899"},
		},
		{
			name:          "wildcard IPv4",
			bind:          "0.0.0.0",
			wantAddresses: []string{"0.0.0.0:8899"},
		},
		{
			name:          "exact IPv4",
			bind:          "192.0.2.10",
			wantAddresses: []string{"192.0.2.10:8899", "127.0.0.1:8899"},
		},
		{
			name:          "loopback IPv6",
			bind:          "::1",
			wantAddresses: []string{"[::1]:8899"},
		},
		{
			name:          "wildcard IPv6",
			bind:          "::",
			wantAddresses: []string{"[::]:8899"},
		},
		{
			name:          "exact IPv6",
			bind:          "2001:db8::10",
			wantAddresses: []string{"[2001:db8::10]:8899", "[::1]:8899"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var gotAddresses []string
			listen := func(_, address string) (net.Listener, error) {
				gotAddresses = append(gotAddresses, address)
				addr, err := net.ResolveTCPAddr("tcp", address)
				if err != nil {
					return nil, err
				}
				return &stubRPCListener{addr: addr}, nil
			}

			listeners, err := openRPCListeners(test.bind, net.ParseIP(test.bind), 8899, listen)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotAddresses, test.wantAddresses) {
				t.Fatalf("listen addresses = %v, want %v", gotAddresses, test.wantAddresses)
			}
			if len(listeners) != len(test.wantAddresses) {
				t.Fatalf("listener count = %d, want %d", len(listeners), len(test.wantAddresses))
			}
		})
	}
}

func TestOpenRPCListenersClosesPrimaryWhenCompanionFails(t *testing.T) {
	primary := &stubRPCListener{addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 8899}}
	calls := 0
	listen := func(_, _ string) (net.Listener, error) {
		calls++
		if calls == 1 {
			return primary, nil
		}
		return nil, errors.New("companion unavailable")
	}

	if _, err := openRPCListeners("192.0.2.10", net.ParseIP("192.0.2.10"), 8899, listen); err == nil {
		t.Fatal("expected companion listener failure")
	}
	if !primary.closed {
		t.Fatal("primary listener remained open after companion failure")
	}
}

func TestShutdownClosesEveryRPCListener(t *testing.T) {
	listeners := make([]rpcBoundListener, 0, 2)
	servers := make([]*http.Server, 0, 2)
	for range 2 {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		bound := rpcBoundListener{listener: listener, bindIP: net.ParseIP(DefaultRPCBindAddress)}
		listeners = append(listeners, bound)
		servers = append(servers, newRPCHTTPServer(http.NotFoundHandler()))
	}
	rpcServer := &RpcServer{listeners: listeners, httpServers: servers}
	rpcServer.Start()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := rpcServer.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	for _, bound := range listeners {
		conn, err := net.DialTimeout("tcp", bound.listener.Addr().String(), 50*time.Millisecond)
		if err == nil {
			conn.Close()
			t.Fatalf("listener %s accepted a connection after shutdown", bound.listener.Addr())
		}
	}
}

func TestClusterRefreshHasBoundedLifetime(t *testing.T) {
	rpcServer := rpcHandlerTestServer()
	rpcServer.clusterNodesRefreshTimeout = 20 * time.Millisecond
	rpcServer.clusterNodesFetcher = func(ctx context.Context) ([]*solanarpc.GetClusterNodesResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	started := time.Now()
	err := rpcServer.refreshLeaderTPUCacheBounded(context.Background())

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("refresh error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded refresh took %v", elapsed)
	}
}

func TestShutdownCancelsClusterRefreshAndTicker(t *testing.T) {
	rpcServer := rpcHandlerTestServer()
	rpcServer.clusterNodesRefreshEvery = 5 * time.Millisecond
	rpcServer.clusterNodesRefreshTimeout = time.Minute
	started := make(chan struct{})
	stopped := make(chan struct{})
	var calls atomic.Int32
	rpcServer.clusterNodesFetcher = func(ctx context.Context) ([]*solanarpc.GetClusterNodesResult, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			close(stopped)
			return nil, ctx.Err()
		}
		return nil, errors.New("unexpected refresh after cancellation")
	}

	rpcServer.Start()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("cluster refresh did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := rpcServer.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("cluster refresh was not canceled before Shutdown returned")
	}

	callCount := calls.Load()
	time.Sleep(20 * time.Millisecond)
	if got := calls.Load(); got != callCount {
		t.Fatalf("refresh ticker survived shutdown: calls changed from %d to %d", callCount, got)
	}
}

func TestShutdownBeforeStartPreventsClusterRefresh(t *testing.T) {
	rpcServer := rpcHandlerTestServer()
	called := make(chan struct{}, 1)
	rpcServer.clusterNodesFetcher = func(context.Context) ([]*solanarpc.GetClusterNodesResult, error) {
		called <- struct{}{}
		return nil, nil
	}

	if err := rpcServer.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	rpcServer.Start()
	select {
	case <-called:
		t.Fatal("cluster refresh started after Shutdown")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestLocalRPCAddress(t *testing.T) {
	tests := []struct {
		name string
		bind string
		want string
	}{
		{name: "default", want: "127.0.0.1:8899"},
		{name: "specific IPv4", bind: "192.0.2.10", want: "127.0.0.1:8899"},
		{name: "wildcard IPv4", bind: "0.0.0.0", want: "127.0.0.1:8899"},
		{name: "specific IPv6", bind: "2001:db8::10", want: "[::1]:8899"},
		{name: "wildcard IPv6", bind: "::", want: "[::1]:8899"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := LocalRPCAddress(test.bind, "8899"); got != test.want {
				t.Fatalf("LocalRPCAddress(%q) = %q, want %q", test.bind, got, test.want)
			}
		})
	}
}

func TestLocalRPCURLDisabled(t *testing.T) {
	if got := LocalRPCURL(DefaultRPCBindAddress, 0); got != "" {
		t.Fatalf("LocalRPCURL with disabled port = %q, want empty", got)
	}
}
