package monitor

import (
	"strconv"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Roles and collectors are bounded label sets. A configured URL must never
// become a label value: provider endpoints carry API keys, and a metric label
// is the most durable place a secret can end up.
const (
	RoleNode              = "node"
	RoleReferencePrimary  = "reference_primary"
	RoleReferenceFallback = "reference_fallback"

	// NodeViewLocalReplay is the node's own replay position. It is not called
	// "confirmed": getEpochInfo.absoluteSlot is local state without commitment
	// semantics.
	NodeViewLocalReplay = "local_replay"

	// CommitmentConfirmed is the commitment the reference providers are probed
	// at, and is the only commitment a delta is computed against.
	CommitmentConfirmed = "confirmed"

	CollectorNodeRPC      = "node_rpc"
	CollectorReferenceRPC = "reference_rpc"
	CollectorState        = "state"
)

// allRoles and allCollectors exist so every series is initialized at startup.
// An absent series is indistinguishable from a scrape failure, so absence must
// never be how "down" is expressed.
var (
	allRoles      = []string{RoleNode, RoleReferencePrimary, RoleReferenceFallback}
	allCollectors = []string{CollectorNodeRPC, CollectorReferenceRPC, CollectorState}
)

// Metrics is the monitor's exported series set.
type Metrics struct {
	ProbeSuccess       *prometheus.GaugeVec
	ProbeLastSuccessAt *prometheus.GaugeVec
	NodeReplaySlot     *prometheus.GaugeVec
	ReferenceSlot      *prometheus.GaugeVec
	NodeSlotDelta      *prometheus.GaugeVec
	ReferenceDisagree  *prometheus.GaugeVec
	LastCollectionAt   prometheus.Gauge
	state              *stateMetrics
}

// NewMetrics registers the series and initializes every member, so a scrape
// taken before the first collection returns a complete set.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		ProbeSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mithril_rpc_probe_success",
			Help: "1 when the last probe of this role succeeded, 0 otherwise.",
		}, []string{"role"}),
		ProbeLastSuccessAt: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mithril_rpc_probe_last_success_timestamp_seconds",
			Help: "Unix seconds of the last successful probe of this role.",
		}, []string{"role"}),
		NodeReplaySlot: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mithril_node_replay_slot",
			Help: "Node replay position from getEpochInfo.absoluteSlot. " +
				"node_view=local_replay: this is local state with no commitment semantics.",
		}, []string{"node_view"}),
		ReferenceSlot: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mithril_reference_slot",
			Help: "Reference provider slot at confirmed commitment.",
		}, []string{"role", "commitment"}),
		NodeSlotDelta: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mithril_node_slot_delta",
			Help: "Signed provider slot minus local replay slot. Never clamped: a negative " +
				"value means the node is ahead of the reference, which is real evidence.",
		}, []string{"role", "node_view", "commitment"}),
		ReferenceDisagree: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mithril_reference_slot_disagreement_slots",
			Help: "Absolute difference between the two reference providers.",
		}, []string{"commitment"}),
		LastCollectionAt: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "mithril_monitor_last_collection_timestamp_seconds",
			Help: "Unix seconds when the latest collection cycle completed.",
		}),
		state: newStateMetrics(),
	}

	for _, c := range []prometheus.Collector{
		m.ProbeSuccess, m.ProbeLastSuccessAt, m.NodeReplaySlot,
		m.ReferenceSlot, m.NodeSlotDelta, m.ReferenceDisagree,
		m.LastCollectionAt, m.state,
	} {
		reg.MustRegister(c)
	}

	m.initialize()
	return m
}

// initialize publishes every series at zero so a scrape before the first cycle
// still returns the full inventory. The alert rules count these series; a
// missing one must read as a failure, not as an absent metric.
func (m *Metrics) initialize() {
	for _, role := range allRoles {
		m.ProbeSuccess.WithLabelValues(role).Set(0)
		m.ProbeLastSuccessAt.WithLabelValues(role).Set(0)
	}
	m.NodeReplaySlot.WithLabelValues(NodeViewLocalReplay).Set(0)
	for _, role := range []string{"primary", "fallback"} {
		m.ReferenceSlot.WithLabelValues(role, CommitmentConfirmed).Set(0)
		m.NodeSlotDelta.WithLabelValues(role, NodeViewLocalReplay, CommitmentConfirmed).Set(0)
	}
	m.ReferenceDisagree.WithLabelValues(CommitmentConfirmed).Set(0)
	m.LastCollectionAt.Set(0)
}

func (m *Metrics) setCollectSuccess(collector string, success bool) {
	m.state.setCollectSuccess(collector, success)
}

func (m *Metrics) configureIdentity(deploymentID, systemdUnit, systemdScope string) {
	m.state.configureIdentity(deploymentID, systemdUnit, systemdScope)
}

func (m *Metrics) publishState(
	manifest *DeploymentManifest,
	filesystems map[string]filesystemMeasurement,
	success bool,
) {
	m.state.publish(manifest, filesystems, success)
}

type stateSnapshot struct {
	collectSuccess map[string]bool
	targets        []ExpectedTarget
	filesystems    []ExpectedFilesystem
	measurements   map[string]filesystemMeasurement
	deploymentID   string
	systemdUnit    string
	systemdScope   string
}

// stateMetrics publishes the signed inventory and its node-exporter values
// from one immutable snapshot. A scrape therefore cannot combine stale disk
// values with a new collection verdict.
type stateMetrics struct {
	mu       sync.RWMutex
	snapshot stateSnapshot

	configuredDeploymentID string
	configuredSystemdUnit  string
	configuredSystemdScope string

	collectSuccessDesc     *prometheus.Desc
	identityInfoDesc       *prometheus.Desc
	expectedTargetDesc     *prometheus.Desc
	expectedFilesystemDesc *prometheus.Desc
	filesystemAvailDesc    *prometheus.Desc
	filesystemSizeDesc     *prometheus.Desc
}

func newStateMetrics() *stateMetrics {
	collectSuccess := make(map[string]bool, len(allCollectors))
	for _, collector := range allCollectors {
		collectSuccess[collector] = false
	}
	return &stateMetrics{
		snapshot: stateSnapshot{
			collectSuccess: collectSuccess,
			measurements:   map[string]filesystemMeasurement{},
		},
		collectSuccessDesc: prometheus.NewDesc(
			"mithril_monitor_collect_success",
			"1 when this collector completed its work. Exactly three series exist.",
			[]string{"collector"},
			nil,
		),
		identityInfoDesc: prometheus.NewDesc(
			"mithril_monitor_identity_info",
			"Signed deployment ID and bounded system service monitored by this process.",
			[]string{"signed_deployment_id", "systemd_unit", "systemd_scope"},
			nil,
		),
		expectedTargetDesc: prometheus.NewDesc(
			"mithril_expected_target",
			"Signed deployment target inventory. Every valid row is fixed to 1.",
			[]string{"target_job", "required"},
			nil,
		),
		expectedFilesystemDesc: prometheus.NewDesc(
			"mithril_expected_filesystem_role",
			"Signed filesystem role inventory. Every valid row is fixed to 1.",
			[]string{"role", "required"},
			nil,
		),
		filesystemAvailDesc: prometheus.NewDesc(
			"mithril_filesystem_avail_bytes",
			"Available bytes for a signed filesystem role.",
			[]string{"role"},
			nil,
		),
		filesystemSizeDesc: prometheus.NewDesc(
			"mithril_filesystem_size_bytes",
			"Total bytes for a signed filesystem role.",
			[]string{"role"},
			nil,
		),
	}
}

func (m *stateMetrics) Describe(ch chan<- *prometheus.Desc) {
	ch <- m.collectSuccessDesc
	ch <- m.identityInfoDesc
	ch <- m.expectedTargetDesc
	ch <- m.expectedFilesystemDesc
	ch <- m.filesystemAvailDesc
	ch <- m.filesystemSizeDesc
}

func (m *stateMetrics) Collect(ch chan<- prometheus.Metric) {
	snapshot := m.copySnapshot()
	for _, collector := range allCollectors {
		ch <- prometheus.MustNewConstMetric(
			m.collectSuccessDesc,
			prometheus.GaugeValue,
			boolGauge(snapshot.collectSuccess[collector]),
			collector,
		)
	}
	if snapshot.deploymentID != "" {
		ch <- prometheus.MustNewConstMetric(
			m.identityInfoDesc,
			prometheus.GaugeValue,
			1,
			snapshot.deploymentID,
			snapshot.systemdUnit,
			snapshot.systemdScope,
		)
	}
	for _, target := range snapshot.targets {
		ch <- prometheus.MustNewConstMetric(
			m.expectedTargetDesc,
			prometheus.GaugeValue,
			1,
			target.TargetJob,
			strconv.FormatBool(target.Required),
		)
	}
	for _, filesystem := range snapshot.filesystems {
		ch <- prometheus.MustNewConstMetric(
			m.expectedFilesystemDesc,
			prometheus.GaugeValue,
			1,
			filesystem.Role,
			strconv.FormatBool(filesystem.Required),
		)
		if measurement, ok := snapshot.measurements[filesystem.Role]; ok {
			ch <- prometheus.MustNewConstMetric(
				m.filesystemAvailDesc,
				prometheus.GaugeValue,
				measurement.available,
				filesystem.Role,
			)
			ch <- prometheus.MustNewConstMetric(
				m.filesystemSizeDesc,
				prometheus.GaugeValue,
				measurement.size,
				filesystem.Role,
			)
		}
	}
}

func (m *stateMetrics) configureIdentity(deploymentID, systemdUnit, systemdScope string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.configuredDeploymentID = deploymentID
	m.configuredSystemdUnit = systemdUnit
	m.configuredSystemdScope = systemdScope
}

func (m *stateMetrics) setCollectSuccess(collector string, success bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.snapshot.collectSuccess[collector]; !ok {
		return
	}
	m.snapshot.collectSuccess[collector] = success
}

func (m *stateMetrics) publish(
	manifest *DeploymentManifest,
	filesystems map[string]filesystemMeasurement,
	success bool,
) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if manifest != nil {
		m.snapshot.targets = append([]ExpectedTarget(nil), manifest.Targets...)
		m.snapshot.filesystems = append([]ExpectedFilesystem(nil), manifest.Filesystems...)
		if manifest.ManifestID == m.configuredDeploymentID &&
			systemdUnitPattern.MatchString(m.configuredSystemdUnit) &&
			m.configuredSystemdScope == "system" {
			m.snapshot.deploymentID = m.configuredDeploymentID
			m.snapshot.systemdUnit = m.configuredSystemdUnit
			m.snapshot.systemdScope = m.configuredSystemdScope
		}
	}
	m.snapshot.measurements = make(map[string]filesystemMeasurement, len(filesystems))
	for role, measurement := range filesystems {
		m.snapshot.measurements[role] = measurement
	}
	m.snapshot.collectSuccess[CollectorState] = success
}

func (m *stateMetrics) copySnapshot() stateSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := stateSnapshot{
		collectSuccess: make(map[string]bool, len(m.snapshot.collectSuccess)),
		targets:        append([]ExpectedTarget(nil), m.snapshot.targets...),
		filesystems:    append([]ExpectedFilesystem(nil), m.snapshot.filesystems...),
		measurements:   make(map[string]filesystemMeasurement, len(m.snapshot.measurements)),
		deploymentID:   m.snapshot.deploymentID,
		systemdUnit:    m.snapshot.systemdUnit,
		systemdScope:   m.snapshot.systemdScope,
	}
	for collector, success := range m.snapshot.collectSuccess {
		out.collectSuccess[collector] = success
	}
	for role, measurement := range m.snapshot.measurements {
		out.measurements[role] = measurement
	}
	return out
}
