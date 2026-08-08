package monitor

import (
	"bytes"
	"context"
	"io"
	"math"
	"net/http"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

const (
	maxNodeExporterResponseBytes = 4 << 20
	maxNodeExporterLines         = 50_000
)

type filesystemMeasurement struct {
	available float64
	size      float64
}

type stateResult struct {
	manifest     *DeploymentManifest
	filesystems  map[string]filesystemMeasurement
	collectValid bool
}

func (c *Collector) collectState(ctx context.Context) stateResult {
	signed, err := loadSignedManifest(
		c.cfg.InventoryManifestPath,
		c.cfg.InventorySignaturePath,
		c.cfg.InventoryPublicKeyPath,
	)
	if err != nil ||
		signed.manifest.ManifestID != c.cfg.DeploymentID ||
		!c.acceptManifest(signed) {
		return stateResult{}
	}

	filesystems, complete := c.fetchFilesystemSnapshot(ctx, signed.manifest.Filesystems)
	manifest := signed.manifest
	return stateResult{
		manifest:     &manifest,
		filesystems:  filesystems,
		collectValid: complete,
	}
}

// acceptManifest pins the signed bytes for the lifetime of the process.
// Deployment changes therefore require a deliberate monitor restart instead
// of silently changing the inventory underneath alert evaluation.
func (c *Collector) acceptManifest(candidate signedManifest) bool {
	c.manifestMu.Lock()
	defer c.manifestMu.Unlock()
	if !c.manifestPinned {
		c.manifestPinned = true
		c.manifestID = candidate.manifest.ManifestID
		c.manifestDigest = candidate.digest
		return true
	}
	return c.manifestID == candidate.manifest.ManifestID &&
		c.manifestDigest == candidate.digest
}

func (c *Collector) fetchFilesystemSnapshot(
	ctx context.Context,
	expected []ExpectedFilesystem,
) (map[string]filesystemMeasurement, bool) {
	if !isNodeExporterMetricsURL(c.cfg.NodeExporterMetricsURL) {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.ProbeTimeout())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.NodeExporterMetricsURL, nil)
	if err != nil {
		return nil, false
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxNodeExporterResponseBytes+1))
	if err != nil || len(raw) > maxNodeExporterResponseBytes {
		return nil, false
	}
	if bytes.Count(raw, []byte{'\n'}) > maxNodeExporterLines {
		return nil, false
	}

	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(bytes.NewReader(raw))
	if err != nil {
		return nil, false
	}
	return mapFilesystemSnapshot(families, expected)
}

func mapFilesystemSnapshot(
	families map[string]*dto.MetricFamily,
	expected []ExpectedFilesystem,
) (map[string]filesystemMeasurement, bool) {
	requested := make(map[string]struct{}, len(expected))
	for _, filesystem := range expected {
		requested[filesystem.Mountpoint] = struct{}{}
	}
	available, availableBad := filesystemValues(
		families["node_filesystem_avail_bytes"],
		requested,
	)
	size, sizeBad := filesystemValues(
		families["node_filesystem_size_bytes"],
		requested,
	)

	out := make(map[string]filesystemMeasurement, len(expected))
	complete := true
	for _, filesystem := range expected {
		mountpoint := filesystem.Mountpoint
		availableValue, availableOK := available[mountpoint]
		sizeValue, sizeOK := size[mountpoint]
		valid := availableOK &&
			sizeOK &&
			!availableBad[mountpoint] &&
			!sizeBad[mountpoint] &&
			sizeValue > 0 &&
			availableValue <= sizeValue
		if !valid {
			if filesystem.Required {
				complete = false
			}
			continue
		}
		out[filesystem.Role] = filesystemMeasurement{
			available: availableValue,
			size:      sizeValue,
		}
	}
	return out, complete
}

func filesystemValues(
	family *dto.MetricFamily,
	requested map[string]struct{},
) (map[string]float64, map[string]bool) {
	values := make(map[string]float64, len(requested))
	bad := make(map[string]bool)
	if family == nil {
		return values, bad
	}
	for _, metric := range family.Metric {
		mountpoint, ok := exactLabel(metric, "mountpoint")
		if !ok {
			continue
		}
		if _, wanted := requested[mountpoint]; !wanted {
			continue
		}
		value, ok := gaugeValue(metric)
		if !ok ||
			math.IsNaN(value) ||
			math.IsInf(value, 0) ||
			value < 0 ||
			value > float64(maxExactMetricInteger) ||
			math.Trunc(value) != value {
			bad[mountpoint] = true
			delete(values, mountpoint)
			continue
		}
		if _, duplicate := values[mountpoint]; duplicate {
			bad[mountpoint] = true
			delete(values, mountpoint)
			continue
		}
		if bad[mountpoint] {
			continue
		}
		values[mountpoint] = value
	}
	return values, bad
}

func exactLabel(metric *dto.Metric, name string) (string, bool) {
	var value string
	found := false
	for _, label := range metric.Label {
		if label.GetName() != name {
			continue
		}
		if found {
			return "", false
		}
		found = true
		value = label.GetValue()
	}
	return value, found
}

func gaugeValue(metric *dto.Metric) (float64, bool) {
	if metric == nil || metric.Gauge == nil || metric.Gauge.Value == nil {
		return 0, false
	}
	return metric.Gauge.GetValue(), true
}
