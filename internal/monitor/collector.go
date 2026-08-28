package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	maxProbeResponseBytes = 64 << 10
	maxExactMetricInteger = uint64(1<<53 - 1)
)

// Collector probes the node and both reference providers. This path must keep
// working when in-process diagnostics or the entire node host is unavailable.
type Collector struct {
	cfg     Config
	metrics *Metrics
	client  *http.Client
	now     func() time.Time

	manifestMu     sync.Mutex
	manifestPinned bool
	manifestID     string
	manifestDigest [32]byte
}

// New builds a collector. The HTTP client is bounded so one slow endpoint
// cannot stall the cycle that is meant to report it as unreachable.
func New(cfg Config, metrics *Metrics) *Collector {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{}
	}
	transport := base.Clone()
	// Endpoints come from the protected config. Ambient proxy variables must
	// not redirect those credential-bearing requests.
	transport.Proxy = nil
	metrics.configureIdentity(cfg.DeploymentID, cfg.SystemdUnit, cfg.SystemdScope)
	return &Collector{
		cfg:     cfg,
		metrics: metrics,
		client: &http.Client{
			Timeout:   cfg.ProbeTimeout(),
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		now: time.Now,
	}
}

// probeResult is one endpoint observation.
type probeResult struct {
	slot uint64
	ok   bool
}

// ProbeOnce probes a single endpoint and returns its slot.
//
// The node and the providers are asked DIFFERENT questions on purpose. Mithril
// does not register getSlot, and its getEpochInfo returns local state without
// commitment semantics, so the node is asked for getEpochInfo.absoluteSlot and
// reported as local_replay. Providers are asked getSlot at confirmed. Treating
// the two as the same measurement would compare a local replay position against
// a network commitment and call the difference lag.
func (c *Collector) ProbeOnce(ctx context.Context, url string, method string) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.ProbeTimeout())
	defer cancel()

	var params any = []any{}
	switch method {
	case "getSlot":
		params = []any{map[string]string{"commitment": CommitmentConfirmed}}
	case "getEpochInfo":
	default:
		return 0, errors.New("probe method is unsupported")
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	if err != nil {
		return 0, errors.New("probe request could not be encoded")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		// The error from url parsing embeds the URL, which carries the API key.
		return 0, errors.New("probe request could not be created")
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		// net/http errors quote the full URL including its query string.
		return 0, errors.New("probe request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("probe returned HTTP %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeResponseBytes+1))
	if err != nil {
		return 0, errors.New("probe response could not be read")
	}
	if len(raw) > maxProbeResponseBytes {
		return 0, errors.New("probe response was too large")
	}
	return decodeSlot(raw, method)
}

// decodeSlot extracts the slot from a JSON-RPC response. Malformed and
// error responses are rejected rather than yielding a zero slot, which would
// look like a node stuck at genesis.
func decodeSlot(raw []byte, method string) (uint64, error) {
	if !utf8.Valid(raw) {
		return 0, errors.New("probe response was not valid JSON")
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return 0, errors.New("probe response was not valid JSON")
	}
	var envelope struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil || requireJSONEOF(decoder) != nil {
		return 0, errors.New("probe response was not valid JSON")
	}
	if envelope.JSONRPC != "2.0" || !bytes.Equal(bytes.TrimSpace(envelope.ID), []byte("1")) {
		return 0, errors.New("probe response carried an invalid JSON-RPC envelope")
	}
	hasResult := len(envelope.Result) != 0
	hasError := len(envelope.Error) != 0
	if hasResult == hasError {
		return 0, errors.New("probe response must carry exactly one of result or error")
	}
	if hasError {
		if bytes.Equal(bytes.TrimSpace(envelope.Error), []byte("null")) {
			return 0, errors.New("probe response carried a malformed JSON-RPC error")
		}
		var rpcError struct {
			Code    *int            `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data,omitempty"`
		}
		errorDecoder := json.NewDecoder(bytes.NewReader(envelope.Error))
		errorDecoder.DisallowUnknownFields()
		if err := errorDecoder.Decode(&rpcError); err != nil || requireJSONEOF(errorDecoder) != nil {
			return 0, errors.New("probe response carried a malformed JSON-RPC error")
		}
		if rpcError.Code == nil {
			return 0, errors.New("probe response carried a malformed JSON-RPC error")
		}
		return 0, fmt.Errorf("probe returned JSON-RPC error code %d", *rpcError.Code)
	}

	if method == "getSlot" {
		var slot *uint64
		if err := json.Unmarshal(envelope.Result, &slot); err != nil {
			return 0, errors.New("getSlot did not return a number")
		}
		if slot == nil {
			return 0, errors.New("getSlot returned null")
		}
		return checkedSlot(*slot)
	}

	var info struct {
		AbsoluteSlot *uint64 `json:"absoluteSlot"`
	}
	if err := json.Unmarshal(envelope.Result, &info); err != nil {
		return 0, errors.New("getEpochInfo did not return an object")
	}
	if info.AbsoluteSlot == nil {
		return 0, errors.New("getEpochInfo carried no absoluteSlot")
	}
	return checkedSlot(*info.AbsoluteSlot)
}

func checkedSlot(slot uint64) (uint64, error) {
	if slot > maxExactMetricInteger {
		return 0, errors.New("probe slot exceeds the exact metric range")
	}
	return slot, nil
}

// Collect runs one full cycle and publishes every metric.
//
// A failed probe leaves the previous slot value untouched and sets its success
// gauge to 0. It never publishes a zero slot, because a zero would be
// indistinguishable from a node at genesis and would produce an enormous fake
// delta against a live provider.
func (c *Collector) Collect(ctx context.Context) {
	probes := []struct {
		role   string
		url    string
		method string
	}{
		{RoleNode, c.cfg.NodeRPCURL, "getEpochInfo"},
		{RoleReferencePrimary, c.cfg.ReferencePrimaryURL, "getSlot"},
		{RoleReferenceFallback, c.cfg.ReferenceFallbackURL, "getSlot"},
	}
	results := make([]probeResult, len(probes))
	var state stateResult

	var wg sync.WaitGroup
	wg.Add(len(probes) + 1)
	for i := range probes {
		i := i
		go func() {
			defer wg.Done()
			results[i] = c.probeRole(ctx, probes[i].url, probes[i].method)
		}()
	}
	go func() {
		defer wg.Done()
		state = c.collectState(ctx)
	}()
	wg.Wait()

	observedAt := c.now().Unix()
	for i, probe := range probes {
		c.publishProbeResult(probe.role, results[i], observedAt)
	}

	node, primary, fallback := results[0], results[1], results[2]

	if node.ok {
		c.metrics.NodeReplaySlot.WithLabelValues(NodeViewLocalReplay).Set(float64(node.slot))
	}
	c.metrics.setCollectSuccess(CollectorNodeRPC, node.ok)

	for _, ref := range []struct {
		label  string
		result probeResult
	}{{"primary", primary}, {"fallback", fallback}} {
		if !ref.result.ok {
			continue
		}
		c.metrics.ReferenceSlot.WithLabelValues(ref.label, CommitmentConfirmed).Set(float64(ref.result.slot))
		// The delta is computed only while BOTH probes are fresh, and is signed:
		// a negative value means the node is ahead of the reference, which is
		// real evidence and must not be clamped away.
		if node.ok {
			delta := int64(ref.result.slot) - int64(node.slot)
			c.metrics.NodeSlotDelta.WithLabelValues(ref.label, NodeViewLocalReplay, CommitmentConfirmed).
				Set(float64(delta))
		}
	}
	c.metrics.setCollectSuccess(CollectorReferenceRPC, primary.ok || fallback.ok)

	// Disagreement is only meaningful when both references answered.
	if primary.ok && fallback.ok {
		c.metrics.ReferenceDisagree.WithLabelValues(CommitmentConfirmed).Set(float64(absDiff(primary.slot, fallback.slot)))
	}
	c.metrics.publishState(state.manifest, state.filesystems, state.collectValid)
	c.metrics.LastCollectionAt.Set(float64(observedAt))
}

// probeRole probes one endpoint without publishing partial cycle state.
func (c *Collector) probeRole(ctx context.Context, url, method string) probeResult {
	slot, err := c.ProbeOnce(ctx, url, method)
	if err != nil {
		return probeResult{}
	}
	return probeResult{slot: slot, ok: true}
}

func (c *Collector) publishProbeResult(role string, result probeResult, observedAt int64) {
	c.metrics.ProbeSuccess.WithLabelValues(role).Set(boolGauge(result.ok))
	if result.ok {
		c.metrics.ProbeLastSuccessAt.WithLabelValues(role).Set(float64(observedAt))
	}
}

func boolGauge(v bool) float64 {
	if v {
		return 1
	}
	return 0
}

func absDiff(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return b - a
}
