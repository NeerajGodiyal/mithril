package monitor

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type inventoryFixture struct {
	dir           string
	manifestPath  string
	signaturePath string
	publicKeyPath string
	privateKey    ed25519.PrivateKey
	raw           []byte
}

func newInventoryFixture(t *testing.T) *inventoryFixture {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(validManifestWire())
	if err != nil {
		t.Fatal(err)
	}
	dir := trustedTempDir(t)
	fixture := &inventoryFixture{
		dir:           dir,
		manifestPath:  filepath.Join(dir, "inventory.json"),
		signaturePath: filepath.Join(dir, "inventory.sig"),
		publicKeyPath: filepath.Join(dir, "inventory.pem"),
		privateKey:    privateKey,
		raw:           raw,
	}
	fixture.writeRaw(t, raw)
	if err := os.WriteFile(
		fixture.publicKeyPath,
		pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *inventoryFixture) writeRaw(t *testing.T, raw []byte) {
	t.Helper()
	f.raw = append([]byte(nil), raw...)
	if err := os.WriteFile(f.manifestPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.signaturePath, ed25519.Sign(f.privateKey, raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func boolPointer(value bool) *bool {
	return &value
}

func validManifestWire() manifestWire {
	targets := make([]targetWire, 0, len(expectedTargetJobs))
	for _, job := range expectedTargetJobs {
		required := job != TargetAgent
		targets = append(targets, targetWire{TargetJob: job, Required: boolPointer(required)})
	}
	return manifestWire{
		SchemaVersion: inventorySchemaVersion,
		ManifestID:    "deployment-a",
		Targets:       targets,
		Filesystems: []filesystemWire{
			{Role: FilesystemRoot, Mountpoint: "/", Required: boolPointer(true)},
			{Role: FilesystemAccounts, Mountpoint: "/mnt/accounts", Required: boolPointer(true)},
			{Role: FilesystemLedger, Mountpoint: "/mnt/ledger", Required: boolPointer(true)},
		},
	}
}

func TestLoadSignedManifest(t *testing.T) {
	fixture := newInventoryFixture(t)
	got, err := loadSignedManifest(fixture.manifestPath, fixture.signaturePath, fixture.publicKeyPath)
	if err != nil {
		t.Fatalf("load signed manifest: %v", err)
	}
	if got.manifest.SchemaVersion != 1 || got.manifest.ManifestID != "deployment-a" {
		t.Fatalf("manifest identity = %+v", got.manifest)
	}
	if len(got.manifest.Targets) != 8 || len(got.manifest.Filesystems) != 3 {
		t.Fatalf("inventory sizes = %d targets, %d filesystems", len(got.manifest.Targets), len(got.manifest.Filesystems))
	}
	for _, target := range got.manifest.Targets {
		if target.TargetJob == TargetAgent && target.Required {
			t.Fatal("the pre-Stage-4 agent target was not preserved as optional")
		}
	}
}

func TestSignedManifestRejectsCryptographicAndFileFailures(t *testing.T) {
	t.Run("tampered manifest", func(t *testing.T) {
		fixture := newInventoryFixture(t)
		if err := os.WriteFile(fixture.manifestPath, append(fixture.raw, ' '), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadFixture(fixture); err == nil {
			t.Fatal("tampered manifest was accepted")
		}
	})
	t.Run("wrong key", func(t *testing.T) {
		fixture := newInventoryFixture(t)
		otherPublic, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		der, _ := x509.MarshalPKIXPublicKey(otherPublic)
		if err := os.WriteFile(fixture.publicKeyPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadFixture(fixture); err == nil {
			t.Fatal("signature verified with the wrong key")
		}
	})
	t.Run("malformed public key", func(t *testing.T) {
		fixture := newInventoryFixture(t)
		if err := os.WriteFile(fixture.publicKeyPath, []byte("not pem"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadFixture(fixture); err == nil {
			t.Fatal("malformed public key was accepted")
		}
	})
	t.Run("extra PEM block", func(t *testing.T) {
		fixture := newInventoryFixture(t)
		raw, _ := os.ReadFile(fixture.publicKeyPath)
		if err := os.WriteFile(fixture.publicKeyPath, append(raw, raw...), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadFixture(fixture); err == nil {
			t.Fatal("multiple public keys were accepted")
		}
	})
	t.Run("short signature", func(t *testing.T) {
		fixture := newInventoryFixture(t)
		if err := os.WriteFile(fixture.signaturePath, make([]byte, ed25519.SignatureSize-1), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadFixture(fixture); err == nil {
			t.Fatal("short signature was accepted")
		}
	})
	t.Run("symlinks", func(t *testing.T) {
		for _, field := range []string{"manifest", "signature", "public key"} {
			t.Run(field, func(t *testing.T) {
				fixture := newInventoryFixture(t)
				path := map[string]string{
					"manifest":   fixture.manifestPath,
					"signature":  fixture.signaturePath,
					"public key": fixture.publicKeyPath,
				}[field]
				link := filepath.Join(fixture.dir, strings.ReplaceAll(field, " ", "-")+".link")
				if err := os.Symlink(path, link); err != nil {
					t.Fatal(err)
				}
				switch field {
				case "manifest":
					fixture.manifestPath = link
				case "signature":
					fixture.signaturePath = link
				default:
					fixture.publicKeyPath = link
				}
				if _, err := loadFixture(fixture); err == nil {
					t.Fatal("symlinked inventory input was accepted")
				}
			})
		}
	})
	t.Run("unsafe write permissions", func(t *testing.T) {
		fixture := newInventoryFixture(t)
		if err := os.Chmod(fixture.publicKeyPath, 0o620); err != nil {
			t.Fatal(err)
		}
		if _, err := loadFixture(fixture); err == nil {
			t.Fatal("group-writable trust anchor was accepted")
		}
	})
	t.Run("untrusted owner", func(t *testing.T) {
		fixture := newInventoryFixture(t)
		if err := os.Chown(fixture.publicKeyPath, 65534, -1); err != nil {
			if errors.Is(err, os.ErrPermission) {
				t.Skip("changing file ownership requires elevated test permissions")
			}
			t.Fatal(err)
		}
		if _, err := loadFixture(fixture); err == nil {
			t.Fatal("untrusted public-key owner was accepted")
		}
	})
	t.Run("unclean path", func(t *testing.T) {
		fixture := newInventoryFixture(t)
		fixture.manifestPath = fixture.dir + "/./inventory.json"
		if _, err := loadFixture(fixture); err == nil {
			t.Fatal("unclean inventory path was accepted")
		}
	})
}

func TestSignedManifestStrictValidation(t *testing.T) {
	cases := map[string]func(*manifestWire){
		"wrong schema":    func(w *manifestWire) { w.SchemaVersion = 2 },
		"bad manifest id": func(w *manifestWire) { w.ManifestID = "bad id" },
		"missing target":  func(w *manifestWire) { w.Targets = w.Targets[:7] },
		"duplicate target": func(w *manifestWire) {
			w.Targets[7].TargetJob = w.Targets[0].TargetJob
		},
		"unknown target": func(w *manifestWire) { w.Targets[7].TargetJob = "other" },
		"missing target required": func(w *manifestWire) {
			w.Targets[0].Required = nil
		},
		"optional non-agent": func(w *manifestWire) {
			w.Targets[0].Required = boolPointer(false)
		},
		"missing filesystem": func(w *manifestWire) {
			w.Filesystems = w.Filesystems[:2]
		},
		"duplicate filesystem": func(w *manifestWire) {
			w.Filesystems[2].Role = w.Filesystems[1].Role
		},
		"unknown filesystem": func(w *manifestWire) {
			w.Filesystems[2].Role = "cache"
		},
		"optional filesystem": func(w *manifestWire) {
			w.Filesystems[1].Required = boolPointer(false)
		},
		"relative mountpoint": func(w *manifestWire) {
			w.Filesystems[1].Mountpoint = "accounts"
		},
		"unclean mountpoint": func(w *manifestWire) {
			w.Filesystems[1].Mountpoint = "/mnt/../accounts"
		},
		"wrong root mountpoint": func(w *manifestWire) {
			w.Filesystems[0].Mountpoint = "/rootfs"
		},
		"overlong mountpoint": func(w *manifestWire) {
			w.Filesystems[1].Mountpoint = "/" + strings.Repeat("a", maxMountpointBytes)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newInventoryFixture(t)
			wire := validManifestWire()
			mutate(&wire)
			raw, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			fixture.writeRaw(t, raw)
			if _, err := loadFixture(fixture); err == nil {
				t.Fatal("invalid manifest was accepted")
			}
		})
	}

	for name, raw := range map[string][]byte{
		"unknown field": append([]byte(`{"unknown":true,`), validJSONWithoutOpeningBrace(t)...),
		"duplicate key": []byte(strings.Replace(
			string(mustJSON(t, validManifestWire())),
			`"manifest_id":"deployment-a"`,
			`"manifest_id":"deployment-a","manifest_id":"deployment-a"`,
			1,
		)),
		"trailing value": append(mustJSON(t, validManifestWire()), []byte(` {}`)...),
		"invalid UTF-8":  append(mustJSON(t, validManifestWire()), 0xff),
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newInventoryFixture(t)
			fixture.writeRaw(t, raw)
			if _, err := loadFixture(fixture); err == nil {
				t.Fatal("malformed signed JSON was accepted")
			}
		})
	}
}

func TestSignedManifestSizeBound(t *testing.T) {
	fixture := newInventoryFixture(t)
	raw := append(fixture.raw, bytes.Repeat([]byte{' '}, maxInventoryBytes)...)
	fixture.writeRaw(t, raw)
	if _, err := loadFixture(fixture); err == nil {
		t.Fatal("oversized signed manifest was accepted")
	}
}

func TestStateCollectionPublishesSignedInventoryAndRemoteFilesystems(t *testing.T) {
	fixture := newInventoryFixture(t)
	server := nodeExporterServer(t, validNodeExporterMetrics())
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)
	collector := New(stateConfig(fixture, server.URL), metrics)

	result := collector.collectState(context.Background())
	if !result.collectValid || result.manifest == nil || len(result.filesystems) != 3 {
		t.Fatalf("state result = %+v", result)
	}
	metrics.publishState(result.manifest, result.filesystems, result.collectValid)

	if got := metricValues(t, registry, "mithril_expected_target"); len(got) != 8 {
		t.Fatalf("expected target rows = %d, want 8: %v", len(got), got)
	} else {
		for labels := range got {
			for _, label := range strings.Split(labels, ",") {
				if strings.HasPrefix(label, "job=") {
					t.Fatalf("inventory used the reserved job label: %q", labels)
				}
			}
		}
	}
	if got := metricValues(t, registry, "mithril_expected_filesystem_role"); len(got) != 3 {
		t.Fatalf("expected filesystem rows = %d, want 3: %v", len(got), got)
	}
	if got := metricValues(t, registry, "mithril_filesystem_avail_bytes"); len(got) != 3 ||
		got["role=root"] != 900 || got["role=accounts"] != 800 || got["role=ledger"] != 700 {
		t.Fatalf("available filesystem values = %v", got)
	}
	if got := metricValues(t, registry, "mithril_monitor_collect_success")["collector=state"]; got != 1 {
		t.Fatalf("state collector success = %v, want 1", got)
	}
	if got := metricValues(t, registry, "mithril_monitor_identity_info"); len(got) != 1 ||
		got["signed_deployment_id=deployment-a,systemd_scope=system,systemd_unit=mithril-mainnet.service"] != 1 {
		t.Fatalf("monitor identity = %v", got)
	}
}

func TestStateCollectionRejectsManifestForAnotherDeployment(t *testing.T) {
	fixture := newInventoryFixture(t)
	server := nodeExporterServer(t, validNodeExporterMetrics())
	registry := prometheus.NewRegistry()
	cfg := stateConfig(fixture, server.URL)
	cfg.DeploymentID = "deployment-b"
	collector := New(cfg, NewMetrics(registry))

	result := collector.collectState(context.Background())
	if result.collectValid || result.manifest != nil || len(result.filesystems) != 0 {
		t.Fatalf("foreign deployment manifest was accepted: %+v", result)
	}
	collector.metrics.publishState(result.manifest, result.filesystems, result.collectValid)
	if got := metricValues(t, registry, "mithril_monitor_identity_info"); len(got) != 0 {
		t.Fatalf("foreign identity was published: %v", got)
	}
}

func TestStateCollectionWithholdsOnlyBadRoleAndClearsStaleValues(t *testing.T) {
	fixture := newInventoryFixture(t)
	body := validNodeExporterMetrics()
	serverBody := body
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(serverBody))
	}))
	t.Cleanup(server.Close)

	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)
	collector := New(stateConfig(fixture, server.URL), metrics)
	first := collector.collectState(context.Background())
	metrics.publishState(first.manifest, first.filesystems, first.collectValid)
	if got := metricValues(t, registry, "mithril_filesystem_avail_bytes"); len(got) != 3 {
		t.Fatalf("baseline filesystem rows = %v", got)
	}

	serverBody = strings.ReplaceAll(body,
		`node_filesystem_avail_bytes{mountpoint="/mnt/ledger"} 700`+"\n",
		"",
	)
	second := collector.collectState(context.Background())
	if second.collectValid {
		t.Fatal("partial node-exporter response reported a complete state collection")
	}
	metrics.publishState(second.manifest, second.filesystems, second.collectValid)
	got := metricValues(t, registry, "mithril_filesystem_avail_bytes")
	if len(got) != 2 || got["role=ledger"] != 0 {
		t.Fatalf("partial snapshot retained a stale ledger value: %v", got)
	}
	if state := metricValues(t, registry, "mithril_monitor_collect_success")["collector=state"]; state != 0 {
		t.Fatalf("partial snapshot state = %v, want 0", state)
	}
}

func TestStateCollectionReverifiesAndPinsManifest(t *testing.T) {
	fixture := newInventoryFixture(t)
	server := nodeExporterServer(t, validNodeExporterMetrics())
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)
	collector := New(stateConfig(fixture, server.URL), metrics)

	first := collector.collectState(context.Background())
	metrics.publishState(first.manifest, first.filesystems, first.collectValid)
	if !first.collectValid {
		t.Fatal("initial signed manifest was not accepted")
	}

	wire := validManifestWire()
	wire.ManifestID = "deployment-b"
	fixture.writeRaw(t, mustJSON(t, wire))
	changed := collector.collectState(context.Background())
	if changed.collectValid || changed.manifest != nil {
		t.Fatal("a changed manifest was accepted without restarting the monitor")
	}
	metrics.publishState(changed.manifest, changed.filesystems, changed.collectValid)
	// The last verified inventory remains visible, but all measurements are
	// withheld and state=0 makes the failed re-verification explicit.
	if got := metricValues(t, registry, "mithril_expected_target"); len(got) != 8 {
		t.Fatalf("last verified inventory disappeared: %v", got)
	}
	if got := metricValues(t, registry, "mithril_filesystem_avail_bytes"); len(got) != 0 {
		t.Fatalf("measurements survived a manifest change: %v", got)
	}
}

func TestNodeExporterFailuresAreBoundedAndFailClosed(t *testing.T) {
	fixture := newInventoryFixture(t)
	expected := []ExpectedFilesystem{
		{Role: FilesystemRoot, Mountpoint: "/", Required: true},
		{Role: FilesystemAccounts, Mountpoint: "/mnt/accounts", Required: true},
		{Role: FilesystemLedger, Mountpoint: "/mnt/ledger", Required: true},
	}

	tests := []struct {
		name string
		body string
	}{
		{"malformed", `not prometheus`},
		{"missing family", `# TYPE node_filesystem_avail_bytes gauge
node_filesystem_avail_bytes{mountpoint="/"} 900
`},
		{"duplicate sample", validNodeExporterMetrics() +
			`node_filesystem_avail_bytes{mountpoint="/"} 900` + "\n"},
		{"available exceeds size", strings.Replace(
			validNodeExporterMetrics(),
			`node_filesystem_avail_bytes{mountpoint="/"} 900`,
			`node_filesystem_avail_bytes{mountpoint="/"} 1100`,
			1,
		)},
		{"fractional bytes", strings.Replace(
			validNodeExporterMetrics(),
			`node_filesystem_avail_bytes{mountpoint="/"} 900`,
			`node_filesystem_avail_bytes{mountpoint="/"} 900.5`,
			1,
		)},
		{"inexact bytes", strings.Replace(
			validNodeExporterMetrics(),
			`node_filesystem_size_bytes{mountpoint="/"} 1000`,
			`node_filesystem_size_bytes{mountpoint="/"} 9007199254740992`,
			1,
		)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := nodeExporterServer(t, test.body)
			collector := New(stateConfig(fixture, server.URL), NewMetrics(prometheus.NewRegistry()))
			if _, complete := collector.fetchFilesystemSnapshot(context.Background(), expected); complete {
				t.Fatal("bad node-exporter evidence reported complete")
			}
		})
	}

	t.Run("redirect", func(t *testing.T) {
		followed := false
		target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			followed = true
		}))
		t.Cleanup(target.Close)
		redirect := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusTemporaryRedirect))
		t.Cleanup(redirect.Close)
		collector := New(stateConfig(fixture, redirect.URL), NewMetrics(prometheus.NewRegistry()))
		if _, complete := collector.fetchFilesystemSnapshot(context.Background(), expected); complete {
			t.Fatal("redirected node-exporter response was accepted")
		}
		if followed {
			t.Fatal("node-exporter client followed a redirect")
		}
	})

	t.Run("HTTP error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(validNodeExporterMetrics()))
		}))
		t.Cleanup(server.Close)
		collector := New(stateConfig(fixture, server.URL), NewMetrics(prometheus.NewRegistry()))
		if _, complete := collector.fetchFilesystemSnapshot(context.Background(), expected); complete {
			t.Fatal("non-200 node-exporter response was accepted")
		}
	})

	t.Run("oversized", func(t *testing.T) {
		server := nodeExporterServer(t, strings.Repeat("x", maxNodeExporterResponseBytes+1))
		collector := New(stateConfig(fixture, server.URL), NewMetrics(prometheus.NewRegistry()))
		if _, complete := collector.fetchFilesystemSnapshot(context.Background(), expected); complete {
			t.Fatal("oversized node-exporter response was accepted")
		}
	})

	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(3 * time.Second):
				_, _ = w.Write([]byte(validNodeExporterMetrics()))
			}
		}))
		t.Cleanup(server.Close)
		cfg := stateConfig(fixture, server.URL)
		cfg.ProbeTimeoutSeconds = 1
		collector := New(cfg, NewMetrics(prometheus.NewRegistry()))
		start := time.Now()
		if _, complete := collector.fetchFilesystemSnapshot(context.Background(), expected); complete {
			t.Fatal("timed-out node-exporter response was accepted")
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("node-exporter timeout took %v", elapsed)
		}
	})
}

func TestStateMetricsConcurrentPublishAndGather(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)
	wire := validManifestWire()
	manifest, err := validateManifest(wire)
	if err != nil {
		t.Fatal(err)
	}
	measurements := map[string]filesystemMeasurement{
		FilesystemRoot:     {available: 900, size: 1000},
		FilesystemAccounts: {available: 800, size: 1000},
		FilesystemLedger:   {available: 700, size: 1000},
	}

	var wait sync.WaitGroup
	for i := 0; i < 4; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for j := 0; j < 100; j++ {
				metrics.publishState(&manifest, measurements, j%2 == 0)
				metrics.setCollectSuccess(CollectorNodeRPC, j%2 == 0)
			}
		}()
		wait.Add(1)
		go func() {
			defer wait.Done()
			for j := 0; j < 100; j++ {
				if _, err := registry.Gather(); err != nil {
					t.Errorf("concurrent gather: %v", err)
					return
				}
			}
		}()
	}
	wait.Wait()
}

func FuzzDecodeManifest(f *testing.F) {
	f.Add(mustJSONForFuzz(validManifestWire()))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"schema_version":1,"schema_version":1}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > maxInventoryBytes {
			t.Skip()
		}
		_, _ = decodeManifest(raw)
	})
}

func TestConfigValidatesInventoryPathsAndPrivateExporter(t *testing.T) {
	base := Config{
		DeploymentID:           "deployment-a",
		SystemdUnit:            "mithril-mainnet.service",
		SystemdScope:           "system",
		NodeRPCURL:             "http://127.0.0.1:8899",
		ReferencePrimaryURL:    "https://primary.example/rpc",
		ReferenceFallbackURL:   "https://fallback.example/rpc",
		NodeExporterMetricsURL: "http://127.0.0.1:9100/metrics",
		InventoryManifestPath:  "/etc/mithril-monitor/inventory.json",
		InventorySignaturePath: "/etc/mithril-monitor/inventory.sig",
		InventoryPublicKeyPath: "/etc/mithril-monitor/inventory.pem",
	}
	if err := base.validate(); err != nil {
		t.Fatalf("valid state config was rejected: %v", err)
	}
	for _, rawURL := range []string{
		"http://10.0.0.1:9100/metrics",
		"https://192.168.1.10/metrics",
		"http://100.64.0.1:9100/metrics",
		"http://[fd00::1]:9100/metrics",
	} {
		cfg := base
		cfg.NodeExporterMetricsURL = rawURL
		parsed, err := url.Parse(rawURL)
		if err != nil {
			t.Fatal(err)
		}
		cfg.NodeRPCURL = "http://" + net.JoinHostPort(parsed.Hostname(), "8899")
		if err := cfg.validate(); err != nil {
			t.Errorf("same-host private endpoints were rejected: %v", err)
		}
	}
	for name, mutate := range map[string]func(*Config){
		"missing deployment id": func(c *Config) { c.DeploymentID = "" },
		"bad deployment id":     func(c *Config) { c.DeploymentID = "bad id" },
		"bad systemd unit":      func(c *Config) { c.SystemdUnit = "mithril.target" },
		"user systemd scope":    func(c *Config) { c.SystemdScope = "user" },
		"mismatched node hosts": func(c *Config) {
			c.NodeExporterMetricsURL = "http://10.0.0.2:9100/metrics"
		},
		"DNS node host": func(c *Config) {
			c.NodeRPCURL = "https://node.example/rpc"
		},
		"same DNS node host": func(c *Config) {
			c.NodeRPCURL = "http://localhost:8899"
			c.NodeExporterMetricsURL = "http://localhost:9100/metrics"
		},
		"relative manifest": func(c *Config) { c.InventoryManifestPath = "inventory.json" },
		"unclean signature": func(c *Config) {
			c.InventorySignaturePath = "/etc/mithril-monitor/./inventory.sig"
		},
		"duplicate paths": func(c *Config) {
			c.InventorySignaturePath = c.InventoryManifestPath
		},
		"public plaintext exporter": func(c *Config) {
			c.NodeExporterMetricsURL = "http://203.0.113.1:9100/metrics"
		},
		"public plaintext node RPC": func(c *Config) {
			c.NodeRPCURL = "http://203.0.113.1:8899"
		},
		"public HTTPS exporter": func(c *Config) {
			c.NodeExporterMetricsURL = "https://node.example/metrics"
		},
		"exporter query": func(c *Config) {
			c.NodeExporterMetricsURL = "https://node.example/metrics?token=secret"
		},
		"wrong exporter path": func(c *Config) {
			c.NodeExporterMetricsURL = "https://node.example/other"
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := base
			mutate(&cfg)
			if err := cfg.validate(); err == nil {
				t.Fatal("invalid state config was accepted")
			}
		})
	}
}

func loadFixture(f *inventoryFixture) (signedManifest, error) {
	return loadSignedManifest(f.manifestPath, f.signaturePath, f.publicKeyPath)
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func mustJSONForFuzz(value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}

func validJSONWithoutOpeningBrace(t *testing.T) []byte {
	t.Helper()
	raw := mustJSON(t, validManifestWire())
	return raw[1:]
}

func stateConfig(f *inventoryFixture, exporterURL string) Config {
	if !strings.HasSuffix(exporterURL, "/metrics") {
		exporterURL += "/metrics"
	}
	return Config{
		DeploymentID:           "deployment-a",
		SystemdUnit:            "mithril-mainnet.service",
		SystemdScope:           "system",
		NodeExporterMetricsURL: exporterURL,
		InventoryManifestPath:  f.manifestPath,
		InventorySignaturePath: f.signaturePath,
		InventoryPublicKeyPath: f.publicKeyPath,
		ProbeTimeoutSeconds:    2,
	}
}

func nodeExporterServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func validNodeExporterMetrics() string {
	var builder strings.Builder
	builder.WriteString("# TYPE node_filesystem_avail_bytes gauge\n")
	for mountpoint, value := range map[string]float64{
		"/":             900,
		"/mnt/accounts": 800,
		"/mnt/ledger":   700,
	} {
		fmt.Fprintf(&builder, "node_filesystem_avail_bytes{mountpoint=%q} %v\n", mountpoint, value)
	}
	builder.WriteString("# TYPE node_filesystem_size_bytes gauge\n")
	for _, mountpoint := range []string{"/", "/mnt/accounts", "/mnt/ledger"} {
		fmt.Fprintf(&builder, "node_filesystem_size_bytes{mountpoint=%q} 1000\n", mountpoint)
	}
	return builder.String()
}

func metricValues(t *testing.T, registry *prometheus.Registry, family string) map[string]float64 {
	t.Helper()
	gathered, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	out := map[string]float64{}
	for _, metricFamily := range gathered {
		if metricFamily.GetName() != family {
			continue
		}
		for _, metric := range metricFamily.Metric {
			labels := make([]string, 0, len(metric.Label))
			for _, label := range metric.Label {
				labels = append(labels, label.GetName()+"="+label.GetValue())
			}
			out[strings.Join(labels, ",")] = metric.GetGauge().GetValue()
		}
	}
	return out
}
