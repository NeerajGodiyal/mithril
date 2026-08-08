// Package monitor implements the off-host deterministic monitor. It observes
// node and reference-provider reachability and signed slot-position differences
// without involving MCP, a model, or anything running on the node host.
package monitor

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Overclock-Validator/mithril/internal/safefile"
	toml "github.com/pelletier/go-toml/v2"
)

// DefaultConfigPath is the runtime location the monitor reads. Locations are
// read only from this file, never from flags or the environment, so a running
// monitor cannot be redirected at another host by a caller.
const DefaultConfigPath = "/etc/mithril-monitor/providers.toml"

const (
	// DefaultProbeTimeout bounds a single probe. Every probe is bounded so one
	// unresponsive endpoint cannot stall the collection cycle that is supposed
	// to report it as down.
	DefaultProbeTimeout = 5 * time.Second
	maxConfigBytes      = 64 << 10
)

var systemdUnitPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@:-]{0,126}\.service$`)

// Config is the monitor's runtime configuration. DeploymentID and the systemd
// fields are bounded, nonsecret identity. URLs are secrets in practice —
// provider endpoints carry API keys — so they never appear in labels or
// errors.
type Config struct {
	DeploymentID           string `toml:"deployment_id"`
	SystemdUnit            string `toml:"systemd_unit"`
	SystemdScope           string `toml:"systemd_scope"`
	NodeRPCURL             string `toml:"node_rpc_url"`
	ReferencePrimaryURL    string `toml:"reference_primary_url"`
	ReferenceFallbackURL   string `toml:"reference_fallback_url"`
	NodeExporterMetricsURL string `toml:"node_exporter_metrics_url"`
	InventoryManifestPath  string `toml:"inventory_manifest_path"`
	InventorySignaturePath string `toml:"inventory_signature_path"`
	InventoryPublicKeyPath string `toml:"inventory_public_key_path"`
	ProbeTimeoutSeconds    int    `toml:"probe_timeout_seconds"`
}

// ProbeTimeout returns the bounded per-probe timeout.
func (c Config) ProbeTimeout() time.Duration {
	if c.ProbeTimeoutSeconds <= 0 {
		return DefaultProbeTimeout
	}
	return time.Duration(c.ProbeTimeoutSeconds) * time.Second
}

// LoadConfig reads and validates the runtime configuration file.
//
// The file must not be group- or other-readable: it holds provider URLs with
// embedded API keys, and a monitor that silently reads a world-readable
// credentials file would defeat the point of storing them in a file at all.
func LoadConfig(path string) (Config, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return Config{}, errors.New("monitor config path must be a clean absolute path")
	}

	data, err := readConfigFile(path)
	if err != nil {
		return Config{}, err
	}

	var cfg Config
	decoder := toml.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		// The decoder error can quote file content, which is exactly the
		// credential-bearing text this file exists to protect.
		return Config{}, errors.New("monitor config file is malformed")
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func readConfigFile(path string) ([]byte, error) {
	// Provider endpoints, which carry API keys in their query strings.
	data, err := safefile.ReadTrustedRegular(path, safefile.ReadOptions{
		MaxBytes:               maxConfigBytes,
		ForbiddenPerm:          0o077,
		RejectAncestorSymlinks: true,
	})
	if err != nil {
		return nil, fmt.Errorf("monitor config file: %w", err)
	}
	return data, nil
}

// validate rejects a configuration that cannot produce meaningful evidence.
// Error text names the FIELD, never the value, so a misconfigured URL carrying
// a key is not echoed into logs.
func (c Config) validate() error {
	if len(c.DeploymentID) == 0 || len(c.DeploymentID) > maxManifestIDBytes ||
		!manifestIDPattern.MatchString(c.DeploymentID) {
		return errors.New("monitor config deployment_id is invalid")
	}
	if !systemdUnitPattern.MatchString(c.SystemdUnit) {
		return errors.New("monitor config systemd_unit must name one .service unit")
	}
	if c.SystemdScope != "system" {
		return errors.New("monitor config systemd_scope must be system")
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"node_rpc_url", c.NodeRPCURL},
		{"reference_primary_url", c.ReferencePrimaryURL},
		{"reference_fallback_url", c.ReferenceFallbackURL},
		{"node_exporter_metrics_url", c.NodeExporterMetricsURL},
	} {
		if field.value == "" {
			return fmt.Errorf("monitor config %s is required", field.name)
		}
		if !isHTTPURL(field.value) {
			return fmt.Errorf("monitor config %s must be an http or https URL", field.name)
		}
	}
	if !isNodeExporterMetricsURL(c.NodeExporterMetricsURL) {
		return errors.New("monitor config node_exporter_metrics_url must be a private metrics URL")
	}
	seenPaths := map[string]bool{}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"inventory_manifest_path", c.InventoryManifestPath},
		{"inventory_signature_path", c.InventorySignaturePath},
		{"inventory_public_key_path", c.InventoryPublicKeyPath},
	} {
		if field.value == "" || !filepath.IsAbs(field.value) || filepath.Clean(field.value) != field.value {
			return fmt.Errorf("monitor config %s must be a clean absolute path", field.name)
		}
		if seenPaths[field.value] {
			return errors.New("monitor config inventory paths must be distinct")
		}
		seenPaths[field.value] = true
	}
	if c.ProbeTimeoutSeconds < 0 || c.ProbeTimeoutSeconds > 60 {
		return errors.New("monitor config probe_timeout_seconds must be between 0 and 60")
	}
	node, _ := url.Parse(c.NodeRPCURL)
	exporter, _ := url.Parse(c.NodeExporterMetricsURL)
	nodeHost := net.ParseIP(node.Hostname())
	exporterHost := net.ParseIP(exporter.Hostname())
	if nodeHost == nil || exporterHost == nil ||
		!isPrivateNetworkIP(nodeHost) || !isPrivateNetworkIP(exporterHost) ||
		!nodeHost.Equal(exporterHost) {
		return errors.New("monitor config node RPC and node_exporter must use the same numeric private host")
	}
	primary, _ := url.Parse(c.ReferencePrimaryURL)
	fallback, _ := url.Parse(c.ReferenceFallbackURL)
	if primary.Scheme != "https" && !isLoopbackHost(primary.Hostname()) {
		return errors.New("monitor config reference_primary_url must use https outside loopback tests")
	}
	if fallback.Scheme != "https" && !isLoopbackHost(fallback.Hostname()) {
		return errors.New("monitor config reference_fallback_url must use https outside loopback tests")
	}
	if normalizedHostname(primary.Hostname()) == normalizedHostname(fallback.Hostname()) {
		return errors.New("monitor config reference providers must use different hosts")
	}
	// Distinct hosts catch accidental endpoint duplication. Organizational
	// independence is a deployment property and cannot be inferred from URLs.
	return nil
}

func normalizedHostname(host string) string {
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

func isNodeExporterMetricsURL(raw string) bool {
	if !isHTTPURL(raw) {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil ||
		parsed.User != nil ||
		parsed.RawQuery != "" ||
		parsed.RawPath != "" ||
		parsed.Path != "/metrics" {
		return false
	}
	return isPrivateNetworkHost(parsed.Hostname())
}

func isPrivateNetworkHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return isPrivateNetworkIP(ip)
}

func isPrivateNetworkIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return true
	}
	// Tailscale and some provider-private networks use the shared address
	// space from RFC 6598.
	_, shared, _ := net.ParseCIDR("100.64.0.0/10")
	return shared.Contains(ip)
}

func isHTTPURL(raw string) bool {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" || parsed.Fragment != "" {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	return parsed.Host != "" && parsed.Hostname() != ""
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
